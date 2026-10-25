//! Server-owned numeric bounds. Text admission never silently snaps a draft;
//! renderer positions alone may be rounded back onto the advertised grid.
use super::Refusal;
use home_wire_contract::home::registry::v1::NumberBounds;

#[derive(Clone, Debug)]
pub(crate) enum NumericInput {
    Text(String),
    Renderer(f64),
}
impl NumericInput {
    pub(super) fn admit(self, bounds: Bounds) -> Result<f64, Refusal> {
        match self {
            Self::Text(text) => {
                if text.len() > 128 || (text.contains(',') && text.contains('.')) {
                    return Err(Refusal::InvalidValue);
                }
                // A decimal comma is accepted without interpreting grouping.
                // Invalid/incomplete text stays in the native editor at the caller.
                let value = text
                    .trim()
                    .replace(',', ".")
                    .parse::<f64>()
                    .map_err(|_| Refusal::InvalidValue)?;
                bounds.admit(value)
            }
            Self::Renderer(value) => bounds.canonical(value),
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq)]
pub(super) struct Bounds {
    minimum: f64,
    maximum: f64,
    step: Option<f64>,
}

impl Bounds {
    pub fn parse(value: &NumberBounds) -> Option<Self> {
        (value.__buffa_unknown_fields.is_empty()
            && value.minimum.is_finite()
            && value.maximum.is_finite()
            && value.minimum <= value.maximum
            && value.step.is_none_or(|step| step.is_finite() && step > 0.))
        .then_some(Self {
            minimum: value.minimum,
            maximum: value.maximum,
            step: value.step,
        })
    }

    pub fn percentage(value: &NumberBounds) -> Option<Self> {
        // This is the existing light slider's presentation policy. General
        // numeric admission also supports fixed values and a step wider than
        // the interval; those are not movable percentage sliders.
        Self::parse(value).filter(|bounds| {
            bounds.minimum >= 0.
                && bounds.maximum <= 100.
                && bounds.maximum > bounds.minimum
                && bounds
                    .step
                    .is_none_or(|step| step <= bounds.maximum - bounds.minimum)
        })
    }

    pub fn minimum(self) -> f64 {
        self.minimum
    }
    pub fn maximum(self) -> f64 {
        self.maximum
    }
    pub fn step(self) -> Option<f64> {
        self.step
    }

    pub fn admit(self, value: f64) -> Result<f64, Refusal> {
        if !value.is_finite() || value < self.minimum || value > self.maximum {
            return Err(Refusal::InvalidValue);
        }
        if let Some(step) = self.step {
            let units = (value - self.minimum) / step;
            // Same tolerance as contracts.WithinNumberBounds on the server.
            if !units.is_finite() || (units - units.round()).abs() > 1e-9 {
                return Err(Refusal::InvalidValue);
            }
        }
        Ok(value)
    }

    pub fn canonical(self, value: f64) -> Result<f64, Refusal> {
        // Slint's f32 values are positions, not canonical server quantities.
        let magnitude = self.minimum.abs().max(self.maximum.abs()).max(1.);
        let tolerance = f64::from(f32::EPSILON) * magnitude * 2.;
        if !value.is_finite()
            || value < self.minimum - tolerance
            || value > self.maximum + tolerance
        {
            return Err(Refusal::InvalidValue);
        }
        let value = value.clamp(self.minimum, self.maximum);
        let canonical = match self.step {
            Some(step) => {
                let units = (self.maximum - self.minimum) / step;
                if !units.is_finite() {
                    return Err(Refusal::InvalidValue);
                }
                let maximum = if (units - units.round()).abs() <= 1e-9 {
                    units.round()
                } else {
                    units.floor()
                };
                (self.minimum + ((value - self.minimum) / step).round().min(maximum) * step)
                    .clamp(self.minimum, self.maximum)
            }
            None => value,
        };
        self.admit(canonical)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn wire(minimum: f64, maximum: f64, step: Option<f64>) -> NumberBounds {
        NumberBounds {
            minimum,
            maximum,
            step,
            ..Default::default()
        }
    }

    #[test]
    fn renderer_rounding_preserves_decimal_endpoints_and_never_leaves_the_device_grid() {
        let bounds = Bounds::percentage(&wire(0.1, 0.3, Some(0.1))).unwrap();
        assert_eq!(bounds.canonical(f64::from(0.3_f32)).unwrap(), 0.3);
        let bounds = Bounds::percentage(&wire(20., 80., Some(7.))).unwrap();
        assert_eq!(bounds.canonical(80.).unwrap(), 76.);
        assert!(matches!(bounds.canonical(81.), Err(Refusal::InvalidValue)));
    }

    #[test]
    fn signed_quantities_and_present_zero_are_admitted_without_percentage_limits() {
        let bounds = Bounds::parse(&wire(-20., 45., Some(0.5))).unwrap();
        assert_eq!(bounds.admit(-19.5).unwrap(), -19.5);
        assert_eq!(bounds.admit(0.).unwrap(), 0.);
        assert!(Bounds::percentage(&wire(-20., 45., Some(0.5))).is_none());
        assert!(Bounds::percentage(&wire(0., 150., None)).is_none());
    }

    #[test]
    fn typed_off_step_values_are_rejected_while_renderer_positions_are_canonicalized() {
        let bounds = Bounds::parse(&wire(-10., 10., Some(0.5))).unwrap();
        assert!(matches!(bounds.admit(1.1), Err(Refusal::InvalidValue)));
        assert_eq!(bounds.canonical(f64::from(1.1_f32)).unwrap(), 1.);
        assert!(matches!(bounds.admit(10.1), Err(Refusal::InvalidValue)));
    }

    #[test]
    fn fixed_intervals_and_steps_wider_than_the_interval_keep_their_valid_origin() {
        let fixed = Bounds::parse(&wire(0., 0., Some(1.))).unwrap();
        assert_eq!(fixed.admit(0.).unwrap(), 0.);
        assert!(matches!(fixed.admit(1.), Err(Refusal::InvalidValue)));
        let wide_step = Bounds::parse(&wire(-1., 1., Some(3.))).unwrap();
        assert_eq!(wide_step.admit(-1.).unwrap(), -1.);
        assert!(matches!(wide_step.admit(1.), Err(Refusal::InvalidValue)));
        assert!(Bounds::percentage(&wire(0., 0., None)).is_none());
    }

    #[test]
    fn invalid_wire_limits_and_non_finite_values_cannot_become_commands() {
        for value in [
            wire(f64::NAN, 1., None),
            wire(0., f64::INFINITY, None),
            wire(2., 1., None),
            wire(0., 1., Some(0.)),
            wire(0., 1., Some(-1.)),
            wire(0., 1., Some(f64::NAN)),
            wire(0., 1., Some(f64::INFINITY)),
        ] {
            assert!(Bounds::parse(&value).is_none());
        }
        let bounds = Bounds::parse(&wire(-5., 5., None)).unwrap();
        for value in [f64::NAN, f64::INFINITY, f64::NEG_INFINITY, -6., 6.] {
            assert!(matches!(bounds.admit(value), Err(Refusal::InvalidValue)));
            assert!(matches!(
                bounds.canonical(value),
                Err(Refusal::InvalidValue)
            ));
        }
    }
}

//! Color temperature is presented in kelvins; every command remains an integer
//! mired value admitted against the original, unmodified instance constraints.
use super::{
    Refusal,
    number::{Bounds, NumericInput},
};
use crate::NavigationStep;
use home_wire_contract::home::registry::v1::NumberBounds;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) struct Mireds(u32);
impl Mireds {
    pub fn from_wire(value: u32) -> Option<Self> {
        (1..=1000).contains(&value).then_some(Self(value))
    }
    pub fn wire(self) -> u32 {
        self.0
    }
    fn kelvins(self) -> f64 {
        1_000_000. / f64::from(self.0)
    }
    fn projected_kelvins(self) -> f64 {
        (self.kelvins() * 1000.).round() / 1000.
    }
    pub fn text(self) -> String {
        self.projected_kelvins().to_string()
    }
    pub fn observation(self) -> String {
        format!("≈ {} K · {} mireds", self.text(), self.0)
    }
}
#[derive(Clone, Debug, PartialEq)]
pub(super) struct ColorTemperature {
    server: Bounds,
    presentation: Bounds,
    // Ascending kelvins (descending mireds). There are at most 1,000 physical
    // wire values. Intersecting the exact server grid with u32 keeps fractional
    // minima/steps valid without ever inventing a linear Kelvin step.
    values: Vec<Mireds>,
}
impl ColorTemperature {
    pub fn parse(value: &NumberBounds) -> Option<Self> {
        let server = Bounds::parse(value)
            .filter(|bounds| bounds.minimum() >= 1. && bounds.maximum() <= 1000.)?;
        let values = (1..=1000)
            .rev()
            .filter(|value| server.admit(f64::from(*value)).is_ok())
            .map(Mireds)
            .collect::<Vec<_>>();
        let presentation = Bounds::parse(&NumberBounds {
            minimum: values.first()?.projected_kelvins(),
            maximum: values.last()?.projected_kelvins(),
            step: None,
            ..Default::default()
        })?;
        Some(Self {
            server,
            presentation,
            values,
        })
    }
    pub fn presentation(&self) -> Bounds {
        self.presentation
    }
    pub fn can_move(&self) -> bool {
        self.values.len() > 1
    }
    pub fn admit(&self, value: Mireds) -> Result<Mireds, Refusal> {
        self.server.admit(f64::from(value.0))?;
        Ok(value)
    }
    pub fn convert(&self, input: NumericInput) -> Result<Mireds, Refusal> {
        let kelvins = input.admit(self.presentation)?;
        // Kelvin input is a presentation coordinate. Show the actual converted
        // value before Apply; no observation or server constraint is rewritten.
        self.values
            .iter()
            .copied()
            .min_by(|a, b| {
                (a.kelvins() - kelvins)
                    .abs()
                    .total_cmp(&(b.kelvins() - kelvins).abs())
            })
            .ok_or(Refusal::Unsupported)
    }
    pub fn navigate(&self, input: NumericInput, step: NavigationStep) -> Result<Mireds, Refusal> {
        let last = self.values.len() - 1;
        let index = match step {
            NavigationStep::First => 0,
            NavigationStep::Last => last,
            NavigationStep::Next
            | NavigationStep::Previous
            | NavigationStep::PageNext
            | NavigationStep::PagePrevious => {
                let current = self.convert(input)?;
                self.values
                    .iter()
                    .position(|value| *value == current)
                    .ok_or(Refusal::InvalidValue)?
            }
        };
        let index = match step {
            NavigationStep::First => 0,
            NavigationStep::Last => last,
            NavigationStep::Next => (index + 1).min(last),
            NavigationStep::Previous => index.saturating_sub(1),
            NavigationStep::PageNext => (index + 10).min(last),
            NavigationStep::PagePrevious => index.saturating_sub(10),
        };
        Ok(self.values[index])
    }
    pub fn description(&self, input: &str) -> String {
        let bounds = format!(
            "Serveur : {} à {} mireds{}",
            self.server.minimum(),
            self.server.maximum(),
            self.server
                .step()
                .map(|step| format!(", pas {step} mireds"))
                .unwrap_or_default()
        );
        match self.convert(NumericInput::Text(input.into())) {
            Ok(value) => format!("{bounds}. Après conversion : {}.", value.observation()),
            Err(_) => bounds,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn bounds(minimum: f64, maximum: f64, step: Option<f64>) -> NumberBounds {
        NumberBounds {
            minimum,
            maximum,
            step,
            ..Default::default()
        }
    }
    #[test]
    fn kelvin_conversion_never_linearizes_or_broadens_the_server_grid() {
        let color = ColorTemperature::parse(&bounds(250., 454., Some(7.))).unwrap();
        for position in 0..=1000 {
            let display = color.presentation();
            let kelvins = display.minimum()
                + f64::from(position) / 1000. * (display.maximum() - display.minimum());
            let value = color
                .convert(NumericInput::Renderer(f64::from(kelvins as f32)))
                .unwrap();
            assert!(color.server.admit(f64::from(value.wire())).is_ok());
            assert_eq!(
                color.convert(NumericInput::Text(value.text())).unwrap(),
                value
            );
        }
        assert_eq!(
            color
                .convert(NumericInput::Text("2700".into()))
                .unwrap()
                .wire(),
            369
        );
        assert!(
            color
                .description("2700")
                .contains("2710.027 K · 369 mireds")
        );
    }
    #[test]
    fn fractional_and_fixed_grids_keep_only_representable_integer_wire_values() {
        let color = ColorTemperature::parse(&bounds(250.5, 252.5, Some(0.5))).unwrap();
        assert_eq!(
            color.values.iter().map(|v| v.wire()).collect::<Vec<_>>(),
            vec![252, 251]
        );
        let fixed = ColorTemperature::parse(&bounds(333., 333., None)).unwrap();
        assert!(!fixed.can_move());
        assert_eq!(
            fixed
                .convert(NumericInput::Text("3003.003".into()))
                .unwrap()
                .wire(),
            333
        );
        assert!(ColorTemperature::parse(&bounds(250.5, 252.5, Some(1.))).is_none());
        assert!(ColorTemperature::parse(&bounds(0., 1000., None)).is_none());
        assert!(ColorTemperature::parse(&bounds(1., 1001., None)).is_none());
    }
    #[test]
    fn invalid_kelvins_are_rejected_and_navigation_uses_adjacent_server_values() {
        let color = ColorTemperature::parse(&bounds(250., 454., Some(7.))).unwrap();
        for text in ["0", "-1", "NaN", "inf", "", "-", "4001", "1000"] {
            assert!(color.convert(NumericInput::Text(text.into())).is_err());
        }
        let current = color.convert(NumericInput::Text("2700".into())).unwrap();
        assert_eq!(
            color
                .navigate(NumericInput::Text(current.text()), NavigationStep::Next)
                .unwrap()
                .wire(),
            current.wire() - 7
        );
        assert_eq!(
            color
                .navigate(NumericInput::Text(current.text()), NavigationStep::Previous)
                .unwrap()
                .wire(),
            current.wire() + 7
        );
        assert!(color.admit(Mireds::from_wire(370).unwrap()).is_err());
        assert!(Mireds::from_wire(0).is_none());
        assert!(Mireds::from_wire(1001).is_none());
    }
}

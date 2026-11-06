//! Product editing policy for dimmers: whole percentages admitted by the server.
//! Observations and the original capability bounds remain unchanged.
use super::{Refusal, number::Bounds};
use home_wire_contract::home::api::v2::NumberBounds;

/// The slider can express a regular grid. Establish that grid from admitted
/// integers instead of rounding the server's minimum or step independently.
/// An empty intersection, or a numerically irregular one, has no slider domain.
pub(super) fn domain(server: Bounds) -> Option<Bounds> {
    let values = (0..=100)
        .map(f64::from)
        .filter(|value| server.admit(*value).is_ok())
        .collect::<Vec<_>>();
    let minimum = *values.first()?;
    let maximum = *values.last()?;
    let step = values.get(1).map_or(1., |next| next - minimum);
    if values.windows(2).any(|pair| pair[1] - pair[0] != step) {
        return None;
    }
    Bounds::parse(&NumberBounds {
        minimum,
        maximum,
        step: Some(step),
        ..Default::default()
    })
}

pub(super) fn admit(server: Bounds, value: f64) -> Result<f64, Refusal> {
    let editor = domain(server).ok_or(Refusal::Unsupported)?;
    if editor.minimum() == editor.maximum() {
        return Err(Refusal::Unsupported);
    }
    // Renderer inputs are coordinates on the product's integer editing grid.
    // The original server admission is still the final authority.
    server.admit(editor.canonical(value)?)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn bounds(minimum: f64, maximum: f64, step: Option<f64>) -> Bounds {
        Bounds::percentage_definition(&home_wire_contract::home::api::v2::PercentageBounds {
            minimum,
            maximum,
            step,
            ..Default::default()
        })
        .unwrap()
    }

    #[test]
    fn whole_percentages_intersect_continuous_and_fractional_server_grids() {
        for (server, expected) in [
            (bounds(0., 100., None), (0., 100., 1.)),
            (bounds(20.2, 80.6, None), (21., 80., 1.)),
            (bounds(20., 80., Some(7.)), (20., 76., 7.)),
            (bounds(0.5, 20., Some(2.5)), (3., 18., 5.)),
            (bounds(0.1, 10., Some(0.3)), (1., 10., 3.)),
            (bounds(0., 100., Some(0.1)), (0., 100., 1.)),
        ] {
            let editor = domain(server).unwrap();
            assert_eq!(
                (editor.minimum(), editor.maximum(), editor.step().unwrap()),
                expected
            );
            for tick in 0..=1000 {
                let input = editor.minimum()
                    + f64::from(tick) / 1000. * (editor.maximum() - editor.minimum());
                let value = admit(server, f64::from(input as f32)).unwrap();
                assert_eq!(value.fract(), 0.);
                assert_eq!(server.admit(value).unwrap(), value);
            }
        }
        assert_eq!(admit(bounds(0., 100., None), 47.25), Ok(47.));
        assert_eq!(admit(bounds(0., 100., None), 73.75), Ok(74.));
    }

    #[test]
    fn absent_or_fixed_integer_domains_cannot_emit_a_level() {
        for server in [bounds(0.1, 0.9, None), bounds(0.5, 99.5, Some(1.))] {
            assert!(domain(server).is_none());
            assert_eq!(admit(server, 1.), Err(Refusal::Unsupported));
        }
        let server = bounds(0.2, 1.2, None);
        let editor = domain(server).unwrap();
        assert_eq!((editor.minimum(), editor.maximum()), (1., 1.));
        assert_eq!(admit(server, 1.), Err(Refusal::Unsupported));
        let server = bounds(20.2, 80.6, None);
        for value in [f64::NAN, f64::INFINITY, -1., 20.2, 80.6, 101.] {
            assert_eq!(admit(server, value), Err(Refusal::InvalidValue));
        }
    }
}

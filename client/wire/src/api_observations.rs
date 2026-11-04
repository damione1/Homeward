//! Explicit conversion of every declared public quality value; unknown fields
//! or future numeric values cannot become actionable observations.
use crate::home::api::v2 as wire;
use buffa::EnumValue;
use home_client_model::api_observations::*;
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct InvalidQuality;
pub fn parse_quality(
    value: &wire::ObservationQuality,
) -> Result<ObservationQuality, InvalidQuality> {
    use EnumValue::{Known, Unknown};
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(InvalidQuality);
    }
    let availability = match value.availability {
        Known(wire::Availability::Unspecified) => Availability::Unspecified,
        Known(wire::Availability::Available) => Availability::Available,
        Known(wire::Availability::Unavailable) => Availability::Unavailable,
        Known(wire::Availability::Unknown) => Availability::Unknown,
        Unknown(_) => return Err(InvalidQuality),
    };
    let freshness = match value.freshness {
        Known(wire::Freshness::Unspecified) => Freshness::Unspecified,
        Known(wire::Freshness::Current) => Freshness::Current,
        Known(wire::Freshness::Stale) => Freshness::Stale,
        Unknown(_) => return Err(InvalidQuality),
    };
    let provenance = match value.provenance {
        Known(wire::Provenance::Unspecified) => Provenance::Unspecified,
        Known(wire::Provenance::Observed) => Provenance::Observed,
        Known(wire::Provenance::Derived) => Provenance::Derived,
        Known(wire::Provenance::Restored) => Provenance::Restored,
        Known(wire::Provenance::Estimated) => Provenance::Estimated,
        Unknown(_) => return Err(InvalidQuality),
    };
    Ok(ObservationQuality::new(availability, freshness, provenance))
}

pub fn parse_action_access(
    value: EnumValue<wire::ActionAccess>,
) -> Result<ActionAccess, InvalidQuality> {
    match value {
        EnumValue::Known(wire::ActionAccess::Unspecified) => Ok(ActionAccess::Unspecified),
        EnumValue::Known(wire::ActionAccess::Allowed) => Ok(ActionAccess::Allowed),
        EnumValue::Known(wire::ActionAccess::Denied) => Ok(ActionAccess::Denied),
        EnumValue::Unknown(_) => Err(InvalidQuality),
    }
}

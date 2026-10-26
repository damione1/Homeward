//! Admission of the two native solar result payloads. No observer or UI.
use crate::home::solar::v1 as solar;
use buffa::EnumValue;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SolarAdmissionError {
    Invalid,
}

pub struct Admitted<T> {
    wire: T,
}
impl<T> Admitted<T> {
    pub fn wire(&self) -> &T {
        &self.wire
    }
    pub fn into_wire(self) -> T {
        self.wire
    }
}

pub fn event_kind(value: EnumValue<solar::SolarEventKind>) -> bool {
    match value {
        EnumValue::Known(
            solar::SolarEventKind::Dawn
            | solar::SolarEventKind::Dusk
            | solar::SolarEventKind::Midnight
            | solar::SolarEventKind::Noon
            | solar::SolarEventKind::Rising
            | solar::SolarEventKind::Setting,
        ) => true,
        EnumValue::Known(solar::SolarEventKind::Unspecified) | EnumValue::Unknown(_) => false,
    }
}

// The admitted pair compares UTC instants without floating-point epoch math.
fn occurrence(value: &solar::SolarOccurrence) -> Result<Option<(i64, i32)>, SolarAdmissionError> {
    use solar::solar_occurrence::Occurrence;
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(SolarAdmissionError::Invalid);
    }
    match &value.occurrence {
        Some(Occurrence::At(at)) => {
            if !at.__buffa_unknown_fields.is_empty()
                || !(-62_135_596_800..=253_402_300_799).contains(&at.seconds)
                || !(0..1_000_000_000).contains(&at.nanos)
                || (at.seconds == -62_135_596_800 && at.nanos == 0)
            {
                return Err(SolarAdmissionError::Invalid);
            }
            Ok(Some((at.seconds, at.nanos)))
        }
        Some(Occurrence::Absent(reason)) => match reason {
            EnumValue::Known(solar::SolarAbsenceReason::NoEventWithinSearchHorizon) => Ok(None),
            EnumValue::Known(
                solar::SolarAbsenceReason::Unspecified | solar::SolarAbsenceReason::NoTransitOnDate,
            )
            | EnumValue::Unknown(_) => Err(SolarAdmissionError::Invalid),
        },
        None => Err(SolarAdmissionError::Invalid),
    }
}

pub fn admit_solar_value(
    value: &solar::SolarValue,
) -> Result<Admitted<solar::SolarValue>, SolarAdmissionError> {
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(SolarAdmissionError::Invalid);
    }
    let elevation = value
        .elevation_degrees
        .ok_or(SolarAdmissionError::Invalid)?;
    let azimuth = value.azimuth_degrees.ok_or(SolarAdmissionError::Invalid)?;
    let rising = value.rising.ok_or(SolarAdmissionError::Invalid)?;
    if !elevation.is_finite()
        || !(-90.0..=90.0).contains(&elevation)
        || !azimuth.is_finite()
        || !(0.0..=360.0).contains(&azimuth)
    {
        return Err(SolarAdmissionError::Invalid);
    }
    let coherent = match value.horizon {
        EnumValue::Known(solar::SolarHorizon::Above) => elevation > -0.833,
        EnumValue::Known(solar::SolarHorizon::Below) => elevation <= -0.833,
        EnumValue::Known(solar::SolarHorizon::Unspecified) | EnumValue::Unknown(_) => false,
    };
    if !coherent {
        return Err(SolarAdmissionError::Invalid);
    }
    for event in [
        &value.next_dawn,
        &value.next_dusk,
        &value.next_midnight,
        &value.next_noon,
        &value.next_rising,
        &value.next_setting,
    ] {
        occurrence(event.as_option().ok_or(SolarAdmissionError::Invalid)?)?;
    }
    let noon = occurrence(
        value
            .next_noon
            .as_option()
            .ok_or(SolarAdmissionError::Invalid)?,
    )?
    .ok_or(SolarAdmissionError::Invalid)?;
    let midnight = occurrence(
        value
            .next_midnight
            .as_option()
            .ok_or(SolarAdmissionError::Invalid)?,
    )?
    .ok_or(SolarAdmissionError::Invalid)?;
    if rising != (noon < midnight) {
        return Err(SolarAdmissionError::Invalid);
    }
    Ok(Admitted {
        wire: value.clone(),
    })
}

pub fn admit_solar_event_value(
    value: &solar::SolarEventValue,
) -> Result<Admitted<solar::SolarEventValue>, SolarAdmissionError> {
    if !value.__buffa_unknown_fields.is_empty() || !event_kind(value.event) {
        return Err(SolarAdmissionError::Invalid);
    }
    occurrence(
        value
            .occurrence
            .as_option()
            .ok_or(SolarAdmissionError::Invalid)?,
    )?;
    Ok(Admitted {
        wire: value.clone(),
    })
}

//! Helper operations use their own SQL CAS token, never an observation revision.
use super::{
    Refusal,
    number::{Bounds, NumericInput},
};
use chrono::{Datelike, NaiveDate, NaiveDateTime, NaiveTime, Timelike};
use home_wire_contract::home::{helpers::v1 as wire, registry::v1::NumberBounds};

pub(super) fn observation(
    entity: &home_wire_contract::home::state::v1::ViewEntity,
) -> Option<&wire::HelperValue> {
    use home_wire_contract::home::state::v1::observed_state::Value;
    let state = entity
        .state
        .as_option()
        .filter(|value| value.__buffa_unknown_fields.is_empty())?;
    match state.value.as_ref()? {
        Value::Helper(value) => HelperRevision::from_observation(value).map(|_| value.as_ref()),
        Value::SwitchOn(_)
        | Value::Measurement(_)
        | Value::Light(_)
        | Value::Fan(_)
        | Value::Climate(_)
        | Value::Solar(_)
        | Value::SolarEvent(_)
        | Value::Cover(_)
        | Value::Lock(_)
        | Value::Alarm(_) => None,
    }
}
pub(super) fn draft_text(value: &wire::HelperValue, kind: &Kind) -> Option<String> {
    use wire::helper_value::Value as V;
    let same_kind = match value.value.as_ref()? {
        V::Number(_) => matches!(kind, Kind::Number(_)),
        V::Counter(_) => matches!(kind, Kind::Counter { .. }),
        V::Text(_) => matches!(kind, Kind::Text { .. }),
        V::SelectedOption(_) => matches!(kind, Kind::Select(_)),
        V::Date(_) => matches!(kind, Kind::Date),
        V::Time(_) => matches!(kind, Kind::Time),
        V::Datetime(_) => matches!(kind, Kind::DateTime),
        V::Boolean(_) | V::ButtonPressedAt(_) => false,
    };
    if !same_kind {
        return None;
    }
    match value.value.as_ref()? {
        V::Number(value) => Some(value.to_string()),
        V::Counter(value) => Some(value.to_string()),
        V::Text(value) | V::SelectedOption(value) => Some(value.clone()),
        V::Date(value) => Some(format!(
            "{:04}-{:02}-{:02}",
            value.year, value.month, value.day
        )),
        V::Time(value) => Some(format!(
            "{:02}:{:02}:{:02}",
            value.hour, value.minute, value.second
        )),
        V::Datetime(value) => {
            let date = value.date.as_option()?;
            let time = value.time.as_option()?;
            Some(format!(
                "{:04}-{:02}-{:02}T{:02}:{:02}:{:02}",
                date.year, date.month, date.day, time.hour, time.minute, time.second
            ))
        }
        V::Boolean(_) | V::ButtonPressedAt(_) => None,
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) struct HelperRevision(u64);
impl HelperRevision {
    pub fn from_observation(value: &wire::HelperValue) -> Option<Self> {
        use wire::helper_value::Value as V;
        let known = match value.value.as_ref() {
            Some(
                V::Boolean(_) | V::Number(_) | V::Counter(_) | V::SelectedOption(_) | V::Text(_),
            )
            | None => true,
            Some(V::ButtonPressedAt(value)) => value.__buffa_unknown_fields.is_empty(),
            Some(V::Date(value)) => value.__buffa_unknown_fields.is_empty(),
            Some(V::Time(value)) => value.__buffa_unknown_fields.is_empty(),
            Some(V::Datetime(value)) => {
                value.__buffa_unknown_fields.is_empty()
                    && value
                        .date
                        .as_option()
                        .is_none_or(|value| value.__buffa_unknown_fields.is_empty())
                    && value
                        .time
                        .as_option()
                        .is_none_or(|value| value.__buffa_unknown_fields.is_empty())
            }
        };
        (known
            && value.__buffa_unknown_fields.is_empty()
            && value.helper_revision <= i64::MAX as u64)
            .then_some(Self(value.helper_revision))
    }
}
#[derive(Clone, Debug, PartialEq)]
pub(super) enum Kind {
    Boolean,
    Number(Bounds),
    Counter {
        minimum: Option<i64>,
        maximum: Option<i64>,
        step: i64,
    },
    Button,
    Date,
    Time,
    DateTime,
    Select(Vec<String>),
    Text {
        minimum: u32,
        maximum: u32,
    },
}
#[derive(Clone, Debug, PartialEq)]
pub(super) struct Helper {
    kind: Kind,
    operations: Vec<wire::HelperOperation>,
    unavailable: wire::HelperReason,
}
#[derive(Clone, Debug)]
pub(crate) enum HelperIntent {
    Boolean(bool),
    Number(NumericInput),
    Counter(String),
    Text(String),
    Select(String),
    Date(String),
    Time(String),
    DateTime(String),
    Operation(wire::HelperOperation),
}
impl Kind {
    fn permits(&self, operation: wire::HelperOperation) -> bool {
        use wire::HelperOperation as O;
        match operation {
            O::Set => match self {
                Self::Boolean
                | Self::Number(_)
                | Self::Counter { .. }
                | Self::Date
                | Self::Time
                | Self::DateTime
                | Self::Select(_)
                | Self::Text { .. } => true,
                Self::Button => false,
            },
            O::Toggle => matches!(self, Self::Boolean),
            O::Increment | O::Decrement | O::Reset => matches!(self, Self::Counter { .. }),
            O::Press => matches!(self, Self::Button),
            O::Next | O::Previous => matches!(self, Self::Select(_)),
            O::Unspecified => false,
        }
    }
}
impl Helper {
    pub fn restricted_to(&self, operations: &[wire::HelperOperation]) -> Self {
        Self {
            kind: self.kind.clone(),
            operations: self
                .operations
                .iter()
                .copied()
                .filter(|operation| operations.contains(operation))
                .collect(),
            unavailable: self.unavailable,
        }
    }
    pub fn kind(&self) -> &Kind {
        &self.kind
    }
    pub fn operations(&self) -> &[wire::HelperOperation] {
        &self.operations
    }
    pub fn available(&self) -> bool {
        self.unavailable == wire::HelperReason::Unspecified
    }
    pub fn input(&self, text: String, renderer: Option<f64>) -> Result<HelperIntent, Refusal> {
        let intent = match &self.kind {
            Kind::Number(_) => HelperIntent::Number(match renderer {
                Some(value) => NumericInput::Renderer(value),
                None => NumericInput::Text(text),
            }),
            Kind::Counter { .. } => HelperIntent::Counter(text),
            Kind::Text { .. } => HelperIntent::Text(text),
            Kind::Date => HelperIntent::Date(text),
            Kind::Time => HelperIntent::Time(text),
            Kind::DateTime => HelperIntent::DateTime(text),
            Kind::Select(_) => HelperIntent::Select(text),
            Kind::Boolean | Kind::Button => return Err(Refusal::Unsupported),
        };
        self.validated_action(intent.clone())?;
        Ok(intent)
    }
    pub fn parse(value: &wire::HelperConstraints) -> Option<Self> {
        if !value.__buffa_unknown_fields.is_empty()
            || value.operations.len() > 8
            || value.select_options.len() > 256
        {
            return None;
        }
        let kind = match value.kind.as_known()? {
            wire::HelperKind::Boolean => Kind::Boolean,
            wire::HelperKind::Number => Kind::Number(Bounds::parse(&NumberBounds {
                minimum: value.number_minimum?,
                maximum: value.number_maximum?,
                step: Some(value.number_step?),
                ..Default::default()
            })?),
            wire::HelperKind::Counter => {
                let step = value.counter_step?;
                if step <= 0
                    || matches!((value.counter_minimum,value.counter_maximum),(Some(minimum),Some(maximum)) if minimum>maximum)
                {
                    return None;
                }
                Kind::Counter {
                    minimum: value.counter_minimum,
                    maximum: value.counter_maximum,
                    step,
                }
            }
            wire::HelperKind::ButtonPress => Kind::Button,
            wire::HelperKind::Date => Kind::Date,
            wire::HelperKind::Time => Kind::Time,
            wire::HelperKind::Datetime => Kind::DateTime,
            wire::HelperKind::Select => {
                if value.select_options.is_empty() {
                    return None;
                }
                let mut options = Vec::with_capacity(value.select_options.len());
                for option in &value.select_options {
                    if option.is_empty()
                        || option.trim() != option
                        || option.len() > 16384
                        || option.contains('\0')
                        || options.contains(option)
                    {
                        return None;
                    }
                    options.push(option.clone());
                }
                Kind::Select(options)
            }
            wire::HelperKind::Text => {
                let minimum = value.text_minimum_length?;
                let maximum = value.text_maximum_length?;
                if minimum > maximum || maximum > 255 {
                    return None;
                }
                Kind::Text { minimum, maximum }
            }
            wire::HelperKind::Unspecified => return None,
        };
        if !matches!(kind, Kind::Number(_))
            && (value.number_minimum.is_some()
                || value.number_maximum.is_some()
                || value.number_step.is_some())
        {
            return None;
        }
        if !matches!(kind, Kind::Counter { .. })
            && (value.counter_minimum.is_some()
                || value.counter_maximum.is_some()
                || value.counter_step.is_some())
        {
            return None;
        }
        if !matches!(kind, Kind::Text { .. })
            && (value.text_minimum_length.is_some() || value.text_maximum_length.is_some())
        {
            return None;
        }
        if !matches!(kind, Kind::Select(_)) && !value.select_options.is_empty() {
            return None;
        }
        let unavailable = value.unavailable_reason.as_known()?;
        match unavailable {
            wire::HelperReason::Unspecified => {
                if value.operations.is_empty() {
                    return None;
                }
            }
            wire::HelperReason::NoAuthorizedTarget => {
                if !matches!(kind, Kind::Button) || !value.operations.is_empty() {
                    return None;
                }
            }
        }
        let mut operations = Vec::with_capacity(value.operations.len());
        for operation in &value.operations {
            let operation = operation.as_known()?;
            if !kind.permits(operation) || operations.contains(&operation) {
                return None;
            }
            operations.push(operation);
        }
        Some(Self {
            kind,
            operations,
            unavailable,
        })
    }
    pub fn arguments(
        &self,
        intent: HelperIntent,
        revision: Option<HelperRevision>,
    ) -> Result<wire::HelperAction, Refusal> {
        let revision = revision.ok_or(Refusal::Unavailable)?;
        let mut action = self.validated_action(intent)?;
        action.expected_helper_revision = Some(revision.0);
        Ok(action)
    }
    fn validated_action(&self, intent: HelperIntent) -> Result<wire::HelperAction, Refusal> {
        use wire::HelperOperation as O;
        use wire::helper_value::Value as V;
        if self.unavailable != wire::HelperReason::Unspecified {
            return Err(Refusal::Unsupported);
        }
        if intent
            .family()
            .is_some_and(|family| family != self.kind.family())
        {
            return Err(Refusal::Unsupported);
        }
        let (operation, value) = match intent {
            HelperIntent::Boolean(value) => (O::Set, Some(V::Boolean(value))),
            HelperIntent::Number(value) => {
                let Kind::Number(bounds) = self.kind else {
                    return Err(Refusal::Unsupported);
                };
                (O::Set, Some(V::Number(value.admit(bounds)?)))
            }
            HelperIntent::Counter(text) => {
                if text.len() > 32 {
                    return Err(Refusal::InvalidValue);
                }
                let value = text
                    .trim()
                    .parse::<i64>()
                    .map_err(|_| Refusal::InvalidValue)?;
                let Kind::Counter {
                    minimum,
                    maximum,
                    step,
                } = self.kind
                else {
                    return Err(Refusal::Unsupported);
                };
                if minimum.is_some_and(|minimum| value < minimum)
                    || maximum.is_some_and(|maximum| value > maximum)
                    || value % step != 0
                {
                    return Err(Refusal::InvalidValue);
                }
                (O::Set, Some(V::Counter(value)))
            }
            HelperIntent::Text(value) => {
                let Kind::Text { minimum, maximum } = self.kind else {
                    return Err(Refusal::Unsupported);
                };
                let length = value.chars().count();
                if value.len() > 16384
                    || value.contains('\0')
                    || length < minimum as usize
                    || length > maximum as usize
                {
                    return Err(Refusal::InvalidValue);
                }
                (O::Set, Some(V::Text(value)))
            }
            HelperIntent::Select(value) => {
                let Kind::Select(options) = &self.kind else {
                    return Err(Refusal::Unsupported);
                };
                if !options.contains(&value) {
                    return Err(Refusal::InvalidValue);
                }
                (O::Set, Some(V::SelectedOption(value)))
            }
            HelperIntent::Date(text) => (O::Set, Some(V::Date(date(&text)?.into()))),
            HelperIntent::Time(text) => (O::Set, Some(V::Time(clock(&text)?.into()))),
            HelperIntent::DateTime(text) => {
                if text.len() > 32 {
                    return Err(Refusal::InvalidValue);
                }
                let value = NaiveDateTime::parse_from_str(&text, "%Y-%m-%dT%H:%M:%S")
                    .or_else(|_| NaiveDateTime::parse_from_str(&text, "%Y-%m-%dT%H:%M"))
                    .map_err(|_| Refusal::InvalidValue)?;
                (
                    O::Set,
                    Some(V::Datetime(
                        wire::CivilDateTime {
                            date: civil_date(value.date())?.into(),
                            time: civil_time(value.time())?.into(),
                            ..Default::default()
                        }
                        .into(),
                    )),
                )
            }
            HelperIntent::Operation(operation) => {
                if operation == O::Set {
                    return Err(Refusal::InvalidValue);
                }
                (operation, None)
            }
        };
        if !self.operations.contains(&operation) {
            return Err(Refusal::Unsupported);
        }
        Ok(wire::HelperAction {
            operation: operation.into(),
            value: value
                .map(|value| {
                    wire::HelperValue {
                        value: Some(value),
                        helper_revision: 0,
                        ..Default::default()
                    }
                    .into()
                })
                .unwrap_or_default(),
            ..Default::default()
        })
    }
}
impl Kind {
    fn family(&self) -> wire::HelperKind {
        match self {
            Self::Boolean => wire::HelperKind::Boolean,
            Self::Number(_) => wire::HelperKind::Number,
            Self::Counter { .. } => wire::HelperKind::Counter,
            Self::Button => wire::HelperKind::ButtonPress,
            Self::Date => wire::HelperKind::Date,
            Self::Time => wire::HelperKind::Time,
            Self::DateTime => wire::HelperKind::Datetime,
            Self::Select(_) => wire::HelperKind::Select,
            Self::Text { .. } => wire::HelperKind::Text,
        }
    }
}
impl HelperIntent {
    fn family(&self) -> Option<wire::HelperKind> {
        Some(match self {
            Self::Boolean(_) => wire::HelperKind::Boolean,
            Self::Number(_) => wire::HelperKind::Number,
            Self::Counter(_) => wire::HelperKind::Counter,
            Self::Text(_) => wire::HelperKind::Text,
            Self::Select(_) => wire::HelperKind::Select,
            Self::Date(_) => wire::HelperKind::Date,
            Self::Time(_) => wire::HelperKind::Time,
            Self::DateTime(_) => wire::HelperKind::Datetime,
            Self::Operation(_) => return None,
        })
    }
}
fn date(text: &str) -> Result<wire::CivilDate, Refusal> {
    if text.len() > 16 {
        return Err(Refusal::InvalidValue);
    }
    civil_date(NaiveDate::parse_from_str(text, "%Y-%m-%d").map_err(|_| Refusal::InvalidValue)?)
}
fn civil_date(value: NaiveDate) -> Result<wire::CivilDate, Refusal> {
    if !(1..=9999).contains(&value.year()) {
        return Err(Refusal::InvalidValue);
    }
    Ok(wire::CivilDate {
        year: value.year() as u32,
        month: value.month(),
        day: value.day(),
        ..Default::default()
    })
}
fn clock(text: &str) -> Result<wire::CivilTime, Refusal> {
    if text.len() > 16 {
        return Err(Refusal::InvalidValue);
    }
    civil_time(
        NaiveTime::parse_from_str(text, "%H:%M:%S")
            .or_else(|_| NaiveTime::parse_from_str(text, "%H:%M"))
            .map_err(|_| Refusal::InvalidValue)?,
    )
}
fn civil_time(value: NaiveTime) -> Result<wire::CivilTime, Refusal> {
    if value.nanosecond() != 0 {
        return Err(Refusal::InvalidValue);
    }
    Ok(wire::CivilTime {
        hour: value.hour(),
        minute: value.minute(),
        second: value.second(),
        ..Default::default()
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use buffa::Message;
    use wire::{HelperKind as K, HelperOperation as O, helper_value::Value as V};
    fn constraints(kind: K, operations: &[O]) -> wire::HelperConstraints {
        wire::HelperConstraints {
            kind: kind.into(),
            operations: operations.iter().copied().map(Into::into).collect(),
            ..Default::default()
        }
    }
    fn revision(value: u64) -> Option<HelperRevision> {
        HelperRevision::from_observation(&wire::HelperValue {
            helper_revision: value,
            ..Default::default()
        })
    }
    #[test]
    fn zero_sql_revision_and_present_false_are_preserved_without_observation_cas() {
        let helper = Helper::parse(&constraints(K::Boolean, &[O::Set, O::Toggle])).unwrap();
        let action = helper
            .arguments(HelperIntent::Boolean(false), revision(0))
            .unwrap();
        assert_eq!(action.expected_helper_revision, Some(0));
        assert_eq!(action.value.value, Some(V::Boolean(false)));
        assert_eq!(action.value.helper_revision, 0);
        assert!(matches!(
            helper.arguments(HelperIntent::Boolean(true), None),
            Err(Refusal::Unavailable)
        ));
        assert!(revision(i64::MAX as u64 + 1).is_none());
        assert!(
            helper
                .arguments(HelperIntent::Operation(O::Toggle), revision(0))
                .unwrap()
                .value
                .as_option()
                .is_none()
        );
        assert!(matches!(
            helper.arguments(HelperIntent::Operation(O::Set), revision(0)),
            Err(Refusal::InvalidValue)
        ));
    }
    #[test]
    fn signed_number_limits_and_step_are_instance_specific_and_zero_is_not_absence() {
        let mut value = constraints(K::Number, &[O::Set]);
        value.number_minimum = Some(-10.);
        value.number_maximum = Some(10.);
        value.number_step = Some(0.5);
        let helper = Helper::parse(&value).unwrap();
        let action = helper
            .arguments(
                HelperIntent::Number(NumericInput::Text("0".into())),
                revision(3),
            )
            .unwrap();
        assert_eq!(action.value.value, Some(V::Number(0.)));
        assert!(matches!(
            helper.arguments(
                HelperIntent::Number(NumericInput::Text("0.1".into())),
                revision(3)
            ),
            Err(Refusal::InvalidValue)
        ));
        value.number_step = Some(0.);
        assert!(Helper::parse(&value).is_none());
    }
    #[test]
    fn counters_keep_all_64_bits_and_set_alignment_is_separate_from_relative_operations() {
        let mut value = constraints(K::Counter, &[O::Set, O::Increment, O::Decrement, O::Reset]);
        value.counter_minimum = Some(0);
        value.counter_maximum = Some(i64::MAX);
        value.counter_step = Some(2);
        let helper = Helper::parse(&value).unwrap();
        let action = helper
            .arguments(
                HelperIntent::Counter((i64::MAX - 1).to_string()),
                revision(4),
            )
            .unwrap();
        assert_eq!(action.value.value, Some(V::Counter(i64::MAX - 1)));
        assert!(matches!(
            helper.arguments(HelperIntent::Counter("3".into()), revision(4)),
            Err(Refusal::InvalidValue)
        ));
        assert!(matches!(
            helper.arguments(
                HelperIntent::Counter("9223372036854775808".into()),
                revision(4)
            ),
            Err(Refusal::InvalidValue)
        ));
        let increment = helper
            .arguments(HelperIntent::Operation(O::Increment), revision(4))
            .unwrap();
        assert_eq!(increment.operation.as_known(), Some(O::Increment));
        assert!(increment.value.as_option().is_none());
        // SET is aligned to zero, never to a nonaligned instance minimum.
        value.counter_minimum = Some(1);
        let helper = Helper::parse(&value).unwrap();
        assert!(
            helper
                .arguments(HelperIntent::Counter("2".into()), revision(4))
                .is_ok()
        );
        value.counter_step = Some(1);
        assert_eq!(
            Helper::parse(&value)
                .unwrap()
                .arguments(HelperIntent::Counter(i64::MAX.to_string()), revision(4))
                .unwrap()
                .value
                .value,
            Some(V::Counter(i64::MAX))
        );
    }
    #[test]
    fn text_and_select_keep_empty_unicode_and_exact_open_values_with_server_bounds() {
        let mut value = constraints(K::Text, &[O::Set]);
        value.text_minimum_length = Some(0);
        value.text_maximum_length = Some(2);
        let helper = Helper::parse(&value).unwrap();
        assert_eq!(
            helper
                .arguments(HelperIntent::Text(String::new()), revision(8))
                .unwrap()
                .value
                .value,
            Some(V::Text(String::new()))
        );
        assert!(
            helper
                .arguments(HelperIntent::Text("é🦊".into()), revision(8))
                .is_ok()
        );
        assert!(matches!(
            helper.arguments(HelperIntent::Text("abc".into()), revision(8)),
            Err(Refusal::InvalidValue)
        ));
        assert!(matches!(
            helper.arguments(HelperIntent::Text("\0".into()), revision(8)),
            Err(Refusal::InvalidValue)
        ));
        let mut value = constraints(K::Select, &[O::Set, O::Next, O::Previous]);
        let long = "z".repeat(1024);
        value.select_options = vec!["custom user option".into(), long.clone()];
        let helper = Helper::parse(&value).unwrap();
        assert_eq!(
            helper
                .arguments(HelperIntent::Select(long.clone()), revision(8))
                .unwrap()
                .value
                .value,
            Some(V::SelectedOption(long))
        );
        assert!(matches!(
            helper.arguments(
                HelperIntent::Select("Custom user option".into()),
                revision(8)
            ),
            Err(Refusal::InvalidValue)
        ));
        assert!(
            helper
                .arguments(HelperIntent::Operation(O::Next), revision(8))
                .is_ok()
        );
    }
    #[test]
    fn civil_dates_and_times_are_validated_without_timezone_or_leap_second_invention() {
        let date = Helper::parse(&constraints(K::Date, &[O::Set])).unwrap();
        assert!(
            date.arguments(HelperIntent::Date("2028-02-29".into()), revision(2))
                .is_ok()
        );
        assert!(matches!(
            date.arguments(HelperIntent::Date("2027-02-29".into()), revision(2)),
            Err(Refusal::InvalidValue)
        ));
        let clock = Helper::parse(&constraints(K::Time, &[O::Set])).unwrap();
        let action = clock
            .arguments(HelperIntent::Time("00:00".into()), revision(2))
            .unwrap();
        assert!(
            matches!(&action.value.value,Some(V::Time(value)) if value.hour==0 && value.minute==0 && value.second==0)
        );
        assert!(matches!(
            clock.arguments(HelperIntent::Time("23:59:60".into()), revision(2)),
            Err(Refusal::InvalidValue)
        ));
        let datetime = Helper::parse(&constraints(K::Datetime, &[O::Set])).unwrap();
        assert!(
            datetime
                .arguments(
                    HelperIntent::DateTime("2026-11-01T01:30".into()),
                    revision(2)
                )
                .is_ok()
        );
        assert!(matches!(
            datetime.arguments(
                HelperIntent::DateTime("2026-11-01T01:30Z".into()),
                revision(2)
            ),
            Err(Refusal::InvalidValue)
        ));
    }
    #[test]
    fn button_with_no_authorized_target_and_future_or_mismatched_operations_are_inert() {
        let mut value = constraints(K::ButtonPress, &[]);
        value.unavailable_reason = wire::HelperReason::NoAuthorizedTarget.into();
        let helper = Helper::parse(&value).unwrap();
        assert!(matches!(
            helper.arguments(HelperIntent::Operation(O::Press), revision(1)),
            Err(Refusal::Unsupported)
        ));
        assert!(Helper::parse(&constraints(K::Number, &[O::Press])).is_none());
        assert!(Helper::parse(&constraints(K::Boolean, &[O::Set, O::Set])).is_none());
        let mut value = constraints(K::Boolean, &[O::Set]);
        value.number_minimum = Some(0.);
        assert!(Helper::parse(&value).is_none());
        let mut value = constraints(K::Boolean, &[O::Set]);
        value.operations = vec![buffa::EnumValue::Unknown(99)];
        assert!(Helper::parse(&value).is_none());
        let mut value = constraints(K::Boolean, &[O::Set]);
        value.kind = buffa::EnumValue::Unknown(99);
        assert!(Helper::parse(&value).is_none());
        let unknown = wire::HelperValue::decode_from_slice(&[0xa0, 0x06, 0x01]).unwrap();
        assert!(HelperRevision::from_observation(&unknown).is_none());
    }
}

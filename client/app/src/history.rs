//! Homeward's history adapter. Server results stay immutable; chart coordinates
//! are disposable projections and never become energy or monetary arithmetic.
use buffa::Message;
use home_client_cache::history::{Content, Pages, Snapshot};
use home_wire_contract::home::{history::v1 as wire, state::v1 as state};
use slint_kit_pro::charts::{Sample, Series, SeriesId, Timestamp};
use std::{collections::BTreeMap, sync::Arc};

const MAX_GROUPS: usize = 128;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Statistic {
    Observation,
    Minimum,
    Maximum,
    Mean,
    Contribution,
}
impl Statistic {
    pub fn label(self) -> &'static str {
        match self {
            Self::Observation => "Observation source",
            Self::Minimum => "Minimum serveur",
            Self::Maximum => "Maximum serveur",
            Self::Mean => "Moyenne serveur",
            Self::Contribution => "Contribution serveur",
        }
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SourceRef {
    Observation { page: usize, point: usize },
    Aggregate { page: usize, point: usize },
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ProjectionFailure {
    WrongStatistic,
    InvalidDecimal,
    InvalidTime,
    InvalidNumeric,
    GroupCapacity,
}

/// Exact server text. No arithmetic API; even decimals wider than the field
/// editor's 96-bit range remain intact. Floating conversion is only for pixels.
#[derive(Clone, Debug, PartialEq)]
pub enum Value {
    Number(f64),
    Decimal(Arc<str>),
}
impl Value {
    pub fn text(&self) -> String {
        match self {
            Self::Number(number) => number.to_string(),
            Self::Decimal(source) => source.to_string(),
        }
    }
    fn coordinate(&self) -> Result<f64, ProjectionFailure> {
        let value = match self {
            Self::Number(number) => *number,
            Self::Decimal(source) => {
                // Reuse field-domain admission when representable. Wider exact
                // source strings use the same canonical grammar, without
                // rounding them into the editor's narrower storage range.
                if slint_kit::DecimalValue::parse_canonical(source).is_err()
                    && !canonical_decimal(source)
                {
                    return Err(ProjectionFailure::InvalidDecimal);
                }
                source
                    .parse()
                    .map_err(|_| ProjectionFailure::InvalidDecimal)?
            }
        };
        if !value.is_finite() {
            return Err(ProjectionFailure::InvalidNumeric);
        }
        Ok(value)
    }
}
fn canonical_decimal(source: &str) -> bool {
    let unsigned = source.strip_prefix(['-', '+']).unwrap_or(source);
    let mut parts = unsigned.split('.');
    let digits = |part: &str| !part.is_empty() && part.bytes().all(|byte| byte.is_ascii_digit());
    parts.next().is_some_and(digits) && parts.next().is_none_or(digits) && parts.next().is_none()
}

pub struct Group {
    pub descriptor: wire::NumericDescriptor,
    pub quality: state::Quality,
    pub series: Arc<Series>,
    sources: Vec<SourceRef>,
    values: Vec<Value>,
}
impl Group {
    /// Stable sorted display index, including every coincident observation.
    /// Resolves back to the untouched page and logical admission position.
    pub fn source(&self, index: usize) -> Option<SourceRef> {
        self.sources.get(index).copied()
    }
    pub fn value(&self, index: usize) -> Option<&Value> {
        self.values.get(index)
    }
}
pub struct Data {
    pub snapshot: Arc<Snapshot>,
    pub statistic: Statistic,
    pub groups: Vec<Group>,
    pub non_numeric_or_absent: usize,
    pub incomplete_coverage: bool,
    pub unknown_utc: bool,
}
struct Pending {
    descriptor: wire::NumericDescriptor,
    quality: state::Quality,
    points: Vec<(Timestamp, f64, SourceRef, Value)>,
}
impl Data {
    pub fn from_snapshot(
        snapshot: Arc<Snapshot>,
        statistic: Statistic,
    ) -> Result<Self, ProjectionFailure> {
        let mut groups = BTreeMap::<Vec<u8>, Pending>::new();
        let mut absent = 0;
        let mut incomplete = false;
        let mut unknown_utc = false;
        let mut record_coverage = |coverage: &wire::Coverage| {
            incomplete |= coverage.verdict != wire::CoverageVerdict::CompleteUnderPolicy
                || coverage.analytical_tail_unproven
                || coverage.recorder_tail.is_set();
            unknown_utc |= !coverage.logical.gaps.is_empty() || coverage.recorder_tail.is_set();
        };
        match &snapshot.content {
            Content::NotRetained(_) => {}
            Content::Retained(Pages::Observations(pages)) => {
                if statistic != Statistic::Observation {
                    return Err(ProjectionFailure::WrongStatistic);
                }
                for (page_index, page) in pages.iter().enumerate() {
                    record_coverage(&page.coverage);
                    for (index, point) in page.points.iter().enumerate() {
                        match &point.state.value {
                            Some(state::observed_state::Value::Measurement(measurement)) => {
                                let descriptor = point
                                    .descriptor
                                    .as_option()
                                    .ok_or(ProjectionFailure::InvalidNumeric)?;
                                // A historical unit must come from its historical
                                // descriptor, never the entity's current schema.
                                if descriptor.unit != measurement.unit {
                                    return Err(ProjectionFailure::InvalidNumeric);
                                }
                                let value = match &measurement.decimal_value {
                                    Some(source) => Value::Decimal(Arc::from(source.as_str())),
                                    None => Value::Number(measurement.value),
                                };
                                add(
                                    &mut groups,
                                    descriptor,
                                    &point.state.quality,
                                    timestamp(&point.state.received_at)?,
                                    value,
                                    SourceRef::Observation {
                                        page: page_index,
                                        point: index,
                                    },
                                )?;
                            }
                            Some(
                                state::observed_state::Value::SwitchOn(_)
                                | state::observed_state::Value::Light(_)
                                | state::observed_state::Value::Fan(_)
                                | state::observed_state::Value::Climate(_)
                                | state::observed_state::Value::Helper(_)
                                | state::observed_state::Value::Solar(_)
                                | state::observed_state::Value::SolarEvent(_)
                                | state::observed_state::Value::Cover(_)
                                | state::observed_state::Value::Lock(_)
                                | state::observed_state::Value::Alarm(_)
                                | state::observed_state::Value::Weather(_)
                                | state::observed_state::Value::Presence(_)
                                | state::observed_state::Value::PresenceDetails(_),
                            )
                            | None => absent += 1,
                        }
                    }
                }
            }
            Content::Retained(Pages::Hourly(pages)) => {
                if statistic == Statistic::Observation {
                    return Err(ProjectionFailure::WrongStatistic);
                }
                for (page_index, page) in pages.iter().enumerate() {
                    record_coverage(&page.coverage);
                    for (index, point) in page.points.iter().enumerate() {
                        let value = match statistic {
                            Statistic::Minimum => point.minimum,
                            Statistic::Maximum => point.maximum,
                            Statistic::Mean => point.mean,
                            Statistic::Contribution => point.contribution,
                            Statistic::Observation => {
                                return Err(ProjectionFailure::WrongStatistic);
                            }
                        };
                        if let Some(value) = value {
                            add(
                                &mut groups,
                                &point.descriptor,
                                &point.quality,
                                timestamp(&point.bucket.start)?,
                                Value::Number(value),
                                SourceRef::Aggregate {
                                    page: page_index,
                                    point: index,
                                },
                            )?;
                        } else {
                            absent += 1;
                        }
                    }
                }
            }
        }
        let mut ready = Vec::with_capacity(groups.len());
        for (index, (_, mut group)) in groups.into_iter().enumerate() {
            // Stable visual ordering only. Source pages/positions and equal-time
            // ordering remain intact; Scatter conveys no logical continuity.
            group.points.sort_by_key(|point| point.0);
            let samples = group
                .points
                .iter()
                .map(|p| Sample::new(p.0, p.1).map_err(|_| ProjectionFailure::InvalidNumeric))
                .collect::<Result<Vec<_>, _>>()?;
            let id = SeriesId::new(format!("history-group-{index}"))
                .map_err(|_| ProjectionFailure::InvalidNumeric)?;
            let series =
                Series::new(id, samples, vec![]).map_err(|_| ProjectionFailure::InvalidNumeric)?;
            let (sources, values) = group.points.into_iter().map(|p| (p.2, p.3)).unzip();
            ready.push(Group {
                descriptor: group.descriptor,
                quality: group.quality,
                series: Arc::new(series),
                sources,
                values,
            });
        }
        Ok(Self {
            snapshot,
            statistic,
            groups: ready,
            non_numeric_or_absent: absent,
            incomplete_coverage: incomplete,
            unknown_utc,
        })
    }
}
fn add(
    groups: &mut BTreeMap<Vec<u8>, Pending>,
    descriptor: &wire::NumericDescriptor,
    quality: &state::Quality,
    at: Timestamp,
    value: Value,
    source: SourceRef,
) -> Result<(), ProjectionFailure> {
    let mut key = descriptor.encode_to_vec();
    let mut framed = (key.len() as u32).to_be_bytes().to_vec();
    framed.append(&mut key);
    framed.extend(quality.encode_to_vec());
    if !groups.contains_key(&framed) && groups.len() >= MAX_GROUPS {
        return Err(ProjectionFailure::GroupCapacity);
    }
    let coordinate = value.coordinate()?;
    groups
        .entry(framed)
        .or_insert_with(|| Pending {
            descriptor: descriptor.clone(),
            quality: quality.clone(),
            points: vec![],
        })
        .points
        .push((at, coordinate, source, value));
    Ok(())
}
pub fn timestamp(
    time: &buffa_types::google::protobuf::Timestamp,
) -> Result<Timestamp, ProjectionFailure> {
    let nanos = u32::try_from(time.nanos).map_err(|_| ProjectionFailure::InvalidTime)?;
    Timestamp::from_unix_parts(time.seconds, nanos).map_err(|_| ProjectionFailure::InvalidTime)
}

#[cfg(test)]
mod tests;

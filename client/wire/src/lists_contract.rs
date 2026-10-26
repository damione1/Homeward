//! Structural admission for the generated lists contract. The Go lists service
//! remains authoritative for permissions, current revisions, SQL and zones.

use crate::home::lists::v1 as lists;
use buffa::EnumValue;
use buffa_types::google::protobuf::Timestamp;
use home_client_model::lists::{ItemUid, ListId};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AdmissionError {
    Invalid,
}

pub struct Admitted<T> {
    wire: T,
}
impl<T> Admitted<T> {
    pub fn wire(&self) -> &T {
        &self.wire
    }
}

fn list_id(value: &lists::ListId) -> bool {
    value.value.parse::<ListId>().is_ok() && value.__buffa_unknown_fields.is_empty()
}
fn item_uid(value: &lists::ItemUid) -> bool {
    value.value.parse::<ItemUid>().is_ok() && value.__buffa_unknown_fields.is_empty()
}
fn ha_alias(value: &str) -> bool {
    let Some((domain, name)) = value.split_once('.') else {
        return false;
    };
    value.len() <= 255
        && !domain.is_empty()
        && !name.is_empty()
        && !name.contains('.')
        && value
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'_' || b == b'.')
}
fn timestamp(value: &Timestamp) -> bool {
    (-62_135_596_800..=253_402_300_799).contains(&value.seconds)
        && (0..1_000_000_000).contains(&value.nanos)
        && value.nanos % 1_000 == 0
        && value.__buffa_unknown_fields.is_empty()
}
fn name(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 512
        && !value.bytes().any(|b| matches!(b, b'\0' | b'\r' | b'\n'))
}
fn civil_date(value: &str) -> bool {
    let raw = value.as_bytes();
    if raw.len() != 10 || raw[4] != b'-' || raw[7] != b'-' {
        return false;
    }
    if !raw
        .iter()
        .enumerate()
        .all(|(i, b)| i == 4 || i == 7 || b.is_ascii_digit())
    {
        return false;
    }
    let (Some(year), Some(month), Some(day)) = (
        value[0..4].parse::<u32>().ok(),
        value[5..7].parse::<u32>().ok(),
        value[8..10].parse::<u32>().ok(),
    ) else {
        return false;
    };
    if year == 0 || !(1..=12).contains(&month) {
        return false;
    }
    let leap = year % 4 == 0 && (year % 100 != 0 || year % 400 == 0);
    let days = match month {
        2 if leap => 29,
        2 => 28,
        4 | 6 | 9 | 11 => 30,
        1 | 3 | 5 | 7 | 8 | 10 | 12 => 31,
        _ => return false,
    };
    (1..=days).contains(&day)
}
fn list_revision(value: &lists::ListRevision, id: &lists::ListId) -> bool {
    value.number > 0
        && value.number <= i64::MAX as u64
        && value
            .list_id
            .as_option()
            .is_some_and(|v| list_id(v) && v.value == id.value)
        && value.__buffa_unknown_fields.is_empty()
}
fn item_revision(value: &lists::ItemRevision, id: &lists::ListId, uid: &lists::ItemUid) -> bool {
    value.number > 0
        && value.number <= i64::MAX as u64
        && value
            .list_id
            .as_option()
            .is_some_and(|v| list_id(v) && v.value == id.value)
        && value
            .item_uid
            .as_option()
            .is_some_and(|v| item_uid(v) && v.value == uid.value)
        && value.__buffa_unknown_fields.is_empty()
}

pub fn admit_item_payload(
    value: &lists::ItemPayload,
) -> Result<Admitted<lists::ItemPayload>, AdmissionError> {
    use lists::item_payload::Due;
    let due = match &value.due {
        None => true,
        Some(Due::DueDate(text)) => civil_date(text),
        Some(Due::DueInstant(instant)) => {
            instant.at.as_option().is_some_and(timestamp)
                && !instant.zone.is_empty()
                && instant.zone.len() <= 128
                && instant.zone != "Local"
                && !instant
                    .zone
                    .bytes()
                    .any(|b| matches!(b, b'\0' | b'\r' | b'\n'))
                && instant.__buffa_unknown_fields.is_empty()
        }
    };
    let description = value
        .description
        .as_ref()
        .is_none_or(|text| text.len() <= 4096 && !text.contains('\0'));
    let completed_at = value
        .completed_at
        .as_option()
        .is_none_or(|at| value.completed == Some(true) && timestamp(at));
    if name(&value.summary)
        && value.completed.is_some()
        && description
        && due
        && completed_at
        && value.__buffa_unknown_fields.is_empty()
    {
        Ok(Admitted {
            wire: value.clone(),
        })
    } else {
        Err(AdmissionError::Invalid)
    }
}

pub fn admit_list_snapshot(
    value: &lists::ListSnapshot,
) -> Result<Admitted<lists::ListSnapshot>, AdmissionError> {
    let origin = match value.origin {
        EnumValue::Known(
            lists::ListOrigin::LocalTodo
            | lists::ListOrigin::ShoppingList
            | lists::ListOrigin::Native,
        ) => true,
        EnumValue::Known(lists::ListOrigin::Unspecified) | EnumValue::Unknown(_) => false,
    };
    let valid = value.id.as_option().is_some_and(|id| {
        list_id(id)
            && value
                .revision
                .as_option()
                .is_some_and(|revision| list_revision(revision, id))
    });
    let alias = match value.origin {
        EnumValue::Known(lists::ListOrigin::Native) => value.legacy_alias.is_none(),
        EnumValue::Known(lists::ListOrigin::LocalTodo | lists::ListOrigin::ShoppingList) => {
            value.legacy_alias.as_ref().is_some_and(|s| ha_alias(s))
        }
        EnumValue::Known(lists::ListOrigin::Unspecified) | EnumValue::Unknown(_) => false,
    };
    if valid
        && origin
        && alias
        && name(&value.name)
        && value.updated_at.as_option().is_some_and(timestamp)
        && value.__buffa_unknown_fields.is_empty()
    {
        Ok(Admitted {
            wire: value.clone(),
        })
    } else {
        Err(AdmissionError::Invalid)
    }
}
pub fn admit_item_snapshot(
    value: &lists::ItemSnapshot,
) -> Result<Admitted<lists::ItemSnapshot>, AdmissionError> {
    let valid = value.list_id.as_option().is_some_and(|id| {
        list_id(id)
            && value.uid.as_option().is_some_and(|uid| {
                item_uid(uid)
                    && value
                        .revision
                        .as_option()
                        .is_some_and(|revision| item_revision(revision, id, uid))
            })
    });
    if valid
        && value.position >= 0
        && value
            .value
            .as_option()
            .is_some_and(|payload| admit_item_payload(payload).is_ok())
        && value.__buffa_unknown_fields.is_empty()
    {
        Ok(Admitted {
            wire: value.clone(),
        })
    } else {
        Err(AdmissionError::Invalid)
    }
}
pub fn admit_receipt(
    value: &lists::MutationReceipt,
) -> Result<Admitted<lists::MutationReceipt>, AdmissionError> {
    let valid = value.list_id.as_option().is_some_and(|id| {
        list_id(id)
            && value
                .list_revision
                .as_option()
                .is_some_and(|revision| list_revision(revision, id))
            && match (value.item_uid.as_option(), value.item_revision.as_option()) {
                (None, None) => true,
                (Some(uid), Some(revision)) => item_uid(uid) && item_revision(revision, id, uid),
                (None, Some(_)) | (Some(_), None) => false,
            }
    });
    if valid && value.__buffa_unknown_fields.is_empty() {
        Ok(Admitted {
            wire: value.clone(),
        })
    } else {
        Err(AdmissionError::Invalid)
    }
}
pub fn admit_error(value: &lists::ListError) -> Result<Admitted<lists::ListError>, AdmissionError> {
    let known = match value.code {
        EnumValue::Known(
            lists::ListErrorCode::Invalid
            | lists::ListErrorCode::Denied
            | lists::ListErrorCode::NotFound
            | lists::ListErrorCode::Conflict
            | lists::ListErrorCode::StaleCursor
            | lists::ListErrorCode::Capacity
            | lists::ListErrorCode::Unavailable
            | lists::ListErrorCode::CommitUnknown
            | lists::ListErrorCode::CommittedResponseWithheld
            | lists::ListErrorCode::Closed,
        ) => true,
        EnumValue::Known(lists::ListErrorCode::Unspecified) | EnumValue::Unknown(_) => false,
    };
    let path = value.field_path.as_ref().is_none_or(|s| {
        !s.is_empty()
            && s.len() <= 128
            && s.bytes()
                .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b"_.[]".contains(&b))
    });
    if known && path && value.__buffa_unknown_fields.is_empty() {
        Ok(Admitted {
            wire: value.clone(),
        })
    } else {
        Err(AdmissionError::Invalid)
    }
}
pub fn admit_watch_response(
    value: &lists::WatchListsResponse,
) -> Result<Admitted<lists::WatchListsResponse>, AdmissionError> {
    use lists::watch_lists_response::Frame;
    let valid = match &value.frame {
        Some(Frame::Ready(ready)) => ready.__buffa_unknown_fields.is_empty(),
        Some(Frame::Changed(change)) => {
            change.list_id.as_option().is_some_and(|id| {
                list_id(id)
                    && change
                        .revision
                        .as_option()
                        .is_some_and(|revision| list_revision(revision, id))
            }) && change.__buffa_unknown_fields.is_empty()
        }
        Some(Frame::ResyncRequired(resync)) => resync.__buffa_unknown_fields.is_empty(),
        None => false,
    };
    if value.stream_sequence > 0 && valid && value.__buffa_unknown_fields.is_empty() {
        Ok(Admitted {
            wire: value.clone(),
        })
    } else {
        Err(AdmissionError::Invalid)
    }
}

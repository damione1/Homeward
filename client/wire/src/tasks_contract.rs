//! Current task wire/domain boundary. Unknown response fields invalidate the
//! protected projection; commands are rebuilt from admitted model values.
use crate::home::api::v2 as wire;
use buffa::EnumValue;
use buffa_types::google::protobuf::Timestamp;
use home_client_model::{
    api_tasks::{
        TaskContent, TaskDescription, TaskDue, TaskDueInstant, TaskError, TaskEtag, TaskListName,
        TaskListOrigin, TaskListResource, TaskListRevision, TaskName, TaskResource, TaskRevision,
        TaskText,
    },
    calendars::CivilDate,
    local_time::{DisplayTimeZone, UtcInstant},
};

fn instant(value: &Timestamp) -> Result<UtcInstant, TaskError> {
    if !value.__buffa_unknown_fields.is_empty()
        || value.nanos % 1000 != 0
        || (value.seconds == -62_135_596_800 && value.nanos == 0)
    {
        return Err(TaskError::Instant);
    }
    UtcInstant::new(value.seconds, value.nanos).map_err(|_| TaskError::Instant)
}
fn date(value: &crate::google::r#type::Date) -> Result<CivilDate, TaskError> {
    if !value.__buffa_unknown_fields.is_empty()
        || !(1..=9999).contains(&value.year)
        || !(1..=12).contains(&value.month)
        || !(1..=31).contains(&value.day)
    {
        return Err(TaskError::Content);
    }
    format!("{:04}-{:02}-{:02}", value.year, value.month, value.day)
        .parse()
        .map_err(|_| TaskError::Content)
}
pub fn parse_task_content(value: &wire::Task) -> Result<TaskContent, TaskError> {
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(TaskError::Content);
    }
    let summary = TaskText::try_from(value.summary.as_str())?;
    let description = value
        .description
        .as_deref()
        .map(TaskDescription::try_from)
        .transpose()?;
    let due = match &value.due {
        None => TaskDue::None,
        Some(wire::task::Due::DueDate(value)) => TaskDue::Date(date(value)?),
        Some(wire::task::Due::DueInstant(value)) => {
            if !value.__buffa_unknown_fields.is_empty() {
                return Err(TaskError::Content);
            }
            let time = instant(value.due_time.as_option().ok_or(TaskError::Instant)?)?;
            let zone: DisplayTimeZone = value.time_zone.parse().map_err(|_| TaskError::Instant)?;
            TaskDue::Instant(TaskDueInstant::new(time, zone)?)
        }
    };
    Ok(TaskContent::new(summary, value.completed, description, due))
}
pub fn parse_task_list(value: &wire::TaskList) -> Result<TaskListResource, TaskError> {
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(TaskError::Content);
    }
    let name = TaskListName::try_from(value.name.as_str())?;
    let etag = TaskEtag::try_from(value.etag.as_str())?;
    let display = TaskText::try_from(value.display_name.as_str())?;
    let origin = match value.origin {
        EnumValue::Known(wire::TaskListOrigin::LocalTodo) => TaskListOrigin::LocalTodo,
        EnumValue::Known(wire::TaskListOrigin::ShoppingList) => TaskListOrigin::ShoppingList,
        EnumValue::Known(wire::TaskListOrigin::Native) => TaskListOrigin::Native,
        EnumValue::Known(wire::TaskListOrigin::Unspecified) | EnumValue::Unknown(_) => {
            return Err(TaskError::Content);
        }
    };
    let update_time = instant(value.update_time.as_option().ok_or(TaskError::Instant)?)?;
    TaskListResource::new(
        TaskListRevision::new(name, etag),
        display,
        origin,
        update_time,
    )
}
pub fn parse_task(value: &wire::Task) -> Result<TaskResource, TaskError> {
    let name = TaskName::try_from(value.name.as_str())?;
    let etag = TaskEtag::try_from(value.etag.as_str())?;
    let content = parse_task_content(value)?;
    let completion_time = value.completion_time.as_option().map(instant).transpose()?;
    TaskResource::new(
        TaskRevision::new(name, etag),
        content,
        value.position,
        completion_time,
    )
}
fn encode_date(value: CivilDate) -> crate::google::r#type::Date {
    let (year, month, day) = value.components();
    crate::google::r#type::Date {
        year,
        month: month as i32,
        day: day as i32,
        ..Default::default()
    }
}
fn encode_instant(value: UtcInstant) -> Timestamp {
    Timestamp {
        seconds: value.seconds(),
        nanos: value.nanos(),
        ..Default::default()
    }
}
pub fn encode_task_content(value: &TaskContent) -> wire::Task {
    let due = match value.due() {
        TaskDue::None => None,
        TaskDue::Date(value) => Some(wire::task::Due::DueDate(Box::new(encode_date(value)))),
        TaskDue::Instant(value) => Some(wire::task::Due::DueInstant(Box::new(
            wire::TaskDueInstant {
                due_time: encode_instant(value.time()).into(),
                time_zone: value.zone().name().to_owned(),
                ..Default::default()
            },
        ))),
    };
    wire::Task {
        summary: value.summary().as_str().to_owned(),
        completed: value.completed(),
        description: value.description().map(|text| text.as_str().to_owned()),
        due,
        ..Default::default()
    }
}
pub fn encode_task_list(value: &TaskListResource) -> wire::TaskList {
    wire::TaskList {
        name: value.name().as_str().to_owned(),
        etag: value.revision().etag().as_str().to_owned(),
        display_name: value.display_name().as_str().to_owned(),
        origin: match value.origin() {
            TaskListOrigin::LocalTodo => wire::TaskListOrigin::LocalTodo,
            TaskListOrigin::ShoppingList => wire::TaskListOrigin::ShoppingList,
            TaskListOrigin::Native => wire::TaskListOrigin::Native,
        }
        .into(),
        update_time: encode_instant(value.update_time()).into(),
        ..Default::default()
    }
}
pub fn encode_task(value: &TaskResource) -> wire::Task {
    let mut out = encode_task_content(value.content());
    out.name = value.name().as_str().to_owned();
    out.etag = value.revision().etag().as_str().to_owned();
    out.position = value.position();
    out.completion_time = value.completion_time().map(encode_instant).into();
    out
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum TaskWatchFrame {
    Ready,
    Changed { list: TaskListName, etag: TaskEtag },
    Resync,
}
pub fn parse_task_watch(
    value: &wire::WatchTaskListsResponse,
) -> Result<(u64, TaskWatchFrame), TaskError> {
    use wire::watch_task_lists_response::Frame;
    let sequence = u64::try_from(value.sequence).map_err(|_| TaskError::Content)?;
    if !value.__buffa_unknown_fields.is_empty() || sequence == 0 {
        return Err(TaskError::Content);
    }
    let frame = match &value.frame {
        Some(Frame::Ready(empty)) if empty.__buffa_unknown_fields.is_empty() => {
            TaskWatchFrame::Ready
        }
        Some(Frame::Changed(change)) if change.__buffa_unknown_fields.is_empty() => {
            TaskWatchFrame::Changed {
                list: TaskListName::try_from(change.task_list.as_str())?,
                etag: TaskEtag::try_from(change.etag.as_str())?,
            }
        }
        Some(Frame::ResyncRequired(empty)) if empty.__buffa_unknown_fields.is_empty() => {
            TaskWatchFrame::Resync
        }
        Some(Frame::Ready(_) | Frame::Changed(_) | Frame::ResyncRequired(_)) | None => {
            return Err(TaskError::Content);
        }
    };
    Ok((sequence, frame))
}

/// Append accepts editable content only; resource identity and etag cannot enter this request.
pub fn encode_append_content(value: &TaskContent) -> wire::TaskContent {
    let task = encode_task_content(value);
    wire::TaskContent {
        summary: task.summary,
        completed: task.completed,
        description: task.description,
        due: task.due.map(|due| match due {
            wire::task::Due::DueDate(date) => wire::task_content::Due::DueDate(date),
            wire::task::Due::DueInstant(instant) => wire::task_content::Due::DueInstant(instant),
        }),
        ..Default::default()
    }
}

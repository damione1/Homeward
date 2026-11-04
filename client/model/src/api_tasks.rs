//! Current task resource identities, opaque etags and admitted editable content.
//! Native imported IDs remain lossless data; labels and aliases never identify a
//! resource. This module contains no generated protobuf or transport types.
use crate::{
    api::HomeName,
    calendars::CivilDate,
    lists::{ItemKey, ItemUid, ListId},
    local_time::{DisplayTimeZone, UtcInstant},
};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TaskError {
    Name,
    Etag,
    Content,
    Instant,
    Position,
}
fn encode_native(value: &str) -> String {
    const HEX: &[u8] = b"0123456789abcdef";
    let mut out = String::with_capacity(3 + value.len() * 2);
    out.push_str("id-");
    for byte in value.bytes() {
        out.push(char::from(HEX[usize::from(byte >> 4)]));
        out.push(char::from(HEX[usize::from(byte & 15)]));
    }
    out
}
fn decode_native(value: &str) -> Result<String, TaskError> {
    let hex = value.strip_prefix("id-").ok_or(TaskError::Name)?;
    if hex.is_empty() || hex.len() > 510 || hex.len() % 2 != 0 {
        return Err(TaskError::Name);
    }
    fn digit(byte: u8) -> Result<u8, TaskError> {
        match byte {
            b'0'..=b'9' => Ok(byte - b'0'),
            b'a'..=b'f' => Ok(byte - b'a' + 10),
            _ => Err(TaskError::Name),
        }
    }
    let bytes: Result<Vec<_>, _> = hex
        .as_bytes()
        .as_chunks::<2>()
        .0
        .iter()
        .map(|pair| Ok(digit(pair[0])? * 16 + digit(pair[1])?))
        .collect();
    String::from_utf8(bytes?).map_err(|_| TaskError::Name)
}
#[derive(Debug, Clone, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct TaskListName {
    value: String,
    home: HomeName,
    native: ListId,
}
impl TaskListName {
    pub fn new(home: HomeName, native: ListId) -> Self {
        Self {
            value: format!(
                "{}/taskLists/{}",
                home.as_str(),
                encode_native(native.as_str())
            ),
            home,
            native,
        }
    }
    pub fn as_str(&self) -> &str {
        &self.value
    }
    pub fn home(&self) -> &HomeName {
        &self.home
    }
    pub fn native_id(&self) -> &ListId {
        &self.native
    }
}
impl TryFrom<&str> for TaskListName {
    type Error = TaskError;
    fn try_from(value: &str) -> Result<Self, Self::Error> {
        if value.len() > 1200 {
            return Err(TaskError::Name);
        }
        let parts: Vec<_> = value.split('/').collect();
        if parts.len() != 4 || parts[2] != "taskLists" {
            return Err(TaskError::Name);
        }
        let home = HomeName::try_from(format!("{}/{}", parts[0], parts[1]).as_str())
            .map_err(|_| TaskError::Name)?;
        let native = decode_native(parts[3])?
            .parse()
            .map_err(|_| TaskError::Name)?;
        let admitted = Self::new(home, native);
        if admitted.as_str() != value {
            return Err(TaskError::Name);
        }
        Ok(admitted)
    }
}
#[derive(Debug, Clone, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct TaskName {
    value: String,
    parent: TaskListName,
    uid: ItemUid,
}
impl TaskName {
    pub fn new(parent: TaskListName, uid: ItemUid) -> Self {
        Self {
            value: format!("{}/tasks/{}", parent.as_str(), encode_native(uid.as_str())),
            parent,
            uid,
        }
    }
    pub fn as_str(&self) -> &str {
        &self.value
    }
    pub fn parent(&self) -> &TaskListName {
        &self.parent
    }
    pub fn native_uid(&self) -> &ItemUid {
        &self.uid
    }
    pub fn key(&self) -> ItemKey {
        ItemKey::new(self.parent.native_id().clone(), self.uid.clone())
    }
}
impl TryFrom<&str> for TaskName {
    type Error = TaskError;
    fn try_from(value: &str) -> Result<Self, Self::Error> {
        if value.len() > 1800 {
            return Err(TaskError::Name);
        }
        let (parent, uid) = value.rsplit_once("/tasks/").ok_or(TaskError::Name)?;
        let parent = TaskListName::try_from(parent)?;
        let uid = decode_native(uid)?.parse().map_err(|_| TaskError::Name)?;
        let admitted = Self::new(parent, uid);
        if admitted.as_str() != value {
            return Err(TaskError::Name);
        }
        Ok(admitted)
    }
}
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TaskEtag(String);
impl TryFrom<&str> for TaskEtag {
    type Error = TaskError;
    fn try_from(value: &str) -> Result<Self, Self::Error> {
        if value.len() < 3
            || value.len() > 256
            || !value.starts_with('"')
            || !value.ends_with('"')
            || !value.as_bytes()[1..value.len() - 1]
                .iter()
                .all(|byte| byte.is_ascii_graphic() && *byte != b'"')
        {
            return Err(TaskError::Etag);
        }
        Ok(Self(value.to_owned()))
    }
}
impl TaskEtag {
    pub fn as_str(&self) -> &str {
        &self.0
    }
}
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TaskListRevision {
    name: TaskListName,
    etag: TaskEtag,
}
impl TaskListRevision {
    pub fn new(name: TaskListName, etag: TaskEtag) -> Self {
        Self { name, etag }
    }
    pub fn name(&self) -> &TaskListName {
        &self.name
    }
    pub fn list(&self) -> &ListId {
        self.name.native_id()
    }
    pub fn etag(&self) -> &TaskEtag {
        &self.etag
    }
}
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TaskRevision {
    name: TaskName,
    etag: TaskEtag,
    key: ItemKey,
}
impl TaskRevision {
    pub fn new(name: TaskName, etag: TaskEtag) -> Self {
        let key = name.key();
        Self { name, etag, key }
    }
    pub fn name(&self) -> &TaskName {
        &self.name
    }
    pub fn item(&self) -> &ItemKey {
        &self.key
    }
    pub fn etag(&self) -> &TaskEtag {
        &self.etag
    }
}
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TaskText(String);
impl TryFrom<&str> for TaskText {
    type Error = TaskError;
    fn try_from(value: &str) -> Result<Self, Self::Error> {
        if value.is_empty()
            || value.len() > 512
            || value.bytes().any(|byte| matches!(byte, 0 | b'\r' | b'\n'))
        {
            return Err(TaskError::Content);
        }
        Ok(Self(value.to_owned()))
    }
}
impl TaskText {
    pub fn as_str(&self) -> &str {
        &self.0
    }
}
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TaskDescription(String);
impl TryFrom<&str> for TaskDescription {
    type Error = TaskError;
    fn try_from(value: &str) -> Result<Self, Self::Error> {
        if value.len() > 4096 || value.contains('\0') {
            return Err(TaskError::Content);
        }
        Ok(Self(value.to_owned()))
    }
}
impl TaskDescription {
    pub fn as_str(&self) -> &str {
        &self.0
    }
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct TaskDueInstant {
    time: UtcInstant,
    zone: DisplayTimeZone,
}
impl TaskDueInstant {
    pub fn new(time: UtcInstant, zone: DisplayTimeZone) -> Result<Self, TaskError> {
        if time.nanos() % 1000 != 0 || (time.seconds() == -62_135_596_800 && time.nanos() == 0) {
            return Err(TaskError::Instant);
        }
        Ok(Self { time, zone })
    }
    pub fn time(self) -> UtcInstant {
        self.time
    }
    pub fn zone(self) -> DisplayTimeZone {
        self.zone
    }
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TaskDue {
    None,
    Date(CivilDate),
    Instant(TaskDueInstant),
}
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TaskContent {
    summary: TaskText,
    completed: bool,
    description: Option<TaskDescription>,
    due: TaskDue,
}
impl TaskContent {
    pub fn new(
        summary: TaskText,
        completed: bool,
        description: Option<TaskDescription>,
        due: TaskDue,
    ) -> Self {
        Self {
            summary,
            completed,
            description,
            due,
        }
    }
    pub fn summary(&self) -> &TaskText {
        &self.summary
    }
    pub fn completed(&self) -> bool {
        self.completed
    }
    pub fn description(&self) -> Option<&TaskDescription> {
        self.description.as_ref()
    }
    pub fn due(&self) -> TaskDue {
        self.due
    }
    pub fn with_completed(&self, completed: bool) -> Self {
        let mut out = self.clone();
        out.completed = completed;
        out
    }
    pub fn with_summary(&self, summary: TaskText) -> Self {
        let mut result = self.clone();
        result.summary = summary;
        result
    }
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TaskListOrigin {
    LocalTodo,
    ShoppingList,
    Native,
}
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TaskListResource {
    revision: TaskListRevision,
    display_name: TaskText,
    origin: TaskListOrigin,
    update_time: UtcInstant,
}
impl TaskListResource {
    pub fn new(
        revision: TaskListRevision,
        display_name: TaskText,
        origin: TaskListOrigin,
        update_time: UtcInstant,
    ) -> Result<Self, TaskError> {
        if update_time.nanos() % 1000 != 0
            || (update_time.seconds() == -62_135_596_800 && update_time.nanos() == 0)
        {
            return Err(TaskError::Instant);
        }
        Ok(Self {
            revision,
            display_name,
            origin,
            update_time,
        })
    }
    pub fn revision(&self) -> &TaskListRevision {
        &self.revision
    }
    pub fn name(&self) -> &TaskListName {
        self.revision.name()
    }
    pub fn native_id(&self) -> &ListId {
        self.name().native_id()
    }
    pub fn display_name(&self) -> &TaskText {
        &self.display_name
    }
    pub fn origin(&self) -> TaskListOrigin {
        self.origin
    }
    pub fn update_time(&self) -> UtcInstant {
        self.update_time
    }
}
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TaskResource {
    revision: TaskRevision,
    content: TaskContent,
    position: i64,
    completion_time: Option<UtcInstant>,
}
impl TaskResource {
    pub fn new(
        revision: TaskRevision,
        content: TaskContent,
        position: i64,
        completion_time: Option<UtcInstant>,
    ) -> Result<Self, TaskError> {
        if position < 0
            || completion_time.is_some_and(|time| {
                !content.completed()
                    || time.nanos() % 1000 != 0
                    || (time.seconds() == -62_135_596_800 && time.nanos() == 0)
            })
        {
            return Err(TaskError::Position);
        }
        Ok(Self {
            revision,
            content,
            position,
            completion_time,
        })
    }
    pub fn revision(&self) -> &TaskRevision {
        &self.revision
    }
    pub fn name(&self) -> &TaskName {
        self.revision.name()
    }
    pub fn key(&self) -> &ItemKey {
        self.revision.item()
    }
    pub fn content(&self) -> &TaskContent {
        &self.content
    }
    pub fn position(&self) -> i64 {
        self.position
    }
    pub fn with_content(&self, content: TaskContent) -> Result<Self, TaskError> {
        Self::new(
            self.revision.clone(),
            content,
            self.position,
            self.completion_time,
        )
    }
    pub fn completion_time(&self) -> Option<UtcInstant> {
        self.completion_time
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn resource_names_roundtrip_imported_ids_and_reject_alternate_spellings() {
        let home = HomeName::try_from("homes/synthetic").unwrap();
        for raw in ["native-list", " Imported / tâche 同じ ", &"x".repeat(255)] {
            let name = TaskListName::new(home.clone(), raw.parse().unwrap());
            let task = TaskName::new(name, raw.parse().unwrap());
            assert_eq!(TaskName::try_from(task.as_str()).unwrap(), task);
            assert_eq!(task.key().uid().as_str(), raw);
            assert!(TaskName::try_from(task.as_str().to_uppercase().as_str()).is_err());
        }
        assert!(TaskListName::try_from("homes/synthetic/taskLists/id-0A").is_err());
        assert!(TaskListName::try_from("homes/synthetic/taskLists/id-ff").is_err());
    }
    #[test]
    fn opaque_etags_keep_identity_binding_without_client_revision_decoding() {
        let home = HomeName::try_from("homes/synthetic").unwrap();
        let one = TaskListName::new(home.clone(), "one".parse().unwrap());
        let two = TaskListName::new(home, "two".parse().unwrap());
        let etag = TaskEtag::try_from("\"opaque-server-value\"").unwrap();
        let first = TaskListRevision::new(one, etag.clone());
        let second = TaskListRevision::new(two, etag);
        assert_ne!(first, second);
        for raw in ["", "bad\nvalue", &"x".repeat(257)] {
            assert!(TaskEtag::try_from(raw).is_err());
        }
    }
    #[test]
    fn civil_due_and_microsecond_instant_remain_distinct() {
        let date: CivilDate = "2026-11-03".parse().unwrap();
        let zone: DisplayTimeZone = "America/Toronto".parse().unwrap();
        let exact = TaskDueInstant::new(UtcInstant::new(0, 0).unwrap(), zone).unwrap();
        assert_ne!(TaskDue::Date(date), TaskDue::Instant(exact));
        assert!(TaskDueInstant::new(UtcInstant::new(0, 1).unwrap(), zone).is_err());
        assert!(TaskDueInstant::new(UtcInstant::new(-62_135_596_800, 0).unwrap(), zone).is_err());
    }
}

use std::collections::BTreeSet;

use crate::administration::Refusal;
use home_wire_contract::{
    config_plans::{self, PlanRequest},
    home::configurationplan::v1 as wire,
};
use zeroize::Zeroizing;

pub const MAX_SOURCE_BYTES: usize = 1_048_576;

/// Constructed only at an explicit input/import boundary, never from metadata.
#[derive(Clone)]
pub struct UserSource(Zeroizing<Vec<u8>>);
impl UserSource {
    pub fn supplied(bytes: Vec<u8>) -> Result<Self, Refusal> {
        let bytes = Zeroizing::new(bytes);
        if bytes.is_empty() || bytes.len() > MAX_SOURCE_BYTES {
            return Err(Refusal::Invalid);
        }
        Ok(Self(bytes))
    }
    pub fn bytes(&self) -> &[u8] {
        &self.0
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SourceFamily {
    Operational,
    DeclarativeV1,
    DeclarativeV2,
    Authorization,
    Ownership,
    Dashboard,
    Automation,
    Script,
    Scene,
}
impl SourceFamily {
    pub const ALL: [Self; 9] = [
        Self::Operational,
        Self::DeclarativeV1,
        Self::DeclarativeV2,
        Self::Authorization,
        Self::Ownership,
        Self::Dashboard,
        Self::Automation,
        Self::Script,
        Self::Scene,
    ];
    pub fn label(self) -> &'static str {
        match self {
            Self::Operational => "Paramètres du serveur",
            Self::DeclarativeV1 => "Déclarations v1",
            Self::DeclarativeV2 => "Déclarations v2",
            Self::Authorization => "Droits humains",
            Self::Ownership => "Propriété des équipements",
            Self::Dashboard => "Dashboard",
            Self::Automation => "Automatisation",
            Self::Script => "Script",
            Self::Scene => "Scène",
        }
    }
    pub fn to_wire(self) -> wire::SourceFamily {
        match self {
            Self::Operational => wire::SourceFamily::Operational,
            Self::DeclarativeV1 => wire::SourceFamily::DeclarativeV1,
            Self::DeclarativeV2 => wire::SourceFamily::DeclarativeV2,
            Self::Authorization => wire::SourceFamily::Authorization,
            Self::Ownership => wire::SourceFamily::Ownership,
            Self::Dashboard => wire::SourceFamily::Dashboard,
            Self::Automation => wire::SourceFamily::Automation,
            Self::Script => wire::SourceFamily::Script,
            Self::Scene => wire::SourceFamily::Scene,
        }
    }
    pub fn simulatable(self) -> bool {
        match self {
            Self::Automation | Self::Script | Self::Scene => true,
            Self::Operational
            | Self::DeclarativeV1
            | Self::DeclarativeV2
            | Self::Authorization
            | Self::Ownership
            | Self::Dashboard => false,
        }
    }
}
fn label(value: &str, maximum: usize) -> bool {
    !value.is_empty()
        && value.trim() == value
        && value.len() <= maximum
        && !value.contains(['\0', '\r', '\n'])
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SourceIdentity(String);
impl SourceIdentity {
    pub fn parse(value: &str) -> Result<Self, Refusal> {
        if !label(value, 255) {
            return Err(Refusal::Invalid);
        }
        Ok(Self(value.into()))
    }
    pub fn as_str(&self) -> &str {
        &self.0
    }
}
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub struct LogicalPath(String);
impl LogicalPath {
    pub fn parse(value: &str) -> Result<Self, Refusal> {
        if !label(value, 4096) {
            return Err(Refusal::Invalid);
        }
        Ok(Self(value.into()))
    }
    pub fn as_str(&self) -> &str {
        &self.0
    }
}
#[derive(Clone)]
pub struct SourceFile {
    path: LogicalPath,
    source: UserSource,
}
impl SourceFile {
    pub fn supplied(path: &str, source: UserSource) -> Result<Self, Refusal> {
        Ok(Self {
            path: LogicalPath::parse(path)?,
            source,
        })
    }
    pub fn path(&self) -> &LogicalPath {
        &self.path
    }
    pub fn source(&self) -> &UserSource {
        &self.source
    }
}
#[derive(Clone)]
pub struct Candidate {
    family: SourceFamily,
    identity: SourceIdentity,
    entry: LogicalPath,
    files: Vec<SourceFile>,
}
impl Candidate {
    pub fn supplied(
        family: SourceFamily,
        identity: SourceIdentity,
        entry: &str,
        files: Vec<SourceFile>,
    ) -> Result<Self, Refusal> {
        let entry = LogicalPath::parse(entry)?;
        if !(1..=64).contains(&files.len()) {
            return Err(Refusal::Invalid);
        }
        let mut paths = BTreeSet::new();
        let mut bytes = 0usize;
        for file in &files {
            bytes = bytes
                .checked_add(file.source.bytes().len())
                .ok_or(Refusal::Invalid)?;
            if bytes > MAX_SOURCE_BYTES || !paths.insert(&file.path) {
                return Err(Refusal::Invalid);
            }
        }
        if !paths.contains(&entry) {
            return Err(Refusal::Invalid);
        }
        let value = Self {
            family,
            identity,
            entry,
            files,
        };
        let request = value.build_request();
        config_plans::admit_plan_request(PlanRequest::Build(&request))
            .map_err(|_| Refusal::Invalid)?;
        Ok(value)
    }
    pub fn family(&self) -> SourceFamily {
        self.family
    }
    pub fn identity(&self) -> &SourceIdentity {
        &self.identity
    }
    pub fn entry(&self) -> &LogicalPath {
        &self.entry
    }
    pub fn files(&self) -> &[SourceFile] {
        &self.files
    }
    pub fn to_wire(&self) -> wire::Candidate {
        wire::Candidate {
            family: self.family.to_wire().into(),
            identity: self.identity.0.clone(),
            entry: self.entry.0.clone(),
            files: self
                .files
                .iter()
                .map(|value| wire::SourceFile {
                    logical_path: value.path.0.clone(),
                    exact_source: value.source.bytes().to_vec(),
                    ..Default::default()
                })
                .collect(),
            ..Default::default()
        }
    }
    pub fn build_request(&self) -> wire::BuildPlanRequest {
        wire::BuildPlanRequest {
            candidate: self.to_wire().into(),
            ..Default::default()
        }
    }
}

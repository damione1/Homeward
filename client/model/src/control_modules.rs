//! Source functions are independent of visual family. Each binding is a
//! separate command fence; composition never creates an atomic group.
use crate::api::{CapabilityName, CoverOperation, DefinitionEtag, SelectorKey};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum VisualFamily {
    Binary,
    ContinuousLevel,
    SteppedLevel,
    Blind,
    Garage,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ModuleRole {
    Power,
    Range,
    Steps,
    Color,
    Preset,
    Motion,
}
#[derive(Clone, Debug, PartialEq)]
pub struct NativeRange {
    pub minimum: f64,
    pub maximum: f64,
    pub step: Option<f64>,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Choice {
    pub key: SelectorKey,
    pub display_name: String,
}
#[derive(Clone, Debug, PartialEq)]
pub enum ModuleDefinition {
    Power,
    Range(NativeRange),
    Steps(Vec<Choice>),
    Color {
        temperature_mireds: Option<NativeRange>,
        rgb: bool,
    },
    Preset(Vec<Choice>),
    Motion(Vec<CoverOperation>),
}
#[derive(Clone, Debug, PartialEq)]
pub struct ControlModule {
    pub capability: CapabilityName,
    pub etag: DefinitionEtag,
    pub allowed: bool,
    pub role: ModuleRole,
    pub definition: ModuleDefinition,
}

//! Admitted API v2 values. Generated protobuf types stay outside this crate.
//! Syntax does not prove permission, supported capabilities or current expiry.
use std::{error::Error, fmt};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AdmissionError {
    ResourceName,
    Percentage,
    RequestId,
    Deadline,
    Intent,
}
impl fmt::Display for AdmissionError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::ResourceName => "invalid resource name",
            Self::Percentage => "percentage must be finite and in [0,100]",
            Self::RequestId => "request ID must be a UUIDv4",
            Self::Deadline => "invalid deadline",
            Self::Intent => "invalid explicit intention",
        })
    }
}
impl Error for AdmissionError {}

pub(crate) fn resource_name(value: &str, collections: &[&str]) -> Result<(), AdmissionError> {
    let maximum = collections.iter().map(|c| c.len() + 65).sum::<usize>() - 1;
    if value.len() > maximum {
        return Err(AdmissionError::ResourceName);
    }
    let parts: Vec<_> = value.split('/').collect();
    if parts.len() != 2 * collections.len() {
        return Err(AdmissionError::ResourceName);
    }
    for (i, collection) in collections.iter().enumerate() {
        let id = parts[2 * i + 1];
        if parts[2 * i] != *collection
            || id.is_empty()
            || id.len() > 63
            || id.starts_with('-')
            || id.ends_with('-')
            || !id
                .bytes()
                .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
        {
            return Err(AdmissionError::ResourceName);
        }
    }
    Ok(())
}

macro_rules! name {
    ($type:ident, [$($collection:literal),+]) => {
        #[derive(Debug, Clone, PartialEq, Eq, PartialOrd, Ord, Hash)]
        pub struct $type(String);
        impl TryFrom<&str> for $type {
            type Error = AdmissionError;
            fn try_from(value: &str) -> Result<Self, Self::Error> {
                resource_name(value, &[$($collection),+])?;
                Ok(Self(value.to_owned()))
            }
        }
        impl $type {
            pub fn as_str(&self) -> &str { &self.0 }
        }
    };
}
name!(HomeName, ["homes"]);
name!(OperationName, ["homes", "operations"]);
name!(EntityName, ["homes", "entities"]);
name!(EntityViewName, ["homes", "entityViews"]);
name!(DeviceName, ["homes", "devices"]);
name!(DashboardName, ["homes", "dashboards"]);
name!(CapabilityName, ["homes", "entities", "capabilities"]);

impl CapabilityName {
    /// Parent derived only from an already admitted canonical resource name.
    pub fn entity(&self) -> EntityName {
        let (parent, _) = self
            .0
            .rsplit_once("/capabilities/")
            .expect("admitted capability name");
        EntityName(parent.to_owned())
    }
    pub fn home(&self) -> HomeName {
        self.entity().home()
    }
}
impl EntityName {
    pub fn home(&self) -> HomeName {
        let (parent, _) = self
            .0
            .rsplit_once("/entities/")
            .expect("admitted entity name");
        HomeName(parent.to_owned())
    }
}

macro_rules! home_parent {
    ($type:ident, $collection:literal) => {
        impl $type {
            pub fn home(&self) -> HomeName {
                let (parent, _) = self
                    .0
                    .rsplit_once(concat!("/", $collection, "/"))
                    .expect("admitted resource name");
                HomeName(parent.to_owned())
            }
        }
    };
}
home_parent!(EntityViewName, "entityViews");
home_parent!(DeviceName, "devices");
home_parent!(DashboardName, "dashboards");
home_parent!(OperationName, "operations");

/// Only the constructor admits external numeric values; zero is legitimate.
///
/// ```compile_fail
/// use home_client_model::api::Percentage;
/// let bypass = Percentage(f64::NAN);
/// ```
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Percentage(f64);
impl TryFrom<f64> for Percentage {
    type Error = AdmissionError;
    fn try_from(value: f64) -> Result<Self, Self::Error> {
        if value.is_finite() && (0.0..=100.0).contains(&value) {
            Ok(Self(value))
        } else {
            Err(AdmissionError::Percentage)
        }
    }
}
impl Percentage {
    pub fn value(self) -> f64 {
        self.0
    }
}

/// Closed operational intent. Unspecified and unknown wire enums never enter it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Power {
    On,
    Off,
}

#[derive(Debug, Clone, PartialEq)]
pub enum Intent {
    Power(Power),
    Level(Percentage),
    Light(LightIntent),
    Fan(FanIntent),
    Thermal(ThermalIntent),
    Cover(CoverIntent),
    Lock(LockOperation),
    Alarm(AlarmOperation),
    Helper(HelperIntent),
    Media(crate::api_media::MediaIntent),
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RequestId(String);
impl TryFrom<&str> for RequestId {
    type Error = AdmissionError;
    fn try_from(value: &str) -> Result<Self, Self::Error> {
        let bytes = value.as_bytes();
        if bytes.len() != 36
            || bytes[14] != b'4'
            || !matches!(bytes[19], b'8' | b'9' | b'a' | b'A' | b'b' | b'B')
            || !bytes.iter().enumerate().all(|(i, b)| {
                if matches!(i, 8 | 13 | 18 | 23) {
                    *b == b'-'
                } else {
                    b.is_ascii_hexdigit()
                }
            })
        {
            return Err(AdmissionError::RequestId);
        }
        Ok(Self(value.to_ascii_lowercase()))
    }
}
impl RequestId {
    pub fn as_str(&self) -> &str {
        &self.0
    }
}

/// Protobuf timestamp representation. Runtime deadline checks belong at effect.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Deadline {
    seconds: i64,
    nanos: i32,
}
impl Deadline {
    pub fn new(seconds: i64, nanos: i32) -> Result<Self, AdmissionError> {
        if !(-62_135_596_800..=253_402_300_799).contains(&seconds)
            || !(0..1_000_000_000).contains(&nanos)
            || (seconds == -62_135_596_800 && nanos == 0)
        {
            return Err(AdmissionError::Deadline);
        }
        Ok(Self { seconds, nanos })
    }
    pub fn seconds(self) -> i64 {
        self.seconds
    }
    pub fn nanos(self) -> i32 {
        self.nanos
    }
}

/// Immutable admitted syntax, with no Default or public mutable representation.
#[derive(Debug, Clone, PartialEq)]
pub struct Command {
    target: CapabilityName,
    intent: Intent,
    request_id: Option<RequestId>,
    deadline: Deadline,
    definition_etag: Option<DefinitionEtag>,
}
impl Command {
    pub fn new(
        target: CapabilityName,
        intent: Intent,
        request_id: Option<RequestId>,
        deadline: Deadline,
    ) -> Self {
        Self {
            target,
            intent,
            request_id,
            deadline,
            definition_etag: None,
        }
    }
    pub fn with_definition_etag(mut self, etag: DefinitionEtag) -> Self {
        self.definition_etag = Some(etag);
        self
    }
    pub fn definition_etag(&self) -> Option<&DefinitionEtag> {
        self.definition_etag.as_ref()
    }
    pub fn target(&self) -> &CapabilityName {
        &self.target
    }
    pub fn intent(&self) -> Intent {
        self.intent.clone()
    }
    pub fn request_id(&self) -> Option<&RequestId> {
        self.request_id.as_ref()
    }
    pub fn deadline(&self) -> Deadline {
        self.deadline
    }
}

/// Finite Celsius temperature; device limits are admitted by the server.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Celsius(f64);
impl TryFrom<f64> for Celsius {
    type Error = AdmissionError;
    fn try_from(v: f64) -> Result<Self, Self::Error> {
        if v.is_finite() && v >= -273.15 {
            Ok(Self(v))
        } else {
            Err(AdmissionError::Intent)
        }
    }
}
impl Celsius {
    pub fn value(self) -> f64 {
        self.0
    }
}
#[derive(Debug, Clone, Copy, PartialEq)]
pub enum TemperatureTarget {
    Single(Celsius),
    Range(TemperatureRange),
}
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct TemperatureRange {
    low: Celsius,
    high: Celsius,
}
impl TemperatureRange {
    pub fn new(low: Celsius, high: Celsius) -> Result<Self, AdmissionError> {
        if low.0 <= high.0 {
            Ok(Self { low, high })
        } else {
            Err(AdmissionError::Intent)
        }
    }
    pub fn low(self) -> Celsius {
        self.low
    }
    pub fn high(self) -> Celsius {
        self.high
    }
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TemperatureMode {
    Off,
    Heat,
    Cool,
    Auto,
    Dry,
    FanOnly,
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TemperatureFanTarget {
    On,
    Auto,
}
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct ThermalIntent {
    target: Option<TemperatureTarget>,
    mode: Option<TemperatureMode>,
    fan: Option<TemperatureFanTarget>,
}
impl ThermalIntent {
    pub fn new(
        target: Option<TemperatureTarget>,
        mode: Option<TemperatureMode>,
        fan: Option<TemperatureFanTarget>,
    ) -> Result<Self, AdmissionError> {
        if target.is_none() && mode.is_none() && fan.is_none() {
            return Err(AdmissionError::Intent);
        }
        Ok(Self { target, mode, fan })
    }
    pub fn target(self) -> Option<TemperatureTarget> {
        self.target
    }
    pub fn mode(self) -> Option<TemperatureMode> {
        self.mode
    }
    pub fn fan(self) -> Option<TemperatureFanTarget> {
        self.fan
    }
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Transition {
    seconds: i64,
    nanos: i32,
}
impl Transition {
    pub fn new(seconds: i64, nanos: i32) -> Result<Self, AdmissionError> {
        if !(0..=6553).contains(&seconds)
            || !(0..1_000_000_000).contains(&nanos)
            || seconds == 6553 && nanos != 0
        {
            return Err(AdmissionError::Intent);
        }
        Ok(Self { seconds, nanos })
    }
    pub fn seconds(self) -> i64 {
        self.seconds
    }
    pub fn nanos(self) -> i32 {
        self.nanos
    }
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ColorTemperature(i32);
impl TryFrom<i32> for ColorTemperature {
    type Error = AdmissionError;
    fn try_from(v: i32) -> Result<Self, Self::Error> {
        if (1..=1000).contains(&v) {
            Ok(Self(v))
        } else {
            Err(AdmissionError::Intent)
        }
    }
}
impl ColorTemperature {
    pub fn mireds(self) -> i32 {
        self.0
    }
}
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct LightIntent {
    on: Option<bool>,
    brightness: Option<Percentage>,
    color: Option<ColorTemperature>,
    transition: Option<Transition>,
}
impl LightIntent {
    pub fn new(
        on: Option<bool>,
        brightness: Option<Percentage>,
        color: Option<ColorTemperature>,
        transition: Option<Transition>,
    ) -> Result<Self, AdmissionError> {
        if on.is_none() && brightness.is_none() && color.is_none()
            || transition.is_some()
                && on == Some(false)
                && (brightness.is_some() || color.is_some())
        {
            return Err(AdmissionError::Intent);
        }
        Ok(Self {
            on,
            brightness,
            color,
            transition,
        })
    }
    pub fn on(self) -> Option<bool> {
        self.on
    }
    pub fn brightness(self) -> Option<Percentage> {
        self.brightness
    }
    pub fn color(self) -> Option<ColorTemperature> {
        self.color
    }
    pub fn transition(self) -> Option<Transition> {
        self.transition
    }
}
/// Exact open catalogue key; no inferred labels or case normalization.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SelectorKey(String);
impl TryFrom<&str> for SelectorKey {
    type Error = AdmissionError;
    fn try_from(v: &str) -> Result<Self, Self::Error> {
        if v.is_empty() || v.len() > 16384 || v.contains('\0') {
            Err(AdmissionError::Intent)
        } else {
            Ok(Self(v.into()))
        }
    }
}
impl SelectorKey {
    pub fn as_str(&self) -> &str {
        &self.0
    }
}
#[derive(Debug, Clone, PartialEq)]
pub struct FanIntent {
    on: Option<bool>,
    speed: Option<SelectorKey>,
}
impl FanIntent {
    pub fn new(on: Option<bool>, speed: Option<SelectorKey>) -> Result<Self, AdmissionError> {
        if on.is_none() && speed.is_none() {
            return Err(AdmissionError::Intent);
        }
        Ok(Self { on, speed })
    }
    pub fn on(&self) -> Option<bool> {
        self.on
    }
    pub fn speed(&self) -> Option<&SelectorKey> {
        self.speed.as_ref()
    }
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CoverOperation {
    Open,
    Close,
    Stop,
}
#[derive(Debug, Clone, Copy, PartialEq)]
pub enum CoverIntent {
    Operation(CoverOperation),
    Position(Percentage),
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum LockOperation {
    Lock,
    Unlock,
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AlarmOperation {
    ArmHome,
    ArmAway,
    ArmNight,
    Disarm,
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum HelperOperation {
    Set,
    Toggle,
    Increment,
    Decrement,
    Reset,
    Press,
    Next,
    Previous,
}
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Finite(f64);
impl TryFrom<f64> for Finite {
    type Error = AdmissionError;
    fn try_from(v: f64) -> Result<Self, Self::Error> {
        if v.is_finite() {
            Ok(Self(v))
        } else {
            Err(AdmissionError::Intent)
        }
    }
}
impl Finite {
    pub fn value(self) -> f64 {
        self.0
    }
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct LocalDate {
    year: i32,
    month: i32,
    day: i32,
}
impl LocalDate {
    pub fn new(year: i32, month: i32, day: i32) -> Result<Self, AdmissionError> {
        if !(1..=9999).contains(&year)
            || chrono::NaiveDate::from_ymd_opt(year, month as u32, day as u32).is_none()
        {
            return Err(AdmissionError::Intent);
        }
        Ok(Self { year, month, day })
    }
    pub fn components(self) -> (i32, i32, i32) {
        (self.year, self.month, self.day)
    }
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ClockTime {
    hour: i32,
    minute: i32,
    second: i32,
}
impl ClockTime {
    pub fn new(hour: i32, minute: i32, second: i32) -> Result<Self, AdmissionError> {
        if !(0..24).contains(&hour) || !(0..60).contains(&minute) || !(0..60).contains(&second) {
            return Err(AdmissionError::Intent);
        }
        Ok(Self {
            hour,
            minute,
            second,
        })
    }
    pub fn components(self) -> (i32, i32, i32) {
        (self.hour, self.minute, self.second)
    }
}
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct HelperText(String);
impl TryFrom<&str> for HelperText {
    type Error = AdmissionError;
    fn try_from(v: &str) -> Result<Self, Self::Error> {
        if v.len() > 16384 || v.contains('\0') {
            Err(AdmissionError::Intent)
        } else {
            Ok(Self(v.into()))
        }
    }
}
impl HelperText {
    pub fn as_str(&self) -> &str {
        &self.0
    }
}
#[derive(Debug, Clone, PartialEq)]
pub enum HelperValue {
    Boolean(bool),
    Number(Finite),
    Counter(i64),
    Date(LocalDate),
    Time(ClockTime),
    LocalDateTime(LocalDate, ClockTime),
    Selected(SelectorKey),
    Text(HelperText),
}
#[derive(Debug, Clone, PartialEq)]
pub struct HelperIntent {
    operation: HelperOperation,
    value: Option<HelperValue>,
    revision: i64,
}
impl HelperIntent {
    pub fn new(
        operation: HelperOperation,
        value: Option<HelperValue>,
        revision: i64,
    ) -> Result<Self, AdmissionError> {
        if revision < 0 || (operation == HelperOperation::Set) != value.is_some() {
            return Err(AdmissionError::Intent);
        }
        Ok(Self {
            operation,
            value,
            revision,
        })
    }
    pub fn operation(&self) -> HelperOperation {
        self.operation
    }
    pub fn value(&self) -> Option<&HelperValue> {
        self.value.as_ref()
    }
    pub fn revision(&self) -> i64 {
        self.revision
    }
}

/// Opaque source definition binding, exact and bounded; never an authority.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DefinitionEtag(String);
impl TryFrom<&str> for DefinitionEtag {
    type Error = AdmissionError;
    fn try_from(value: &str) -> Result<Self, Self::Error> {
        if value.is_empty() || value.len() > 256 || value.chars().any(char::is_control) {
            Err(AdmissionError::Intent)
        } else {
            Ok(Self(value.into()))
        }
    }
}
impl DefinitionEtag {
    pub fn as_str(&self) -> &str {
        &self.0
    }
}

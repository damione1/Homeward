//! Native notification admission and evidence. An OS-accepted request is not a
//! presentation receipt. No permissions are requested by construction/query.
use std::time::{Duration, SystemTime};

#[cfg(target_os = "macos")]
mod macos;

pub const MAX_LIFETIME: Duration = Duration::from_secs(3600);

#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub struct PresentationId(String);
impl PresentationId {
    pub fn parse(value: &str) -> Result<Self, Error> {
        if value.len() != 32
            || !value
                .bytes()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
        {
            return Err(Error::InvalidContent);
        }
        Ok(Self(value.into()))
    }
    pub fn as_str(&self) -> &str {
        &self.0
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Permission {
    NotRequested,
    Denied,
    Authorized,
    Provisional,
    Ephemeral,
    Unknown,
}
impl Permission {
    pub fn label(self) -> &'static str {
        match self {
            Self::NotRequested => "Autorisation non demandée",
            Self::Denied => "Autorisation refusée",
            Self::Authorized => "Autorisation accordée",
            Self::Provisional => "Autorisation provisoire",
            Self::Ephemeral => "Autorisation temporaire",
            Self::Unknown => "Autorisation non reconnue",
        }
    }
    pub fn allows_delivery(self) -> bool {
        match self {
            Self::Authorized | Self::Provisional | Self::Ephemeral => true,
            Self::NotRequested | Self::Denied | Self::Unknown => false,
        }
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Setting {
    Enabled,
    Disabled,
    Unsupported,
    Unknown,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Settings {
    pub permission: Permission,
    pub alerts: Setting,
    pub notification_center: Setting,
}
impl Settings {
    pub fn allows_delivery(self) -> bool {
        self.permission.allows_delivery()
            && (self.alerts == Setting::Enabled || self.notification_center == Setting::Enabled)
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Submission {
    AcceptedByOs,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum DeliveryEvidence {
    PresentInOsCenter,
    NotObserved,
    Unknown,
}
impl DeliveryEvidence {
    pub fn label(self) -> &'static str {
        match self {
            Self::PresentInOsCenter => "Remise attestée au centre de notifications OS",
            Self::NotObserved => "Remise non observée",
            Self::Unknown => "Remise inconnue",
        }
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Error {
    Unsupported,
    PendingQualification,
    BundleRequired,
    Timeout,
    NativeUnavailable,
    PermissionDenied,
    InvalidContent,
    Expired,
    ClockUncertain,
}
impl Error {
    pub fn label(self) -> &'static str {
        match self {
            Self::Unsupported => "Notifications natives non prises en charge",
            Self::PendingQualification => "Notifications à qualifier sur cet appareil",
            Self::BundleRequired => "Le bundle macOS est requis pour les notifications OS",
            Self::Timeout => "Résultat OS inconnu ; aucun renvoi automatique",
            Self::NativeUnavailable => "Service de notifications OS indisponible",
            Self::PermissionDenied => "Les réglages OS ne permettent pas la remise",
            Self::InvalidContent => "Contenu de notification non admis",
            Self::Expired => "Notification expirée",
            Self::ClockUncertain => "Fraîcheur de notification non vérifiable",
        }
    }
}
#[derive(Clone, Debug)]
pub struct Content {
    id: PresentationId,
    title: String,
    body: String,
    issued_at: SystemTime,
    expires_at: SystemTime,
}
impl Content {
    pub fn new(
        id: PresentationId,
        title: String,
        body: String,
        issued_at: SystemTime,
        expires_at: SystemTime,
        now: SystemTime,
    ) -> Result<Self, Error> {
        if !text(&title, 128, false) || !text(&body, 1024, true) {
            return Err(Error::InvalidContent);
        }
        let lifetime = expires_at
            .duration_since(issued_at)
            .map_err(|_| Error::InvalidContent)?;
        if lifetime.is_zero() || lifetime > MAX_LIFETIME {
            return Err(Error::InvalidContent);
        }
        let value = Self {
            id,
            title,
            body,
            issued_at,
            expires_at,
        };
        value.check_fresh(now)?;
        Ok(value)
    }
    pub fn id(&self) -> &PresentationId {
        &self.id
    }
    pub fn title(&self) -> &str {
        &self.title
    }
    pub fn body(&self) -> &str {
        &self.body
    }
    pub fn check_fresh(&self, now: SystemTime) -> Result<(), Error> {
        if now < self.issued_at {
            Err(Error::ClockUncertain)
        } else if now >= self.expires_at {
            Err(Error::Expired)
        } else {
            Ok(())
        }
    }
}
fn text(value: &str, maximum: usize, multiline: bool) -> bool {
    !value.trim().is_empty()
        && value.len() <= maximum
        && !value
            .chars()
            .any(|ch| ch.is_control() && !(multiline && ch == '\n'))
}

/// Synchronous native callbacks are bounded by a timeout. Call off the UI
/// thread. Timeout on submission is uncertain, and must never trigger retry.
pub fn settings() -> Result<Settings, Error> {
    #[cfg(target_os = "macos")]
    return macos::settings();
    #[cfg(target_os = "android")]
    return Err(Error::PendingQualification);
    #[cfg(not(any(target_os = "macos", target_os = "android")))]
    Err(Error::Unsupported)
}
/// Explicit user intention only. Reading settings never invokes this method.
pub fn request_authorization() -> Result<Settings, Error> {
    #[cfg(target_os = "macos")]
    return macos::request_authorization();
    #[cfg(target_os = "android")]
    return Err(Error::PendingQualification);
    #[cfg(not(any(target_os = "macos", target_os = "android")))]
    Err(Error::Unsupported)
}
pub fn submit_once(content: &Content) -> Result<Submission, Error> {
    content.check_fresh(SystemTime::now())?;
    #[cfg(target_os = "macos")]
    return macos::submit_once(content);
    #[cfg(target_os = "android")]
    return Err(Error::PendingQualification);
    #[cfg(not(any(target_os = "macos", target_os = "android")))]
    Err(Error::Unsupported)
}
pub fn delivery_evidence(id: &PresentationId) -> Result<DeliveryEvidence, Error> {
    #[cfg(target_os = "macos")]
    return macos::delivery_evidence(id);
    #[cfg(target_os = "android")]
    return Err(Error::PendingQualification);
    #[cfg(not(any(target_os = "macos", target_os = "android")))]
    Err(Error::Unsupported)
}
/// Removes only the supplied opaque app-owned identifier, never all messages
/// belonging to this application or another native session.
pub fn remove(id: &PresentationId) -> Result<(), Error> {
    #[cfg(target_os = "macos")]
    return macos::remove(id);
    #[cfg(target_os = "android")]
    return Err(Error::PendingQualification);
    #[cfg(not(any(target_os = "macos", target_os = "android")))]
    Err(Error::Unsupported)
}

#[cfg(test)]
mod tests {
    use super::*;
    fn time(seconds: u64) -> SystemTime {
        SystemTime::UNIX_EPOCH + Duration::from_secs(seconds)
    }
    fn id() -> PresentationId {
        PresentationId::parse("0123456789abcdef0123456789abcdef").unwrap()
    }
    #[test]
    fn expired_and_backward_clock_never_admit_a_native_presentation() {
        let content = Content::new(
            id(),
            "Title".into(),
            "Body".into(),
            time(10),
            time(20),
            time(10),
        )
        .unwrap();
        assert_eq!(content.check_fresh(time(19)), Ok(()));
        assert_eq!(content.check_fresh(time(20)), Err(Error::Expired));
        assert_eq!(content.check_fresh(time(9)), Err(Error::ClockUncertain));
        assert!(
            Content::new(
                id(),
                "Title".into(),
                "Body".into(),
                time(10),
                time(3611),
                time(10)
            )
            .is_err()
        );
    }
    #[test]
    fn native_identity_and_plain_text_are_bounded_without_control_sequences() {
        for invalid in [
            "",
            "ABCDEF0123456789abcdef0123456789ab",
            "notify.device",
            "0000000000000000000000000000000/",
        ] {
            assert_eq!(PresentationId::parse(invalid), Err(Error::InvalidContent));
        }
        assert!(
            Content::new(
                id(),
                "Title".into(),
                "First\nSecond".into(),
                time(10),
                time(20),
                time(10)
            )
            .is_ok()
        );
        for invalid in ["", " \n ", "Body\u{1b}[31m", "Body\rsecret"] {
            assert!(
                Content::new(
                    id(),
                    "Title".into(),
                    invalid.into(),
                    time(10),
                    time(20),
                    time(10)
                )
                .is_err()
            );
        }
        assert!(
            Content::new(
                id(),
                "T".repeat(129),
                "B".into(),
                time(10),
                time(20),
                time(10)
            )
            .is_err()
        );
        assert!(
            Content::new(
                id(),
                "T".into(),
                "B".repeat(1025),
                time(10),
                time(20),
                time(10)
            )
            .is_err()
        );
    }
    #[test]
    fn os_permission_and_surface_setting_are_separate_from_submission() {
        for permission in [
            Permission::NotRequested,
            Permission::Denied,
            Permission::Unknown,
        ] {
            assert!(
                !Settings {
                    permission,
                    alerts: Setting::Enabled,
                    notification_center: Setting::Enabled
                }
                .allows_delivery()
            );
        }
        assert!(
            !Settings {
                permission: Permission::Authorized,
                alerts: Setting::Disabled,
                notification_center: Setting::Disabled
            }
            .allows_delivery()
        );
        assert!(
            Settings {
                permission: Permission::Authorized,
                alerts: Setting::Enabled,
                notification_center: Setting::Disabled
            }
            .allows_delivery()
        );
        assert!(
            Settings {
                permission: Permission::Provisional,
                alerts: Setting::Disabled,
                notification_center: Setting::Enabled
            }
            .allows_delivery()
        );
        assert_ne!(
            DeliveryEvidence::NotObserved,
            DeliveryEvidence::PresentInOsCenter
        );
    }
}

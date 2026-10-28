use super::*;
use home_client_platform::notifications as os;

pub(super) trait Source: Send + Sync {
    fn backend(&self) -> os::Backend;
    fn settings(&self, request: bool) -> Result<os::Settings, os::Error>;
    fn present(
        &self,
        scope: &os::PresentationScope,
        content: &os::Content,
    ) -> Result<os::Submission, os::Error>;
    fn evidence(&self, id: &os::PresentationId) -> Result<os::DeliveryEvidence, os::Error>;
    fn remove(&self, id: &os::PresentationId);
}
pub(super) struct Native;
impl Source for Native {
    fn backend(&self) -> os::Backend {
        os::backend()
    }
    fn settings(&self, request: bool) -> Result<os::Settings, os::Error> {
        if request {
            os::request_authorization()
        } else {
            os::settings()
        }
    }
    fn present(
        &self,
        scope: &os::PresentationScope,
        content: &os::Content,
    ) -> Result<os::Submission, os::Error> {
        scope.submit_once(content)
    }
    fn evidence(&self, id: &os::PresentationId) -> Result<os::DeliveryEvidence, os::Error> {
        os::delivery_evidence(id)
    }
    fn remove(&self, id: &os::PresentationId) {
        let _ = os::remove(id);
    }
}
pub(super) fn permission(value: Result<os::Settings, os::Error>) -> wire::PermissionSnapshot {
    permission_for(value, os::backend())
}
pub(super) fn permission_for(
    value: Result<os::Settings, os::Error>,
    backend: os::Backend,
) -> wire::PermissionSnapshot {
    let empty = |source| wire::PermissionSnapshot {
        permission: wire::Permission::Unknown.into(),
        alerts: wire::Setting::Unsupported.into(),
        notification_center: wire::Setting::Unsupported.into(),
        source,
        ..Default::default()
    };
    match value {
        Ok(value) => wire::PermissionSnapshot {
            source: match backend {
                os::Backend::MacOs => wire::PermissionSource::MacOsUserNotifications,
                os::Backend::Android => wire::PermissionSource::AndroidNotificationManager,
                os::Backend::Unavailable => {
                    return empty(wire::PermissionSource::Unsupported.into());
                }
            }
            .into(),
            permission: match value.permission {
                os::Permission::NotRequested => wire::Permission::NotRequested,
                os::Permission::Denied => wire::Permission::Denied,
                os::Permission::Authorized => wire::Permission::Authorized,
                os::Permission::Provisional => wire::Permission::Provisional,
                os::Permission::Ephemeral => wire::Permission::Ephemeral,
                os::Permission::Unknown => wire::Permission::Unknown,
            }
            .into(),
            alerts: setting(value.alerts).into(),
            notification_center: setting(value.notification_center).into(),
            ..Default::default()
        },
        Err(os::Error::BundleRequired) => empty(wire::PermissionSource::BundleRequired.into()),
        Err(os::Error::PendingQualification) => {
            empty(wire::PermissionSource::AndroidPendingQualification.into())
        }
        Err(
            os::Error::Unsupported
            | os::Error::Timeout
            | os::Error::NativeUnavailable
            | os::Error::PermissionDenied
            | os::Error::InvalidContent
            | os::Error::Expired
            | os::Error::ClockUncertain
            | os::Error::ScopeClosed
            | os::Error::AlreadyAttempted
            | os::Error::Capacity,
        ) => empty(wire::PermissionSource::Unsupported.into()),
    }
}
fn setting(value: os::Setting) -> wire::Setting {
    match value {
        os::Setting::Enabled => wire::Setting::Enabled,
        os::Setting::Disabled => wire::Setting::Disabled,
        os::Setting::Unsupported => wire::Setting::Unsupported,
        os::Setting::Unknown => wire::Setting::Unknown,
    }
}

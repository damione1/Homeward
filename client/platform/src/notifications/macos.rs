//! UserNotifications is confined to a packaged application. Querying is read
//! only; requesting OS consent and submitting each message are separate calls.
use super::*;
use block2::RcBlock;
use objc2::rc::{Retained, autoreleasepool};
use objc2_foundation::{NSArray, NSBundle, NSError, NSString};
use objc2_user_notifications::{
    UNAuthorizationOptions, UNMutableNotificationContent, UNNotification, UNNotificationRequest,
    UNNotificationSettings, UNUserNotificationCenter,
};
use std::{ptr::NonNull, sync::mpsc};

const CALLBACK_TIMEOUT: Duration = Duration::from_secs(5);
fn center() -> Result<Retained<UNUserNotificationCenter>, Error> {
    let bundle = NSBundle::mainBundle();
    // SAFETY: public read-only metadata from the retained main bundle.
    let packaged = unsafe {
        bundle.bundleIdentifier().is_some() && bundle.bundlePath().to_string().ends_with(".app")
    };
    if !packaged {
        // UNUserNotificationCenter may raise an Objective-C exception for an
        // unbundled CLI; never call it on this path, including read-only probes.
        return Err(Error::BundleRequired);
    }
    // SAFETY: a packaged app with a bundle identifier; retained shared center.
    Ok(unsafe { UNUserNotificationCenter::currentNotificationCenter() })
}
fn permission(raw: isize) -> Permission {
    match raw {
        0 => Permission::NotRequested,
        1 => Permission::Denied,
        2 => Permission::Authorized,
        3 => Permission::Provisional,
        4 => Permission::Ephemeral,
        _ => Permission::Unknown,
    }
}
fn setting(raw: isize) -> Setting {
    match raw {
        0 => Setting::Unsupported,
        1 => Setting::Disabled,
        2 => Setting::Enabled,
        _ => Setting::Unknown,
    }
}
pub(super) fn settings() -> Result<Settings, Error> {
    autoreleasepool(|_| {
        let center = center()?;
        let (sender, receiver) = mpsc::sync_channel(1);
        let callback = RcBlock::new(move |value: NonNull<UNNotificationSettings>| {
            // SAFETY: UserNotifications provides a non-null settings object
            // valid throughout this callback. Copy only primitive enum values.
            let result = unsafe {
                let value = value.as_ref();
                Settings {
                    permission: permission(value.authorizationStatus().0),
                    alerts: setting(value.alertSetting().0),
                    notification_center: setting(value.notificationCenterSetting().0),
                }
            };
            let _ = sender.try_send(result);
        });
        // SAFETY: the OS copies the block, which owns its bounded sender; no
        // Foundation object or borrowed pointer escapes the callback.
        unsafe { center.getNotificationSettingsWithCompletionHandler(&callback) };
        receiver
            .recv_timeout(CALLBACK_TIMEOUT)
            .map_err(|_| Error::Timeout)
    })
}
pub(super) fn request_authorization() -> Result<Settings, Error> {
    autoreleasepool(|_| {
        let center = center()?;
        let (sender, receiver) = mpsc::sync_channel(1);
        let callback = RcBlock::new(move |_granted: objc2::runtime::Bool, error: *mut NSError| {
            // The Bool isn't a fresh settings snapshot; read settings again.
            let _ = sender.try_send(error.is_null());
        });
        // SAFETY: packaged app; explicit caller intention; captured sender has
        // owned lifetime. No provisional/critical bypass or sound is requested.
        unsafe {
            center.requestAuthorizationWithOptions_completionHandler(
                UNAuthorizationOptions::UNAuthorizationOptionAlert,
                &callback,
            );
        }
        if !receiver
            .recv_timeout(CALLBACK_TIMEOUT)
            .map_err(|_| Error::Timeout)?
        {
            return Err(Error::NativeUnavailable);
        }
        settings()
    })
}
fn identifier(id: &PresentationId) -> Retained<NSString> {
    NSString::from_str(&format!("homeward.{}", id.as_str()))
}
pub(super) fn submit_once(value: &Content) -> Result<Submission, Error> {
    if !settings()?.allows_delivery() {
        return Err(Error::PermissionDenied);
    }
    autoreleasepool(|_| {
        let center = center()?;
        value.check_fresh(SystemTime::now())?;
        // SAFETY: retained mutable content is private to this submission.
        let content = unsafe { UNMutableNotificationContent::new() };
        let title = NSString::from_str(&value.title);
        let body = NSString::from_str(&value.body);
        // SAFETY: bounded plain UTF-8 converted to retained NSStrings. No
        // attachments, URI, provider token, sound, userInfo or executable action.
        unsafe {
            content.setTitle(&title);
            content.setBody(&body);
        }
        let id = identifier(value.id());
        // SAFETY: immediate request (no delayed/background trigger or queue).
        let request = unsafe {
            UNNotificationRequest::requestWithIdentifier_content_trigger(&id, &content, None)
        };
        let (sender, receiver) = mpsc::sync_channel(1);
        let callback = RcBlock::new(move |error: *mut NSError| {
            let _ = sender.try_send(error.is_null());
        });
        // SAFETY: the OS copies the request/block. An ambiguous timeout below
        // is never interpreted as rejection or retried by this adapter.
        unsafe { center.addNotificationRequest_withCompletionHandler(&request, Some(&callback)) };
        if receiver
            .recv_timeout(CALLBACK_TIMEOUT)
            .map_err(|_| Error::Timeout)?
        {
            Ok(Submission::AcceptedByOs)
        } else {
            Err(Error::NativeUnavailable)
        }
    })
}
pub(super) fn delivery_evidence(id: &PresentationId) -> Result<DeliveryEvidence, Error> {
    autoreleasepool(|_| {
        let center = center()?;
        let expected = format!("homeward.{}", id.as_str());
        let (sender, receiver) = mpsc::sync_channel(1);
        let callback = RcBlock::new(move |items: NonNull<NSArray<UNNotification>>| {
            // SAFETY: array/items are provided by the OS and remain valid
            // during this callback. Inspect identifiers only, not other bodies.
            let result = unsafe {
                let items = items.as_ref();
                if items.len() > 512 {
                    DeliveryEvidence::Unknown
                } else if (0..items.len())
                    .filter_map(|index| items.get(index))
                    .any(|item| item.request().identifier().to_string() == expected)
                {
                    DeliveryEvidence::PresentInOsCenter
                } else {
                    DeliveryEvidence::NotObserved
                }
            };
            let _ = sender.try_send(result);
        });
        // SAFETY: callback owns only bounded native result/channel state.
        unsafe { center.getDeliveredNotificationsWithCompletionHandler(&callback) };
        receiver
            .recv_timeout(CALLBACK_TIMEOUT)
            .map_err(|_| Error::Timeout)
    })
}
pub(super) fn remove(id: &PresentationId) -> Result<(), Error> {
    autoreleasepool(|_| {
        let center = center()?;
        let id = identifier(id);
        let ids = NSArray::from_vec(vec![id]);
        // SAFETY: a single identifier, bounded and app-prefixed; remove only
        // this message, never all messages of this app or another session.
        unsafe {
            center.removePendingNotificationRequestsWithIdentifiers(&ids);
            center.removeDeliveredNotificationsWithIdentifiers(&ids);
        }
        Ok(())
    })
}

//! Notification admission atop the shared bounded Android JNI worker.
use super::*;
use crate::android::host::{self, Context, enqueue, wait};
use android_activity::AndroidApp;
use jni::{
    JNIEnv, NativeMethod,
    objects::{JIntArray, JObject, JString, JValue},
};
use std::{
    collections::HashMap,
    sync::{Arc, Mutex, OnceLock, Weak},
};

type Routes = HashMap<PresentationId, Weak<scope::Inner>>;
static ROUTES: OnceLock<Mutex<Routes>> = OnceLock::new();
impl From<host::Failure> for Error {
    fn from(value: host::Failure) -> Self {
        match value {
            host::Failure::Unavailable => Self::NativeUnavailable,
            host::Failure::Timeout => Self::Timeout,
            host::Failure::Capacity => Self::Capacity,
            host::Failure::Closed => Self::ScopeClosed,
        }
    }
}
pub fn initialize(app: &AndroidApp) -> Result<(), Error> {
    host::initialize(app, |env, activity| {
        let class = env.get_object_class(activity)?;
        // Exact signature of the private Java method. Only an opaque ID is
        // copied; resource authority remains with the fresh server grant.
        env.register_native_methods(
            &class,
            &[NativeMethod {
                name: "notificationOpened".into(),
                sig: "(Ljava/lang/String;)V".into(),
                fn_ptr: opened as *mut std::ffi::c_void,
            }],
        )?;
        env.call_method(activity, "attachNotifications", "()V", &[])?;
        Ok(())
    })
    .map_err(Error::from)
}
pub fn destroyed() {
    host::destroyed();
}
impl Context {
    fn settings(&self) -> Result<Settings, Error> {
        let raw = self
            .with_env(|env| {
                let raw = env
                    .call_method(self.activity.as_obj(), "notificationSettings", "()[I", &[])?
                    .l()?;
                let array = JIntArray::from(raw);
                if env.get_array_length(&array)? != 3 {
                    return Ok(None);
                }
                let mut values = [0; 3];
                env.get_int_array_region(&array, 0, &mut values)?;
                Ok(Some(values))
            })?
            .ok_or(Error::NativeUnavailable)?;
        Ok(Settings {
            permission: match raw[0] {
                1 => Permission::NotRequested,
                2 => Permission::Denied,
                3 => Permission::Authorized,
                _ => Permission::Unknown,
            },
            alerts: setting(raw[1]),
            notification_center: setting(raw[2]),
        })
    }
    fn remove(&self, id: &PresentationId) -> Result<(), Error> {
        self.with_env(|env| {
            let id = env.new_string(id.as_str())?;
            env.call_method(
                self.activity.as_obj(),
                "removeNotification",
                "(Ljava/lang/String;)V",
                &[JValue::Object(id.as_ref())],
            )?;
            Ok(())
        })
        .map_err(Error::from)
    }
}
fn setting(raw: i32) -> Setting {
    match raw {
        1 => Setting::Enabled,
        2 => Setting::Disabled,
        3 => Setting::Unsupported,
        _ => Setting::Unknown,
    }
}
pub(super) fn settings() -> Result<Settings, Error> {
    wait(enqueue(Context::settings)?)
}
pub(super) fn request_authorization() -> Result<Settings, Error> {
    wait(enqueue(|context| {
        if !context.is_alive() {
            return Err(Error::ScopeClosed);
        }
        context.with_env(|env| {
            env.call_method(
                context.activity.as_obj(),
                "requestNotifications",
                "()V",
                &[],
            )?;
            Ok(())
        })?;
        // A queued prompt is not a grant. Later reads observe the OS result.
        context.settings()
    })?)
}
pub(super) fn delivery_evidence(id: &PresentationId) -> Result<DeliveryEvidence, Error> {
    let id = id.clone();
    wait(enqueue(move |context| {
        context
            .with_env(|env| {
                let id = env.new_string(id.as_str())?;
                let raw = env
                    .call_method(
                        context.activity.as_obj(),
                        "notificationEvidence",
                        "(Ljava/lang/String;)I",
                        &[JValue::Object(id.as_ref())],
                    )?
                    .i()?;
                Ok(match raw {
                    1 => DeliveryEvidence::PresentInOsCenter,
                    2 => DeliveryEvidence::NotObserved,
                    _ => DeliveryEvidence::Unknown,
                })
            })
            .map_err(Error::from)
    })?)
}
pub(super) fn remove(id: &PresentationId) -> Result<(), Error> {
    let id = id.clone();
    wait(enqueue(move |context| context.remove(&id))?)
}
pub(super) fn submit_once(value: &Content, scope: &PresentationScope) -> Result<Submission, Error> {
    let mut routes = ROUTES
        .get_or_init(Mutex::default)
        .lock()
        .map_err(|_| Error::NativeUnavailable)?;
    routes.retain(|_, scope| {
        scope
            .upgrade()
            .is_some_and(|s| PresentationScope(s).allows_presentation())
    });
    if routes.len() >= 64 {
        return Err(Error::Capacity);
    }
    routes.insert(value.id().clone(), Arc::downgrade(&scope.0));
    drop(routes);
    let content = value.clone();
    let admitted = scope.clone();
    // Only enqueue is serialized with close/retire, never a Binder call.
    let receiver = scope.dispatch(value.id(), SystemTime::now(), || {
        enqueue(move |context| {
            let current = || {
                context.is_alive()
                    && content.check_fresh(SystemTime::now()).is_ok()
                    && admitted
                        .dispatch(content.id(), SystemTime::now(), || ())
                        .is_ok()
            };
            if !current() {
                return Err(Error::ScopeClosed);
            }
            if !context.settings()?.allows_delivery() {
                return Err(Error::PermissionDenied);
            }
            if !current() {
                return Err(Error::ScopeClosed);
            }
            let result = context.with_env(|env| {
                let id = env.new_string(content.id().as_str())?;
                let title = env.new_string(content.title())?;
                let body = env.new_string(content.body())?;
                let remaining = content
                    .expires_at
                    .duration_since(SystemTime::now())
                    .unwrap_or_default();
                env.call_method(
                    context.activity.as_obj(),
                    "postNotification",
                    "(Ljava/lang/String;Ljava/lang/String;Ljava/lang/String;J)V",
                    &[
                        JValue::Object(id.as_ref()),
                        JValue::Object(title.as_ref()),
                        JValue::Object(body.as_ref()),
                        JValue::Long(remaining.as_millis().clamp(1, i64::MAX as u128) as i64),
                    ],
                )?;
                Ok(Submission::AcceptedByOs)
            });
            // Late or uncertain completion never revives a closed/retired scope.
            if !current() {
                let _ = context.remove(content.id());
                return Err(Error::ScopeClosed);
            }
            result.map_err(Error::from)
        })
    })??;
    wait(receiver)
}
unsafe extern "system" fn opened(mut env: JNIEnv<'_>, _this: JObject<'_>, raw: JString<'_>) {
    // No Java string longer than a presentation ID enters Rust-owned memory.
    let result = (|| -> jni::errors::Result<()> {
        if env.call_method(&raw, "length", "()I", &[])?.i()? != 32 {
            return Ok(());
        }
        let value: String = env.get_string(&raw)?.into();
        if let Ok(id) = PresentationId::parse(&value) {
            let route = ROUTES
                .get()
                .and_then(|routes| routes.lock().ok()?.get(&id)?.upgrade());
            if let Some(route) = route {
                PresentationScope(route).opened(&id, SystemTime::now());
            }
        }
        Ok(())
    })();
    if result.is_err() || env.exception_check().unwrap_or(true) {
        let _ = env.exception_clear();
    }
}

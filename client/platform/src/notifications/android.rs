//! Android NotificationManager with one bounded JNI worker. No native Binder
//! call holds a presentation-scope lock or blocks the UI thread.
use super::*;
use android_activity::AndroidApp;
use jni::{
    JNIEnv, JavaVM, NativeMethod,
    objects::{GlobalRef, JIntArray, JObject, JString, JValue},
};
use std::{
    collections::HashMap,
    sync::{
        Arc, Mutex, OnceLock, Weak,
        atomic::{AtomicBool, Ordering},
        mpsc,
    },
    time::Instant,
};

const TIMEOUT: Duration = Duration::from_secs(5);
const MAX_PENDING: usize = 8;
type Operation = Box<dyn FnOnce(&Context) + Send>;
struct Job {
    until: Instant,
    operation: Operation,
}
struct Context {
    vm: JavaVM,
    activity: GlobalRef,
    alive: Arc<AtomicBool>,
}
#[derive(Clone)]
struct Handle {
    jobs: mpsc::SyncSender<Job>,
    alive: Arc<AtomicBool>,
}
static CONTEXT: Mutex<Option<Handle>> = Mutex::new(None);
type Routes = HashMap<PresentationId, Weak<scope::Inner>>;
static ROUTES: OnceLock<Mutex<Routes>> = OnceLock::new();

/// Install only from the real Android entry point; an ELF shell has no app
/// identity. The global reference stays valid even after Activity destruction.
pub fn initialize(app: &AndroidApp) -> Result<(), Error> {
    // SAFETY: AndroidApp owns the VM and its activity throughout this borrow.
    let vm = unsafe { JavaVM::from_raw(app.vm_as_ptr().cast()) }
        .map_err(|_| Error::NativeUnavailable)?;
    let activity = {
        let mut env = vm
            .attach_current_thread()
            .map_err(|_| Error::NativeUnavailable)?;
        // SAFETY: borrowed valid activity, promoted to our own global ref below.
        let activity = unsafe { JObject::from_raw(app.activity_as_ptr().cast()) };
        let result = env.with_local_frame(16, |env| -> jni::errors::Result<_> {
            let class = env.get_object_class(&activity)?;
            // Signature exactly matches the private Java native method;
            // callback copies only a bounded opaque ID and never authorizes it.
            env.register_native_methods(
                &class,
                &[NativeMethod {
                    name: "notificationOpened".into(),
                    sig: "(Ljava/lang/String;)V".into(),
                    fn_ptr: opened as *mut std::ffi::c_void,
                }],
            )?;
            env.call_method(&activity, "attachNotifications", "()V", &[])?;
            env.new_global_ref(activity)
        });
        if env.exception_check().unwrap_or(true) {
            let _ = env.exception_clear();
            return Err(Error::NativeUnavailable);
        }
        result.map_err(|_| Error::NativeUnavailable)?
    };
    let alive = Arc::new(AtomicBool::new(true));
    let context = Context {
        vm,
        activity,
        alive: alive.clone(),
    };
    let (jobs, receiver) = mpsc::sync_channel::<Job>(MAX_PENDING);
    std::thread::Builder::new()
        .name("android-notifications".into())
        .spawn(move || {
            while let Ok(job) = receiver.recv() {
                if Instant::now() < job.until {
                    (job.operation)(&context);
                }
            }
        })
        .map_err(|_| Error::NativeUnavailable)?;
    let mut slot = CONTEXT.lock().map_err(|_| Error::NativeUnavailable)?;
    if let Some(previous) = slot.replace(Handle { jobs, alive }) {
        previous.alive.store(false, Ordering::Release);
    }
    Ok(())
}
/// Activity lifecycle cancellation is immediate, even if a Binder call stalls.
pub fn destroyed() {
    if let Ok(slot) = CONTEXT.lock()
        && let Some(previous) = slot.as_ref()
    {
        // Keep the one bounded worker for targeted cleanup after destruction.
        // A replacement Activity drops this handle; old jobs then join naturally.
        previous.alive.store(false, Ordering::Release);
    }
}
impl Context {
    fn with_env<T>(
        &self,
        f: impl FnOnce(&mut JNIEnv<'_>) -> jni::errors::Result<T>,
    ) -> Result<T, Error> {
        let mut env = self
            .vm
            .attach_current_thread()
            .map_err(|_| Error::NativeUnavailable)?;
        let result = env.with_local_frame(64, f);
        if env.exception_check().unwrap_or(true) {
            let _ = env.exception_clear();
            return Err(Error::NativeUnavailable);
        }
        result.map_err(|_| Error::NativeUnavailable)
    }
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
fn enqueue<T: Send + 'static>(
    operation: impl FnOnce(&Context) -> Result<T, Error> + Send + 'static,
) -> Result<mpsc::Receiver<Result<T, Error>>, Error> {
    let handle = CONTEXT
        .lock()
        .map_err(|_| Error::NativeUnavailable)?
        .clone()
        .ok_or(Error::NativeUnavailable)?;
    let (sender, receiver) = mpsc::sync_channel(1);
    handle
        .jobs
        .try_send(Job {
            until: Instant::now() + TIMEOUT,
            operation: Box::new(move |context| {
                let _ = sender.try_send(operation(context));
            }),
        })
        .map_err(|error| match error {
            mpsc::TrySendError::Full(_) => Error::Capacity,
            mpsc::TrySendError::Disconnected(_) => Error::NativeUnavailable,
        })?;
    Ok(receiver)
}
fn wait<T>(receiver: mpsc::Receiver<Result<T, Error>>) -> Result<T, Error> {
    receiver
        .recv_timeout(TIMEOUT)
        .map_err(|error| match error {
            mpsc::RecvTimeoutError::Timeout => Error::Timeout,
            mpsc::RecvTimeoutError::Disconnected => Error::NativeUnavailable,
        })?
}
pub(super) fn settings() -> Result<Settings, Error> {
    wait(enqueue(Context::settings)?)
}
pub(super) fn request_authorization() -> Result<Settings, Error> {
    wait(enqueue(|context| {
        if !context.alive.load(Ordering::Acquire) {
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
        context.with_env(|env| {
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
                context.alive.load(Ordering::Acquire)
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
            result
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

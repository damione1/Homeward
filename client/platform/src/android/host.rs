//! One bounded JNI worker shared by Android platform adapters. A caller's
//! timeout never creates another thread or an unbounded queue of Binder calls.
use android_activity::AndroidApp;
use jni::{
    JNIEnv, JavaVM,
    objects::{GlobalRef, JObject},
};
use std::{
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
        mpsc,
    },
    time::{Duration, Instant},
};

const TIMEOUT: Duration = Duration::from_secs(5);
const MAX_PENDING: usize = 8;
#[derive(Clone, Copy, Debug)]
pub(crate) enum Failure {
    Unavailable,
    Timeout,
    Capacity,
    Closed,
}
type Operation = Box<dyn FnOnce(&Context) + Send>;
struct Job {
    until: Instant,
    operation: Operation,
}
pub(crate) struct Context {
    vm: JavaVM,
    pub(crate) activity: GlobalRef,
    alive: Arc<AtomicBool>,
}
#[derive(Clone)]
struct Handle {
    jobs: mpsc::SyncSender<Job>,
    alive: Arc<AtomicBool>,
}
static CONTEXT: Mutex<Option<Handle>> = Mutex::new(None);

type Configure = fn(&mut JNIEnv<'_>, &JObject<'_>) -> jni::errors::Result<()>;
pub(crate) fn initialize(app: &AndroidApp, configure: Configure) -> Result<(), Failure> {
    // SAFETY: AndroidApp owns the VM/activity throughout this borrow. The
    // borrowed Java reference is promoted to our own retained global reference.
    let vm =
        unsafe { JavaVM::from_raw(app.vm_as_ptr().cast()) }.map_err(|_| Failure::Unavailable)?;
    let activity = {
        let mut env = vm
            .attach_current_thread()
            .map_err(|_| Failure::Unavailable)?;
        // SAFETY: valid borrowed AndroidApp activity; JObject deletes no ref.
        let activity = unsafe { JObject::from_raw(app.activity_as_ptr().cast()) };
        let result = env.with_local_frame(16, |env| -> jni::errors::Result<_> {
            configure(env, &activity)?;
            env.new_global_ref(activity)
        });
        if env.exception_check().unwrap_or(true) {
            let _ = env.exception_clear();
            return Err(Failure::Unavailable);
        }
        result.map_err(|_| Failure::Unavailable)?
    };
    let alive = Arc::new(AtomicBool::new(true));
    let context = Context {
        vm,
        activity,
        alive: alive.clone(),
    };
    let (jobs, receiver) = mpsc::sync_channel::<Job>(MAX_PENDING);
    std::thread::Builder::new()
        .name("android-platform".into())
        .spawn(move || {
            while let Ok(job) = receiver.recv() {
                if Instant::now() < job.until {
                    (job.operation)(&context);
                }
            }
        })
        .map_err(|_| Failure::Unavailable)?;
    let mut slot = CONTEXT.lock().map_err(|_| Failure::Unavailable)?;
    if let Some(previous) = slot.replace(Handle { jobs, alive }) {
        previous.alive.store(false, Ordering::Release);
    }
    Ok(())
}
pub(crate) fn destroyed() {
    if let Ok(slot) = CONTEXT.lock()
        && let Some(previous) = slot.as_ref()
    {
        // Retain the one worker for targeted notification removal. Replacing
        // the Activity drops the handle; its old queue then joins naturally.
        previous.alive.store(false, Ordering::Release);
    }
}
impl Context {
    pub(crate) fn is_alive(&self) -> bool {
        self.alive.load(Ordering::Acquire)
    }
    pub(crate) fn with_env<T>(
        &self,
        f: impl FnOnce(&mut JNIEnv<'_>) -> jni::errors::Result<T>,
    ) -> Result<T, Failure> {
        let mut env = self
            .vm
            .attach_current_thread()
            .map_err(|_| Failure::Unavailable)?;
        let result = env.with_local_frame(64, f);
        if env.exception_check().unwrap_or(true) {
            let _ = env.exception_clear();
            return Err(Failure::Unavailable);
        }
        result.map_err(|_| Failure::Unavailable)
    }
}
pub(crate) fn enqueue<T: Send + 'static, E: From<Failure> + Send + 'static>(
    operation: impl FnOnce(&Context) -> Result<T, E> + Send + 'static,
) -> Result<mpsc::Receiver<Result<T, E>>, E> {
    let handle = CONTEXT
        .lock()
        .map_err(|_| Failure::Unavailable)?
        .clone()
        .ok_or(Failure::Unavailable)?;
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
            mpsc::TrySendError::Full(_) => Failure::Capacity,
            mpsc::TrySendError::Disconnected(_) => Failure::Unavailable,
        })?;
    Ok(receiver)
}
pub(crate) fn wait<T, E: From<Failure>>(receiver: mpsc::Receiver<Result<T, E>>) -> Result<T, E> {
    receiver
        .recv_timeout(TIMEOUT)
        .map_err(|error| match error {
            mpsc::RecvTimeoutError::Timeout => Failure::Timeout,
            mpsc::RecvTimeoutError::Disconnected => Failure::Unavailable,
        })?
}
pub(crate) fn call<T: Send + 'static>(
    operation: impl FnOnce(&Context) -> Result<T, Failure> + Send + 'static,
) -> Result<T, Failure> {
    wait(enqueue(operation)?)
}

//! One process-lifetime delegate. The OS holds a weak delegate reference;
//! routing holds only weak references to bounded, revocable presentation scopes.
use super::*;
use block2::Block;
use objc2::{
    ClassType, DeclaredClass, declare_class, msg_send_id,
    mutability::InteriorMutable,
    rc::Retained,
    runtime::{NSObject, NSObjectProtocol, ProtocolObject},
};
use objc2_user_notifications::{
    UNNotification, UNNotificationDefaultActionIdentifier, UNNotificationPresentationOptions,
    UNNotificationResponse, UNUserNotificationCenter, UNUserNotificationCenterDelegate,
};
use std::{
    collections::HashMap,
    sync::{Arc, Mutex, OnceLock, Weak},
};

static INSTALLED: Mutex<bool> = Mutex::new(false);
static ROUTES: OnceLock<Mutex<HashMap<PresentationId, Weak<scope::Inner>>>> = OnceLock::new();
fn routes() -> &'static Mutex<HashMap<PresentationId, Weak<scope::Inner>>> {
    ROUTES.get_or_init(|| Mutex::new(HashMap::new()))
}
fn lookup(notification: &UNNotification) -> Option<(PresentationId, PresentationScope)> {
    // SAFETY: OS-owned notification is valid for the duration of the delegate
    // callback. Copy the identifier only; never parse content as a URL/action.
    let raw = unsafe { notification.request().identifier().to_string() };
    let id = PresentationId::parse(raw.strip_prefix("homeward.")?).ok()?;
    let owner = routes().lock().ok()?.get(&id)?.upgrade()?;
    Some((id, PresentationScope(owner)))
}
pub(super) fn install(center: &UNUserNotificationCenter) -> Result<(), Error> {
    let mut installed = INSTALLED.lock().map_err(|_| Error::NativeUnavailable)?;
    if *installed {
        return Ok(());
    }
    // SAFETY: public packaged-app center. Do not replace another subsystem's
    // delegate silently; this adapter owns the center only when it is vacant.
    if unsafe { center.delegate().is_some() } {
        return Err(Error::NativeUnavailable);
    }
    // SAFETY: NSObject subclass with no ivars and no thread-affine state. Its
    // only methods copy opaque IDs and access synchronized Rust scope state.
    let delegate: Retained<HomewardNotificationDelegate> =
        unsafe { msg_send_id![HomewardNotificationDelegate::class(), new] };
    unsafe {
        center.setDelegate(Some(ProtocolObject::from_ref(&*delegate)));
    }
    // UserNotifications keeps a weak reference. Retain exactly one stateless
    // instance for process lifetime; no credential/content is retained here.
    let _permanent_delegate = Retained::into_raw(delegate);
    *installed = true;
    Ok(())
}
pub(super) fn register(scope: &PresentationScope, id: &PresentationId) -> Result<(), Error> {
    let mut routes = routes().lock().map_err(|_| Error::NativeUnavailable)?;
    let now = SystemTime::now();
    routes.retain(|_, owner| {
        owner
            .upgrade()
            .is_some_and(|owner| PresentationScope(owner).current(now))
    });
    if routes.contains_key(id) {
        return Err(Error::AlreadyAttempted);
    }
    if routes.len() >= 128 {
        return Err(Error::Capacity);
    }
    routes.insert(id.clone(), Arc::downgrade(&scope.0));
    Ok(())
}

declare_class!(
    struct HomewardNotificationDelegate;
    // SAFETY: NSObject superclass, no ivars or Drop; synchronization resides
    // entirely in the Rust routing table and scope mutexes/atomics.
    unsafe impl ClassType for HomewardNotificationDelegate {
        type Super=NSObject;
        type Mutability=InteriorMutable;
        const NAME:&'static str="HomewardNotificationDelegate";
    }
    impl DeclaredClass for HomewardNotificationDelegate {}
    unsafe impl NSObjectProtocol for HomewardNotificationDelegate {}
    unsafe impl UNUserNotificationCenterDelegate for HomewardNotificationDelegate {
        #[method(userNotificationCenter:willPresentNotification:withCompletionHandler:)]
        fn will_present(&self,_center:&UNUserNotificationCenter,notification:&UNNotification,
            completion:&Block<dyn Fn(UNNotificationPresentationOptions)>) {
            let allowed=lookup(notification).is_some_and(|(id,scope)|scope.foreground(&id,SystemTime::now()));
            let options=if allowed {
                UNNotificationPresentationOptions::UNNotificationPresentationOptionBanner |
                    UNNotificationPresentationOptions::UNNotificationPresentationOptionList
            } else {UNNotificationPresentationOptions::empty()};
            // Always complete exactly once, including foreign, expired and
            // cancelled messages. No sound/critical-alert bypass is requested.
            completion.call((options,));
        }
        #[method(userNotificationCenter:didReceiveNotificationResponse:withCompletionHandler:)]
        fn did_receive(&self,_center:&UNUserNotificationCenter,response:&UNNotificationResponse,
            completion:&Block<dyn Fn()>) {
            // SAFETY: read-only OS response and exported constant. Only the
            // default open action yields an opaque navigation intent.
            let is_open=unsafe{response.actionIdentifier().as_ref()==UNNotificationDefaultActionIdentifier};
            if is_open {
                let notification=unsafe{response.notification()};
                if let Some((id,scope))=lookup(&notification) {
                    scope.opened(&id,SystemTime::now());
                }
            }
            completion.call(());
        }
    }
);

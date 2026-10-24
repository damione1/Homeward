//! Own the runtime and session outside Slint. Every job is joined on profile
//! replacement/exit; UI callbacks enqueue bounded typed intentions only.
use crate::{
    AlertSeverity, HomewardWindow, ProAccessStage,
    cache_view::{self, CacheProjection},
    preferences::Preferences,
    profile::{ProfileFailure, TrustedEndpoint},
};
use home_client_cache::{
    ActivationPermit, CacheHandle, CacheOwner, DeliveryTicket, Filter, ProjectionMailbox,
    Publication, PublicationRefusal, Viewport,
};
use home_client_session::{
    CredentialKey, Phase, SecureStore, ServerRevocation, SessionCoordinator, SessionView,
    StoreFailure,
};
use slint::{ComponentHandle, Model};
use std::{
    path::PathBuf,
    sync::{
        Arc, Mutex,
        atomic::{AtomicU64, Ordering},
    },
    time::Duration,
};
use tokio::{
    sync::{mpsc, oneshot, watch},
    task::JoinSet,
};
use tokio_util::sync::CancellationToken;
use zeroize::Zeroizing;

pub(crate) enum NativeServices {
    #[cfg(target_os = "macos")]
    MacOs,
    #[cfg(target_os = "android")]
    Android(Result<home_client_platform::NativeStoreContext, StoreFailure>),
    #[cfg(not(any(target_os = "macos", target_os = "android")))]
    Unsupported,
}
impl NativeServices {
    fn directory(&self) -> Result<PathBuf, StoreFailure> {
        match self {
            #[cfg(target_os = "macos")]
            Self::MacOs => home_client_platform::app_data_directory(),
            #[cfg(target_os = "android")]
            Self::Android(context) => context
                .as_ref()
                .map(|value| value.data_directory().to_path_buf())
                .map_err(|error| *error),
            #[cfg(not(any(target_os = "macos", target_os = "android")))]
            Self::Unsupported => Err(StoreFailure::Unsupported),
        }
    }
    fn store(&self, key: CredentialKey) -> Result<Arc<dyn SecureStore>, StoreFailure> {
        match self {
            #[cfg(target_os = "macos")]
            Self::MacOs => Ok(Arc::new(home_client_platform::NativeStore::new(key)?)),
            #[cfg(target_os = "android")]
            Self::Android(context) => Ok(Arc::new(
                context.as_ref().map_err(|error| *error)?.store(key)?,
            )),
            #[cfg(not(any(target_os = "macos", target_os = "android")))]
            Self::Unsupported => Err(StoreFailure::Unsupported),
        }
    }
}

#[derive(Clone, Copy, PartialEq, Eq)]
struct ProfileEpoch(u64);

enum Action {
    Login {
        login: String,
        password: Zeroizing<String>,
    },
    Recovery(Zeroizing<String>),
    Logout,
    Cancel,
    Read(ActivationPermit<CacheProjection>),
}
enum Intent {
    Configure {
        epoch: ProfileEpoch,
        origin: String,
        certificates: String,
    },
    Session {
        epoch: ProfileEpoch,
        action: Action,
    },
    Close,
}

#[derive(Clone)]
struct CacheBinding {
    epoch: ProfileEpoch,
    handle: CacheHandle<CacheProjection>,
}

struct ForegroundAdmission {
    epoch: ProfileEpoch,
    permit: ActivationPermit<CacheProjection>,
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum ReadIntent {
    NotRequested,
    Requested(ProfileEpoch),
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Lifecycle {
    Foreground,
    Suspended,
}

#[derive(Clone)]
pub(crate) struct LifecycleBridge {
    events: watch::Sender<Lifecycle>,
    transition: Arc<Mutex<()>>,
    cache_handle: Arc<Mutex<Option<CacheBinding>>>,
    window: Arc<Mutex<slint::Weak<HomewardWindow>>>,
    foreground_admission: Arc<Mutex<Option<ForegroundAdmission>>>,
    read_intent: Arc<Mutex<ReadIntent>>,
}
impl LifecycleBridge {
    pub(crate) fn new() -> Self {
        let (events, _) = watch::channel(Lifecycle::Foreground);
        Self {
            events,
            transition: Arc::new(Mutex::new(())),
            cache_handle: Arc::new(Mutex::new(None)),
            window: Arc::new(Mutex::new(slint::Weak::default())),
            foreground_admission: Arc::new(Mutex::new(None)),
            read_intent: Arc::new(Mutex::new(ReadIntent::NotRequested)),
        }
    }
    // Called by native event listeners on the UI thread. Cancel publication and
    // clear the visual model before the background worker joins the read futures.
    pub(crate) fn set(&self, state: Lifecycle) {
        let _transition = self.transition.lock().expect("lifecycle gate poisoned");
        if *self.events.borrow() == state {
            return;
        }
        // Capture admission at the native event, before its worker handling can
        // be delayed. A later suspend/logout invalidates this exact permit.
        let admission = match state {
            Lifecycle::Foreground => self
                .cache_handle
                .lock()
                .expect("UI cache handle mutex poisoned")
                .as_ref()
                .and_then(|binding| {
                    // Keep this lock through permit capture. UI revocation must
                    // not be followed by a permit based on an earlier intent.
                    let intent = self.read_intent.lock().expect("read intent mutex poisoned");
                    if *intent != ReadIntent::Requested(binding.epoch) {
                        return None;
                    }
                    binding
                        .handle
                        .prepare_activation()
                        .ok()
                        .map(|permit| ForegroundAdmission {
                            epoch: binding.epoch,
                            permit,
                        })
                }),
            Lifecycle::Suspended => None,
        };
        *self
            .foreground_admission
            .lock()
            .expect("foreground admission mutex poisoned") = admission;
        if state == Lifecycle::Suspended {
            let binding = self
                .cache_handle
                .lock()
                .expect("UI cache handle mutex poisoned")
                .clone();
            if let Some(binding) = binding {
                let _ = binding.handle.invalidate();
            }
            let window = self
                .window
                .lock()
                .expect("lifecycle window mutex poisoned")
                .clone();
            if let Some(window) = window.upgrade() {
                cache_view::clear(&window);
            }
        }
        self.events.send_replace(state);
    }

    fn take_foreground_admission(
        &self,
        epoch: ProfileEpoch,
    ) -> Option<ActivationPermit<CacheProjection>> {
        let _transition = self.transition.lock().expect("lifecycle gate poisoned");
        if *self.events.borrow() != Lifecycle::Foreground {
            return None;
        }
        self.foreground_admission
            .lock()
            .expect("foreground admission mutex poisoned")
            .take()
            .filter(|admission| {
                admission.epoch == epoch
                    && *self.read_intent.lock().expect("read intent mutex poisoned")
                        == ReadIntent::Requested(epoch)
            })
            .map(|admission| admission.permit)
    }
}

fn should_resume_view(requested: bool, phase: Phase) -> bool {
    requested && matches!(phase, Phase::Ready | Phase::Refreshing)
}
// A projection target must not retain the slot containing its own CacheHandle.
// Otherwise Inner.notify -> Ui -> binding -> Inner forms a reference cycle.
struct ProjectionTarget {
    window: slint::Weak<HomewardWindow>,
    epoch: Arc<AtomicU64>,
    profile: ProfileEpoch,
}
impl ProjectionTarget {
    fn post(&self, ticket: DeliveryTicket<CacheProjection>) {
        let (epoch, profile) = (self.epoch.clone(), self.profile);
        let _ = self.window.upgrade_in_event_loop(move |window| {
            if epoch.load(Ordering::SeqCst) == profile.0 {
                ticket.deliver(|generation, projection| {
                    cache_view::render(&window, generation, projection)
                });
            }
        });
    }
}
#[derive(Clone)]
struct Ui {
    window: slint::Weak<HomewardWindow>,
    epoch: Arc<AtomicU64>,
    cache_handle: Arc<Mutex<Option<CacheBinding>>>,
    read_intent: Arc<Mutex<ReadIntent>>,
    projection_request: crate::dashboard::SharedRequest,
    viewport: watch::Sender<Viewport>,
    dashboard_retry: Arc<tokio::sync::Notify>,
}
impl Ui {
    fn invalidate_cache(&self) {
        *self.read_intent.lock().expect("read intent mutex poisoned") = ReadIntent::NotRequested;
        let current = self
            .cache_handle
            .lock()
            .expect("UI cache handle mutex poisoned")
            .clone();
        if let Some(binding) = current
            && binding.epoch.0 == self.epoch.load(Ordering::SeqCst)
        {
            let _ = binding.handle.invalidate();
        }
    }
    fn request_view(&self, epoch: ProfileEpoch) {
        if self.epoch.load(Ordering::SeqCst) == epoch.0 {
            *self.read_intent.lock().expect("read intent mutex poisoned") =
                ReadIntent::Requested(epoch);
        }
    }
    fn post(&self, epoch: ProfileEpoch, update: impl FnOnce(HomewardWindow) + Send + 'static) {
        let current = self.epoch.clone();
        let _ = self.window.upgrade_in_event_loop(move |window| {
            if current.load(Ordering::SeqCst) == epoch.0 {
                update(window);
            }
        });
    }
    fn setup_error(&self, epoch: ProfileEpoch, message: &'static str) {
        self.post(epoch, move |window| {
            window.set_endpoint_ready(false);
            window.set_show_endpoint(true);
            window.set_setup_message(message.into());
        });
    }
    fn view(&self, epoch: ProfileEpoch, view: SessionView) {
        self.post(epoch, move |window| render_view(&window, view));
    }
    fn projection_target(&self, profile: ProfileEpoch) -> ProjectionTarget {
        ProjectionTarget {
            window: self.window.clone(),
            epoch: self.epoch.clone(),
            profile,
        }
    }
    async fn cache_event(
        &self,
        epoch: ProfileEpoch,
        event: home_client_cache::CacheEvent,
        cancel: &CancellationToken,
    ) {
        let (ack, received) = oneshot::channel();
        self.post(epoch, move |window| {
            window.set_cache_error(cache_view::event_message(event).into());
            let _ = ack.send(());
        });
        // One reliable error callback in the UI queue, independently of the
        // replaceable visual projection. Closing the profile interrupts the ack.
        tokio::select! { biased; () = cancel.cancelled() => {}, _ = received => {} }
    }
}

fn profile_message(error: ProfileFailure) -> &'static str {
    match error {
        ProfileFailure::InvalidEndpoint => {
            "Indiquez une adresse HTTPS sans identifiants, chemin, paramètres ou fragment."
        }
        ProfileFailure::InvalidTrust => "Le certificat de confiance PEM est absent ou invalide.",
        ProfileFailure::Unavailable => {
            "La configuration du serveur ne peut pas être enregistrée sur cet appareil."
        }
        ProfileFailure::UnsupportedConfiguration => {
            "La configuration enregistrée est inconnue ou endommagée. Renseignez à nouveau le serveur."
        }
    }
}
fn render_view(window: &HomewardWindow, view: SessionView) {
    window.set_busy(matches!(view.phase, Phase::Busy | Phase::Refreshing));
    window.set_can_recover(false);
    window.set_message_title("".into());
    window.set_message("".into());
    window.set_message_severity(AlertSeverity::Error);
    window.set_session_title("Session sécurisée active".into());
    window.set_session_description(
        "Vos identifiants de session sont conservés dans le stockage sécurisé de cet appareil."
            .into(),
    );
    window.set_session_severity(AlertSeverity::Success);
    if !matches!(view.phase, Phase::Ready | Phase::Refreshing) {
        cache_view::clear(window);
    }
    let message = match view.phase {
        Phase::Ready | Phase::Refreshing => {
            window.set_access_stage(ProAccessStage::Connected);
            None
        }
        Phase::Challenge { recovery, fido2: _ } => {
            window.set_access_stage(ProAccessStage::Challenge);
            window.set_can_recover(recovery);
            None
        }
        Phase::EnrollmentRequired => {
            window.set_access_stage(ProAccessStage::Enrollment);
            None
        }
        Phase::Busy => None,
        Phase::SignedOut | Phase::Closed => {
            window.set_user_draft("".into());
            window.set_access_stage(ProAccessStage::Identity);
            None
        }
        Phase::NeedsLogin => {
            window.set_user_draft("".into());
            window.set_access_stage(ProAccessStage::Identity);
            Some((
                "Connexion nécessaire",
                "Reconnectez-vous pour ouvrir une session. Un renouvellement incertain ne sera pas rejoué.",
            ))
        }
        Phase::CredentialRejected => {
            window.set_access_stage(ProAccessStage::Identity);
            Some((
                "Identifiants refusés",
                "Vérifiez votre identifiant et votre mot de passe.",
            ))
        }
        Phase::RateLimited => {
            window.set_access_stage(ProAccessStage::Identity);
            Some((
                "Trop de tentatives",
                "Attendez avant de réessayer. Aucun nouvel essai n’est lancé automatiquement.",
            ))
        }
        Phase::NetworkUnavailable => {
            window.set_access_stage(ProAccessStage::Identity);
            Some((
                "Connexion impossible",
                "Le serveur est injoignable ou a refusé la connexion. Vérifiez l’adresse et la confiance TLS.",
            ))
        }
        Phase::StorageUnavailable => {
            window.set_access_stage(ProAccessStage::Identity);
            Some((
                "Stockage sécurisé indisponible",
                "La session ne peut pas être conservée de façon sûre. Aucune connexion sans stockage sécurisé n’est proposée.",
            ))
        }
        Phase::UnsupportedResponse => {
            window.set_access_stage(ProAccessStage::Identity);
            Some((
                "Connexion interrompue",
                "La réponse du serveur ou l’horloge de cet appareil ne permet pas de poursuivre.",
            ))
        }
        Phase::AuthorityUnavailable => {
            window.set_access_stage(ProAccessStage::Connected);
            window.set_session_title("Session suspendue".into());
            window.set_session_description(
                "Les données restent masquées tant que le serveur ne peut pas vérifier vos droits."
                    .into(),
            );
            window.set_session_severity(AlertSeverity::Warning);
            Some((
                "Vérification des droits indisponible",
                "Votre session est conservée. Vous pouvez tenter une nouvelle lecture.",
            ))
        }
        Phase::RightsChanged => {
            window.set_access_stage(ProAccessStage::Connected);
            window.set_session_title("Droits actualisés".into());
            window.set_session_description(
                "Une nouvelle lecture est nécessaire pour afficher les données autorisées.".into(),
            );
            window.set_session_severity(AlertSeverity::Warning);
            Some((
                "Droits modifiés",
                "Les anciennes données ont été masquées. Rechargez une vue autorisée.",
            ))
        }
    };
    if let Some((title, message)) = message {
        window.set_message_title(title.into());
        window.set_message(message.into());
    }
    if !matches!(view.phase, Phase::Busy | Phase::Challenge { .. }) {
        window.set_password_draft("".into());
        window.set_recovery_draft("".into());
    }
}

struct Profile {
    epoch: ProfileEpoch,
    owner: SessionCoordinator,
    cache: CacheOwner<CacheProjection>,
    cancel: CancellationToken,
}
async fn close_profile(profile: &mut Option<Profile>, tasks: &mut JoinSet<()>, ui: &Ui) {
    if let Some(previous) = profile.take() {
        {
            let mut binding = ui
                .cache_handle
                .lock()
                .expect("UI cache handle mutex poisoned");
            if binding
                .as_ref()
                .is_some_and(|binding| binding.epoch == previous.epoch)
            {
                *binding = None;
            }
        }
        previous.cancel.cancel();
        previous.cache.close().await;
        previous.owner.close().await;
        while tasks.join_next().await.is_some() {}
        // Drop the OS process lease before constructing a replacement profile.
    }
}
async fn install_profile(
    endpoint: TrustedEndpoint,
    epoch: ProfileEpoch,
    services: &NativeServices,
    ui: &Ui,
    tasks: &mut JoinSet<()>,
) -> Result<Profile, StoreFailure> {
    let store = services.store(endpoint.key())?;
    endpoint
        .save(&services.directory()?)
        .map_err(|_| StoreFailure::Unavailable)?;
    let connection = endpoint.connection();
    let owner = SessionCoordinator::new(connection.auth(), store);
    let cancel = CancellationToken::new();
    let origin = endpoint.origin().to_owned();
    let certificates = endpoint.certificates().to_owned();
    let fingerprint = endpoint
        .fingerprint()
        .as_bytes()
        .chunks(8)
        .map(|chunk| std::str::from_utf8(chunk).expect("hex ASCII"))
        .collect::<Vec<_>>()
        .join(" ");
    ui.post(epoch, move |window| {
        window.set_server_draft(origin.clone().into());
        window.set_certificate_draft(certificates.into());
        window.set_endpoint_label(origin.into());
        window.set_trust_fingerprint(fingerprint.into());
        window.set_endpoint_ready(true);
        window.set_show_endpoint(false);
        window.set_setup_message("".into());
        cache_view::clear(&window);
    });
    let cache_target = ui.projection_target(epoch);
    let presentation = ui.projection_request.clone();
    let (cache, mut events) = CacheOwner::start(
        owner.clone(),
        connection.state(),
        Filter::all(),
        move |phase, view, viewport| {
            let request = presentation
                .lock()
                .expect("presentation request mutex poisoned")
                .clone();
            cache_view::project_request(phase, view, viewport, &request)
        },
        move |ticket| cache_target.post(ticket),
    );
    let cache_handle = cache.handle();
    let (dashboard_session, dashboard_ui, dashboard_cancel) =
        (owner.clone(), ui.clone(), cancel.clone());
    let dashboard_transport = connection.dashboard();
    let dashboard_retry = ui.dashboard_retry.clone();
    tasks.spawn(async move {
        let guard = dashboard_session.clone();
        let mailbox = ProjectionMailbox::new();
        crate::dashboard_host::run(
            dashboard_transport,
            dashboard_session,
            dashboard_cancel,
            dashboard_retry,
            move |generation, layout| {
                let ticket = match mailbox.publish(mailbox.generation(), (generation, layout)) {
                    Publication::Schedule(ticket) => ticket,
                    Publication::Coalesced
                    | Publication::Rejected(
                        PublicationRefusal::Closed | PublicationRefusal::Obsolete,
                    ) => return,
                };
                let (guard, presentation) =
                    (guard.clone(), dashboard_ui.projection_request.clone());
                dashboard_ui.post(epoch, move |window| {
                    ticket.deliver(|_, (generation, layout)| {
                        if !guard.is_current(generation) {
                            return;
                        }
                        {
                            let mut request = presentation
                                .lock()
                                .expect("presentation request mutex poisoned");
                            if request.dashboard == layout {
                                return;
                            }
                            request.dashboard = layout;
                        }
                        window.invoke_dashboard_changed();
                    });
                });
            },
        )
        .await;
    });
    *ui.cache_handle
        .lock()
        .expect("UI cache handle mutex poisoned") = Some(CacheBinding {
        epoch,
        handle: cache_handle.clone(),
    });
    let (event_ui, event_cancel) = (ui.clone(), cancel.clone());
    tasks.spawn(async move {
        loop {
            let event = tokio::select! { biased; () = event_cancel.cancelled() => break, event = events.recv() => event };
            let Some(event) = event else { break; };
            event_ui.cache_event(epoch, event, &event_cancel).await;
        }
    });
    let (observer, observer_cancel, observer_ui) = (owner.clone(), cancel.clone(), ui.clone());
    tasks.spawn(async move {
        let mut changes = observer.subscribe();
        loop {
            let view = *changes.borrow_and_update();
            let _ = cache_handle.session_changed(view.phase);
            observer_ui.view(epoch, view);
            tokio::select! {
                () = observer_cancel.cancelled() => break,
                changed = changes.changed() => if changed.is_err() { break; },
            }
        }
    });
    let restore = owner.clone();
    tasks.spawn(async move {
        let _ = restore.restore().await;
    });
    Ok(Profile {
        epoch,
        owner,
        cache,
        cancel,
    })
}

async fn session_action(
    action: Action,
    owner: SessionCoordinator,
    cache: CacheHandle<CacheProjection>,
    ui: Ui,
    epoch: ProfileEpoch,
) {
    match action {
        Action::Login { login, password } => {
            let _ = cache.invalidate();
            let _ = owner.login(login, password).await;
        }
        Action::Recovery(code) => {
            let _ = owner.complete_recovery(code).await;
        }
        Action::Cancel => {
            let _ = cache.invalidate();
            let _ = owner.logout_local().await;
        }
        Action::Logout => {
            let _ = cache.invalidate();
            if let Ok(ServerRevocation::Uncertain) = owner.logout().await {
                ui.post(epoch, |window| {
                    window.set_message_title("Déconnecté sur cet appareil".into());
                    window.set_message(
                        "La révocation sur le serveur n’a pas pu être confirmée.".into(),
                    );
                    window.set_message_severity(AlertSeverity::Warning);
                });
            }
        }
        Action::Read(permit) => {
            ui.dashboard_retry.notify_one();
            let phase = owner.subscribe().borrow().phase;
            if matches!(phase, Phase::AuthorityUnavailable | Phase::RightsChanged)
                && owner.allow_new_view_attempt().await.is_err()
            {
                return;
            }
            let _ = cache.activate_prepared(permit);
        }
    }
}

async fn worker(
    mut input: mpsc::Receiver<Intent>,
    services: NativeServices,
    ui: Ui,
    mut viewport: watch::Receiver<Viewport>,
    mut preferences: watch::Receiver<Preferences>,
    bridge: LifecycleBridge,
) {
    let mut lifecycle = bridge.events.subscribe();
    let mut profile = None;
    let mut tasks = JoinSet::new();
    let epoch = ProfileEpoch(0);
    match services.directory() {
        Ok(directory) => match TrustedEndpoint::load(&directory) {
            Ok(Some(endpoint)) => match install_profile(endpoint, epoch, &services, &ui, &mut tasks)
                .await
            {
                Ok(value) => profile = Some(value),
                Err(_) => ui.setup_error(
                    epoch,
                    "Le stockage sécurisé est indisponible ou déjà utilisé par une autre instance.",
                ),
            },
            Ok(None) => {}
            Err(error) => ui.setup_error(epoch, profile_message(error)),
        },
        Err(_) => ui.setup_error(
            epoch,
            "Les services sécurisés de cet appareil sont indisponibles.",
        ),
    }
    let mut lifetime = tokio::time::interval(Duration::from_secs(1));
    lifetime.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            intent = input.recv() => match intent {
                Some(Intent::Configure { epoch, origin, certificates }) => {
                    if ui.epoch.load(Ordering::SeqCst) != epoch.0 { continue; }
                    close_profile(&mut profile, &mut tasks, &ui).await;
                    match TrustedEndpoint::parse(&origin, &certificates) {
                        Ok(endpoint) => match install_profile(endpoint, epoch, &services, &ui, &mut tasks).await {
                            Ok(value) => profile = Some(value),
                            Err(_) => ui.setup_error(epoch, "Le stockage sécurisé est indisponible ou déjà utilisé par une autre instance."),
                        },
                        Err(error) => ui.setup_error(epoch, profile_message(error)),
                    }
                }
                Some(Intent::Session { epoch, action }) => {
                    if let Some(profile) = &mut profile
                        && profile.epoch == epoch && ui.epoch.load(Ordering::SeqCst) == epoch.0 {
                        if matches!(&action, Action::Read(_)) && *lifecycle.borrow() == Lifecycle::Suspended { continue; }
                        match action {
                            Action::Cancel | Action::Logout => session_action(action, profile.owner.clone(), profile.cache.handle(), ui.clone(), epoch).await,
                            Action::Login { .. } | Action::Recovery(_) | Action::Read(_) => {
                                if tasks.len() < 20 {
                                    tasks.spawn(session_action(action, profile.owner.clone(), profile.cache.handle(), ui.clone(), epoch));
                                } else {
                                    ui.post(epoch, |window| {
                                        window.set_message_title("Opération en attente".into());
                                        window.set_message("Réessayez dans un instant.".into());
                                    });
                                }
                            }
                        }
                    }
                }
                Some(Intent::Close) | None => break,
            },
            _ = tasks.join_next(), if !tasks.is_empty() => {},
            changed = viewport.changed() => {
                if changed.is_err() { break; }
                let visible = *viewport.borrow_and_update();
                if let Some(profile) = &profile { let _ = profile.cache.handle().viewport(visible); }
            },
            changed = preferences.changed() => {
                if changed.is_err() { break; }
                let value = *preferences.borrow_and_update();
                let saved = services.directory().ok().is_some_and(|directory| value.save(&directory).is_ok());
                let window = ui.window.clone();
                let _ = window.upgrade_in_event_loop(move |window| {
                    // An older save cannot acknowledge a more recent edit.
                    if Preferences::current(&window) == value {
                        window.set_preferences_status(if saved {
                            "Préférences enregistrées sur cet appareil."
                        } else {
                            "Ces préférences sont actives, mais leur enregistrement a échoué."
                        }.into());
                    }
                });
            },
            changed = lifecycle.changed() => {
                if changed.is_err() { break; }
                let state = *lifecycle.borrow_and_update();
                if let Some(profile) = &profile {
                    let cache = profile.cache.handle();
                    match state {
                        // The native callback already invalidated synchronously.
                        // Only join it: a fresh suspension control here could
                        // invalidate an admission from a newer Foreground event.
                        Lifecycle::Suspended => { let _ = cache.wait_for_control().await; },
                        Lifecycle::Foreground => {
                            ui.dashboard_retry.notify_one();
                            let phase = profile.owner.subscribe().borrow().phase;
                            let requested = *bridge.read_intent.lock().expect("read intent mutex poisoned") == ReadIntent::Requested(profile.epoch);
                            if should_resume_view(requested, phase)
                                && let Some(permit) = bridge.take_foreground_admission(profile.epoch) {
                                let _ = cache.activate_prepared(permit);
                            }
                        },
                    }
                }
            },
            _ = lifetime.tick() => {
                if let Some(profile) = &profile { let _ = profile.owner.revalidate_lifetime().await; }
            },
        }
    }
    if preferences.has_changed().unwrap_or(false)
        && let Ok(directory) = services.directory()
    {
        let _ = preferences.borrow_and_update().save(&directory);
    }
    close_profile(&mut profile, &mut tasks, &ui).await;
}

pub(crate) fn run(services: NativeServices) -> Result<(), slint::PlatformError> {
    run_with_lifecycle(services, LifecycleBridge::new())
}

pub(crate) fn run_with_lifecycle(
    services: NativeServices,
    lifecycle: LifecycleBridge,
) -> Result<(), slint::PlatformError> {
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .map_err(|_| {
            slint::PlatformError::Other("Le moteur de connexion n’a pas pu démarrer.".into())
        })?;
    let window = HomewardWindow::new()?;
    let initial_preferences = match services.directory() {
        Ok(directory) => match Preferences::load(&directory) {
            Ok(value) => value,
            Err(_) => {
                window.set_preferences_status(
                    "Les préférences enregistrées n’ont pas pu être chargées.".into(),
                );
                Preferences::default()
            }
        },
        Err(_) => Preferences::default(),
    };
    initial_preferences.apply(&window);
    let (preferences_tx, preferences) = watch::channel(initial_preferences);
    let preferences_window = window.as_weak();
    window.on_preferences_changed(move |dark, opaque, large, reduced| {
        preferences_tx.send_replace(Preferences::from_ui(dark, opaque, large, reduced));
        if let Some(window) = preferences_window.upgrade() {
            window.set_preferences_status("Enregistrement…".into());
        }
    });
    *lifecycle
        .window
        .lock()
        .expect("lifecycle window mutex poisoned") = window.as_weak();
    #[cfg(not(target_os = "android"))]
    {
        use slint::winit_030::{EventResult, WinitWindowAccessor, winit::event::WindowEvent};
        let events = lifecycle.clone();
        window.window().on_winit_window_event(move |_, event| {
            if let WindowEvent::Occluded(occluded) = event {
                events.set(if *occluded {
                    Lifecycle::Suspended
                } else {
                    Lifecycle::Foreground
                });
            } else if matches!(event, WindowEvent::Destroyed) {
                events.set(Lifecycle::Suspended);
            }
            EventResult::Propagate
        });
    }
    cache_view::bind(&window);
    let (viewport_tx, viewport) = watch::channel(Viewport::default());
    let ui = Ui {
        window: window.as_weak(),
        epoch: Arc::new(AtomicU64::new(0)),
        cache_handle: lifecycle.cache_handle.clone(),
        read_intent: lifecycle.read_intent.clone(),
        projection_request: Arc::new(Mutex::new(crate::dashboard::ProjectionRequest::default())),
        viewport: viewport_tx.clone(),
        dashboard_retry: Arc::new(tokio::sync::Notify::new()),
    };
    let (sender, input) = mpsc::channel(16);
    let presentation_ui = ui.clone();
    crate::navigation::bind(&window, ui.projection_request.clone(), move || {
        let binding = presentation_ui
            .cache_handle
            .lock()
            .expect("UI cache handle mutex poisoned")
            .clone();
        if let Some(binding) = binding
            && binding.epoch.0 == presentation_ui.epoch.load(Ordering::SeqCst)
        {
            let _ = binding.handle.viewport(*presentation_ui.viewport.borrow());
        }
    });
    let clock_window = window.as_weak();
    let clock = slint::Timer::default();
    let update_clock = move || {
        if let Some(window) = clock_window.upgrade() {
            let now = chrono::Local::now();
            window.set_local_clock(now.format("%H:%M").to_string().into());
            window.set_local_date(now.format("%d/%m/%Y").to_string().into());
        }
    };
    update_clock();
    clock.start(
        slint::TimerMode::Repeated,
        Duration::from_secs(30),
        update_clock,
    );
    let visible = viewport_tx.clone();
    window.on_cache_viewport(move |first, count| {
        let first = (first.max(0) as usize)
            .saturating_sub(8)
            .min(home_client_cache::MAX_ENTITIES - 1);
        let count = count.clamp(1, 64) as usize;
        if let Ok(viewport) = Viewport::new(first, count) {
            visible.send_if_modified(|previous| {
                if *previous == viewport {
                    false
                } else {
                    *previous = viewport;
                    true
                }
            });
        }
    });
    let weak = window.as_weak();
    window.on_cache_navigate(move |step| {
        use crate::NavigationStep;
        let Some(window) = weak.upgrade() else {
            return;
        };
        let count = window.get_cache_rows().row_count();
        if count == 0 {
            return;
        }
        let current = window.get_cache_active_index().max(0) as usize;
        let page = viewport_tx.borrow().count().saturating_sub(16).max(1);
        let next = match step {
            NavigationStep::Next => current.saturating_add(1),
            NavigationStep::Previous => current.saturating_sub(1),
            NavigationStep::First => 0,
            NavigationStep::Last => count - 1,
            NavigationStep::PageNext => current.saturating_add(page),
            NavigationStep::PagePrevious => current.saturating_sub(page),
        }
        .min(count - 1);
        window.set_cache_active_index(next as i32);
    });
    let host = runtime.spawn(worker(
        input,
        services,
        ui.clone(),
        viewport,
        preferences,
        lifecycle.clone(),
    ));
    let (configure_sender, configure_ui) = (sender.clone(), ui.clone());
    window.on_connect_endpoint(move |origin, certificates| {
        configure_ui.invalidate_cache();
        let epoch = ProfileEpoch(configure_ui.epoch.fetch_add(1, Ordering::SeqCst) + 1);
        if let Some(window) = configure_ui.window.upgrade() {
            window.set_user_draft("".into());
            window.set_password_draft("".into());
            window.set_recovery_draft("".into());
            window.set_endpoint_ready(false);
            cache_view::clear(&window);
            window.set_setup_message("Ouverture du stockage sécurisé…".into());
        }
        if configure_sender
            .try_send(Intent::Configure {
                epoch,
                origin: origin.into(),
                certificates: certificates.into(),
            })
            .is_err()
        {
            configure_ui.setup_error(
                epoch,
                "Une opération est en cours. Réessayez dans un instant.",
            );
        }
    });
    let send = {
        let sender = sender.clone();
        let ui = ui.clone();
        move |action| {
            if let Some(window) = ui.window.upgrade() {
                if !window.get_endpoint_ready() {
                    return;
                }
                if matches!(
                    &action,
                    Action::Login { .. } | Action::Logout | Action::Cancel
                ) {
                    ui.invalidate_cache();
                    cache_view::clear(&window);
                }
                let epoch = ProfileEpoch(ui.epoch.load(Ordering::SeqCst));
                let requesting_read = matches!(&action, Action::Read(_));
                if sender.try_send(Intent::Session { epoch, action }).is_err() {
                    window.set_message_title("Opération en attente".into());
                    window.set_message("Réessayez dans un instant.".into());
                } else if requesting_read {
                    ui.request_view(epoch);
                    // Move focus as part of this admitted user intention. A
                    // later data publication must not steal a newer focus.
                    window.invoke_take_shell_focus(crate::ProShellFocus::Workspace);
                }
            }
        }
    };
    let (login, weak) = (send.clone(), window.as_weak());
    window.on_sign_in(move |login_name, password| {
        let password = Zeroizing::new(password.to_string());
        if let Some(window) = weak.upgrade() {
            window.set_password_draft("".into());
        }
        login(Action::Login {
            login: login_name.into(),
            password,
        });
    });
    let (recover, weak) = (send.clone(), window.as_weak());
    window.on_verify_recovery(move |code| {
        let code = Zeroizing::new(code.to_string());
        if let Some(window) = weak.upgrade() {
            window.set_recovery_draft("".into());
        }
        recover(Action::Recovery(code));
    });
    let logout = send.clone();
    window.on_sign_out(move || logout(Action::Logout));
    let cancel = send.clone();
    window.on_cancel_access(move || cancel(Action::Cancel));
    window.on_read_protected(move || {
        let binding = ui
            .cache_handle
            .lock()
            .expect("UI cache handle mutex poisoned")
            .clone();
        if let Some(binding) = binding
            && binding.epoch.0 == ui.epoch.load(Ordering::SeqCst)
            && let Ok(permit) = binding.handle.prepare_activation()
        {
            send(Action::Read(permit));
        }
    });
    let result = window.run();
    let _ = sender.blocking_send(Intent::Close);
    let _ = runtime.block_on(host);
    result
}

#[cfg(test)]
mod tests {
    use super::*;

    struct UnusedCredentialStore;
    impl SecureStore for UnusedCredentialStore {
        fn load(&self) -> Result<Option<Zeroizing<Vec<u8>>>, StoreFailure> {
            Ok(None)
        }
        fn save(&self, _: &[u8]) -> Result<(), StoreFailure> {
            Err(StoreFailure::Unavailable)
        }
        fn clear(&self) -> Result<(), StoreFailure> {
            Ok(())
        }
    }

    #[tokio::test]
    async fn native_foreground_admission_cannot_outlive_suspension_or_logout() {
        use home_client_cache::{ControlFailure, ControlOutcome};
        let connection = home_client_transport::Connection::new(
            "https://127.0.0.1:1",
            include_bytes!("../tests/fixtures/trust-a.pem"),
        )
        .unwrap();
        // No login or restore: the test exercises actual cache controls without
        // credentials or network calls, independently of session admission.
        let session = SessionCoordinator::new(connection.auth(), Arc::new(UnusedCredentialStore));
        let (owner, _events) = CacheOwner::start(
            session.clone(),
            connection.state(),
            Filter::all(),
            cache_view::project,
            drop,
        );
        let handle = owner.handle();
        let bridge = LifecycleBridge::new();
        let epoch = ProfileEpoch(7);
        *bridge.cache_handle.lock().unwrap() = Some(CacheBinding {
            epoch,
            handle: handle.clone(),
        });
        let ui = Ui {
            window: slint::Weak::default(),
            epoch: Arc::new(AtomicU64::new(epoch.0)),
            cache_handle: bridge.cache_handle.clone(),
            read_intent: bridge.read_intent.clone(),
            projection_request: Arc::new(
                Mutex::new(crate::dashboard::ProjectionRequest::default()),
            ),
            viewport: watch::channel(Viewport::default()).0,
            dashboard_retry: Arc::new(tokio::sync::Notify::new()),
        };
        ui.request_view(epoch);

        bridge.set(Lifecycle::Suspended);
        bridge.set(Lifecycle::Foreground);
        let copied_by_worker = *bridge.events.borrow();
        assert_eq!(copied_by_worker, Lifecycle::Foreground);
        // The worker is delayed after copying Foreground. A native suspension
        // must prevent it from obtaining a fresh activation afterward.
        bridge.set(Lifecycle::Suspended);
        assert!(bridge.take_foreground_admission(epoch).is_none());

        bridge.set(Lifecycle::Foreground);
        let taken_before_suspend = bridge.take_foreground_admission(epoch).unwrap();
        bridge.set(Lifecycle::Suspended);
        assert_eq!(
            handle.activate_prepared(taken_before_suspend),
            Ok(ControlOutcome::Superseded)
        );

        bridge.set(Lifecycle::Foreground);
        // Native foreground is queued, then the UI logout invalidates before
        // the worker consumes the event. It cannot manufacture a newer permit.
        let queued_before_logout = bridge.take_foreground_admission(epoch).unwrap();
        ui.invalidate_cache();
        assert_eq!(
            handle.activate_prepared(queued_before_logout),
            Ok(ControlOutcome::Superseded)
        );

        // Logout is still queued in the worker. A subsequent native cycle must
        // not mint any fresh permit from the previous read intention.
        bridge.set(Lifecycle::Suspended);
        bridge.set(Lifecycle::Foreground);
        assert!(bridge.take_foreground_admission(epoch).is_none());

        // Also cover logout before the already captured event is consumed.
        ui.request_view(epoch);
        bridge.set(Lifecycle::Suspended);
        bridge.set(Lifecycle::Foreground);
        ui.invalidate_cache();
        assert!(bridge.take_foreground_admission(epoch).is_none());
        ui.request_view(ProfileEpoch(8));
        assert!(matches!(
            *bridge.read_intent.lock().unwrap(),
            ReadIntent::NotRequested
        ));

        // Only a fresh admitted read for this profile permits resume again.
        ui.request_view(epoch);
        bridge.set(Lifecycle::Suspended);
        bridge.set(Lifecycle::Foreground);
        assert!(bridge.take_foreground_admission(ProfileEpoch(8)).is_none());
        assert!(bridge.take_foreground_admission(epoch).is_none());

        bridge.set(Lifecycle::Suspended);
        let copied_suspension = *bridge.events.borrow();
        assert_eq!(copied_suspension, Lifecycle::Suspended);
        bridge.set(Lifecycle::Foreground);
        // Foreground arrives while the worker still handles the copied suspend.
        // Joining the existing invalidation must not invalidate this new permit.
        assert_eq!(handle.wait_for_control().await, Ok(ControlOutcome::Applied));
        let current = bridge.take_foreground_admission(epoch).unwrap();
        assert_eq!(
            handle.activate_prepared(current),
            Ok(ControlOutcome::Applied)
        );
        assert!(bridge.take_foreground_admission(epoch).is_none());
        owner.close().await;
        assert_eq!(handle.resume(), Err(ControlFailure::Closed));
        assert_eq!(handle.wait_for_control().await, Err(ControlFailure::Closed));
        session.close().await;
    }

    #[test]
    fn a_queued_projection_target_does_not_retain_its_own_control_slot() {
        let ui = Ui {
            window: slint::Weak::default(),
            epoch: Arc::new(AtomicU64::new(0)),
            cache_handle: Arc::new(Mutex::new(None)),
            read_intent: Arc::new(Mutex::new(ReadIntent::NotRequested)),
            projection_request: Arc::new(
                Mutex::new(crate::dashboard::ProjectionRequest::default()),
            ),
            viewport: watch::channel(Viewport::default()).0,
            dashboard_retry: Arc::new(tokio::sync::Notify::new()),
        };
        let slot = Arc::downgrade(&ui.cache_handle);
        let target = ui.projection_target(ProfileEpoch(0));
        let callback = move |ticket| target.post(ticket);
        drop(ui);
        assert!(
            slot.upgrade().is_none(),
            "projection callback retains the cache-control slot"
        );
        drop(callback);
    }

    #[test]
    fn lifecycle_notifications_are_typed_and_duplicate_native_events_do_not_restart_work() {
        let bridge = LifecycleBridge::new();
        let mut events = bridge.events.subscribe();
        assert_eq!(*events.borrow(), Lifecycle::Foreground);
        bridge.set(Lifecycle::Suspended);
        assert!(events.has_changed().unwrap());
        assert_eq!(*events.borrow_and_update(), Lifecycle::Suspended);
        bridge.set(Lifecycle::Suspended);
        assert!(!events.has_changed().unwrap());
        bridge.set(Lifecycle::Foreground);
        assert!(events.has_changed().unwrap());
        assert_eq!(*events.borrow_and_update(), Lifecycle::Foreground);
    }

    #[test]
    fn foreground_resume_never_opens_an_unrequested_or_suspended_authority_view() {
        for phase in [Phase::Ready, Phase::Refreshing] {
            assert!(should_resume_view(true, phase));
            assert!(!should_resume_view(false, phase));
        }
        for phase in [
            Phase::SignedOut,
            Phase::Busy,
            Phase::Challenge {
                recovery: false,
                fido2: true,
            },
            Phase::EnrollmentRequired,
            Phase::NeedsLogin,
            Phase::CredentialRejected,
            Phase::RateLimited,
            Phase::NetworkUnavailable,
            Phase::AuthorityUnavailable,
            Phase::RightsChanged,
            Phase::StorageUnavailable,
            Phase::UnsupportedResponse,
            Phase::Closed,
        ] {
            assert!(!should_resume_view(true, phase));
        }
    }
}

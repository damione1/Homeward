//! Own the runtime and session outside Slint. Every job is joined on profile
//! replacement/exit; UI callbacks enqueue bounded typed intentions only.
use crate::{
    AlertSeverity, HomewardWindow, ProAccessStage,
    profile::{ProfileFailure, TrustedEndpoint},
};
use home_client_session::{
    CredentialKey, Phase, SecureStore, ServerRevocation, SessionCoordinator, SessionView,
    StoreFailure,
};
use home_client_transport::Connection;
use slint::ComponentHandle;
use std::{
    path::PathBuf,
    sync::{
        Arc,
        atomic::{AtomicU64, Ordering},
    },
    time::Duration,
};
use tokio::{sync::mpsc, task::JoinSet};
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
    Read,
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
struct Ui {
    window: slint::Weak<HomewardWindow>,
    epoch: Arc<AtomicU64>,
}
impl Ui {
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
    if view.phase != Phase::Ready {
        window.set_read_status("".into());
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
    connection: Connection,
    cancel: CancellationToken,
}
async fn close_profile(profile: &mut Option<Profile>, tasks: &mut JoinSet<()>) {
    if let Some(previous) = profile.take() {
        previous.cancel.cancel();
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
    });
    let (observer, observer_cancel, observer_ui) = (owner.clone(), cancel.clone(), ui.clone());
    tasks.spawn(async move {
        let mut changes = observer.subscribe();
        loop {
            let view = *changes.borrow_and_update();
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
        connection,
        cancel,
    })
}

async fn session_action(
    action: Action,
    owner: SessionCoordinator,
    connection: Connection,
    cancel: CancellationToken,
    ui: Ui,
    epoch: ProfileEpoch,
) {
    match action {
        Action::Login { login, password } => {
            let _ = owner.login(login, password).await;
        }
        Action::Recovery(code) => {
            let _ = owner.complete_recovery(code).await;
        }
        Action::Cancel => {
            let _ = owner.logout_local().await;
        }
        Action::Logout => {
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
        Action::Read => {
            let phase = owner.subscribe().borrow().phase;
            if matches!(phase, Phase::AuthorityUnavailable | Phase::RightsChanged)
                && owner.allow_new_view_attempt().await.is_err()
            {
                return;
            }
            let Ok(lease) = owner.access(&cancel).await else {
                return;
            };
            let result = connection
                .state()
                .open_view(
                    crate::wire::home::state::v1::OpenViewRequest {
                        filter: crate::wire::home::state::v1::ViewFilter::default().into(),
                        page_size: 16,
                        ..Default::default()
                    },
                    lease.token(),
                    Duration::from_secs(10),
                    &lease.cancellation,
                )
                .await;
            if !owner.is_current(lease.generation) {
                return;
            }
            match result {
                Ok(page) => {
                    let count = page.entities.len();
                    let current = lease.cancellation;
                    ui.post(epoch, move |window| {
                        if !current.is_cancelled() {
                            window.set_read_status(
                                format!(
                                    "Lecture autorisée : {count} {} dans cette première page.",
                                    if count == 1 { "entité" } else { "entités" }
                                )
                                .into(),
                            );
                        }
                    });
                }
                Err(failure) => {
                    let _ = owner.observe_failure(lease.generation, &failure).await;
                    let current = lease.cancellation;
                    ui.post(epoch, move |window| {
                        if !current.is_cancelled() {
                            window.set_read_status("Lecture impossible. La session est conservée tant qu’elle n’est pas révoquée.".into());
                        }
                    });
                }
            }
        }
    }
}

async fn worker(mut input: mpsc::Receiver<Intent>, services: NativeServices, ui: Ui) {
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
                    close_profile(&mut profile, &mut tasks).await;
                    match TrustedEndpoint::parse(&origin, &certificates) {
                        Ok(endpoint) => match install_profile(endpoint, epoch, &services, &ui, &mut tasks).await {
                            Ok(value) => profile = Some(value),
                            Err(_) => ui.setup_error(epoch, "Le stockage sécurisé est indisponible ou déjà utilisé par une autre instance."),
                        },
                        Err(error) => ui.setup_error(epoch, profile_message(error)),
                    }
                }
                Some(Intent::Session { epoch, action }) => {
                    if let Some(profile) = &profile
                        && profile.epoch == epoch && ui.epoch.load(Ordering::SeqCst) == epoch.0 {
                        match action {
                            Action::Cancel | Action::Logout => session_action(action, profile.owner.clone(), profile.connection.clone(), profile.cancel.clone(), ui.clone(), epoch).await,
                            Action::Login { .. } | Action::Recovery(_) | Action::Read => {
                                if tasks.len() < 20 {
                                    tasks.spawn(session_action(action, profile.owner.clone(), profile.connection.clone(), profile.cancel.clone(), ui.clone(), epoch));
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
            _ = lifetime.tick() => {
                if let Some(profile) = &profile { let _ = profile.owner.revalidate_lifetime().await; }
            },
        }
    }
    close_profile(&mut profile, &mut tasks).await;
}

pub(crate) fn run(services: NativeServices) -> Result<(), slint::PlatformError> {
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .map_err(|_| {
            slint::PlatformError::Other("Le moteur de connexion n’a pas pu démarrer.".into())
        })?;
    let window = HomewardWindow::new()?;
    let ui = Ui {
        window: window.as_weak(),
        epoch: Arc::new(AtomicU64::new(0)),
    };
    let (sender, input) = mpsc::channel(16);
    let host = runtime.spawn(worker(input, services, ui.clone()));
    let (configure_sender, configure_ui) = (sender.clone(), ui.clone());
    window.on_connect_endpoint(move |origin, certificates| {
        let epoch = ProfileEpoch(configure_ui.epoch.fetch_add(1, Ordering::SeqCst) + 1);
        if let Some(window) = configure_ui.window.upgrade() {
            window.set_user_draft("".into());
            window.set_password_draft("".into());
            window.set_recovery_draft("".into());
            window.set_endpoint_ready(false);
            window.set_read_status("".into());
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
                if sender
                    .try_send(Intent::Session {
                        epoch: ProfileEpoch(ui.epoch.load(Ordering::SeqCst)),
                        action,
                    })
                    .is_err()
                {
                    window.set_message_title("Opération en attente".into());
                    window.set_message("Réessayez dans un instant.".into());
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
    window.on_read_protected(move || send(Action::Read));
    let result = window.run();
    let _ = sender.blocking_send(Intent::Close);
    let _ = runtime.block_on(host);
    result
}

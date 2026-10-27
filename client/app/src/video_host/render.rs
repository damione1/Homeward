use super::*;
use crate::{ProVideoPresentation, ProVideoState, VideoPresentation};
use home_client_model::cameras::Provenance;
pub(super) struct Projection {
    selection: Option<Selection>,
    source: Option<(SessionCoordinator, Generation)>,
    generation: i32,
    phase: Playback,
}
impl Projection {
    pub fn new(state: &State) -> Self {
        Self {
            selection: state.selection.clone(),
            source: state.source.clone(),
            generation: state.generation,
            phase: state.phase.clone(),
        }
    }
}
pub(super) fn clear(window: &HomewardWindow) {
    window.set_video(VideoPresentation::default());
}
pub(super) fn state(value: &Playback) -> (ProVideoState, &'static str, bool) {
    match value {
        Playback::Idle => (ProVideoState::Idle, "Vidéo arrêtée", false),
        Playback::Connecting => (
            ProVideoState::Connecting,
            "Ouverture d’une session vidéo autorisée…",
            true,
        ),
        Playback::Playing(_) => (
            ProVideoState::Playing,
            "Lecture native autorisée · sans audio",
            true,
        ),
        Playback::Paused(PauseReason::User) => (
            ProVideoState::Paused,
            "Vidéo en pause · reprendre ouvre une nouvelle session",
            true,
        ),
        Playback::Paused(PauseReason::OutsideViewport) => (
            ProVideoState::Paused,
            "Vidéo arrêtée hors écran · reprendre pour relancer",
            true,
        ),
        Playback::Expired => (
            ProVideoState::Expired,
            "Session vidéo expirée · relancer pour autoriser une nouvelle lecture",
            true,
        ),
        Playback::Revoked => (
            ProVideoState::Revoked,
            "Accès retiré · image vidéo effacée",
            true,
        ),
        Playback::Offline => (
            ProVideoState::Offline,
            "Connexion interrompue · lecture arrêtée",
            true,
        ),
        Playback::Unavailable => (
            ProVideoState::Unavailable,
            "Vidéo temporairement indisponible · utiliser l’instantané",
            true,
        ),
        Playback::Unsupported => (
            ProVideoState::Unsupported,
            "Vidéo ou décodeur natif indisponible · utiliser l’instantané",
            true,
        ),
        Playback::Invalid => (
            ProVideoState::Invalid,
            "Flux vidéo non admis · lecture arrêtée",
            true,
        ),
    }
}
pub(super) fn render(window: &HomewardWindow, value: Projection) {
    let Some(selection) = value.selection else {
        clear(window);
        return;
    };
    if !value.source.as_ref().is_some_and(|(session, generation)| {
        *generation == selection.authority && session.is_current(*generation)
    }) {
        clear(window);
        return;
    }
    let (phase, label, visible) = state(&value.phase);
    let mut video = ProVideoPresentation {
        token: selection.token,
        generation: value.generation,
        title: selection.camera.title.into(),
        source: selection.camera.source.into(),
        identity: selection.camera.id.entity().as_str().into(),
        provenance: match selection.camera.provenance {
            Provenance::Synthetic => "MOCK ISOLÉ · vidéo synthétique · aucun appareil réel",
            Provenance::Observed => "Source autorisée · vidéo sans audio",
        }
        .into(),
        state: phase,
        state_label: label.into(),
        ..Default::default()
    };
    let mut remaining_ms = 0;
    match &value.phase {
        Playback::Playing(frame) => {
            if frame.deadline <= Instant::now() {
                video.state = ProVideoState::Expired;
                video.state_label = "Session vidéo expirée · image retirée".into();
            } else {
                video.picture = slint::Image::from_rgba8(frame.pixels.clone());
                let millis = frame.presentation_micros / 1000;
                video.position_label = format!(
                    "{:02}:{:02}.{:03} · sans audio",
                    millis / 60_000,
                    (millis / 1000) % 60,
                    millis % 1000
                )
                .into();
                remaining_ms = i32::try_from(
                    frame
                        .deadline
                        .saturating_duration_since(Instant::now())
                        .as_millis(),
                )
                .unwrap_or(i32::MAX)
                .max(1);
            }
        }
        Playback::Idle
        | Playback::Connecting
        | Playback::Paused(_)
        | Playback::Expired
        | Playback::Revoked
        | Playback::Offline
        | Playback::Unavailable
        | Playback::Unsupported
        | Playback::Invalid => {}
    }
    window.set_video(VideoPresentation {
        visible,
        video,
        play_enabled: value.phase.can_play(),
        pause_enabled: value.phase.can_pause(),
        remaining_ms,
    });
}

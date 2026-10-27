use super::*;
use crate::{
    CameraPresentation, ContentItem, ProCameraEventRow, ProCameraPresentation, ProCameraState,
};
use home_client_model::cameras::Provenance;
use slint::{Model, ModelNotify, ModelRc, ModelTracker};
use std::time::SystemTime;
pub(super) struct Projection {
    source: Option<(SessionCoordinator, Generation)>,
    generation: i32,
    catalog: Arc<Vec<home_wire_contract::cameras_contract::Camera>>,
    selected: Option<home_client_model::cameras::CameraId>,
    events: Events,
    target: Option<ImageKey>,
    picture: Option<Arc<crate::camera::Frame>>,
    status: Status,
    catalog_status: Status,
}
impl Projection {
    pub fn new(state: &State, source: Option<(SessionCoordinator, Generation)>) -> Self {
        Self {
            source,
            generation: state.generation,
            catalog: state.catalog.clone(),
            selected: state.selected.clone(),
            events: state.events.clone(),
            target: state.target.clone(),
            picture: state.picture.clone(),
            status: state.status,
            catalog_status: state.catalog_status,
        }
    }
}
fn status(value: Status) -> (ProCameraState, &'static str) {
    match value {
        Status::Hidden => (ProCameraState::Hidden, "Aperçu retiré hors du viewport"),
        Status::Loading => (ProCameraState::Loading, "Chargement de l’image…"),
        Status::Ready => (ProCameraState::Ready, "Image autorisée de la caméra"),
        Status::Missing => (ProCameraState::Missing, "Aucune image pour cette source"),
        Status::Expired => (
            ProCameraState::Expired,
            "Session ou image expirée — rechargez pour relire",
        ),
        Status::Revoked => (
            ProCameraState::Revoked,
            "Accès retiré — images privées effacées",
        ),
        Status::WrongCamera => (
            ProCameraState::WrongCamera,
            "Identité de caméra incorrecte — image refusée",
        ),
        Status::Offline => (
            ProCameraState::Offline,
            "Connexion interrompue — images privées retirées",
        ),
        Status::Unavailable => (
            ProCameraState::Unavailable,
            "Source temporairement indisponible",
        ),
        Status::Invalid => (ProCameraState::Invalid, "Réponse ou image non admise"),
        Status::Unsupported => (
            ProCameraState::Unsupported,
            "Le serveur ne propose pas encore cette interface caméra",
        ),
    }
}
fn timestamp(value: SystemTime) -> String {
    chrono::DateTime::<chrono::Utc>::from(value).to_rfc3339()
}
struct Rows {
    events: Events,
    generation: i32,
    notify: ModelNotify,
}
impl Model for Rows {
    type Data = ProCameraEventRow;
    fn row_count(&self) -> usize {
        self.events.rows.len()
    }
    fn row_data(&self, index: usize) -> Option<Self::Data> {
        let event = self.events.rows.get(index)?;
        Some(ProCameraEventRow {
            token: self.events.token(index)?,
            generation: self.generation,
            title: event.title.clone().into(),
            source: event.source.clone().into(),
            captured: timestamp(event.captured_at).into(),
            identity: format!(
                "{} · {}",
                event.key.camera().entity().as_str(),
                event.key.event().as_str()
            )
            .into(),
            image_label: if event.has_image {
                "Vignette disponible"
            } else {
                "Sans vignette"
            }
            .into(),
        })
    }
    fn model_tracker(&self) -> &dyn ModelTracker {
        &self.notify
    }
    fn as_any(&self) -> &dyn std::any::Any {
        self
    }
}
pub(super) fn clear(window: &HomewardWindow) {
    window.set_cameras_publishing(true);
    window.set_cameras(CameraPresentation::default());
    window.set_cameras_publishing(false);
}
pub(super) fn render(window: &HomewardWindow, value: Projection) {
    if value
        .source
        .as_ref()
        .is_some_and(|(s, g)| !s.is_current(*g))
        || !value.catalog.is_empty() && value.source.is_none()
    {
        clear(window);
        return;
    }
    let catalog: Vec<_> = value
        .catalog
        .iter()
        .enumerate()
        .map(|(index, camera)| ContentItem {
            token: i32::try_from(index + 1).unwrap_or(0),
            title: camera.title.clone().into(),
            metadata: format!("{} · {}", camera.source, camera.id.entity().as_str()).into(),
            selected: value.selected.as_ref() == Some(&camera.id),
            ..Default::default()
        })
        .collect();
    let selected = value
        .selected
        .as_ref()
        .and_then(|id| value.catalog.iter().enumerate().find(|(_, c)| &c.id == id));
    let mut camera = ProCameraPresentation {
        generation: value.generation,
        ..Default::default()
    };
    if let Some((index, metadata)) = selected {
        camera.token = i32::try_from(index + 1).unwrap_or(0);
        camera.title = metadata.title.clone().into();
        camera.source = metadata.source.clone().into();
        camera.identity = metadata.id.entity().as_str().into();
        camera.provenance = match metadata.provenance {
            Provenance::Observed => "Observation serveur",
            Provenance::Synthetic => "MOCK ISOLÉ · Aucun appareil réel",
        }
        .into();
        if let Some(ImageKey::EventThumbnail(key)) = &value.target {
            camera.identity = format!(
                "{} · {}",
                key.camera().entity().as_str(),
                key.event().as_str()
            )
            .into();
        }
    }
    let mut picture_status = value.status;
    let mut remaining = 0;
    if let Some(frame) = value.picture {
        if frame.deadline > Instant::now() && value.target.as_ref() == Some(&frame.key) {
            camera.picture = slint::Image::from_rgba8(
                slint::SharedPixelBuffer::<slint::Rgba8Pixel>::clone_from_slice(
                    &frame.pixels.rgba,
                    frame.pixels.width,
                    frame.pixels.height,
                ),
            );
            camera.captured = timestamp(frame.captured_at).into();
            remaining = frame
                .deadline
                .saturating_duration_since(Instant::now())
                .as_millis()
                .saturating_add(1)
                .min(i32::MAX as u128) as i32;
        } else {
            picture_status = Status::Expired;
        }
    }
    let (state, label) = status(picture_status);
    camera.state = state;
    camera.state_label = label.into();
    let index = value
        .target
        .as_ref()
        .and_then(|target| match target {
            ImageKey::Snapshot(_) => None,
            ImageKey::EventThumbnail(key) => {
                value.events.rows.iter().position(|event| &event.key == key)
            }
        })
        .and_then(|i| i32::try_from(i).ok())
        .unwrap_or(-1);
    let notice = match value.catalog_status {
        Status::Ready => {
            if value.catalog.is_empty() {
                "Aucune caméra autorisée"
            } else {
                "Les images restent privées et leur session est courte"
            }
        }
        Status::Hidden
        | Status::Loading
        | Status::Missing
        | Status::Expired
        | Status::Revoked
        | Status::WrongCamera
        | Status::Offline
        | Status::Unavailable
        | Status::Invalid
        | Status::Unsupported => status(value.catalog_status).1,
    };
    let data = CameraPresentation {
        generation: value.generation,
        catalog: ModelRc::new(slint::VecModel::from(catalog)),
        notice: notice.into(),
        selected: selected.is_some(),
        camera,
        events: ModelRc::new(Rows {
            events: value.events,
            generation: value.generation,
            notify: ModelNotify::default(),
        }),
        event_index: index,
        snapshot_selected: matches!(value.target, Some(ImageKey::Snapshot(_))),
        remaining_ms: remaining,
    };
    window.set_cameras_publishing(true);
    window.set_cameras(data);
    window.set_cameras_publishing(false);
}

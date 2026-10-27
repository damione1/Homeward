use super::state::{ChoiceKind, ChoiceToken, Draft, Picture, State, Status};
use crate::{
    HomewardWindow, ProArtworkState, ProDeviceValue, ProMediaAction, ProMediaOperation,
    ProMediaOption, ProMediaOptionKind, ProMediaPresentation, ProValueKind, ProValueMode,
    media::{Data, Field, Refusal},
};
use buffa::EnumValue;
use home_client_session::{Generation, SessionCoordinator};
use home_wire_contract::{
    home::{media::v1 as wire, state::v1 as observation},
    media_contract as admit,
};
use slint::{Model, ModelRc, SharedString};
use std::{collections::BTreeMap, sync::Arc, time::SystemTime};

pub(super) struct Projection {
    guard: Option<(SessionCoordinator, Generation)>,
    generation: i32,
    status: Status,
    data: Option<Arc<Data>>,
    picture: Picture,
    picture_id: i32,
    editable: bool,
    volume: Draft,
    position: Draft,
    options: BTreeMap<ChoiceToken, super::state::Choice>,
    notice: String,
    action: String,
}
impl Projection {
    pub fn new(state: &State, guard: Option<(SessionCoordinator, Generation)>) -> Self {
        let (_, label, detail) =
            crate::device_view::progress::presentation(state.progress.as_deref());
        Self {
            guard,
            generation: state.generation,
            status: state.status,
            data: state.data.clone(),
            picture: state.picture.clone(),
            picture_id: state.picture_id,
            editable: state.editable(),
            volume: state.volume.clone(),
            position: state.position.clone(),
            options: state.choices.clone(),
            notice: state.notice.clone(),
            action: if state.pending.is_some() {
                "Vérification de la commande…".into()
            } else {
                [label, detail]
                    .into_iter()
                    .filter(|v| !v.is_empty())
                    .collect::<Vec<_>>()
                    .join("\n")
            },
        }
    }
}
fn rows<T: Clone + PartialEq + 'static>(current: ModelRc<T>, values: Vec<T>) -> ModelRc<T> {
    if let Some(model) = current.as_any().downcast_ref::<slint::VecModel<T>>()
        && model.row_count() == values.len()
    {
        for (index, value) in values.into_iter().enumerate() {
            if model.row_data(index).as_ref() != Some(&value) {
                model.set_row_data(index, value);
            }
        }
        current
    } else {
        ModelRc::new(slint::VecModel::from(values))
    }
}
pub(super) fn refusal(value: Refusal) -> &'static str {
    match value {
        Refusal::Contract => "La réponse du serveur n’est pas compatible.",
        Refusal::Unsupported => "Cette action n’est pas déclarée pour ce lecteur.",
        Refusal::Permission => "Commande non envoyée : droits ou capacités modifiés.",
        Refusal::Invalid => "Saisissez un nombre positif sans exposant.",
        Refusal::Precision => "Trop de décimales pour cette quantité.",
        Refusal::Bounds => "La valeur dépasse les limites du lecteur.",
        Refusal::Step => "La valeur ne respecte pas le pas du lecteur.",
    }
}
fn operation(value: wire::Operation) -> Option<(ProMediaOperation, &'static str)> {
    match value {
        wire::Operation::Play => Some((ProMediaOperation::Play, "Lire")),
        wire::Operation::Pause => Some((ProMediaOperation::Pause, "Mettre en pause")),
        wire::Operation::Stop => Some((ProMediaOperation::Stop, "Arrêter")),
        wire::Operation::Previous => Some((ProMediaOperation::Previous, "Précédent")),
        wire::Operation::Next => Some((ProMediaOperation::Next, "Suivant")),
        wire::Operation::Unspecified => None,
    }
}
fn field(data: &Data, field: Field, draft: &Draft) -> Option<ProDeviceValue> {
    let limits = data.limits(field)?;
    let (label, suffix) = match field {
        Field::Volume => ("Volume", "%"),
        Field::Position => ("Position", "s"),
    };
    let reading = data
        .observed(field)
        .map(|v| format!("{} {suffix}", v.text()))
        .unwrap_or_else(|| "non fournie".into());
    Some(ProDeviceValue {
        token: field.token(),
        kind: ProValueKind::Numeric,
        mode: ProValueMode::Single,
        label: label.into(),
        suffix: suffix.into(),
        description: format!(
            "Observation serveur : {reading}. L’édition est une intention distincte."
        )
        .into(),
        lower: draft.text.clone().into(),
        lower_position: draft.value.map_or(0., |q| data.position(q)),
        slider_enabled: limits.minimum < limits.maximum,
        can_step: limits.minimum < limits.maximum,
        error: draft.error.clone().into(),
        ..Default::default()
    })
}
pub(super) fn clear(window: &HomewardWindow) {
    window.set_media_publishing(true);
    window.set_media_visible(false);
    window.set_media_has_data(false);
    window.set_media_editable(false);
    window.set_media_generation(0);
    window.set_media_artwork_id(0);
    window.set_media_artwork_remaining_ms(0);
    window.set_media_data(Default::default());
    window.set_media_actions(Default::default());
    window.set_media_options(Default::default());
    window.set_media_values(Default::default());
    window.set_media_members(Default::default());
    window.set_media_group_title("".into());
    window.set_media_coordinator("".into());
    window.set_media_notice("".into());
    window.set_media_publishing(false);
}
pub(super) fn render(window: &HomewardWindow, value: Projection) {
    if value.guard.as_ref().is_some_and(|(s, g)| !s.is_current(*g))
        || value.data.is_some() && value.guard.is_none()
    {
        clear(window);
        return;
    }
    let visible = match value.status {
        Status::Hidden | Status::Unsupported => false,
        Status::Loading | Status::Ready | Status::Denied | Status::Offline | Status::Contract => {
            true
        }
    };
    if !visible {
        clear(window);
        return;
    }
    window.set_media_publishing(true);
    let status = match value.status {
        Status::Hidden | Status::Unsupported | Status::Ready => "",
        Status::Loading => "Lecture des informations média…",
        Status::Denied => "Lecture média non autorisée. Les données privées ont été retirées.",
        Status::Offline => {
            "Serveur indisponible. Lecture datée ; aucune commande n’est rejouée à la reconnexion."
        }
        Status::Contract => "Réponse média incompatible. Aucune commande envoyée.",
    };
    window.set_media_generation(value.generation);
    window.set_media_visible(true);
    window.set_media_editable(value.editable);
    window.set_media_has_data(value.data.is_some());
    window.set_media_notice(
        [status, value.notice.as_str()]
            .into_iter()
            .filter(|v| !v.is_empty())
            .collect::<Vec<_>>()
            .join("\n")
            .into(),
    );
    let Some(data) = value.data else {
        window.set_media_data(Default::default());
        window.set_media_actions(Default::default());
        window.set_media_values(Default::default());
        window.set_media_options(Default::default());
        window.set_media_members(Default::default());
        window.set_media_group_title("".into());
        window.set_media_coordinator("".into());
        window.set_media_artwork_id(0);
        window.set_media_publishing(false);
        return;
    };
    let snapshot = data.snapshot();
    let playback = match snapshot.playback {
        EnumValue::Known(wire::PlaybackState::Playing) => "En lecture",
        EnumValue::Known(wire::PlaybackState::Paused) => "En pause",
        EnumValue::Known(wire::PlaybackState::Stopped) => "Arrêté",
        EnumValue::Known(wire::PlaybackState::Buffering) => "Mise en mémoire tampon",
        EnumValue::Known(wire::PlaybackState::Unspecified) => "Lecture inconnue",
        EnumValue::Unknown(_) => "Lecture non prise en charge",
    };
    let quality = crate::cache_view::quality_text(&observation::ObservedState {
        quality: snapshot.quality.clone(),
        ..Default::default()
    });
    let received = snapshot
        .received_at
        .as_option()
        .and_then(|t| chrono::DateTime::<chrono::Utc>::from_timestamp(t.seconds, t.nanos as u32))
        .map(|t| format!("Reçu {}", t.format("%Y-%m-%d %H:%M:%S%.3f UTC")))
        .unwrap_or_else(|| "Horodatage non fourni".into());
    let mut presentation = ProMediaPresentation {
        token: 1,
        generation: value.generation,
        title: snapshot
            .title
            .clone()
            .filter(|v| !v.is_empty())
            .unwrap_or_else(|| "Titre non fourni".into())
            .into(),
        subtitle: snapshot.artist.clone().unwrap_or_default().into(),
        collection: snapshot.collection.clone().unwrap_or_default().into(),
        playback: playback.into(),
        quality: [quality.as_str(), snapshot.provenance.as_str()]
            .into_iter()
            .filter(|v| !v.is_empty())
            .collect::<Vec<_>>()
            .join(" · ")
            .into(),
        freshness: received.into(),
        notice: value.action.into(),
        ..Default::default()
    };
    let mut remaining = 0;
    let (state, label) = match value.picture {
        Picture::Absent => (ProArtworkState::Absent, "Pochette non fournie"),
        Picture::Loading => (ProArtworkState::Loading, "Chargement de la pochette…"),
        Picture::Denied => (ProArtworkState::Denied, "Pochette non autorisée"),
        Picture::Expired => (ProArtworkState::Expired, "Pochette expirée"),
        Picture::Failed => (ProArtworkState::Failed, "Pochette indisponible"),
        Picture::Ready(image) => match image.expires().duration_since(SystemTime::now()) {
            Ok(duration) => {
                remaining = duration.as_millis().saturating_add(1).min(i32::MAX as u128) as i32;
                presentation.artwork = if window.get_media_artwork_id() == value.picture_id {
                    window.get_media_data().artwork
                } else {
                    slint::Image::from_rgba8(
                        slint::SharedPixelBuffer::<slint::Rgba8Pixel>::clone_from_slice(
                            image.rgba(),
                            image.width(),
                            image.height(),
                        ),
                    )
                };
                window.set_media_artwork_id(value.picture_id);
                (ProArtworkState::Ready, "Pochette du média")
            }
            Err(_) => (ProArtworkState::Expired, "Pochette expirée"),
        },
    };
    if state != ProArtworkState::Ready {
        window.set_media_artwork_id(0);
    }
    presentation.artwork_state = state;
    presentation.artwork_label = label.into();
    window.set_media_artwork_remaining_ms(remaining);
    window.set_media_data(presentation);
    let actions = snapshot
        .capabilities
        .operations
        .iter()
        .filter_map(|op| admit::operation(*op).and_then(operation))
        .map(|(operation, label)| ProMediaAction {
            operation,
            label: label.into(),
        })
        .collect::<Vec<_>>();
    window.set_media_actions(rows(window.get_media_actions(), actions));
    let fields = [
        (Field::Volume, &value.volume),
        (Field::Position, &value.position),
    ]
    .into_iter()
    .filter_map(|(kind, draft)| field(&data, kind, draft))
    .collect::<Vec<_>>();
    window.set_media_values(rows(window.get_media_values(), fields));
    let options = value
        .options
        .iter()
        .filter_map(|(token, choice)| {
            let (kind, options, selected) = match choice.kind {
                ChoiceKind::Source => (
                    ProMediaOptionKind::Source,
                    &snapshot.capabilities.sources,
                    snapshot.source_id.as_deref(),
                ),
                ChoiceKind::Group => (
                    ProMediaOptionKind::Group,
                    &snapshot.capabilities.groups,
                    snapshot.group_id.as_deref(),
                ),
            };
            options
                .iter()
                .find(|option| option.id == choice.id.0)
                .map(|option| ProMediaOption {
                    token: token.0,
                    kind,
                    label: option.label.clone().into(),
                    selected: selected == Some(choice.id.0.as_str()),
                })
        })
        .collect::<Vec<_>>();
    window.set_media_options(rows(window.get_media_options(), options));
    let group = snapshot
        .group_id
        .as_ref()
        .and_then(|id| snapshot.capabilities.groups.iter().find(|g| g.id == *id))
        .map(|g| g.label.as_str())
        .unwrap_or("Groupe média");
    window.set_media_group_title(
        if snapshot.group_id.is_some() || snapshot.members.len() > 1 {
            group.into()
        } else {
            "".into()
        },
    );
    window.set_media_coordinator(
        snapshot
            .members
            .iter()
            .find(|m| m.entity_id == data.coordinator().as_str())
            .map(|m| format!("Coordonnateur : {}", m.label))
            .unwrap_or_else(|| "Coordonnateur non exposé".into())
            .into(),
    );
    window.set_media_members(rows(
        window.get_media_members(),
        snapshot
            .members
            .iter()
            .map(|m| SharedString::from(m.label.as_str()))
            .collect::<Vec<_>>(),
    ));
    window.set_media_publishing(false);
}

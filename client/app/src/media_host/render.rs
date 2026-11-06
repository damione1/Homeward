use super::state::{ChoiceKind, ChoiceToken, Draft, Picture, State, Status};
use crate::{
    HomewardWindow, ProArtworkState, ProDeviceValue, ProMediaAction, ProMediaOperation,
    ProMediaOption, ProMediaOptionKind, ProMediaPresentation, ProValueKind, ProValueMode,
    media::{Data, Field, Refusal},
};
use home_client_model::{
    api_media::{MediaDataProvenance, MediaOperation},
    api_observations::{Availability, Freshness, Provenance},
    api_playback::PlaybackState,
};
use home_client_session::{Generation, SessionCoordinator};
use slint::{Model, ModelRc, SharedString};
use std::{collections::BTreeMap, sync::Arc};

pub(super) struct Projection {
    guard: Option<(SessionCoordinator, Generation)>,
    generation: i32,
    retention: Option<super::state::Retention>,
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
    pub fn retained_generation(&self) -> Option<i32> {
        self.retention.map(|_| self.generation)
    }
    pub fn new(state: &State, guard: Option<(SessionCoordinator, Generation)>) -> Self {
        let (_, label, detail) = super::progress::presentation(state.progress.as_deref());
        Self {
            guard,
            generation: state.generation,
            retention: state.retention,
            status: state.status,
            data: state.data.clone(),
            picture: state.picture.clone(),
            picture_id: state.picture_id,
            editable: state.editable(),
            volume: state.volume.clone(),
            position: state.position.clone(),
            options: state.choices.clone(),
            notice: state.notice.clone(),
            action: [label, detail]
                .into_iter()
                .filter(|v| !v.is_empty())
                .collect::<Vec<_>>()
                .join("\n"),
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
        Refusal::Contract => "The server response is incompatible.",
        Refusal::Unsupported => "This action is not declared for this player.",
        Refusal::Permission => "Command not sent: permissions or capabilities changed.",
        Refusal::Invalid => "Enter a nonnegative number without an exponent.",
        Refusal::ContextUnavailable => "Unable to prepare this command. No request was sent.",
        Refusal::Precision => "Too many decimal places for this quantity.",
        Refusal::Bounds => "The value exceeds the player’s limits.",
        Refusal::Step => "The value does not match the player’s step.",
    }
}
fn operation(value: MediaOperation) -> (ProMediaOperation, &'static str) {
    match value {
        MediaOperation::Play => (ProMediaOperation::Play, "Play"),
        MediaOperation::Pause => (ProMediaOperation::Pause, "Pause"),
        MediaOperation::Stop => (ProMediaOperation::Stop, "Stop"),
        MediaOperation::Previous => (ProMediaOperation::Previous, "Previous"),
        MediaOperation::Next => (ProMediaOperation::Next, "Next"),
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
        .unwrap_or_else(|| "not provided".into());
    Some(ProDeviceValue {
        token: field.token(),
        kind: ProValueKind::Numeric,
        mode: ProValueMode::Single,
        label: label.into(),
        suffix: suffix.into(),
        description: format!("Server observation: {reading}. Editing is a separate action.").into(),
        lower: draft.text.clone().into(),
        lower_position: draft.value.map_or(0., |q| data.position(q)),
        slider_enabled: draft.value.is_some() && limits.minimum() < limits.maximum(),
        can_step: draft.value.is_some() && limits.minimum() < limits.maximum(),
        error: draft.error.clone().into(),
        ..Default::default()
    })
}
pub(super) fn clear(window: &HomewardWindow) {
    window.set_media_retention_ms(0);
    window.set_media_publishing(true);
    window.set_media_visible(false);
    window.set_media_progress_known(false);
    window.set_media_progress(0.);
    window.set_media_position_label("".into());
    window.set_media_duration_label("".into());
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
    let now = chrono::Utc::now();
    if value.retention.is_some_and(|retention| {
        retention.deadline <= (now.timestamp(), now.timestamp_subsec_nanos() as i32)
    }) {
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
        Status::Loading => "",
        Status::Denied => "Media access denied. Private data was removed.",
        Status::Offline => {
            "Server unavailable. Dated reading; no commands will be replayed on reconnection."
        }
        Status::Contract => "Incompatible media response. No command was sent.",
    };
    window.set_media_generation(value.generation);
    window.set_media_retention_ms(value.retention.map_or(0, |retention| {
        let remaining = retention
            .deadline
            .0
            .saturating_mul(1000)
            .saturating_add(i64::from(retention.deadline.1 / 1_000_000))
            .saturating_sub(chrono::Utc::now().timestamp_millis());
        i32::try_from(remaining.max(1)).unwrap_or(i32::MAX)
    }));
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
        window.set_media_progress_known(false);
        window.set_media_progress(0.);
        window.set_media_position_label("".into());
        window.set_media_duration_label("".into());
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
    let reading = snapshot.observation();
    let definition = snapshot.definition();
    let position = reading
        .and_then(|reading| reading.position())
        .map(|value| value.value() as u64);
    let duration = reading
        .and_then(|reading| reading.duration())
        .map(|value| value.value() as u64);
    let progress = position
        .zip(duration)
        .filter(|(position, duration)| *duration > 0 && position <= duration);
    window.set_media_progress_known(progress.is_some());
    window.set_media_progress(
        progress.map_or(0., |(position, duration)| position as f32 / duration as f32),
    );
    let time_label = |millis: u64| {
        let seconds = millis / 1000;
        if seconds >= 3600 {
            format!(
                "{}:{:02}:{:02}",
                seconds / 3600,
                (seconds / 60) % 60,
                seconds % 60
            )
        } else {
            format!("{}:{:02}", seconds / 60, seconds % 60)
        }
    };
    window.set_media_position_label(position.map(time_label).unwrap_or_default().into());
    window.set_media_duration_label(duration.map(time_label).unwrap_or_default().into());
    let playback = match reading
        .map(|reading| reading.playback())
        .unwrap_or(PlaybackState::Unknown)
    {
        PlaybackState::Playing => "Playing",
        PlaybackState::Paused => "Paused",
        PlaybackState::Stopped => "Stopped",
        PlaybackState::Buffering => "Buffering",
        PlaybackState::Unknown => "Playback unknown",
    };
    let playback = match reading.and_then(|reading| reading.powered_on()) {
        Some(true) => format!("{playback} · Power ON"),
        Some(false) => format!("{playback} · Power OFF"),
        None => playback.to_owned(),
    };
    let quality = reading
        .map(|reading| {
            let quality = reading.quality();
            let availability = match quality.availability() {
                Availability::Available => "Available",
                Availability::Unavailable => "Unavailable",
                Availability::Unknown => "Availability unknown",
                Availability::Unspecified => "Availability not specified",
            };
            let freshness = match quality.freshness() {
                Freshness::Current => "Current",
                Freshness::Stale => "Stale",
                Freshness::Unspecified => "Freshness unknown",
            };
            let provenance = match quality.provenance() {
                Provenance::Observed => "Observed",
                Provenance::Derived => "Derived",
                Provenance::Restored => "Restored",
                Provenance::Estimated => "Estimated",
                Provenance::Unspecified => "Observation origin unknown",
            };
            format!("{availability} · {freshness} · {provenance}")
        })
        .unwrap_or_else(|| "Observation not provided".to_owned());
    let data_origin = match snapshot.data_provenance() {
        MediaDataProvenance::Observed => "Installation data",
        MediaDataProvenance::Synthetic => "ISOLATED MOCK · No real devices",
    };
    let observed = reading
        .and_then(|reading| reading.observed_time())
        .and_then(|time| {
            chrono::DateTime::<chrono::Utc>::from_timestamp(time.seconds(), time.nanos() as u32)
        })
        .map(|time| format!("Observed {}", time.format("%Y-%m-%d %H:%M:%S%.3f UTC")))
        .unwrap_or_else(|| "Observation time not provided".to_owned());
    let mut presentation = ProMediaPresentation {
        token: 1,
        generation: value.generation,
        title: reading
            .and_then(|reading| reading.title())
            .unwrap_or("Title not provided")
            .into(),
        subtitle: reading
            .and_then(|reading| reading.artist())
            .unwrap_or_default()
            .into(),
        collection: reading
            .and_then(|reading| reading.collection())
            .unwrap_or_default()
            .into(),
        playback: playback.into(),
        quality: format!("{quality} · {data_origin}").into(),
        freshness: observed.into(),
        notice: value.action.into(),
        ..Default::default()
    };
    let mut remaining = 0;
    let (state, label) = match value.picture {
        Picture::Absent => (ProArtworkState::Absent, "Artwork not provided"),
        Picture::Loading => (ProArtworkState::Loading, "Loading artwork…"),
        Picture::Denied => (ProArtworkState::Denied, "Artwork unauthorized"),
        Picture::Expired => (ProArtworkState::Expired, "Artwork expired"),
        Picture::Failed => (ProArtworkState::Failed, "Artwork unavailable"),
        Picture::Ready(image) => match image
            .deadline()
            .checked_duration_since(std::time::Instant::now())
        {
            Some(duration) => {
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
                (
                    ProArtworkState::Ready,
                    match image.provenance() {
                        MediaDataProvenance::Observed => "Installation artwork",
                        MediaDataProvenance::Synthetic => "ISOLATED MOCK · Synthetic artwork",
                    },
                )
            }
            None => (ProArtworkState::Expired, "Artwork expired"),
        },
    };
    if state != ProArtworkState::Ready {
        window.set_media_artwork_id(0);
    }
    presentation.artwork_state = state;
    presentation.artwork_label = label.into();
    window.set_media_artwork_remaining_ms(remaining);
    window.set_media_data(presentation);
    let actions = definition
        .map(|definition| definition.operations())
        .unwrap_or_default()
        .iter()
        .copied()
        .map(operation)
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
                    definition
                        .map(|definition| definition.sources())
                        .unwrap_or_default(),
                    reading
                        .and_then(|reading| reading.source())
                        .map(|source| source.as_str()),
                ),
                ChoiceKind::Group => (
                    ProMediaOptionKind::Group,
                    definition
                        .map(|definition| definition.groups())
                        .unwrap_or_default(),
                    reading
                        .and_then(|reading| reading.group())
                        .map(|group| group.as_str()),
                ),
            };
            options
                .iter()
                .find(|option| option.id().as_str() == choice.id.0)
                .map(|option| ProMediaOption {
                    token: token.0,
                    kind,
                    label: option.display_name().into(),
                    selected: selected == Some(choice.id.0.as_str()),
                })
        })
        .collect::<Vec<_>>();
    window.set_media_options(rows(window.get_media_options(), options));
    let group_id = reading.and_then(|reading| reading.group());
    let groups = definition
        .map(|definition| definition.groups())
        .unwrap_or_default();
    let members = reading.map(|reading| reading.members()).unwrap_or_default();
    let group = group_id
        .and_then(|id| groups.iter().find(|group| group.id() == id))
        .map(|group| group.display_name())
        .unwrap_or("Media Group");
    window.set_media_group_title(if group_id.is_some() || members.len() > 1 {
        group.into()
    } else {
        "".into()
    });
    window.set_media_coordinator(
        data.coordinator()
            .and_then(|coordinator| members.iter().find(|member| member.entity() == coordinator))
            .map(|member| format!("Coordinator: {}", member.display_name()))
            .unwrap_or_else(|| "Coordinator not exposed".to_owned())
            .into(),
    );
    window.set_media_members(rows(
        window.get_media_members(),
        members
            .iter()
            .map(|member| SharedString::from(member.display_name()))
            .collect::<Vec<_>>(),
    ));
    window.set_media_publishing(false);
}

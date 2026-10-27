use super::{
    Data,
    state::{Failure, State, Status},
};
use crate::{
    HistoryPresentation, HomewardWindow, ProChartPhase, ProChartSeries, history::SourceRef,
};
use home_client_cache::history::{Content, Pages};
use home_wire_contract::home::{history::v1 as wire, state::v1 as state};
use slint_kit_pro::charts::{
    PixelHeight, PixelWidth, PlotKind, PreparedFrame, RasterStyle, RenderRequest, SeriesPaint,
    SeriesSet, TimeSpan, Timestamp, ValueRange,
};
use std::sync::Arc;

pub(super) struct Projection {
    guard: Option<(
        home_client_session::SessionCoordinator,
        home_client_session::Generation,
    )>,
    generation: i32,
    status: Status,
    description: String,
    notice: String,
    unit: String,
    group: String,
    inspection: String,
    frame: Option<Arc<PreparedFrame>>,
    accent: [u8; 3],
    cursor: Option<f32>,
    range: Option<(f32, f32)>,
    start: String,
    end: String,
}
impl Projection {
    pub fn new(
        state: &State,
        guard: Option<(
            home_client_session::SessionCoordinator,
            home_client_session::Generation,
        )>,
    ) -> Self {
        let mut value = Self { guard, generation: state.generation,status: state.status,description: "Période UTC, fin exclusive. Observations : 7 jours maximum ; agrégats : heures UTC entières, 500 heures maximum. Choisissez une statistique puis Afficher / relire.".into(),notice: String::new(),unit: String::new(),group: String::new(),inspection: String::new(),frame: state.frame.clone(),accent: state.accent,cursor: None,range: state.range,start: String::new(),end: String::new() };
        if let Some(data) = &state.data {
            value.start = utc(&data.snapshot.query.period().start);
            value.end = utc(&data.snapshot.query.period().end);
            value.description = format!(
                "{} · [{} ; {}) UTC · {} points source, {} sans valeur numérique pour cette statistique. Aucun total recalculé.",
                data.statistic.label(),
                value.start,
                value.end,
                data.snapshot.points,
                data.non_numeric_or_absent
            );
            if let Some(scope) = scope(data) {
                value.description.push_str(&format!(
                    " Lecture au {} ; rétention depuis {}.",
                    utc(&scope.as_of),
                    utc(&scope.retention_cutoff)
                ));
                if let Some(prefix) = scope.not_retained_prefix.as_option() {
                    value.notice.push_str(&format!(
                        "Préfixe hors rétention [{} ; {}). ",
                        utc(&prefix.start),
                        utc(&prefix.end)
                    ));
                }
            }
            if matches!(data.snapshot.content, Content::NotRetained(_)) {
                value
                    .notice
                    .push_str("Période hors rétention : aucune valeur fabriquée.");
            } else if data.incomplete_coverage {
                value.notice.push_str(
                    "Couverture incomplète : ces observations ne prouvent pas un total fiable. ",
                );
            } else {
                value
                    .notice
                    .push_str("Couverture complète sous la politique Recorder du serveur. ");
            }
            if data.unknown_utc {
                value.notice.push_str(
                    "Trous logiques sans bornes UTC : aucun emplacement ou durée n’est déduit. ",
                );
            }
            if let Some(group) = data.groups.get(state.group) {
                value.group = format!(
                    "Groupe {} / {} · {} · {}",
                    state.group + 1,
                    data.groups.len(),
                    group.descriptor.unit,
                    quality(&group.quality)
                );
                value.unit = format!(
                    "Unité source {} · canonique {} · {} · {} · précision {} · échelle {} · décalage {}. Tracé approximatif ; inspection = valeur source exacte.",
                    group.descriptor.unit,
                    group.descriptor.canonical_unit,
                    aggregation(group.descriptor.aggregation),
                    semantic(group.descriptor.semantic),
                    group.descriptor.precision,
                    group.descriptor.scale,
                    group.descriptor.offset
                );
                if let Some(index) = state.cursor {
                    if let Some(sample) = group.series.samples().get(index) {
                        value.cursor = Some(fraction(data, sample.time()));
                    }
                    value.inspection = inspection(data, state.group, index);
                }
            }
        }
        value
    }
}
pub(super) fn scope(data: &Data) -> Option<&wire::ReadScope> {
    match &data.snapshot.content {
        Content::NotRetained(scope) => Some(scope),
        Content::Retained(Pages::Observations(pages)) => pages.first().map(|p| &*p.scope),
        Content::Retained(Pages::Hourly(pages)) => pages.first().map(|p| &*p.scope),
    }
}
pub(super) fn utc(time: &buffa_types::google::protobuf::Timestamp) -> String {
    u32::try_from(time.nanos)
        .ok()
        .and_then(|n| chrono::DateTime::from_timestamp(time.seconds, n))
        .map(|t| t.to_rfc3339_opts(chrono::SecondsFormat::AutoSi, true))
        .unwrap_or_else(|| "UTC non renseignée".into())
}
pub(super) fn utc_seconds(seconds: i64) -> String {
    utc(&buffa_types::google::protobuf::Timestamp {
        seconds,
        ..Default::default()
    })
}
pub(super) fn fraction(data: &Data, at: Timestamp) -> f32 {
    let period = data.snapshot.query.period();
    let start = crate::history::timestamp(&period.start)
        .expect("admitted query time")
        .unix_nanos();
    let end = crate::history::timestamp(&period.end)
        .expect("admitted query time")
        .unix_nanos();
    ((at.unix_nanos() - start) as f64 / (end - start) as f64).clamp(0., 1.) as f32
}
pub(super) fn time_at(data: &Data, fraction: f32) -> Option<Timestamp> {
    let period = data.snapshot.query.period();
    let start = crate::history::timestamp(&period.start).ok()?.unix_nanos();
    let end = crate::history::timestamp(&period.end).ok()?.unix_nanos();
    let at = start + ((end - start) as f64 * f64::from(fraction.clamp(0., 1.))).round() as i128;
    Timestamp::from_unix_parts(
        i64::try_from(at.div_euclid(1_000_000_000)).ok()?,
        at.rem_euclid(1_000_000_000) as u32,
    )
    .ok()
}
pub(super) fn request(state: &State) -> Option<RenderRequest> {
    let data = state.data.as_ref()?;
    let group = data.groups.get(state.group)?;
    let samples = group.series.samples();
    let min = samples.iter().map(|p| p.value()).reduce(f64::min)?;
    let max = samples.iter().map(|p| p.value()).reduce(f64::max)?;
    let padding = (min.abs().max(max.abs()) / 10.).max(1.);
    let values = ValueRange::new(
        (min - padding).max(-f64::MAX),
        (max + padding).min(f64::MAX),
    )
    .ok()?;
    let interval = TimeSpan::new(
        crate::history::timestamp(&data.snapshot.query.period().start).ok()?,
        crate::history::timestamp(&data.snapshot.query.period().end).ok()?,
    )
    .ok()?;
    Some(RenderRequest {
        data: Arc::new(SeriesSet::new(vec![group.series.clone()]).ok()?),
        viewport: slint_kit_pro::charts::Viewport {
            interval,
            width: PixelWidth::new(state.width).ok()?,
        },
        height: PixelHeight::new(state.height).ok()?,
        values,
        kind: PlotKind::Scatter,
        style: RasterStyle::new(
            state.background,
            vec![
                SeriesPaint::new(
                    group.series.id().clone(),
                    state.accent,
                    255,
                    (state.density * 2.).ceil().clamp(1., 8.) as u8,
                )
                .ok()?,
            ],
        )
        .ok()?,
    })
}
fn inspection(data: &Data, group_index: usize, index: usize) -> String {
    let group = &data.groups[group_index];
    let Some(source) = group.source(index) else {
        return String::new();
    };
    let Some(value) = group.value(index) else {
        return String::new();
    };
    let text = value.text();
    match (&data.snapshot.content, source) {
        (Content::Retained(Pages::Observations(pages)), SourceRef::Observation { page, point }) => {
            let point = &pages[page].points[point];
            let source = point
                .state
                .observed_at
                .as_option()
                .map_or_else(|| "non fournie".into(), utc);
            let epoch = point
                .position
                .epoch
                .iter()
                .map(|v| format!("{v:02x}"))
                .collect::<String>();
            format!(
                "{text} {} · {} · reçu {} · capture source {source} · session source {} · position {epoch}/{} · observation {} / {}",
                group.descriptor.unit,
                quality(&group.quality),
                utc(&point.state.received_at),
                point.state.source_session,
                point.position.sequence,
                index + 1,
                group.series.samples().len()
            )
        }
        (Content::Retained(Pages::Hourly(pages)), SourceRef::Aggregate { page, point }) => {
            let point = &pages[page].points[point];
            format!(
                "{} = {text} {} · {} · bucket [{} ; {}) · durée serveur {} ns · reset {:?} · causes {:?}",
                data.statistic.label(),
                group.descriptor.unit,
                quality(&group.quality),
                utc(&point.bucket.start),
                utc(&point.bucket.end),
                point.duration_nanos,
                point.reset,
                point.reasons
            )
        }
        (Content::NotRetained(_), _)
        | (Content::Retained(Pages::Observations(_)), SourceRef::Aggregate { .. })
        | (Content::Retained(Pages::Hourly(_)), SourceRef::Observation { .. }) => String::new(),
    }
}
fn quality(value: &state::Quality) -> String {
    use buffa::EnumValue::{Known, Unknown};
    let availability = match value.availability {
        Known(state::Availability::Available) => "disponible",
        Known(state::Availability::Unknown) => "disponibilité inconnue",
        Known(state::Availability::Unavailable) => "indisponible",
        Known(state::Availability::Unspecified) | Unknown(_) => "disponibilité non précisée",
    };
    let freshness = match value.freshness {
        Known(state::Freshness::Fresh) => "fraîche",
        Known(state::Freshness::Stale) => "ancienne",
        Known(state::Freshness::Unspecified) | Unknown(_) => "fraîcheur non précisée",
    };
    let provenance = match value.provenance {
        Known(state::Provenance::Observed) => "observée",
        Known(state::Provenance::Restored) => "restaurée",
        Known(state::Provenance::Estimated) => "estimée",
        Known(state::Provenance::Unspecified) | Unknown(_) => "provenance non précisée",
    };
    format!("{availability} · {freshness} · {provenance}")
}
fn aggregation(
    value: buffa::EnumValue<home_wire_contract::home::registry::v1::MeasurementAggregation>,
) -> &'static str {
    use buffa::EnumValue::{Known, Unknown};
    use home_wire_contract::home::registry::v1::MeasurementAggregation as A;
    match value {
        Known(A::Current) => "valeur courante",
        Known(A::Sum) => "somme serveur",
        Known(A::Mean) => "moyenne serveur",
        Known(A::Unspecified) | Unknown(_) => "agrégation non précisée",
    }
}
fn semantic(
    value: buffa::EnumValue<home_wire_contract::home::registry::v1::MeasurementSemantic>,
) -> &'static str {
    use buffa::EnumValue::{Known, Unknown};
    use home_wire_contract::home::registry::v1::MeasurementSemantic as S;
    match value {
        Known(S::Measurement) => "mesure",
        Known(S::Total) => "compteur cumulatif",
        Known(S::TotalIncreasing) => "compteur croissant",
        Known(S::Unspecified) | Unknown(_) => "interprétation non précisée",
    }
}
fn axis(value: f64) -> String {
    // Only approximate pixel coordinates are formatted here. Exact monetary
    // source strings remain intact in the inspection readout.
    if value.abs() >= 1000. || (value != 0. && value.abs() < 0.001) {
        format!("≈ {value:.2e}")
    } else {
        let rounded = format!("{value:.3}");
        format!("≈ {}", rounded.trim_end_matches('0').trim_end_matches('.'))
    }
}
pub(super) fn render(window: &HomewardWindow, value: Projection) {
    // Delivery happens under the mailbox invalidation barrier. Check session
    // authority here too, after any time spent waiting in the UI queue, without
    // re-entering the mailbox or the history owner's mutex.
    if value
        .guard
        .as_ref()
        .is_some_and(|(session, generation)| !session.is_current(*generation))
    {
        window.set_history_frame_id(0);
        window.set_history_presentation(HistoryPresentation {
            generation: value.generation,
            phase: ProChartPhase::Error,
            state_label: "Session modifiée : les données ont été retirées. Relisez explicitement."
                .into(),
            ..Default::default()
        });
        return;
    }
    let (phase, label) = match value.status {
        Status::Draft => (
            ProChartPhase::Empty,
            "Appliquez la période pour lire l’historique.",
        ),
        Status::Loading => (ProChartPhase::Loading, "Lecture des pages du serveur…"),
        Status::Preparing => (ProChartPhase::Loading, "Préparation du graphique…"),
        Status::Ready => (ProChartPhase::Ready, ""),
        Status::Empty => (
            ProChartPhase::Empty,
            "Aucune valeur numérique pour cette statistique ; aucune valeur zéro n’est ajoutée.",
        ),
        Status::NotRetained => (ProChartPhase::Empty, "Période hors rétention."),
        Status::Failed(reason) => (
            ProChartPhase::Error,
            match reason {
                Failure::Query => {
                    "Période refusée. Utilisez UTC, une fin après le début et une fenêtre ≤7 jours brute ou ≤500 heures entières pour les agrégats."
                }
                Failure::Stale => {
                    "Pagination expirée ou modifiée. Cliquez Afficher / relire pour recommencer ; les pages partielles ont été retirées."
                }
                Failure::Capacity => {
                    "Limite de lecture ou de rendu atteinte. Réduisez la période puis relisez explicitement."
                }
                Failure::Denied => {
                    "Lecture d’historique non autorisée. Les données ont été retirées."
                }
                Failure::Transport => {
                    "Serveur indisponible. Relisez explicitement après rétablissement."
                }
                Failure::Contract => {
                    "Réponse incohérente : aucune page partielle n’est affichée. Relisez explicitement."
                }
                Failure::Projection => {
                    "Ces données ne peuvent pas être représentées sans perte. Réduisez la période ou changez de statistique."
                }
            },
        ),
    };
    let mut presentation = HistoryPresentation {
        generation: value.generation,
        phase,
        state_label: label.into(),
        description: value.description.into(),
        notice: value.notice.into(),
        unit: value.unit.into(),
        inspection: value.inspection.into(),
        group_label: value.group.into(),
        x_start: value.start.into(),
        x_end: value.end.into(),
        cursor_visible: value.cursor.is_some(),
        cursor: value.cursor.unwrap_or(0.),
        range_visible: value.range.is_some(),
        range_start: value.range.map_or(0., |r| r.0),
        range_end: value.range.map_or(0., |r| r.1),
        ..Default::default()
    };
    if let Some(frame) = value.frame {
        let bitmap = frame.bitmap();
        let previous = window.get_history_presentation();
        if window.get_history_frame_id() == frame.id().generation()
            && previous.generation == value.generation
        {
            presentation.image = previous.image;
            presentation.series = previous.series;
        } else {
            presentation.image = slint::Image::from_rgb8(slint::SharedPixelBuffer::<
                slint::Rgb8Pixel,
            >::clone_from_slice(
                bitmap.pixels(),
                u32::from(bitmap.width()),
                u32::from(bitmap.height()),
            ));
            presentation.series =
                slint::ModelRc::new(slint::VecModel::from(vec![ProChartSeries {
                    token: 1,
                    label: "Valeurs originales".into(),
                    active: true,
                    color: slint::Color::from_rgb_u8(
                        value.accent[0],
                        value.accent[1],
                        value.accent[2],
                    ),
                    ..Default::default()
                }]));
        }
        window.set_history_frame_id(frame.id().generation());
        presentation.source_width = i32::from(bitmap.width());
        presentation.source_height = i32::from(bitmap.height());
        presentation.y_top = axis(frame.request().values.max()).into();
        presentation.y_bottom = axis(frame.request().values.min()).into();
    } else {
        window.set_history_frame_id(0);
    }
    window.set_history_presentation(presentation);
}

#[cfg(test)]
mod tests {
    use super::*;
    use home_client_session::{SecureStore, SessionCoordinator, StoreFailure};
    struct EmptyStore;
    impl SecureStore for EmptyStore {
        fn load(&self) -> Result<Option<zeroize::Zeroizing<Vec<u8>>>, StoreFailure> {
            Ok(None)
        }
        fn save(&self, _: &[u8]) -> Result<(), StoreFailure> {
            Ok(())
        }
        fn clear(&self) -> Result<(), StoreFailure> {
            Ok(())
        }
    }
    #[tokio::test]
    async fn response_queued_before_session_close_cannot_publish_private_text_on_ui() {
        i_slint_backend_testing::init_no_event_loop();
        let window = HomewardWindow::new().unwrap();
        let connection = home_client_transport::Connection::new(
            "https://127.0.0.1:9",
            include_bytes!("../../tests/fixtures/trust-a.pem"),
        )
        .unwrap();
        let session = SessionCoordinator::new(connection.auth(), Arc::new(EmptyStore));
        let mut projection = Projection::new(
            &State::default(),
            Some((session.clone(), session.generation())),
        );
        projection.inspection = "private-history-queued-canary".into();
        projection.notice = "private-history-queued-canary".into();
        let mailbox = home_client_cache::ProjectionMailbox::new();
        let home_client_cache::Publication::Schedule(ticket) =
            mailbox.publish(mailbox.generation(), projection)
        else {
            panic!("first publication must schedule")
        };
        session.close().await;
        ticket.deliver(|_, projection| render(&window, projection));
        let actual = window.get_history_presentation();
        assert_eq!(actual.phase, ProChartPhase::Error);
        assert!(actual.inspection.is_empty() && actual.notice.is_empty() && actual.unit.is_empty());
        assert_eq!(window.get_history_frame_id(), 0);
    }
}

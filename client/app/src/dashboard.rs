//! Server-owned dashboard data. Compiled code contains only bounded native
//! component vocabulary; installation names and exact bindings arrive by RPC.
use home_client_cache::CachedView;
pub(crate) use home_client_model::api::DashboardName;
use home_client_model::api::EntityName;
use home_client_model::dashboard::{Destination, NodeId, Presentation};
use home_wire_contract::home::api::v2 as wire;
use std::{
    collections::BTreeSet,
    sync::{Arc, Mutex},
};

pub(crate) const MAX_ROOMS: usize = 127;
pub(crate) const MAX_OBSERVATIONS: usize = 64;
pub(crate) const ROOM_TOKEN_BASE: i32 = 1000;
pub(crate) const OBSERVATION_TOKEN_BASE: i32 = 2000;

#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub(crate) struct RoomId(String);
impl RoomId {
    pub(crate) fn parse(value: &str) -> Result<Self, LayoutFailure> {
        if text(value) {
            Ok(Self(value.to_owned()))
        } else {
            Err(LayoutFailure::Invalid)
        }
    }
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct ObservationId(String);
/// Server order plus a derived membership index. Room cards preserve the wire
/// order; inventory filtering keeps logarithmic membership checks.
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct RoomEntities {
    ordered: Vec<EntityName>,
    membership: BTreeSet<EntityName>,
}
impl RoomEntities {
    pub(crate) fn new(ordered: Vec<EntityName>) -> Result<Self, LayoutFailure> {
        let membership = ordered.iter().cloned().collect::<BTreeSet<_>>();
        if membership.len() != ordered.len() {
            return Err(LayoutFailure::Invalid);
        }
        Ok(Self {
            ordered,
            membership,
        })
    }
    pub fn contains(&self, id: &EntityName) -> bool {
        self.membership.contains(id)
    }
    pub fn iter(&self) -> impl Iterator<Item = &EntityName> {
        self.ordered.iter()
    }
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct Room {
    pub id: RoomId,
    pub label: String,
    pub entities: RoomEntities,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Icon {
    Home,
    Information,
    Clock,
    Grid,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct Observation {
    pub id: ObservationId,
    pub label: String,
    pub entity: EntityName,
    pub icon: Icon,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct Layout {
    pub id: DashboardName,
    pub etag: String,
    pub title: String,
    pub profile_zone: crate::environment_view::ProfileZone,
    pub capture: Option<home_client_cache::CaptureIncarnation>,
    pub binding_expires: Option<(i64, i32)>,
    pub rooms: Vec<Room>,
    pub observations: Vec<Observation>,
    pub native: Option<crate::dashboard_layout::NativeLayout>,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum LayoutFailure {
    Invalid,
    Unsupported,
    Budget,
}
fn text(value: &str) -> bool {
    !value.is_empty() && value.len() <= 256 && !value.chars().any(char::is_control)
}
impl TryFrom<&wire::Dashboard> for Layout {
    type Error = LayoutFailure;
    fn try_from(value: &wire::Dashboard) -> Result<Self, Self::Error> {
        if !value.__buffa_unknown_fields.is_empty() {
            return Err(LayoutFailure::Unsupported);
        };
        let id =
            DashboardName::try_from(value.name.as_str()).map_err(|_| LayoutFailure::Invalid)?;
        if !text(&value.display_name)
            || value.etag.is_empty()
            || value.etag.len() > 4096
            || value.etag.chars().any(char::is_control)
        {
            return Err(LayoutFailure::Invalid);
        };
        let native = crate::dashboard_layout::NativeLayout::try_from(
            value.layout.as_option().ok_or(LayoutFailure::Invalid)?,
        )?;
        let profile_zone = match value.display_profile.as_option() {
            None => crate::environment_view::ProfileZone::Missing,
            Some(profile) => {
                if !profile.__buffa_unknown_fields.is_empty() {
                    return Err(LayoutFailure::Unsupported);
                };
                crate::environment_view::ProfileZone::from_time_zone(Some(&profile.time_zone))
            }
        };
        let value_home = id.home();
        let mut rooms = Vec::new();
        let mut room_ids = BTreeSet::new();
        for root in &native.views {
            let node = native.nodes.get(root).ok_or(LayoutFailure::Invalid)?;
            if let Some(route) = &node.route {
                if !room_ids.insert(route.clone()) {
                    return Err(LayoutFailure::Invalid);
                };
                let mut entities = Vec::new();
                let mut seen = BTreeSet::new();
                for child in native.ordered_nodes(root) {
                    let mut ids = child.entity.iter().cloned().collect::<Vec<_>>();
                    if let Some(widget) = &child.widget {
                        ids.extend(widget.entities())
                    };
                    for id in ids {
                        if id.home() != value_home {
                            return Err(LayoutFailure::Invalid);
                        };
                        if seen.insert(id.clone()) {
                            entities.push(id)
                        }
                    }
                }
                rooms.push(Room {
                    id: route.clone(),
                    label: node.title.clone(),
                    entities: RoomEntities::new(entities)?,
                })
            }
        }
        if rooms.len() > MAX_ROOMS {
            return Err(LayoutFailure::Budget);
        };
        Ok(Self {
            id,
            etag: value.etag.clone(),
            title: value.display_name.clone(),
            profile_zone,
            capture: None,
            binding_expires: None,
            rooms,
            observations: Vec::new(),
            native: Some(native),
        })
    }
}
impl Layout {
    pub fn join(
        response: &wire::QueryDashboardResponse,
        expected: &DashboardName,
        view: &CachedView,
    ) -> Result<Self, LayoutFailure> {
        if !response.__buffa_unknown_fields.is_empty()
            || response.entity_view != view.name().as_str()
            || response.resume_token != view.checkpoint()
            || response.binding.is_empty()
            || response.binding.len() > 4096
        {
            return Err(LayoutFailure::Invalid);
        };
        let expires = response
            .expire_time
            .as_option()
            .ok_or(LayoutFailure::Invalid)?;
        if !expires.__buffa_unknown_fields.is_empty()
            || (expires.seconds, expires.nanos) > (view.expires().seconds, view.expires().nanos)
            || !(0..1_000_000_000).contains(&expires.nanos)
        {
            return Err(LayoutFailure::Invalid);
        };
        let mut layout = Self::try_from(
            response
                .dashboard
                .as_option()
                .ok_or(LayoutFailure::Invalid)?,
        )?;
        if &layout.id != expected {
            return Err(LayoutFailure::Invalid);
        };
        layout.capture = Some(view.incarnation().clone());
        layout.binding_expires = Some((expires.seconds, expires.nanos));
        Ok(layout)
    }
    pub fn compatible(&self, view: &CachedView) -> bool {
        self.capture.as_ref() == Some(view.incarnation())
            && self.binding_expires.is_some_and(|expires| {
                let now = chrono::Utc::now();
                expires > (now.timestamp(), now.timestamp_subsec_nanos() as i32)
            })
    }
    pub fn room(&self, id: &RoomId) -> Option<&Room> {
        self.rooms.iter().find(|room| room.id == *id)
    }
}
#[derive(Clone, Default, PartialEq, Eq)]
pub(crate) enum LayoutState {
    #[default]
    Loading,
    Unavailable,
    Invalid,
    Unsupported(Arc<str>),
    Ready(Arc<Layout>),
}
impl LayoutState {
    pub fn unsupported(_value: &wire::Dashboard) -> Self {
        Self::Unsupported("Unsupported dashboard configuration.".into())
    }
    pub fn message(&self) -> &str {
        match self {
            Self::Loading => "Loading the dashboard from the server…",
            Self::Unavailable => "The server provides no authorized dashboard.",
            Self::Invalid => "This version cannot display the dashboard definition.",
            Self::Unsupported(message) => message,
            Self::Ready(_) => "Syncing the dashboard and observations…",
        }
    }
    pub fn layout(&self) -> Option<&Layout> {
        match self {
            Self::Ready(layout) => Some(layout),
            Self::Loading | Self::Unavailable | Self::Invalid | Self::Unsupported(_) => None,
        }
    }
    pub fn shared_layout(&self) -> Option<Arc<Layout>> {
        match self {
            Self::Ready(layout) => Some(layout.clone()),
            Self::Loading | Self::Unavailable | Self::Invalid | Self::Unsupported(_) => None,
        }
    }
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) enum Route {
    Home,
    Rooms,
    Inventory,
    Lists,
    Calendars,
    Cameras,
    Room(RoomId),
    Native(Destination),
    Detail(EntityName),
    Settings,
}
#[derive(Clone, Default)]
pub(crate) struct ProjectionRequest {
    pub revision: i32,
    pub query: String,
    pub room: Option<RoomId>,
    pub detail: Option<EntityName>,
    pub dashboard: LayoutState,
    pub catalogue: Option<Arc<crate::dashboard_catalogue::Snapshot>>,
    pub native: Option<Destination>,
}
impl ProjectionRequest {
    pub fn native_root<'a>(&'a self, layout: &'a Layout) -> Option<&'a NodeId> {
        let native = layout.native.as_ref()?;
        match self.native.as_ref() {
            None => native.root(self.room.as_ref()),
            Some(destination) => {
                if destination.dashboard.as_str() != layout.id.as_str() {
                    return None;
                }
                let node = native.nodes.get(&destination.node)?;
                let expected = match destination.presentation {
                    Presentation::Page => crate::dashboard_layout::Kind::View,
                    Presentation::Popup => crate::dashboard_layout::Kind::Popup,
                };
                (node.kind == expected).then_some(&node.id)
            }
        }
    }
}
pub(crate) type SharedRequest = Arc<Mutex<ProjectionRequest>>;

#[cfg(test)]
pub(crate) fn fixture_wire() -> wire::Dashboard {
    let data: serde_json::Value =
        serde_json::from_str(include_str!("../tests/fixtures/overview.json")).unwrap();
    let rooms = data["rooms"]
        .as_array()
        .unwrap()
        .iter()
        .map(|room| wire::OverviewRoom {
            id: room["id"].as_str().unwrap().into(),
            title: room["title"].as_str().unwrap().into(),
            entity_ids: room["entity_ids"]
                .as_array()
                .unwrap()
                .iter()
                .map(|id| id.as_str().unwrap().into())
                .collect(),
            ..Default::default()
        })
        .collect();
    let observations = data["observations"]
        .as_array()
        .unwrap()
        .iter()
        .map(|item| wire::OverviewObservation {
            id: item["id"].as_str().unwrap().into(),
            title: item["title"].as_str().unwrap().into(),
            entity_id: item["entity_id"].as_str().unwrap().into(),
            icon: wire::NativeIcon::Information.into(),
            ..Default::default()
        })
        .collect();
    wire::Dashboard {
        metadata: wire::Metadata {
            id: wire::DashboardId {
                value: "synthetic-dashboard".into(),
                ..Default::default()
            }
            .into(),
            profile: "homeward-dashboard-1".into(),
            version: home_wire_contract::home::configuration::v1::DocumentVersion {
                revision: 1,
                source_sha256: vec![4; 32],
                ..Default::default()
            }
            .into(),
            ..Default::default()
        }
        .into(),
        native_overview: wire::NativeOverview {
            version: 1,
            config_revision: 1,
            authorization_revision: 1,
            config_sha256: vec![1; 32],
            title: data["title"].as_str().unwrap().into(),
            rooms,
            observations,
            mode: wire::ServingMode::Synthetic.into(),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
#[cfg(test)]
pub(crate) fn fixture() -> Layout {
    Layout::try_from(&fixture_wire()).unwrap()
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn an_extended_layout_cannot_be_activated_under_basic_metadata() {
        let mut doc = fixture_wire();
        doc.native_layout = crate::dashboard_layout::tests::extended_forest().into();
        assert_eq!(Layout::try_from(&doc), Err(LayoutFailure::Unsupported));
        doc.metadata.as_option_mut().unwrap().profile = Profile::Recipes.name().into();
        assert!(Layout::try_from(&doc).unwrap().native.is_some());
        doc.native_layout = Default::default();
        assert_eq!(
            Layout::try_from(&doc),
            Err(LayoutFailure::Unsupported),
            "recipes require their tree"
        );
    }
    #[test]
    fn unsupported_profile_keeps_a_bounded_sourced_diagnostic_and_no_layout() {
        let mut document = fixture_wire();
        document.metadata.as_option_mut().unwrap().profile = "homeward-dashboard-99".into();
        document.definition = wire::Definition {
            entry: "external.yaml".into(),
            ..Default::default()
        }
        .into();
        assert_eq!(Layout::try_from(&document), Err(LayoutFailure::Unsupported));
        let unsupported = LayoutState::unsupported(&document);
        assert!(
            unsupported.message().contains("homeward-dashboard-99")
                && unsupported.message().contains("external.yaml")
        );
        assert!(unsupported.layout().is_none());
    }
    #[test]
    fn room_binding_order_is_received_data_and_membership_is_only_a_derived_index() {
        let mut doc = fixture_wire();
        doc.native_overview.as_option_mut().unwrap().rooms[0].entity_ids =
            vec!["ordered-entity-z".into(), "ordered-entity-a".into()];
        let layout = Layout::try_from(&doc).unwrap();
        let bindings = &layout.rooms[0].entities;
        assert_eq!(
            bindings.iter().map(EntityId::as_str).collect::<Vec<_>>(),
            ["ordered-entity-z", "ordered-entity-a"]
        );
        assert!(bindings.contains(&"ordered-entity-a".parse().unwrap()));
        assert!(bindings.contains(&"ordered-entity-z".parse().unwrap()));
        doc.native_overview.as_option_mut().unwrap().rooms[0]
            .entity_ids
            .reverse();
        let reordered = Layout::try_from(&doc).unwrap();
        assert_ne!(
            layout, reordered,
            "a server reorder is a new layout even with identical membership"
        );
    }
    #[test]
    fn rejects_future_profiles_duplicate_bindings_and_unbounded_data() {
        let mut doc = fixture_wire();
        doc.native_overview.as_option_mut().unwrap().version = 2;
        assert_eq!(Layout::try_from(&doc), Err(LayoutFailure::Unsupported));
        let mut doc = fixture_wire();
        doc.native_overview.as_option_mut().unwrap().mode = EnumValue::Unknown(99);
        assert_eq!(Layout::try_from(&doc), Err(LayoutFailure::Unsupported));
        let mut doc = fixture_wire();
        let overview = doc.native_overview.as_option_mut().unwrap();
        overview.rooms.push(overview.rooms[0].clone());
        assert_eq!(Layout::try_from(&doc), Err(LayoutFailure::Invalid));
        let mut doc = fixture_wire();
        let overview = doc.native_overview.as_option_mut().unwrap();
        let id = overview.rooms[0].entity_ids[0].clone();
        overview.rooms[0].entity_ids.push(id);
        assert_eq!(Layout::try_from(&doc), Err(LayoutFailure::Invalid));
        let mut doc = fixture_wire();
        let overview = doc.native_overview.as_option_mut().unwrap();
        overview.rooms = vec![overview.rooms[0].clone(); MAX_ROOMS + 1];
        assert_eq!(Layout::try_from(&doc), Err(LayoutFailure::Budget));
        let mut doc = fixture_wire();
        doc.native_overview.as_option_mut().unwrap().config_sha256 = vec![0; 32];
        assert_eq!(Layout::try_from(&doc), Err(LayoutFailure::Invalid));
    }
}

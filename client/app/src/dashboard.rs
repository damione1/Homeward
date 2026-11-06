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
    pub source: Option<home_client_session::SourceProof>,
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
            source: None,
            capture: None,
            binding_expires: None,
            rooms,
            observations: Vec::new(),
            native: Some(native),
        })
    }
}
impl Layout {
    /// The authorized document is separate from the short-lived proof joining
    /// it to one entity capture. Only this comparison may retain local routes;
    /// command admission continues to compare the complete layout/proof.
    pub fn same_document(&self, other: &Self) -> bool {
        self.id == other.id
            && self.etag == other.etag
            && self.title == other.title
            && self.profile_zone == other.profile_zone
            && self.rooms == other.rooms
            && self.observations == other.observations
            && self.native == other.native
    }

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
        layout.source = view.source_proof().cloned();
        layout.capture = Some(view.incarnation().clone());
        layout.binding_expires = Some((expires.seconds, expires.nanos));
        Ok(layout)
    }
    pub fn publication_current(&self) -> bool {
        self.source
            .as_ref()
            .map_or(cfg!(test), |source| source.is_current())
    }
    pub fn compatible(&self, view: &CachedView) -> bool {
        self.publication_current()
            && self.source.as_ref() == view.source_proof()
            && self.capture.as_ref() == Some(view.incarnation())
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
    use crate::dashboard_layout::tests::{children, node};
    use wire::dashboard_layout_node::Content as C;
    let data: serde_json::Value =
        serde_json::from_str(include_str!("../tests/fixtures/overview.json")).unwrap();
    let mut layout = wire::DashboardLayout::default();
    for room in data["rooms"].as_array().unwrap() {
        let id = room["id"].as_str().unwrap();
        let section = format!("{id}-section");
        let mut root = node(
            id,
            C::View(
                wire::DashboardViewNode {
                    route: id.into(),
                    children: children(&[&section]).into(),
                    ..Default::default()
                }
                .into(),
            ),
        );
        root.display_name = room["title"].as_str().unwrap().into();
        let mut cards = Vec::new();
        for (index, entity) in room["entity_ids"].as_array().unwrap().iter().enumerate() {
            let tile_id = format!("{id}-tile-{index}");
            cards.push(tile_id.clone());
            layout.nodes.push(node(
                &tile_id,
                C::Tile(
                    wire::DashboardTileNode {
                        capability: format!(
                            "homes/synthetic/entities/{}/capabilities/power",
                            entity.as_str().unwrap()
                        ),
                        presentation: wire::DashboardControlPresentation::Controls.into(),
                        ..Default::default()
                    }
                    .into(),
                ),
            ));
        }
        layout.views.push(id.into());
        layout.nodes.push(root);
        layout.nodes.push(node(
            &section,
            C::Section(
                wire::DashboardSectionNode {
                    children: wire::DashboardChildren {
                        children: cards,
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                }
                .into(),
            ),
        ));
    }
    wire::Dashboard {
        name: "homes/synthetic/dashboards/synthetic-dashboard".into(),
        etag: "opaque-dashboard-revision".into(),
        display_name: data["title"].as_str().unwrap().into(),
        layout: layout.into(),
        ..Default::default()
    }
}
#[cfg(test)]
pub(crate) fn fixture() -> Layout {
    Layout::try_from(&fixture_wire()).unwrap()
}

#[cfg(test)]
pub(crate) fn capture_fixture(
    entries: Vec<wire::EntityViewEntry>,
) -> home_client_cache::CachedView {
    use buffa_types::google::protobuf::Timestamp;
    use home_client_cache::{Filter, SnapshotCapture};
    let now = Timestamp::from_unix_secs(chrono::Utc::now().timestamp());
    let expires = Timestamp::from_unix_secs(now.seconds + 60);
    let mut capture = SnapshotCapture::begin(
        home_client_model::api::HomeName::try_from("homes/synthetic").unwrap(),
        Filter::all(),
        256,
        wire::EntityView {
            name: "homes/synthetic/entityViews/capture-fixture".into(),
            create_time: now.clone().into(),
            expire_time: expires.into(),
            ..Default::default()
        },
        &now,
    )
    .unwrap();
    capture
        .push(
            wire::QueryEntityViewResponse {
                entries,
                resume_token: "opaque-fixture-checkpoint".into(),
                ..Default::default()
            },
            &now,
        )
        .unwrap();
    capture.finish(&now).unwrap()
}
#[cfg(test)]
pub(crate) fn query_fixture(
    document: wire::Dashboard,
    view: &home_client_cache::CachedView,
) -> wire::QueryDashboardResponse {
    wire::QueryDashboardResponse {
        dashboard: document.into(),
        entity_view: view.name().as_str().into(),
        resume_token: view.checkpoint().into(),
        binding: "opaque-fixture-binding".into(),
        expire_time: view.expires().clone().into(),
        ..Default::default()
    }
}
#[cfg(test)]
pub(crate) fn joined_fixture(view: &home_client_cache::CachedView) -> Layout {
    let response = query_fixture(fixture_wire(), view);
    Layout::join(
        &response,
        &DashboardName::try_from(response.dashboard.name.as_str()).unwrap(),
        view,
    )
    .unwrap()
}

#[cfg(test)]
mod tests {
    use super::*;
    use buffa::Message;
    use wire::dashboard_layout_node::Content as C;
    #[test]
    fn a_dashboard_requires_its_current_typed_layout() {
        let mut doc = fixture_wire();
        doc.layout = crate::dashboard_layout::tests::extended_forest().into();
        assert!(Layout::try_from(&doc).unwrap().native.is_some());
        doc.layout = Default::default();
        assert_eq!(Layout::try_from(&doc), Err(LayoutFailure::Invalid));
    }
    #[test]
    fn unsupported_fields_produce_no_layout_or_untrusted_diagnostic_contents() {
        let mut bytes = fixture_wire().encode_to_vec();
        bytes.extend_from_slice(&[0x98, 0x06, 0x01]);
        let document = wire::Dashboard::decode_from_slice(&bytes).unwrap();
        assert_eq!(Layout::try_from(&document), Err(LayoutFailure::Unsupported));
        let unsupported = LayoutState::unsupported(&document);
        assert!(unsupported.layout().is_none());
        assert!(!unsupported.message().is_empty());
    }
    #[test]
    fn room_binding_order_is_received_data_and_membership_is_only_a_derived_index() {
        let mut doc = fixture_wire();
        let tree = doc.layout.get_or_insert_default();
        let first = tree
            .nodes
            .iter()
            .position(|node| node.id == "annex-tile-0")
            .unwrap();
        let Some(C::Tile(tile)) = &mut tree.nodes[first].content else {
            panic!()
        };
        tile.capability = "homes/synthetic/entities/ordered-entity-z/capabilities/power".into();
        let mut second = tree.nodes[first].clone();
        second.id = "annex-tile-second".into();
        let Some(C::Tile(tile)) = &mut second.content else {
            panic!()
        };
        tile.capability = "homes/synthetic/entities/ordered-entity-a/capabilities/power".into();
        tree.nodes.push(second);
        let Some(C::Section(section)) = &mut tree
            .nodes
            .iter_mut()
            .find(|n| n.id == "annex-section")
            .unwrap()
            .content
        else {
            panic!()
        };
        section
            .children
            .get_or_insert_default()
            .children
            .push("annex-tile-second".into());
        let layout = Layout::try_from(&doc).unwrap();
        let bindings = &layout.rooms[0].entities;
        assert_eq!(
            bindings.iter().map(EntityName::as_str).collect::<Vec<_>>(),
            [
                "homes/synthetic/entities/ordered-entity-z",
                "homes/synthetic/entities/ordered-entity-a"
            ]
        );
        for name in [
            "homes/synthetic/entities/ordered-entity-a",
            "homes/synthetic/entities/ordered-entity-z",
        ] {
            assert!(bindings.contains(&EntityName::try_from(name).unwrap()));
        }
        let Some(C::Section(section)) = &mut doc
            .layout
            .get_or_insert_default()
            .nodes
            .iter_mut()
            .find(|n| n.id == "annex-section")
            .unwrap()
            .content
        else {
            panic!()
        };
        section.children.get_or_insert_default().children.reverse();
        assert_ne!(
            layout,
            Layout::try_from(&doc).unwrap(),
            "source order must remain visible"
        );
    }
    #[test]
    fn duplicate_routes_foreign_bindings_missing_cas_and_unbounded_data_are_rejected() {
        let mut doc = fixture_wire();
        let tree = doc.layout.get_or_insert_default();
        let Some(C::View(view)) = &mut tree
            .nodes
            .iter_mut()
            .find(|n| n.id == "workshop")
            .unwrap()
            .content
        else {
            panic!()
        };
        view.route = "annex".into();
        assert_eq!(Layout::try_from(&doc), Err(LayoutFailure::Invalid));
        let mut doc = fixture_wire();
        doc.etag.clear();
        assert_eq!(Layout::try_from(&doc), Err(LayoutFailure::Invalid));
        let mut doc = fixture_wire();
        let Some(C::Tile(tile)) = &mut doc
            .layout
            .get_or_insert_default()
            .nodes
            .iter_mut()
            .find(|n| n.id == "annex-tile-0")
            .unwrap()
            .content
        else {
            panic!()
        };
        tile.capability = "homes/foreign/entities/foreign-entity/capabilities/power".into();
        assert_eq!(Layout::try_from(&doc), Err(LayoutFailure::Invalid));
        let mut doc = fixture_wire();
        doc.layout.get_or_insert_default().views = vec!["annex".into(); 129];
        assert_eq!(Layout::try_from(&doc), Err(LayoutFailure::Budget));
    }
}

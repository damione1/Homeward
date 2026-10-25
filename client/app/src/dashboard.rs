//! Server-owned dashboard data. Compiled code contains only bounded native
//! component vocabulary; installation names and exact bindings arrive by RPC.
use buffa::EnumValue;
use home_client_cache::CachedView;
use home_client_model::EntityId;
use home_wire_contract::home::dashboard::v1 as wire;
use std::{
    collections::BTreeSet,
    sync::{Arc, Mutex},
};

pub(crate) const MAX_ROOMS: usize = 127;
pub(crate) const MAX_OBSERVATIONS: usize = 64;
pub(crate) const ROOM_TOKEN_BASE: i32 = 1000;
pub(crate) const OBSERVATION_TOKEN_BASE: i32 = 2000;

#[derive(Clone, Debug, PartialEq, Eq, Hash)]
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
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub(crate) struct DashboardId(String);
impl DashboardId {
    pub fn parse(value: &str) -> Result<Self, LayoutFailure> {
        if text(value) {
            Ok(Self(value.to_owned()))
        } else {
            Err(LayoutFailure::Invalid)
        }
    }
    pub fn as_str(&self) -> &str {
        &self.0
    }
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct ObservationId(String);
/// Server order plus a derived membership index. Room cards preserve the wire
/// order; inventory filtering keeps logarithmic membership checks.
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct RoomEntities {
    ordered: Vec<EntityId>,
    membership: BTreeSet<EntityId>,
}
impl RoomEntities {
    pub(crate) fn new(ordered: Vec<EntityId>) -> Result<Self, LayoutFailure> {
        let membership = ordered.iter().cloned().collect::<BTreeSet<_>>();
        if membership.len() != ordered.len() {
            return Err(LayoutFailure::Invalid);
        }
        Ok(Self {
            ordered,
            membership,
        })
    }
    pub fn contains(&self, id: &EntityId) -> bool {
        self.membership.contains(id)
    }
    pub fn iter(&self) -> impl Iterator<Item = &EntityId> {
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
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum ServingMode {
    Configured,
    Synthetic,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct Observation {
    pub id: ObservationId,
    pub label: String,
    pub entity: EntityId,
    pub icon: Icon,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct Layout {
    pub id: DashboardId,
    pub document_revision: i64,
    pub document_hash: [u8; 32],
    pub config_revision: u64,
    pub authorization_revision: u64,
    pub config_hash: [u8; 32],
    pub title: String,
    pub mode: ServingMode,
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
fn hash(value: &[u8]) -> Result<[u8; 32], LayoutFailure> {
    let result: [u8; 32] = value.try_into().map_err(|_| LayoutFailure::Invalid)?;
    if result == [0; 32] {
        return Err(LayoutFailure::Invalid);
    }
    Ok(result)
}
impl TryFrom<&wire::Dashboard> for Layout {
    type Error = LayoutFailure;
    fn try_from(value: &wire::Dashboard) -> Result<Self, Self::Error> {
        let metadata = value.metadata.as_option().ok_or(LayoutFailure::Invalid)?;
        let id = metadata.id.as_option().ok_or(LayoutFailure::Invalid)?;
        let version = metadata.version.as_option().ok_or(LayoutFailure::Invalid)?;
        let overview = value
            .native_overview
            .as_option()
            .ok_or(LayoutFailure::Unsupported)?;
        if metadata.profile != "homeward-dashboard-1"
            || overview.version != 1
            || !metadata.__buffa_unknown_fields.is_empty()
            || !overview.__buffa_unknown_fields.is_empty()
            || !version.__buffa_unknown_fields.is_empty()
        {
            return Err(LayoutFailure::Unsupported);
        }
        if !text(&id.value)
            || !text(&overview.title)
            || version.revision <= 0
            || overview.config_revision == 0
            || overview.authorization_revision == 0
        {
            return Err(LayoutFailure::Invalid);
        }
        if overview.rooms.len() > MAX_ROOMS || overview.observations.len() > MAX_OBSERVATIONS {
            return Err(LayoutFailure::Budget);
        }
        let mode = match overview.mode {
            EnumValue::Known(wire::ServingMode::Configured) => ServingMode::Configured,
            EnumValue::Known(wire::ServingMode::Synthetic) => ServingMode::Synthetic,
            EnumValue::Known(wire::ServingMode::Unspecified) | EnumValue::Unknown(_) => {
                return Err(LayoutFailure::Unsupported);
            }
        };
        let mut room_ids = BTreeSet::new();
        let mut binding_count = 0usize;
        let mut rooms = Vec::new();
        for room in &overview.rooms {
            if !room.__buffa_unknown_fields.is_empty() {
                return Err(LayoutFailure::Unsupported);
            }
            if !text(&room.id) || !text(&room.title) || !room_ids.insert(room.id.clone()) {
                return Err(LayoutFailure::Invalid);
            }
            binding_count = binding_count.saturating_add(room.entity_ids.len());
            if binding_count > 4096 {
                return Err(LayoutFailure::Budget);
            }
            let mut entities = Vec::new();
            for id in &room.entity_ids {
                if !text(id) {
                    return Err(LayoutFailure::Invalid);
                }
                entities.push(id.parse().map_err(|_| LayoutFailure::Invalid)?);
            }
            rooms.push(Room {
                id: RoomId(room.id.clone()),
                label: room.title.clone(),
                entities: RoomEntities::new(entities)?,
            });
        }
        let mut observation_ids = BTreeSet::new();
        let mut observations = Vec::new();
        for item in &overview.observations {
            if !item.__buffa_unknown_fields.is_empty() {
                return Err(LayoutFailure::Unsupported);
            }
            if !text(&item.id)
                || !text(&item.title)
                || !text(&item.entity_id)
                || !observation_ids.insert(item.id.clone())
            {
                return Err(LayoutFailure::Invalid);
            }
            let icon = match item.icon {
                EnumValue::Known(wire::NativeIcon::Home) => Icon::Home,
                EnumValue::Known(wire::NativeIcon::Information) => Icon::Information,
                EnumValue::Known(wire::NativeIcon::Clock) => Icon::Clock,
                EnumValue::Known(wire::NativeIcon::Grid) => Icon::Grid,
                EnumValue::Known(wire::NativeIcon::Unspecified) | EnumValue::Unknown(_) => {
                    return Err(LayoutFailure::Unsupported);
                }
            };
            observations.push(Observation {
                id: ObservationId(item.id.clone()),
                label: item.title.clone(),
                entity: item.entity_id.parse().map_err(|_| LayoutFailure::Invalid)?,
                icon,
            });
        }
        Ok(Self {
            id: DashboardId::parse(&id.value)?,
            document_revision: version.revision,
            document_hash: hash(&version.source_sha256)?,
            config_revision: overview.config_revision,
            authorization_revision: overview.authorization_revision,
            config_hash: hash(&overview.config_sha256)?,
            title: overview.title.clone(),
            mode,
            rooms,
            observations,
            native: value
                .native_layout
                .as_option()
                .map(crate::dashboard_layout::NativeLayout::try_from)
                .transpose()?,
        })
    }
}
impl Layout {
    pub fn compatible(&self, view: &CachedView) -> bool {
        self.authorization_revision == view.cursor().authorization_revision
            && self.config_revision == view.cursor().config_revision
            && self.config_hash.as_slice() == view.cursor().config_sha256
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
    pub fn unsupported(value: &wire::Dashboard) -> Self {
        let label = |text: &str| {
            if !text.is_empty() && text.len() <= 256 && !text.chars().any(char::is_control) {
                text.to_owned()
            } else {
                "non précisé".into()
            }
        };
        let profile = label(
            value
                .metadata
                .as_option()
                .map_or("", |metadata| metadata.profile.as_str()),
        );
        let source = label(
            value
                .definition
                .as_option()
                .map_or("", |definition| definition.entry.as_str()),
        );
        let version = value
            .native_overview
            .as_option()
            .map_or(0, |overview| overview.version);
        Self::Unsupported(format!("Profil de dashboard non pris en charge : {profile} · version {version} · source {source}").into())
    }
    pub fn message(&self) -> &str {
        match self {
            Self::Loading => "Chargement du dashboard depuis le serveur…",
            Self::Unavailable => "Le serveur ne fournit pas de dashboard autorisé.",
            Self::Invalid => {
                "La définition du dashboard ne peut pas être affichée par cette version."
            }
            Self::Unsupported(message) => message,
            Self::Ready(_) => "Synchronisation du dashboard et des observations…",
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
    Room(RoomId),
    Detail(EntityId),
    Settings,
}
#[derive(Clone, Default)]
pub(crate) struct ProjectionRequest {
    pub revision: i32,
    pub query: String,
    pub room: Option<RoomId>,
    pub detail: Option<EntityId>,
    pub dashboard: LayoutState,
}
pub(crate) type SharedRequest = Arc<Mutex<ProjectionRequest>>;

#[cfg(test)]
fn fixture_wire() -> wire::Dashboard {
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

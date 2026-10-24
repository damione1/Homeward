//! Local development layout translated from the HA Tablet/Overview corpus.
//! These groupings are presentation hints, never registry or authorization data.
use home_client_model::EntityId;
use std::sync::{Arc, Mutex};

pub(crate) const ROOM_GENERATION: i32 = 1;
pub(crate) const ROOM_TOKEN_BASE: i32 = 10;

#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub(crate) struct RoomId(&'static str);

pub(crate) struct Room {
    pub id: RoomId,
    pub label: &'static str,
    pub markers: &'static [&'static str],
}

pub(crate) const ROOMS: [Room; 9] = [
    Room {
        id: RoomId("living-room"),
        label: "Salon",
        markers: &["living_room", "salon"],
    },
    Room {
        id: RoomId("kitchen"),
        label: "Cuisine",
        markers: &["kitchen", "cuisine"],
    },
    Room {
        id: RoomId("dining-room"),
        label: "Salle à manger",
        markers: &["dining_room", "salle_a_manger"],
    },
    Room {
        id: RoomId("master-bedroom"),
        label: "Chambre principale",
        markers: &["master_bedroom", "chambre_principale"],
    },
    Room {
        id: RoomId("eugenie"),
        label: "Chambre d’Eugénie",
        markers: &["eugenie"],
    },
    Room {
        id: RoomId("bathroom"),
        label: "Salle de bain",
        markers: &["bathroom", "salle_de_bain", "salle_de_bains"],
    },
    Room {
        id: RoomId("office"),
        label: "Bureau",
        markers: &["office", "bureau"],
    },
    Room {
        id: RoomId("guest-bedroom"),
        label: "Chambre d’amis",
        markers: &["guest_bedroom", "chambre_invites", "chambre_d_amis"],
    },
    Room {
        id: RoomId("outdoor"),
        label: "Extérieur",
        markers: &["outdoor", "frontyard", "backyard", "exterieur"],
    },
];

pub(crate) fn room_for_alias(alias: &str) -> Option<&'static Room> {
    let (_, object) = alias.split_once('.')?;
    ROOMS
        .iter()
        .find(|room| room.markers.iter().any(|marker| object.contains(marker)))
}

pub(crate) fn room(id: &RoomId) -> &'static Room {
    ROOMS
        .iter()
        .find(|room| room.id == *id)
        .expect("room comes from development layout")
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
}
pub(crate) type SharedRequest = Arc<Mutex<ProjectionRequest>>;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum SummaryKind {
    Climate,
    Weather,
    Fan,
}
impl SummaryKind {
    pub const ALL: [Self; 3] = [Self::Climate, Self::Weather, Self::Fan];
    pub fn token(self) -> i32 {
        match self {
            Self::Climate => 100,
            Self::Weather => 101,
            Self::Fan => 102,
        }
    }
    pub fn label(self) -> &'static str {
        match self {
            Self::Climate => "Confort principal",
            Self::Weather => "Météo",
            Self::Fan => "Ventilation du salon",
        }
    }
    pub fn alias(self) -> &'static str {
        match self {
            Self::Climate => "climate.home",
            Self::Weather => "weather.maison",
            Self::Fan => "fan.living_room_ceiling_fan",
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn development_rooms_preserve_the_ha_anchors_without_assigning_unknown_entities() {
        assert_eq!(
            room_for_alias("climate.plancher_cuisine").unwrap().id,
            RoomId("kitchen")
        );
        assert_eq!(
            room_for_alias("fan.living_room_ceiling_fan").unwrap().id,
            RoomId("living-room")
        );
        assert_eq!(
            room_for_alias("climate.chambre_principale").unwrap().id,
            RoomId("master-bedroom")
        );
        assert!(room_for_alias("climate.home").is_none());
        assert!(room_for_alias("sensor.unassigned_device").is_none());
    }
}

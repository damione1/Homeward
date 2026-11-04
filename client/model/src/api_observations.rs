//! Independent closed quality dimensions. Unspecified values remain explicit;
//! availability never comes from power, labels, timestamps or transport success.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Availability {
    Unspecified,
    Available,
    Unavailable,
    Unknown,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Freshness {
    Unspecified,
    Current,
    Stale,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Provenance {
    Unspecified,
    Observed,
    Derived,
    Restored,
    Estimated,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ObservationQuality {
    availability: Availability,
    freshness: Freshness,
    provenance: Provenance,
}
impl ObservationQuality {
    pub fn new(availability: Availability, freshness: Freshness, provenance: Provenance) -> Self {
        Self {
            availability,
            freshness,
            provenance,
        }
    }
    pub fn availability(self) -> Availability {
        self.availability
    }
    pub fn freshness(self) -> Freshness {
        self.freshness
    }
    pub fn provenance(self) -> Provenance {
        self.provenance
    }
    pub fn current_available(self) -> bool {
        self.availability == Availability::Available && self.freshness == Freshness::Current
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ActionAccess {
    Unspecified,
    Allowed,
    Denied,
}

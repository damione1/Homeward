//! Current public fixtures: canonical resources, distinct capability slots and
//! actual snapshot admission. No native identifier or legacy DTO conversion.
use buffa_types::google::protobuf::Timestamp;
use home_client_cache::{CachedView, Filter, SnapshotCapture};
use home_client_model::api::{CapabilityName, EntityName, HomeName};
use home_wire_contract::home::api::v2 as wire;

pub(crate) fn entry(
    name: &str,
    label: &str,
    capabilities: Vec<wire::Capability>,
) -> wire::EntityViewEntry {
    let name = EntityName::try_from(name).unwrap();
    for capability in &capabilities {
        assert_eq!(
            CapabilityName::try_from(capability.name.as_str())
                .unwrap()
                .entity(),
            name
        );
    }
    wire::EntityViewEntry {
        entity: wire::Entity {
            name: name.as_str().into(),
            display_name: label.into(),
            etag: "entity-definition".into(),
            ..Default::default()
        }
        .into(),
        capabilities,
        ..Default::default()
    }
}
pub(crate) fn capability(name: &str, value: wire::capability::Trait) -> wire::Capability {
    CapabilityName::try_from(name).unwrap();
    wire::Capability {
        name: name.into(),
        etag: "capability-definition".into(),
        action_access: wire::ActionAccess::Allowed.into(),
        r#trait: Some(value),
        ..Default::default()
    }
}
pub(crate) fn quality() -> wire::ObservationQuality {
    wire::ObservationQuality {
        availability: wire::Availability::Available.into(),
        freshness: wire::Freshness::Current.into(),
        provenance: wire::Provenance::Observed.into(),
        ..Default::default()
    }
}
pub(crate) fn power(name: &str, value: Option<wire::PowerValue>) -> wire::Capability {
    capability(
        name,
        wire::capability::Trait::Power(Box::new(wire::PowerCapability {
            observation: wire::PowerObservation {
                value: value.map(Into::into),
                quality: quality().into(),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        })),
    )
}
pub(crate) fn capture(entries: Vec<wire::EntityViewEntry>) -> CachedView {
    let now = Timestamp::from_unix_secs(chrono::Utc::now().timestamp());
    let mut snapshot = SnapshotCapture::begin(
        HomeName::try_from("homes/synthetic").unwrap(),
        Filter::all(),
        256,
        wire::EntityView {
            name: "homes/synthetic/entityViews/fixture".into(),
            create_time: now.clone().into(),
            expire_time: Timestamp::from_unix_secs(now.seconds + 60).into(),
            ..Default::default()
        },
        &now,
    )
    .unwrap();
    let count = entries.len();
    for (index, page) in entries.chunks(256).enumerate() {
        let complete = (index + 1) * 256 >= count;
        snapshot
            .push(
                wire::QueryEntityViewResponse {
                    entries: page.to_vec(),
                    next_page_token: if complete {
                        String::new()
                    } else {
                        format!("opaque-page-{index}")
                    },
                    resume_token: if complete {
                        "opaque-complete-checkpoint".into()
                    } else {
                        String::new()
                    },
                    ..Default::default()
                },
                &now,
            )
            .unwrap();
    }
    if count == 0 {
        snapshot
            .push(
                wire::QueryEntityViewResponse {
                    resume_token: "opaque-complete-checkpoint".into(),
                    ..Default::default()
                },
                &now,
            )
            .unwrap();
    }
    snapshot.finish(&now).unwrap()
}

/// One actual capture and its joined dashboard throughout a synthetic gesture.
/// Replacing entries uses the public Watch protocol and retains the capture's
/// incarnation; creating another Scenario deliberately creates another capture.
pub(crate) struct Scenario {
    pub request: crate::dashboard::ProjectionRequest,
    live: std::cell::RefCell<home_client_cache::LiveView>,
    checkpoint: std::cell::Cell<u64>,
}
impl Scenario {
    pub(crate) fn new() -> Self {
        let view = capture(Vec::new());
        let layout = crate::dashboard::joined_fixture(&view);
        let request = crate::dashboard::ProjectionRequest {
            room: Some(layout.rooms[1].id.clone()),
            dashboard: crate::dashboard::LayoutState::Ready(std::sync::Arc::new(layout)),
            ..Default::default()
        };
        Self {
            request,
            live: std::cell::RefCell::new(home_client_cache::LiveView::new(view)),
            checkpoint: std::cell::Cell::new(0),
        }
    }
    pub(crate) fn view(&self, entries: Vec<wire::EntityViewEntry>) -> CachedView {
        use wire::watch_entity_view_response::Change;
        let mut live = self.live.borrow_mut();
        let names = entries
            .iter()
            .map(|entry| EntityName::try_from(entry.entity.name.as_str()).unwrap())
            .collect::<std::collections::BTreeSet<_>>();
        let removed = live
            .current()
            .unwrap()
            .entities()
            .map(|(name, _)| name.clone())
            .filter(|name| !names.contains(name))
            .collect::<Vec<_>>();
        for change in removed
            .into_iter()
            .map(|name| Change::Remove(name.as_str().into()))
            .chain(
                entries
                    .into_iter()
                    .map(|entry| Change::Upsert(Box::new(entry))),
            )
        {
            let next = self.checkpoint.get().checked_add(1).unwrap();
            self.checkpoint.set(next);
            live.apply(wire::WatchEntityViewResponse {
                resume_token: format!("fixture-checkpoint-{next}"),
                change: Some(change),
                ..Default::default()
            })
            .unwrap();
        }
        live.current().unwrap().clone()
    }
}
impl std::ops::Deref for Scenario {
    type Target = crate::dashboard::ProjectionRequest;
    fn deref(&self) -> &Self::Target {
        &self.request
    }
}
impl std::ops::DerefMut for Scenario {
    fn deref_mut(&mut self) -> &mut Self::Target {
        &mut self.request
    }
}
impl Clone for Scenario {
    fn clone(&self) -> Self {
        Self {
            request: self.request.clone(),
            live: std::cell::RefCell::new(home_client_cache::LiveView::new(
                self.live.borrow().current().unwrap().clone(),
            )),
            checkpoint: std::cell::Cell::new(self.checkpoint.get()),
        }
    }
}
pub(crate) fn light_mut(entry: &mut wire::EntityViewEntry) -> &mut wire::LightControlCapability {
    let Some(wire::capability::Trait::LightControl(value)) = entry.capabilities[0].r#trait.as_mut()
    else {
        panic!("fixture requires exact LightControl trait")
    };
    value
}
pub(crate) fn temperature_mut(
    entry: &mut wire::EntityViewEntry,
) -> &mut wire::TemperatureControlCapability {
    let Some(wire::capability::Trait::TemperatureControl(value)) =
        entry.capabilities[0].r#trait.as_mut()
    else {
        panic!("fixture requires exact TemperatureControl trait")
    };
    value
}
pub(crate) fn helper_mut(
    entry: &mut wire::EntityViewEntry,
    index: usize,
) -> &mut wire::HelperCapability {
    let Some(wire::capability::Trait::Helper(value)) = entry.capabilities[index].r#trait.as_mut()
    else {
        panic!("fixture requires exact Helper trait")
    };
    value
}

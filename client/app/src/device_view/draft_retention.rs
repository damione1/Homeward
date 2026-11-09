//! Local input survives a capture rotation, without retaining an admission,
//! gesture, selector session or pending command. The old proof only bounds
//! these bytes; a joined capture supplies every new command generation.
use super::*;
use crate::{cache_view, dashboard::RoomId};
use home_client_model::dashboard::Destination;

type Stamp = (i64, i32);

pub(super) fn now() -> Stamp {
    let now = chrono::Utc::now();
    (now.timestamp(), now.timestamp_subsec_nanos() as i32)
}

#[derive(Clone, PartialEq)]
struct Scope {
    revision: i32,
    query: String,
    room: Option<RoomId>,
    detail: Option<EntityName>,
    detail_source: Option<crate::dashboard_layout::NodeId>,
    native: Option<Destination>,
}
impl From<&ProjectionRequest> for Scope {
    fn from(request: &ProjectionRequest) -> Self {
        Self {
            revision: request.revision,
            query: request.query.clone(),
            room: request.room.clone(),
            detail: request.detail.clone(),
            detail_source: request.detail_source.clone(),
            native: request.native.clone(),
        }
    }
}

#[derive(Clone)]
pub(super) struct Basis {
    source: Option<home_client_session::SourceProof>,
    scope: Scope,
    layout: Arc<Layout>,
    authority: Vec<cache_view::EntityAuthority>,
    deadline: Stamp,
}
impl Basis {
    pub fn new(
        phase: CachePhase,
        view: Option<&CachedView>,
        request: &ProjectionRequest,
    ) -> Option<Self> {
        let view = view?;
        let layout = request.dashboard.shared_layout()?;
        let deadline = layout
            .binding_expires?
            .min((view.expires().seconds, view.expires().nanos));
        (phase == CachePhase::Live && layout.compatible(view) && deadline > now()).then_some(Self {
            source: view.source_proof().cloned(),
            scope: Scope::from(request),
            layout,
            authority: cache_view::authority(view)?,
            deadline,
        })
    }
    fn permits(
        &self,
        phase: CachePhase,
        view: Option<&CachedView>,
        request: &ProjectionRequest,
        now: Stamp,
    ) -> bool {
        let (Some(view), Some(layout)) = (view, request.dashboard.layout()) else {
            return false;
        };
        phase == CachePhase::Live
            && self.source.as_ref().is_none_or(|old| {
                view.source_proof()
                    .is_some_and(|new| old.same_presentation(new) && new.is_current())
            })
            && self.deadline > now
            && (view.expires().seconds, view.expires().nanos) > now
            && self.scope == Scope::from(request)
            && cache_view::authority(view).as_ref() == Some(&self.authority)
            && if layout.compatible(view) {
                self.layout.same_document(layout)
            } else {
                // The same old proof may bridge only its own unjoined cohort.
                // This does not make that proof compatible with the new view.
                self.layout.as_ref() == layout
                    && layout.capture.as_ref() != Some(view.incarnation())
            }
    }
}

#[derive(PartialEq)]
struct Binding {
    source: ControlSource,
    target: CapabilityName,
    etag: String,
    access: WriteAccess,
    control: Control,
    helper_revision: Option<helper::HelperRevision>,
}
impl From<&Slot> for Binding {
    fn from(slot: &Slot) -> Self {
        Self {
            source: slot.source.clone(),
            target: slot.target.clone(),
            etag: slot.capability.etag.clone(),
            access: slot.access,
            control: slot.control.clone(),
            helper_revision: slot.helper_revision,
        }
    }
}
struct Text {
    binding: Binding,
    field: i32,
    draft: panel::Draft,
}
// Retain the nonsecret editor choice and exact Range text under the same
// bounded source/binding guards. Gestures and command admission never transfer.
struct RangePresentation {
    binding: Binding,
    module: home_client_model::control_modules::ControlModule,
    mode: crate::ProRangeEditorMode,
    text: Option<String>,
}
struct AlarmInput {
    binding: Binding,
    input: alarm_input::Input,
}
pub(super) struct Saved {
    pub serial: u64,
    basis: Basis,
    texts: Vec<Text>,
    alarms: Vec<AlarmInput>,
    ranges: Vec<RangePresentation>,
}
impl Saved {
    pub fn capture(state: &mut State) -> Option<Self> {
        let context = state.context.as_ref()?;
        let basis = state.draft_basis.as_ref()?.clone();
        let texts = context
            .slots
            .iter()
            .filter(|slot| slot.access == WriteAccess::Allowed)
            .flat_map(|slot| {
                state
                    .drafts
                    .iter()
                    .filter_map(move |((generation, field), draft)| {
                        if *generation != slot.generation {
                            return None;
                        }
                        let draft = draft.retained()?;
                        Some(Text {
                            binding: Binding::from(slot),
                            field: *field,
                            draft,
                        })
                    })
            })
            .collect::<Vec<_>>();
        let ranges = context
            .slots
            .iter()
            .filter_map(|slot| {
                let module = slot.module.as_ref()?;
                let draft = state.module_drafts.get(&slot.generation)?;
                (slot.access == WriteAccess::Allowed
                    && module.allowed
                    && observation::fresh(&slot.capability)
                    && modules::current(module, &slot.capability)
                    && draft.range_editor_mode == crate::ProRangeEditorMode::Exact)
                    .then(|| RangePresentation {
                        binding: Binding::from(slot),
                        module: module.clone(),
                        mode: draft.range_editor_mode,
                        text: matches!(
                            module.definition,
                            home_client_model::control_modules::ModuleDefinition::Range(_)
                        )
                        .then(|| draft.text.clone())
                        .flatten(),
                    })
            })
            .collect::<Vec<_>>();
        // Move protected input into the same bounded archive. It never enters
        // a cloned draft, projection, old command or retained admission.
        let alarms = std::mem::take(&mut state.alarm_inputs)
            .into_iter()
            .filter_map(|(generation, input)| {
                let slot = context.slots.iter().find(|slot| {
                    slot.generation == generation && slot.access == WriteAccess::Allowed
                })?;
                Some(AlarmInput {
                    binding: Binding::from(slot),
                    input,
                })
            })
            .collect::<Vec<_>>();
        (!texts.is_empty() || !alarms.is_empty() || !ranges.is_empty()).then_some(Self {
            serial: 0,
            basis,
            texts,
            alarms,
            ranges,
        })
    }
    pub fn permits(
        &self,
        phase: CachePhase,
        view: Option<&CachedView>,
        request: &ProjectionRequest,
        now: Stamp,
    ) -> bool {
        self.basis.permits(phase, view, request, now)
    }
    pub fn permits_renewal(
        &self,
        renewal: &home_client_cache::PresentationSource,
        request: &ProjectionRequest,
        now: Stamp,
    ) -> bool {
        renewal.renewing()
            && self.basis.deadline > now
            && self.basis.deadline <= renewal.deadline()
            && self.basis.source.as_ref() == Some(renewal.source())
            && self.basis.scope == Scope::from(request)
            && request.dashboard.layout() == Some(self.basis.layout.as_ref())
            && self.basis.layout.capture.as_ref() == Some(renewal.incarnation())
    }
    pub fn deadline(&self) -> Stamp {
        self.basis.deadline
    }
    pub fn restore(
        &mut self,
        context: &Context,
        drafts: &mut BTreeMap<(CardGeneration, i32), panel::Draft>,
        alarms: &mut BTreeMap<CardGeneration, alarm_input::Input>,
        module_drafts: &mut BTreeMap<CardGeneration, modules::Draft>,
    ) {
        for slot in &context.slots {
            let binding = Binding::from(slot);
            for text in self.texts.iter().filter(|text| text.binding == binding) {
                drafts
                    .entry((slot.generation, text.field))
                    .or_insert_with(|| text.draft.clone());
            }
        }
        for saved in &self.ranges {
            if let Some(slot) = context.slots.iter().find(|slot| {
                Binding::from(*slot) == saved.binding
                    && slot.module.as_ref() == Some(&saved.module)
                    && observation::fresh(&slot.capability)
                    && modules::current(&saved.module, &slot.capability)
            }) {
                let draft = module_drafts.entry(slot.generation).or_default();
                draft.range_editor_mode = saved.mode;
                if draft.text.is_none() {
                    draft.text = saved.text.clone();
                }
            }
        }
        for saved in self.alarms.drain(..) {
            if let Some(slot) = context
                .slots
                .iter()
                .find(|slot| Binding::from(*slot) == saved.binding)
            {
                alarms.insert(slot.generation, saved.input);
            }
        }
    }
}

/// Return the remaining wait only for the same live archive. Expiry never
/// clears a fresh context's drafts or a replacement archive.
pub(super) fn expire(
    state: &mut State,
    serial: u64,
    deadline: Stamp,
    now: Stamp,
) -> Option<std::time::Duration> {
    let saved = state.retained_drafts.as_ref()?;
    if saved.serial != serial || saved.deadline() != deadline {
        return None;
    }
    if now >= deadline {
        state.retained_drafts = None;
        return None;
    }
    let nanos = (i128::from(deadline.0) - i128::from(now.0)) * 1_000_000_000
        + i128::from(deadline.1)
        - i128::from(now.1);
    Some(std::time::Duration::from_nanos(
        u64::try_from(nanos).unwrap_or(u64::MAX),
    ))
}

impl DeviceStore {
    pub fn discard_retained_drafts(&self) {
        let mut state = self.0.lock().expect("device store mutex");
        state.retained_drafts = None;
        state.draft_basis = None;
        state.alarm_inputs.clear();
    }
    pub(super) fn expire_drafts_at(&self, serial: u64, deadline: Stamp) {
        let Some(runtime) = self.1.as_ref() else {
            return;
        };
        let weak = Arc::downgrade(&self.0);
        runtime.spawn(async move {
            loop {
                let delay = {
                    let Some(store) = weak.upgrade() else {
                        return;
                    };
                    let mut state = store.lock().expect("device store mutex");
                    expire(&mut state, serial, deadline, now())
                };
                let Some(delay) = delay else {
                    return;
                };
                tokio::time::sleep(delay).await;
            }
        });
    }
}

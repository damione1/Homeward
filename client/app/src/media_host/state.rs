use crate::media::{Artwork, Data, Field, Quantity};
use home_client_actions::capabilities::{
    CapabilityHandle, CapabilitySnapshot, CapabilitySubmission, CapabilityTracking,
};
use home_client_cache::ProjectionGeneration;
use home_client_model::api::EntityName;
use home_client_model::{api::Command, api_playback::ArtworkReference};
use home_client_session::Generation;
use std::{collections::BTreeMap, sync::Arc};
use tokio_util::sync::CancellationToken;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum Status {
    Hidden,
    Loading,
    Ready,
    Unsupported,
    Denied,
    Offline,
    Contract,
}
#[derive(Clone)]
pub(super) enum Picture {
    Absent,
    Loading,
    Ready(Arc<Artwork>),
    Denied,
    Expired,
    Failed,
}
#[derive(Clone, Default)]
pub(super) struct Draft {
    pub text: String,
    pub value: Option<Quantity>,
    pub error: String,
    pub editing: bool,
    pub gesture: bool,
}
pub(super) struct Pending {
    pub generation: i32,
    pub request: Command,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub(super) enum ChoiceKind {
    Source,
    Group,
}
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub(super) struct ChoiceId(pub String);
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub(super) struct ChoiceToken(pub i32);
#[derive(Clone)]
pub(super) struct Choice {
    pub kind: ChoiceKind,
    pub id: ChoiceId,
}
pub(super) struct State {
    pub generation: i32,
    pub entity: Option<EntityName>,
    pub view: Option<ProjectionGeneration>,
    pub foreground: bool,
    pub interactive: bool,
    pub active: bool,
    pub status: Status,
    pub source: Option<Generation>,
    pub data: Option<Arc<Data>>,
    pub picture: Picture,
    pub picture_id: i32,
    pub volume: Draft,
    pub position: Draft,
    pub pending: Option<Pending>,
    pub action: Option<CapabilityHandle>,
    pub progress: Option<Arc<CapabilitySnapshot>>,
    pub read_pending: bool,
    pub read_cancel: CancellationToken,
    pub notice: String,
    pub choices: BTreeMap<ChoiceToken, Choice>,
    pub next_choice: i32,
}
impl Default for State {
    fn default() -> Self {
        Self {
            generation: 1,
            entity: None,
            view: None,
            foreground: true,
            interactive: false,
            active: false,
            status: Status::Hidden,
            source: None,
            data: None,
            picture: Picture::Absent,
            picture_id: 0,
            volume: Draft::default(),
            position: Draft::default(),
            pending: None,
            action: None,
            progress: None,
            read_pending: false,
            read_cancel: CancellationToken::new(),
            notice: String::new(),
            choices: BTreeMap::new(),
            next_choice: 0,
        }
    }
}
impl State {
    pub fn draft(&self, field: Field) -> &Draft {
        match field {
            Field::Volume => &self.volume,
            Field::Position => &self.position,
        }
    }
    pub fn draft_mut(&mut self, field: Field) -> &mut Draft {
        match field {
            Field::Volume => &mut self.volume,
            Field::Position => &mut self.position,
        }
    }
    pub fn invalidate(&mut self, stop_tracking: bool) {
        self.generation = if self.generation > 0 {
            self.generation.checked_add(1).unwrap_or(0)
        } else {
            0
        };
        self.read_cancel.cancel();
        self.read_cancel = CancellationToken::new();
        self.pending = None;
        self.volume = Draft::default();
        self.position = Draft::default();
        if stop_tracking {
            if let Some(action) = self.action.take() {
                action.stop_tracking();
            }
            self.progress = None;
        }
    }
    pub fn clear_private(&mut self) {
        self.invalidate(true);
        self.data = None;
        self.picture = Picture::Absent;
        self.source = None;
        self.status = Status::Hidden;
        self.notice.clear();
        self.read_pending = false;
        self.choices.clear();
    }
    pub fn busy(&self) -> bool {
        if self.pending.is_some() {
            return true;
        }
        self.progress.as_ref().is_some_and(|snapshot| {
            if matches!(
                snapshot.tracking,
                CapabilityTracking::Complete | CapabilityTracking::Stopped
            ) {
                return false;
            }
            match snapshot.submission {
                CapabilitySubmission::Prepared
                | CapabilitySubmission::Sending
                | CapabilitySubmission::Uncertain => true,
                CapabilitySubmission::Accepted => snapshot
                    .operation
                    .as_ref()
                    .is_none_or(|operation| !operation.done()),
                CapabilitySubmission::NotSent
                | CapabilitySubmission::Rejected
                | CapabilitySubmission::Closed => false,
            }
        })
    }

    pub fn authorized(&self) -> bool {
        self.active
            && self.foreground
            && self.interactive
            && self.generation > 0
            && self.status == Status::Ready
            && self
                .data
                .as_ref()
                .is_some_and(|data| data.snapshot().can_prepare())
    }

    pub fn editable(&self) -> bool {
        self.authorized() && !self.busy()
    }
    pub fn admits(&self, token: i32, generation: i32) -> bool {
        token == 1 && generation == self.generation && self.editable()
    }
    pub fn apply(&mut self, data: Arc<Data>, source: Generation, picture: Picture) {
        let changed = self
            .data
            .as_ref()
            .is_none_or(|old| !old.same_controls(&data));
        if changed || self.status != Status::Ready {
            // Only an unsent intent is invalidated. An already admitted action
            // keeps its read-only tracking by command ID across topology changes.
            self.invalidate(false);
        }
        if changed {
            self.choices.clear();
            if let Some(capabilities) = data.snapshot().definition() {
                for (kind, options) in [
                    (ChoiceKind::Source, capabilities.sources()),
                    (ChoiceKind::Group, capabilities.groups()),
                ] {
                    for option in options {
                        let Some(token) = self.next_choice.checked_add(1) else {
                            self.generation = 0;
                            self.clear_private();
                            self.status = Status::Contract;
                            return;
                        };
                        self.next_choice = token;
                        self.choices.insert(
                            ChoiceToken(token),
                            Choice {
                                kind,
                                id: ChoiceId(option.id().as_str().to_owned()),
                            },
                        );
                    }
                }
            }
        }
        for field in [Field::Volume, Field::Position] {
            let draft = self.draft_mut(field);
            if !draft.editing {
                draft.value = data.observed(field);
                draft.text = draft.value.map(Quantity::text).unwrap_or_default();
                draft.error.clear();
            }
        }
        let same_picture = match &self.picture {
            Picture::Ready(a) => match &picture {
                Picture::Ready(b) => Arc::ptr_eq(a, b),
                Picture::Absent
                | Picture::Loading
                | Picture::Denied
                | Picture::Expired
                | Picture::Failed => false,
            },
            Picture::Absent
            | Picture::Loading
            | Picture::Denied
            | Picture::Expired
            | Picture::Failed => {
                std::mem::discriminant(&self.picture) == std::mem::discriminant(&picture)
            }
        };
        if !same_picture {
            let Some(id) = self.picture_id.checked_add(1) else {
                self.generation = 0;
                self.clear_private();
                self.status = Status::Contract;
                return;
            };
            self.picture_id = id;
        }
        self.picture = picture;
        self.data = Some(data);
        self.source = Some(source);
        self.status = Status::Ready;
        self.notice.clear();
    }
    pub fn artwork_reference(&self) -> Option<(&ArtworkReference, Picture)> {
        self.data
            .as_ref()?
            .snapshot()
            .observation()?
            .artwork()
            .map(|reference| (reference, self.picture.clone()))
    }
}

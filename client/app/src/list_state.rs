//! Private list presentation state. Names, ordinals and Slint delegate positions
//! never become identities. The owner supplies complete Watch-fenced captures.
use home_client_actions::lists::{
    ItemTarget, ListWriteIntent, ListWriteOutcome, MoveDestination, PreparedListMutation,
};
use home_client_cache::lists::{CapturedRows, ListReadMetadata, ListReadSnapshot};
use home_client_model::lists::{ItemKey, ItemRevision, ListId, ListRevision};
use home_wire_contract::home::lists::v1 as wire;
use std::{collections::BTreeMap, sync::Arc};

#[derive(Clone, Copy, PartialEq, Eq)]
pub(crate) enum Freshness {
    Empty,
    Syncing,
    Online,
    Offline,
}
#[derive(Clone, Copy, PartialEq, Eq)]
pub(crate) enum DraftPhase {
    Editing,
    Conflict,
    Unknown,
}
#[derive(Clone)]
pub(crate) struct Draft {
    pub text: String,
    pub base: ItemTarget,
    pub phase: DraftPhase,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Refusal {
    Stale,
    Busy,
    Invalid,
    Capacity,
}
pub(crate) enum Write {
    Create(String),
    Rename(String),
    DeleteList,
    Add(String),
    Save,
    Complete(bool),
    DeleteItem,
    MoveFirst,
    MoveUp,
    MoveDown,
}
#[derive(Clone, Copy, PartialEq, Eq)]
enum WriteKind {
    Create,
    Rename,
    DeleteList,
    Add,
    Save,
    Complete,
    DeleteItem,
    Move,
}
struct Pending {
    id: u64,
    kind: WriteKind,
    item: Option<ItemKey>,
}
pub(crate) struct Prepared {
    pub mutation: PreparedListMutation,
    pub context: u64,
    pub id: u64,
}

pub(crate) struct State {
    pub context: u64,
    pub generation: i32,
    pub privacy_generation: i32,
    pub active: bool,
    pub foreground: bool,
    pub blocked: bool,
    pub catalog: Option<Arc<ListReadSnapshot>>,
    pub catalog_phase: Freshness,
    pub items: Option<Arc<ListReadSnapshot>>,
    pub item_phase: Freshness,
    pub selected_list: Option<ListId>,
    pub selected_item: Option<ItemKey>,
    pub drafts: BTreeMap<ItemKey, Draft>,
    pub notice: &'static str,
    pub read_notice: &'static str,
    pub item_tokens: Arc<Vec<i32>>,
    pub catalog_tokens: Arc<Vec<i32>>,
    tokens: BTreeMap<ItemKey, i32>,
    list_tokens: BTreeMap<ListId, i32>,
    next_token: i32,
    next_write: u64,
    pending: Option<Pending>,
    reload_draft: bool,
    resume_list: Option<ListId>,
}
impl Default for State {
    fn default() -> Self {
        Self {
            context: 1,
            generation: 1,
            privacy_generation: 1,
            active: false,
            foreground: true,
            blocked: false,
            catalog: None,
            catalog_phase: Freshness::Empty,
            items: None,
            item_phase: Freshness::Empty,
            selected_list: None,
            selected_item: None,
            drafts: BTreeMap::new(),
            notice: "",
            read_notice: "",
            item_tokens: Arc::new(Vec::new()),
            catalog_tokens: Arc::new(Vec::new()),
            tokens: BTreeMap::new(),
            list_tokens: BTreeMap::new(),
            next_token: 0,
            next_write: 0,
            pending: None,
            reload_draft: false,
            resume_list: None,
        }
    }
}
fn key(row: &wire::ItemSnapshot) -> Option<ItemKey> {
    Some(ItemKey::new(
        row.list_id.value.parse().ok()?,
        row.uid.value.parse().ok()?,
    ))
}
impl State {
    pub fn clear(&mut self) {
        self.active = false;
        self.blocked = false;
        self.catalog = None;
        self.items = None;
        self.catalog_phase = Freshness::Empty;
        self.item_phase = Freshness::Empty;
        self.selected_list = None;
        self.selected_item = None;
        self.drafts.clear();
        self.tokens.clear();
        self.list_tokens.clear();
        self.pending = None;
        self.item_tokens = Arc::new(Vec::new());
        self.catalog_tokens = Arc::new(Vec::new());
        self.notice = "";
        self.read_notice = "";
        self.reload_draft = false;
        self.resume_list = None;
        // Exhaustion closes admission rather than reusing an old context.
        self.context = if self.context == 0 {
            0
        } else {
            self.context.checked_add(1).unwrap_or(0)
        };
        self.generation = if self.generation == 0 {
            0
        } else {
            self.generation.checked_add(1).unwrap_or(0)
        };
        self.privacy_generation = self.generation;
    }
    fn advance(&mut self) -> Result<(), Refusal> {
        if self.generation == 0 {
            return Err(Refusal::Capacity);
        }
        self.generation = self.generation.checked_add(1).ok_or(Refusal::Capacity)?;
        Ok(())
    }
    fn token(&mut self) -> Result<i32, Refusal> {
        self.next_token = self.next_token.checked_add(1).ok_or(Refusal::Capacity)?;
        Ok(self.next_token)
    }
    pub fn busy(&self) -> bool {
        self.pending.is_some()
    }
    pub fn reset_for_resync(&mut self) {
        let selected = self.selected_list.clone();
        let uncertain = self.pending.is_some();
        self.clear();
        self.resume_list = selected;
        self.notice = if uncertain {
            "Flux réinitialisé pendant une modification : résultat inconnu. Les données privées ont été retirées ; aucune écriture n’est rejouée."
        } else {
            "Flux réinitialisé : les données et brouillons ont été retirés pour vérifier les autorisations. Aucune écriture n’est rejouée."
        };
    }
    pub fn admits(&self, context: u64, id: u64) -> bool {
        self.context != 0
            && self.context == context
            && self.active
            && self.foreground
            && self.pending.as_ref().is_some_and(|pending| {
                pending.id == id
                    && matches!(self.catalog_phase, Freshness::Online | Freshness::Syncing)
                    && (pending.kind == WriteKind::Create
                        || matches!(self.item_phase, Freshness::Online | Freshness::Syncing))
            })
    }
    fn ready(&self, generation: i32) -> Result<(), Refusal> {
        if self.context == 0 || generation != self.generation || !self.active || !self.foreground {
            return Err(Refusal::Stale);
        }
        if self.pending.is_some() {
            return Err(Refusal::Busy);
        }
        Ok(())
    }
    pub fn publish_catalog(&mut self, snapshot: Arc<ListReadSnapshot>) -> Result<(), Refusal> {
        let CapturedRows::Catalog(rows) = &snapshot.rows else {
            return Err(Refusal::Invalid);
        };
        let mut tokens = Vec::with_capacity(rows.len());
        let mut admitted = BTreeMap::new();
        for row in rows {
            let id: ListId = row.id.value.parse().map_err(|_| Refusal::Invalid)?;
            let token = match self.list_tokens.get(&id) {
                Some(token) => *token,
                None => self.token()?,
            };
            tokens.push(token);
            admitted.insert(id, token);
        }
        self.list_tokens = admitted;
        if let Some(id) = self.resume_list.take()
            && self.list_tokens.contains_key(&id)
        {
            self.selected_list = Some(id);
            self.item_phase = Freshness::Syncing;
            self.context = self.context.checked_add(1).ok_or(Refusal::Capacity)?;
        }
        self.drafts
            .retain(|key, _| self.list_tokens.contains_key(key.list()));
        self.tokens
            .retain(|key, _| self.list_tokens.contains_key(key.list()));
        if self
            .selected_list
            .as_ref()
            .is_some_and(|id| !self.list_tokens.contains_key(id))
        {
            self.selected_list = None;
            self.selected_item = None;
            self.items = None;
            self.item_tokens = Arc::new(Vec::new());
            self.item_phase = Freshness::Empty;
            self.context = self.context.checked_add(1).ok_or(Refusal::Capacity)?;
            self.pending = None;
        }
        self.catalog_tokens = Arc::new(tokens);
        self.catalog = Some(snapshot);
        self.catalog_phase = Freshness::Online;
        self.clear_read_notice_if_online();
        self.advance()
    }
    pub fn select_list(&mut self, token: i32, generation: i32) -> Result<(), Refusal> {
        self.ready(generation)?;
        if self.catalog_phase != Freshness::Online {
            return Err(Refusal::Stale);
        }
        let id = self
            .list_tokens
            .iter()
            .find(|(_, value)| **value == token)
            .map(|(id, _)| id.clone())
            .ok_or(Refusal::Stale)?;
        self.context = self.context.checked_add(1).ok_or(Refusal::Capacity)?;
        self.selected_list = Some(id);
        self.selected_item = None;
        self.items = None;
        self.tokens.clear();
        self.item_tokens = Arc::new(Vec::new());
        self.item_phase = Freshness::Syncing;
        self.notice = "";
        self.advance()
    }
    pub fn publish_items(&mut self, snapshot: Arc<ListReadSnapshot>) -> Result<(), Refusal> {
        let CapturedRows::Items { list, rows } = &snapshot.rows else {
            return Err(Refusal::Invalid);
        };
        if self.selected_list.as_ref() != Some(list) {
            return Err(Refusal::Stale);
        }
        let ListReadMetadata::List(metadata) = &snapshot.metadata else {
            return Err(Refusal::Invalid);
        };
        if metadata.id.value != list.as_str() {
            return Err(Refusal::Invalid);
        }
        let mut tokens = Vec::with_capacity(rows.len());
        let mut admitted = BTreeMap::new();
        for row in rows {
            let key = key(row).ok_or(Refusal::Invalid)?;
            let token = match self.tokens.get(&key) {
                Some(token) => *token,
                None => self.token()?,
            };
            admitted.insert(key, token);
            tokens.push(token);
        }
        self.tokens = admitted;
        self.items = Some(snapshot);
        self.item_tokens = Arc::new(tokens);
        self.item_phase = Freshness::Online;
        self.clear_read_notice_if_online();
        if let Some(selected) = self.selected_item.clone() {
            if let Some((row, target)) = self.current_item(&selected) {
                let text = row.value.summary.clone();
                if self.reload_draft {
                    self.drafts.remove(&selected);
                    self.reload_draft = false;
                } else if let Some(draft) = self.drafts.get_mut(&selected)
                    && !self
                        .pending
                        .as_ref()
                        .is_some_and(|pending| pending.item.as_ref() == Some(&selected))
                    && (draft.base.item_revision() != target.item_revision()
                        || draft.base.list_revision() != target.list_revision())
                {
                    // Even equal text does not prove which actor committed it.
                    if draft.phase == DraftPhase::Editing && draft.text != text {
                        draft.phase = DraftPhase::Conflict;
                    }
                }
            } else {
                self.selected_item = None;
                self.drafts.remove(&selected);
            }
        }
        self.advance()
    }
    pub fn current_item(&self, key: &ItemKey) -> Option<(Arc<wire::ItemSnapshot>, ItemTarget)> {
        let snapshot = self.items.as_ref()?;
        let CapturedRows::Items { list, rows } = &snapshot.rows else {
            return None;
        };
        if list != key.list() {
            return None;
        }
        let row = rows
            .iter()
            .find(|row| row.uid.value == key.uid().as_str())?
            .clone();
        let ListReadMetadata::List(metadata) = &snapshot.metadata else {
            return None;
        };
        let list = ListRevision::new(list.clone(), metadata.revision.number).ok()?;
        let revision = ItemRevision::new(key.clone(), row.revision.number).ok()?;
        Some((row, ItemTarget::new(list, revision).ok()?))
    }
    pub fn selected_target(&self) -> Option<(Arc<wire::ItemSnapshot>, ItemTarget)> {
        self.current_item(self.selected_item.as_ref()?)
    }
    fn clear_read_notice_if_online(&mut self) {
        if self.catalog_phase == Freshness::Online
            && (self.items.is_none() || self.item_phase == Freshness::Online)
        {
            self.read_notice = "";
        }
    }
    pub fn selected_list_token(&self) -> i32 {
        self.selected_list
            .as_ref()
            .and_then(|id| self.list_tokens.get(id))
            .copied()
            .unwrap_or(0)
    }
    pub fn select_item(&mut self, token: i32, generation: i32) -> Result<(), Refusal> {
        self.ready(generation)?;
        let snapshot = self.items.as_ref().ok_or(Refusal::Stale)?;
        let CapturedRows::Items { rows, .. } = &snapshot.rows else {
            return Err(Refusal::Invalid);
        };
        let index = self
            .item_tokens
            .iter()
            .position(|value| *value == token)
            .ok_or(Refusal::Stale)?;
        self.selected_item = Some(key(&rows[index]).ok_or(Refusal::Invalid)?);
        self.advance()
    }
    pub fn edit(&mut self, token: i32, generation: i32, text: String) -> Result<(), Refusal> {
        self.ready(generation)?;
        if self.item_phase != Freshness::Online || text.len() > 512 {
            return Err(Refusal::Invalid);
        }
        let selected = self.selected_item.clone().ok_or(Refusal::Stale)?;
        if self.tokens.get(&selected) != Some(&token) {
            return Err(Refusal::Stale);
        }
        let (_, base) = self.current_item(&selected).ok_or(Refusal::Stale)?;
        if self.drafts.len() >= 128 && !self.drafts.contains_key(&selected) {
            return Err(Refusal::Capacity);
        }
        self.drafts
            .entry(selected)
            .and_modify(|draft| draft.text = text.clone())
            .or_insert(Draft {
                text,
                base,
                phase: DraftPhase::Editing,
            });
        Ok(())
    }
    pub fn resolve(&mut self, generation: i32) -> Result<(), Refusal> {
        self.ready(generation)?;
        if self.item_phase != Freshness::Online {
            return Err(Refusal::Stale);
        }
        let selected = self.selected_item.clone().ok_or(Refusal::Stale)?;
        let (_, base) = self.current_item(&selected).ok_or(Refusal::Stale)?;
        let draft = self.drafts.get_mut(&selected).ok_or(Refusal::Stale)?;
        if draft.phase != DraftPhase::Conflict {
            return Err(Refusal::Stale);
        }
        draft.base = base;
        draft.phase = DraftPhase::Editing;
        self.notice = "Brouillon conservé sur la révision relue. Enregistrez pour proposer cette modification.";
        Ok(())
    }
    pub fn reload(&mut self, generation: i32) -> Result<(), Refusal> {
        self.ready(generation)?;
        self.reload_draft = true;
        self.item_phase = Freshness::Syncing;
        Ok(())
    }
    pub fn prepare(&mut self, generation: i32, write: Write) -> Result<Prepared, Refusal> {
        self.ready(generation)?;
        if self.catalog_phase != Freshness::Online {
            return Err(Refusal::Stale);
        }
        if !matches!(write, Write::Create(_)) && self.item_phase != Freshness::Online {
            return Err(Refusal::Stale);
        }
        if matches!(
            write,
            Write::Save
                | Write::Complete(_)
                | Write::DeleteItem
                | Write::MoveFirst
                | Write::MoveUp
                | Write::MoveDown
        ) && self
            .selected_item
            .as_ref()
            .and_then(|key| self.drafts.get(key))
            .is_some_and(|draft| draft.phase != DraftPhase::Editing)
        {
            return Err(Refusal::Stale);
        }
        let list_revision = || -> Result<ListRevision, Refusal> {
            if self.item_phase != Freshness::Online {
                return Err(Refusal::Stale);
            }
            let ListReadMetadata::List(list) = &self.items.as_ref().ok_or(Refusal::Stale)?.metadata
            else {
                return Err(Refusal::Stale);
            };
            ListRevision::new(
                list.id.value.parse().map_err(|_| Refusal::Invalid)?,
                list.revision.number,
            )
            .map_err(|_| Refusal::Invalid)
        };
        let target = || {
            self.selected_target()
                .map(|(_, target)| target)
                .ok_or(Refusal::Stale)
        };
        let (kind, intent) = match write {
            Write::Create(name) if self.catalog_phase == Freshness::Online => {
                (WriteKind::Create, ListWriteIntent::Create { name })
            }
            Write::Create(_) => return Err(Refusal::Stale),
            Write::Rename(name) => (
                WriteKind::Rename,
                ListWriteIntent::Rename {
                    revision: list_revision()?,
                    name,
                },
            ),
            Write::DeleteList => (
                WriteKind::DeleteList,
                ListWriteIntent::DeleteList {
                    revision: list_revision()?,
                },
            ),
            Write::Add(summary) => (
                WriteKind::Add,
                ListWriteIntent::Add {
                    revision: list_revision()?,
                    payload: wire::ItemPayload {
                        summary,
                        completed: Some(false),
                        ..Default::default()
                    },
                },
            ),
            Write::Save => {
                let selected = self.selected_item.as_ref().ok_or(Refusal::Stale)?;
                let (row, _) = self.current_item(selected).ok_or(Refusal::Stale)?;
                let draft = self.drafts.get(selected).ok_or(Refusal::Stale)?;
                if self.item_phase != Freshness::Online || draft.phase != DraftPhase::Editing {
                    return Err(Refusal::Stale);
                }
                let mut payload = row.value.as_option().ok_or(Refusal::Invalid)?.clone();
                payload.summary = draft.text.clone();
                (
                    WriteKind::Save,
                    ListWriteIntent::Edit {
                        target: draft.base.clone(),
                        payload,
                    },
                )
            }
            Write::Complete(completed) => (
                WriteKind::Complete,
                ListWriteIntent::Complete {
                    target: target()?,
                    completed,
                },
            ),
            Write::DeleteItem => (
                WriteKind::DeleteItem,
                ListWriteIntent::DeleteItem { target: target()? },
            ),
            Write::MoveFirst | Write::MoveUp | Write::MoveDown => {
                let target = target()?;
                let CapturedRows::Items { rows, .. } =
                    &self.items.as_ref().ok_or(Refusal::Stale)?.rows
                else {
                    return Err(Refusal::Stale);
                };
                let index = rows
                    .iter()
                    .position(|row| row.uid.value == target.key().uid().as_str())
                    .ok_or(Refusal::Stale)?;
                let after = match write {
                    Write::MoveFirst => None,
                    Write::MoveUp => {
                        if index < 2 {
                            None
                        } else {
                            key(&rows[index - 2])
                        }
                    }
                    Write::MoveDown => {
                        if index + 1 >= rows.len() {
                            return Err(Refusal::Invalid);
                        }
                        key(&rows[index + 1])
                    }
                    Write::Create(_)
                    | Write::Rename(_)
                    | Write::DeleteList
                    | Write::Add(_)
                    | Write::Save
                    | Write::Complete(_)
                    | Write::DeleteItem => unreachable!(),
                };
                (
                    WriteKind::Move,
                    ListWriteIntent::Move {
                        target,
                        destination: after.map_or(MoveDestination::AtStart, MoveDestination::After),
                    },
                )
            }
        };
        let mutation = PreparedListMutation::new(intent).map_err(|_| Refusal::Invalid)?;
        self.next_write = self.next_write.checked_add(1).ok_or(Refusal::Capacity)?;
        self.pending = Some(Pending {
            id: self.next_write,
            kind,
            item: self.selected_item.clone(),
        });
        self.notice = "Modification en attente du serveur.";
        Ok(Prepared {
            mutation,
            context: self.context,
            id: self.next_write,
        })
    }
    pub fn finish(&mut self, context: u64, id: u64, outcome: ListWriteOutcome) {
        if self.context != context
            || !self
                .pending
                .as_ref()
                .is_some_and(|pending| pending.id == id)
        {
            return;
        }
        let pending = self.pending.take().expect("checked pending write");
        match outcome {
            ListWriteOutcome::Committed(_) => {
                self.notice = "Modification confirmée par le serveur ; actualisation des listes.";
                if matches!(pending.kind, WriteKind::Save | WriteKind::DeleteItem)
                    && let Some(item) = pending.item
                {
                    self.drafts.remove(&item);
                }
                if pending.kind == WriteKind::DeleteList {
                    if let Some(list) = &self.selected_list {
                        self.drafts.retain(|key, _| key.list() != list);
                    }
                    self.selected_list = None;
                    self.selected_item = None;
                    self.items = None;
                    self.item_phase = Freshness::Empty;
                }
            }
            ListWriteOutcome::Conflict => {
                self.notice = "Conflit : relisez la version serveur, puis choisissez explicitement de conserver votre brouillon.";
                if let Some(key) = pending.item
                    && let Some(draft) = self.drafts.get_mut(&key)
                {
                    draft.phase = DraftPhase::Conflict;
                }
            }
            ListWriteOutcome::CommitUnknown(_) => {
                self.notice = "Résultat inconnu. L’écriture ne sera pas renvoyée. Rechargez la version serveur.";
                if let Some(key) = pending.item
                    && let Some(draft) = self.drafts.get_mut(&key)
                {
                    draft.phase = DraftPhase::Unknown;
                }
            }
            ListWriteOutcome::CommittedResponseWithheld | ListWriteOutcome::AuthorityTerminated => {
                self.clear();
                self.notice = "L’accès a changé. Les données privées ont été retirées ; aucune écriture n’est rejouée.";
            }
            ListWriteOutcome::Rejected(_) => {
                self.notice =
                    "Modification refusée par le serveur. Aucune sauvegarde n’est annoncée."
            }
            ListWriteOutcome::NotSent(_) => {
                self.notice = "Modification non envoyée : le contexte a changé."
            }
        }
    }
}

#[cfg(test)]
pub(crate) mod tests;

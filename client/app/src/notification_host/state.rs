use super::*;
use std::collections::BTreeMap;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum Stage {
    Disconnected,
    WaitingParent,
    Unregistered,
    Ready,
    Busy,
    Uncertain,
    Offline,
    Denied,
    Unsupported,
    Expired,
    Revoked,
    Invalid,
    Suspended,
}
impl Stage {
    pub fn label(self) -> &'static str {
        match self {
            Self::Disconnected => "Connectez-vous pour ouvrir les notifications",
            Self::WaitingParent => "Inscrivez d’abord cette installation dans le compagnon",
            Self::Unregistered => "Aucune destination inscrite",
            Self::Ready => "Réception locale au premier plan · serveur de développement isolé",
            Self::Busy => "Opération en cours",
            Self::Uncertain => "Résultat incertain · résolution par lecture",
            Self::Offline => "Réception suspendue · serveur indisponible",
            Self::Denied => "Accès retiré",
            Self::Unsupported => "Service non disponible sur ce serveur ou cet appareil",
            Self::Expired => "Destination expirée",
            Self::Revoked => "Destination révoquée",
            Self::Invalid => "Réponse non admise · réception arrêtée",
            Self::Suspended => "Application suspendue · aucune réception en arrière-plan",
        }
    }
}
#[derive(Clone, Copy)]
pub(super) enum Draft {
    Register,
    Permission,
    Enable,
    Revoke,
}
impl Draft {
    pub fn label(self) -> &'static str {
        match self {
            Self::Register => {
                "Inscrire une destination privée pour cette installation et cette session ? La présentation native restera désactivée."
            }
            Self::Permission => match os::backend() {
                os::Backend::MacOs => {
                    "Demander à macOS l’autorisation d’afficher des notifications ? Ce choix n’active pas la présentation dans Homeward."
                }
                os::Backend::Android => {
                    "Configurer le canal Android et demander l’autorisation si nécessaire ? Ce choix n’active pas la présentation dans Homeward."
                }
                os::Backend::Unavailable => {
                    "Les notifications natives ne sont pas disponibles sur cet appareil."
                }
            },
            Self::Enable => {
                "Activer la présentation native des nouveaux messages encore valides ? La réception nécessite Homeward au premier plan."
            }
            Self::Revoke => {
                "Révoquer cette destination et retirer son inbox ? La présentation locale est déjà arrêtée."
            }
        }
    }
}
#[derive(Clone)]
pub(super) enum Intent {
    Register,
    Permission,
    Enable,
    Disable,
    Revoke,
    Row(admit::DeliveryId, NotificationRowAction),
}
#[derive(Clone)]
pub(super) enum Pending {
    Register(Box<wire::RegisterDestinationRequest>),
    Update(Box<wire::UpdateDestinationRequest>),
    Revoke(Box<wire::RevokeDestinationRequest>),
    Receipt(Box<wire::AcknowledgeDeliveryRequest>),
}
impl Pending {
    pub fn nonce(&self) -> &str {
        match self {
            Self::Register(r) => &r.request_id,
            Self::Update(r) => &r.request_id,
            Self::Revoke(r) => &r.request_id,
            Self::Receipt(r) => &r.request_id,
        }
    }
    pub fn destination_selector(&self) -> wire::GetDestinationRequest {
        let selector = match self {
            Self::Register(r) => {
                wire::get_destination_request::Selector::RegistrationRequestId(r.request_id.clone())
            }
            Self::Update(r) => {
                wire::get_destination_request::Selector::Id(Box::new((*r.id).clone()))
            }
            Self::Revoke(r) => {
                wire::get_destination_request::Selector::Id(Box::new((*r.id).clone()))
            }
            Self::Receipt(r) => {
                wire::get_destination_request::Selector::Id(Box::new((*r.destination_id).clone()))
            }
        };
        wire::GetDestinationRequest {
            selector: Some(selector),
            ..Default::default()
        }
    }
}
#[derive(Clone)]
pub(super) enum Job {
    Read,
    Intent(Intent),
    Cleanup(Vec<os::PresentationId>),
}
pub(super) struct Grant {
    pub tag: i32,
    pub response: wire::ResolveOpenResponse,
    pub requested_at: Instant,
    pub message: wire::Delivery,
    pub parent_stop: CancellationToken,
    pub native_generation: home_client_session::Generation,
}
#[derive(Clone, Copy)]
pub(super) struct RowKey {
    pub token: i32,
    pub generation: i32,
    revision: u64,
}
#[derive(Clone, PartialEq, Eq)]
pub(super) struct OpenBinding {
    scope: u64,
    identity: (AccountId, SessionId),
    parent: parent_admit::InstallationId,
    destination: admit::DestinationId,
}
pub(super) struct NativeOpen {
    binding: OpenBinding,
    id: admit::DeliveryId,
    until: Instant,
}
pub(super) struct State {
    pub scope: u64,
    pub fence: u64,
    pub generation: i32,
    pub active: bool,
    pub visible: bool,
    pub foreground: bool,
    pub running: bool,
    pub requested: bool,
    pub stage: Stage,
    pub notice: String,
    pub validated: Option<home_client_session::Generation>,
    pub identity: Option<(AccountId, SessionId)>,
    pub context: Option<companion::Context>,
    pub parent: Option<companion::Installation>,
    pub destination: Option<wire::Destination>,
    pub entries: Vec<wire::Delivery>,
    pub(super) row_keys: BTreeMap<admit::DeliveryId, RowKey>,
    pub(super) next_row_token: i32,
    pub inbox_revision: u64,
    pub permission: wire::PermissionSnapshot,
    pub draft: Option<Draft>,
    pub intent: Option<Intent>,
    pub pending: Option<Pending>,
    pub resolution_attempted: bool,
    pub local_stopped: bool,
    pub native: Option<os::PresentationScope>,
    pub(super) native_open: Option<NativeOpen>,
    pub native_stop: CancellationToken,
    pub cleanup: Vec<os::PresentationId>,
    pub cancel: CancellationToken,
    pub next_poll: Instant,
    pub grant: Option<Grant>,
    pub enable_fence: Option<u64>,
    pub next_grant: i32,
    pub native_parent_stop: Option<CancellationToken>,
}
impl Default for State {
    fn default() -> Self {
        Self {
            scope: 1,
            fence: 1,
            generation: 1,
            active: false,
            visible: false,
            foreground: true,
            running: false,
            requested: false,
            stage: Stage::Disconnected,
            notice: String::new(),
            validated: None,
            identity: None,
            context: None,
            parent: None,
            destination: None,
            entries: Vec::new(),
            row_keys: BTreeMap::new(),
            next_row_token: 0,
            inbox_revision: 0,
            permission: native::permission(Err(os::Error::Unsupported)),
            draft: None,
            intent: None,
            pending: None,
            resolution_attempted: false,
            local_stopped: true,
            native: None,
            native_open: None,
            native_stop: CancellationToken::new(),
            cleanup: Vec::new(),
            cancel: CancellationToken::new(),
            next_poll: Instant::now(),
            grant: None,
            enable_fence: None,
            next_grant: 0,
            native_parent_stop: None,
        }
    }
}
impl State {
    pub fn open_binding(&self) -> Option<OpenBinding> {
        let destination = self.destination.as_ref()?;
        if !self.active
            || self.local_stopped
            || destination.state != wire::DestinationState::Active
            || !destination.native_delivery
        {
            return None;
        }
        Some(OpenBinding {
            scope: self.scope,
            identity: self.identity.clone()?,
            parent: parent_admit::InstallationId::parse(&self.parent.as_ref()?.id.value).ok()?,
            destination: admit::DestinationId::parse(&destination.id.value).ok()?,
        })
    }
    pub fn capture_native_open(
        &mut self,
        binding: OpenBinding,
        id: admit::DeliveryId,
        now: Instant,
    ) -> bool {
        if self
            .native_open
            .as_ref()
            .is_some_and(|value| now >= value.until)
        {
            self.native_open = None;
        }
        if self.open_binding().as_ref() != Some(&binding) || self.native_open.is_some() {
            return false;
        }
        self.native_open = Some(NativeOpen {
            binding,
            id,
            until: now + os::MAX_SCOPE_FRESHNESS,
        });
        self.requested = true;
        self.next_poll = now;
        true
    }
    pub fn pending_native_open(&mut self, now: Instant) -> Option<admit::DeliveryId> {
        if !self.foreground {
            return None;
        }
        let value = self.native_open.as_ref()?;
        if now >= value.until || self.open_binding().as_ref() != Some(&value.binding) {
            self.native_open = None;
            return None;
        }
        Some(value.id.clone())
    }
    pub fn finish_native_open(&mut self, id: &admit::DeliveryId) {
        if self
            .native_open
            .as_ref()
            .is_some_and(|value| &value.id == id)
        {
            self.native_open = None;
        }
    }
    pub fn bump(&mut self) {
        if let Some(next) = self.generation.checked_add(1) {
            self.generation = next;
            if self.sync_row_keys() {
                return;
            }
        }
        self.active = false;
        self.stage = Stage::Invalid;
        self.row_keys.clear();
        self.cancel.cancel();
        self.stop_native();
    }
    fn sync_row_keys(&mut self) -> bool {
        if self.entries.len() > admit::MAX_ENTRIES {
            return false;
        }
        let current = self
            .entries
            .iter()
            .map(|entry| admit::DeliveryId::parse(&entry.id.value).map(|id| (id, entry.revision)))
            .collect::<Result<BTreeMap<_, _>, _>>();
        let Ok(current) = current else {
            return false;
        };
        if current.len() != self.entries.len() {
            return false;
        }
        self.row_keys.retain(|id, _| current.contains_key(id));
        for (id, revision) in current {
            if let Some(key) = self.row_keys.get_mut(&id) {
                // An unchanged poll or a reorder does not recycle the kit's
                // identity snapshot or cancel a pending dismiss animation.
                if key.revision != revision {
                    key.revision = revision;
                    key.generation = self.generation;
                }
            } else {
                let Some(token) = self.next_row_token.checked_add(1) else {
                    return false;
                };
                self.next_row_token = token;
                self.row_keys.insert(
                    id,
                    RowKey {
                        token,
                        generation: self.generation,
                        revision,
                    },
                );
            }
        }
        true
    }
    pub fn row_key(&self, id: &admit::DeliveryId) -> Option<RowKey> {
        self.row_keys.get(id).copied()
    }
    pub fn stop_native(&mut self) {
        self.native_parent_stop = None;
        self.native_stop.cancel();
        self.native_stop = CancellationToken::new();
        if let Some(scope) = self.native.take() {
            for id in scope.close() {
                if !self.cleanup.contains(&id) {
                    self.cleanup.push(id);
                }
            }
        }
    }
    pub fn interrupt(&mut self) {
        self.stop_native();
        self.cancel.cancel();
        self.cancel = CancellationToken::new();
        self.fence = self.fence.saturating_add(1);
        self.draft = None;
        self.grant = None;
        self.bump();
        self.row_keys.clear();
        if self.fence == u64::MAX {
            self.active = false;
            self.stage = Stage::Invalid;
        }
    }
    pub fn clear(&mut self) {
        self.native_open = None;
        self.interrupt();
        self.active = false;
        self.validated = None;
        self.identity = None;
        self.context = None;
        self.parent = None;
        self.destination = None;
        self.entries.clear();
        self.row_keys.clear();
        self.inbox_revision = 0;
        self.pending = None;
        self.intent = None;
        self.local_stopped = true;
        self.requested = false;
        self.running = false;
        self.stage = Stage::Disconnected;
        self.notice.clear();
        self.permission = native::permission(Err(os::Error::Unsupported));
    }
    pub fn invalid(&mut self) {
        self.native_open = None;
        self.interrupt();
        self.local_stopped = true;
        self.requested = false;
        self.intent = None;
        self.entries.clear();
        self.row_keys.clear();
        self.stage = Stage::Invalid;
        self.notice = "Réponse refusée ; aucune présentation ni ouverture autorisée.".into();
    }
    pub fn expire(&mut self) -> bool {
        let now = now();
        let expired = self
            .entries
            .iter()
            .filter(|e| !fresh(e, &now))
            .filter_map(|e| os::PresentationId::parse(&e.id.value).ok())
            .collect::<Vec<_>>();
        if expired.is_empty() {
            return false;
        }
        for id in expired {
            if let Some(scope) = &self.native {
                scope.retire(&id);
            }
            if !self.cleanup.contains(&id) {
                self.cleanup.push(id);
            }
        }
        self.entries.retain(|e| fresh(e, &now));
        self.bump();
        true
    }
    pub fn action(&mut self, generation: i32, action: NotificationAction) {
        if generation != self.generation || !self.active || !self.visible || !self.foreground {
            return;
        }
        if action == NotificationAction::Disable {
            self.local_stopped = true;
            self.interrupt();
            self.intent = Some(Intent::Disable);
            self.requested = true;
            self.bump();
            return;
        }
        if self.running {
            return;
        }
        match action {
            NotificationAction::Refresh => {
                self.requested = true;
                self.resolution_attempted = false;
                self.draft = None;
            }
            NotificationAction::Register => {
                if self
                    .destination
                    .as_ref()
                    .is_none_or(|d| d.state != wire::DestinationState::Active)
                    && self.parent.is_some()
                    && self.pending.is_none()
                {
                    self.draft = Some(Draft::Register);
                }
            }
            NotificationAction::Permission => {
                self.draft = Some(Draft::Permission);
            }
            NotificationAction::Enable => {
                if self
                    .destination
                    .as_ref()
                    .is_some_and(|d| d.state == wire::DestinationState::Active)
                    && admit::can_present(&self.permission)
                {
                    self.draft = Some(Draft::Enable);
                }
            }
            NotificationAction::Revoke => {
                if self
                    .destination
                    .as_ref()
                    .is_some_and(|d| d.state == wire::DestinationState::Active)
                {
                    self.local_stopped = true;
                    self.stop_native();
                    self.draft = Some(Draft::Revoke);
                }
            }
            NotificationAction::Confirm => {
                if let Some(draft) = self.draft.take() {
                    self.intent = Some(match draft {
                        Draft::Register => Intent::Register,
                        Draft::Permission => Intent::Permission,
                        Draft::Enable => Intent::Enable,
                        Draft::Revoke => Intent::Revoke,
                    });
                }
            }
            NotificationAction::Cancel => {
                self.draft = None;
            }
            NotificationAction::Disable => unreachable!("withdrawal handled before the busy gate"),
        }
        self.bump();
    }
    pub fn row_action(&mut self, token: i32, generation: i32, action: NotificationRowAction) {
        if !self.active
            || !self.visible
            || !self.foreground
            || self.running
            || self.draft.is_some()
            || self.pending.is_some()
        {
            return;
        }
        let Some((id, _)) = self
            .row_keys
            .iter()
            .find(|(_, key)| key.token == token && key.generation == generation)
        else {
            return;
        };
        let Some(entry) = self
            .entries
            .iter()
            .find(|entry| entry.id.value == id.as_str())
        else {
            return;
        };
        if !fresh(entry, &now()) {
            return;
        }
        self.intent = Some(Intent::Row(id.clone(), action));
        self.bump();
    }
    pub fn plan(&mut self) -> Option<Job> {
        if self.running {
            return None;
        }
        if !self.cleanup.is_empty() {
            self.running = true;
            return Some(Job::Cleanup(std::mem::take(&mut self.cleanup)));
        }
        if !self.active || !self.foreground {
            return None;
        }
        if self.pending.is_some() {
            if self.requested && !self.resolution_attempted {
                self.resolution_attempted = true;
                self.requested = false;
                self.running = true;
                return Some(Job::Read);
            }
            return None;
        }
        if let Some(intent) = self.intent.take() {
            self.stage = Stage::Busy;
            self.running = true;
            return Some(Job::Intent(intent));
        }
        if self.requested
            || (self
                .destination
                .as_ref()
                .is_some_and(|d| d.state == wire::DestinationState::Active)
                && Instant::now() >= self.next_poll)
        {
            self.requested = false;
            self.running = true;
            return Some(Job::Read);
        }
        None
    }
}
pub(super) fn fresh(entry: &wire::Delivery, now: &Timestamp) -> bool {
    (entry.created_at.seconds, entry.created_at.nanos) <= (now.seconds, now.nanos)
        && (now.seconds, now.nanos) < (entry.expires_at.seconds, entry.expires_at.nanos)
        && entry.read_state != wire::ReadState::Dismissed
}

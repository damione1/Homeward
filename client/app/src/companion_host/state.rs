use super::*;
use std::time::Instant;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum Stage {
    Disconnected,
    Loading,
    Unregistered,
    Ready,
    Busy,
    Uncertain,
    Suspended,
    Offline,
    Denied,
    Revoked,
    Expired,
    Unsupported,
    Invalid,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum Gate {
    Open,
    NeedsRead,
    UserStopped,
}
#[derive(Clone, Copy)]
#[repr(i32)]
pub(super) enum Field {
    Battery = 1,
    Connectivity = 2,
    Activity = 3,
    Cadence = 4,
}
impl Field {
    pub fn parse(value: i32) -> Option<Self> {
        match value {
            1 => Some(Self::Battery),
            2 => Some(Self::Connectivity),
            3 => Some(Self::Activity),
            4 => Some(Self::Cadence),
            _ => None,
        }
    }
    pub fn label(self) -> &'static str {
        match self {
            Self::Battery => "Batterie",
            Self::Connectivity => "Réseau local",
            Self::Activity => "Visibilité de l’application",
            Self::Cadence => "Cadence",
        }
    }
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub(super) struct Identity {
    pub account: AccountId,
    pub session: SessionId,
}
#[derive(Clone)]
pub(super) enum Intent {
    Register(wire::Context),
    Consent {
        context: wire::Context,
        prior: Box<wire::Installation>,
        desired: wire::Consent,
    },
    Revoke {
        context: wire::Context,
        prior: Box<wire::Installation>,
    },
}
#[derive(Clone)]
pub(super) enum Draft {
    Register(wire::Context),
    Consent {
        context: wire::Context,
        prior: Box<wire::Installation>,
        desired: wire::Consent,
    },
    Revoke {
        context: wire::Context,
        prior: Box<wire::Installation>,
    },
}
#[derive(Clone)]
pub(super) enum Write {
    Register(Box<wire::RegisterInstallationRequest>),
    Consent(Box<wire::UpdateConsentRequest>),
    Publish(Box<wire::PublishTelemetryRequest>),
    Revoke(Box<wire::RevokeInstallationRequest>),
}
#[derive(Clone)]
pub(super) struct Pending {
    pub request: Write,
    pub prior: Option<Box<wire::Installation>>,
    pub privacy: u64,
    pub resolution_read: bool,
}
impl Pending {
    pub fn selector(&self) -> wire::GetInstallationRequest {
        wire::GetInstallationRequest {
            selector: Some(match &self.request {
                Write::Register(r) => {
                    wire::get_installation_request::Selector::RegistrationRequestId(
                        r.request_id.clone(),
                    )
                }
                Write::Consent(r) => {
                    wire::get_installation_request::Selector::Id(Box::new((*r.id).clone()))
                }
                Write::Publish(r) => {
                    wire::get_installation_request::Selector::Id(Box::new((*r.id).clone()))
                }
                Write::Revoke(r) => {
                    wire::get_installation_request::Selector::Id(Box::new((*r.id).clone()))
                }
            }),
            ..Default::default()
        }
    }
    pub fn context(&self) -> &wire::Context {
        match &self.request {
            Write::Register(r) => &r.expected,
            Write::Consent(r) => &r.expected,
            Write::Publish(r) => &r.expected,
            Write::Revoke(r) => &r.expected,
        }
    }
    pub fn applied(&self, record: &wire::Installation) -> bool {
        match &self.request {
            Write::Register(r) => {
                record.registration_request_id == r.request_id
                    && record.device == r.device
                    && record.revision == 1
                    && record.last_sequence == 0
                    && !record.consent.battery
                    && !record.consent.connectivity
                    && !record.consent.activity
                    && record.state == wire::InstallationState::Active
            }
            Write::Consent(r) => {
                record.revision == r.expected_revision + 1
                    && record.consent == r.consent
                    && record.state == wire::InstallationState::Active
            }
            Write::Publish(r) => {
                record.revision == r.expected_revision
                    && record.last_sequence == r.sequence
                    && record.latest == r.snapshot
                    && record.state == wire::InstallationState::Active
            }
            Write::Revoke(r) => {
                record.revision == r.expected_revision + 1
                    && record.state == wire::InstallationState::Revoked
            }
        }
    }
}
pub(super) enum Job {
    Read,
    Intent(Intent),
    Sample,
}

pub(super) struct State {
    pub scope: u64,
    pub fence: u64,
    pub generation: i32,
    pub privacy: u64,
    pub exhausted: bool,
    pub active: bool,
    pub visible: bool,
    pub foreground: bool,
    pub validated: Option<home_client_session::Generation>,
    pub identity: Option<Identity>,
    pub context: Option<wire::Context>,
    pub record: Option<wire::Installation>,
    pub stage: Stage,
    pub gate: Gate,
    pub draft: Option<Draft>,
    pub intent: Option<Intent>,
    pub pending: Option<Pending>,
    pub requested: bool,
    pub running: bool,
    pub selected: usize,
    pub notice: String,
    pub cancel: CancellationToken,
    pub next_sample: Instant,
    pub samples: u64,
    pub sample_time: std::time::Duration,
}
impl Default for State {
    fn default() -> Self {
        Self {
            scope: 1,
            fence: 1,
            generation: 1,
            privacy: 1,
            exhausted: false,
            active: false,
            visible: false,
            foreground: true,
            validated: None,
            identity: None,
            context: None,
            record: None,
            stage: Stage::Disconnected,
            gate: Gate::NeedsRead,
            draft: None,
            intent: None,
            pending: None,
            requested: false,
            running: false,
            selected: 0,
            notice: String::new(),
            cancel: CancellationToken::new(),
            next_sample: Instant::now(),
            samples: 0,
            sample_time: std::time::Duration::ZERO,
        }
    }
}
impl State {
    pub fn bump(&mut self) -> bool {
        let Some(generation) = self.generation.checked_add(1) else {
            self.stage = Stage::Invalid;
            self.exhausted = true;
            self.cancel.cancel();
            self.active = false;
            return false;
        };
        self.generation = generation;
        true
    }
    pub fn interrupt(&mut self) {
        self.cancel.cancel();
        self.cancel = CancellationToken::new();
        self.fence = self.fence.saturating_add(1);
        self.bump();
        if self.fence == u64::MAX {
            self.active = false;
            self.exhausted = true;
            self.stage = Stage::Invalid;
        }
    }
    pub fn clear(&mut self) {
        self.interrupt();
        self.privacy = self.privacy.saturating_add(1);
        self.validated = None;
        self.identity = None;
        self.context = None;
        self.record = None;
        self.draft = None;
        self.intent = None;
        self.pending = None;
        self.gate = Gate::NeedsRead;
        self.active = false;
        self.requested = false;
        self.running = false;
        self.stage = Stage::Disconnected;
        self.notice.clear();
        self.samples = 0;
        self.sample_time = std::time::Duration::ZERO;
    }
    pub fn purge_sample(&mut self) {
        if let Some(r) = &mut self.record {
            r.latest = Default::default();
            r.accepted_at = Default::default();
        }
    }
    pub fn need_read(&mut self) {
        if self.gate != Gate::UserStopped {
            self.gate = Gate::NeedsRead;
        }
    }
    /// Wall time also detects a sleep gap when the OS monotonic clock pauses.
    /// Expiration only removes observations, never infers off/away.
    pub fn expire_values(&mut self, now: i64) -> bool {
        let expired = self
            .record
            .as_ref()
            .and_then(|r| r.latest.as_option().map(|s| (r, s)))
            .is_some_and(|(r, s)| {
                let age = now.saturating_sub(s.captured_at.seconds);
                age < 0
                    || admit::cadence_seconds(r.consent.cadence)
                        .is_none_or(|period| age as u64 >= period * 2)
            });
        if expired {
            self.purge_sample();
            self.need_read();
            self.requested = true;
            self.notice="Mesures périmées : nouvelle vérification, sans rattrapage ni départ géographique déduit.".into();
        }
        expired
    }
    pub fn stop(&mut self) {
        self.privacy = self.privacy.saturating_add(1);
        if self.privacy == u64::MAX {
            self.exhausted = true;
            self.active = false;
        }
        self.gate = Gate::UserStopped;
        self.interrupt();
        self.purge_sample();
        self.intent = None;
        self.notice="Collecte arrêtée localement. Aucun nouvel échantillon n’est envoyé ; un consentement confirmé est nécessaire pour reprendre.".into();
        if self.pending.is_some() {
            self.stage = Stage::Uncertain;
            self.requested = true;
        }
    }
    pub fn foreground(&mut self, value: bool) {
        if self.foreground == value {
            return;
        }
        self.foreground = value;
        self.interrupt();
        self.draft = None;
        self.purge_sample();
        if self.gate != Gate::UserStopped {
            self.gate = Gate::NeedsRead;
        }
        self.requested = true;
        if !value {
            self.stage = Stage::Suspended;
            self.notice =
                "Application suspendue : mesures périmées, aucun rattrapage ne sera envoyé.".into();
        }
    }
    pub fn identity(&mut self, identity: Identity) -> bool {
        match &self.identity {
            Some(current) if current != &identity => {
                self.clear();
                self.active = true;
                self.identity = Some(identity);
                self.requested = true;
                false
            }
            Some(_) => true,
            None => {
                self.identity = Some(identity);
                true
            }
        }
    }
    pub fn choose(&mut self, generation: i32, field: i32, option: i32) {
        if generation != self.generation || self.running || !self.visible || !self.foreground {
            return;
        }
        let Some(field) = Field::parse(field) else {
            return;
        };
        let Some(Draft::Consent { desired, .. }) = &mut self.draft else {
            return;
        };
        let mut withdraw = false;
        match field {
            Field::Battery | Field::Connectivity | Field::Activity => {
                let value = match option {
                    1 => true,
                    2 => false,
                    _ => return,
                };
                match field {
                    Field::Battery => desired.battery = value,
                    Field::Connectivity => desired.connectivity = value,
                    Field::Activity => desired.activity = value,
                    Field::Cadence => unreachable!("toggle field admitted above"),
                }
                withdraw = !value;
            }
            Field::Cadence => {
                desired.cadence = match option {
                    1 => wire::Cadence::Seconds30,
                    2 => wire::Cadence::Minute1,
                    3 => wire::Cadence::Minutes5,
                    _ => return,
                }
                .into()
            }
        }
        if withdraw {
            self.stop();
        } else {
            self.bump();
        }
    }
    pub fn action(&mut self, generation: i32, action: ProAdminAction, editor: bool) {
        if generation != self.generation || !self.active || !self.visible || !self.foreground {
            return;
        }
        if action == ProAdminAction::Disable && !editor {
            self.stop();
            return;
        }
        if self.running {
            return;
        }
        if editor {
            match action {
                ProAdminAction::Cancel => {
                    self.draft = None;
                    self.bump();
                }
                ProAdminAction::Save => {
                    let Some(draft) = self.draft.take() else {
                        return;
                    };
                    self.intent = Some(match draft {
                        Draft::Register(context) => Intent::Register(context),
                        Draft::Consent {
                            context,
                            prior,
                            desired,
                        } => Intent::Consent {
                            context,
                            prior,
                            desired,
                        },
                        Draft::Revoke { context, prior } => Intent::Revoke { context, prior },
                    });
                    self.bump();
                }
                ProAdminAction::Refresh
                | ProAdminAction::Create
                | ProAdminAction::Remove
                | ProAdminAction::More
                | ProAdminAction::Enable
                | ProAdminAction::Disable
                | ProAdminAction::Revoke
                | ProAdminAction::CloseSecret
                | ProAdminAction::Preview
                | ProAdminAction::Revalidate
                | ProAdminAction::Simulate
                | ProAdminAction::Apply
                | ProAdminAction::Restore
                | ProAdminAction::Rollback
                | ProAdminAction::AddFile
                | ProAdminAction::RemoveFile
                | ProAdminAction::NextFile
                | ProAdminAction::Resolve => {}
            }
            return;
        }
        if self.draft.is_some() {
            return;
        }
        match action {
            ProAdminAction::Refresh | ProAdminAction::Resolve => {
                self.requested = true;
                if self.stage == Stage::Invalid && !self.exhausted {
                    self.stage = Stage::Loading;
                }
            }
            ProAdminAction::Create => {
                if self.record.is_none()
                    && self.pending.is_none()
                    && let Some(context) = &self.context
                {
                    self.draft = Some(Draft::Register(context.clone()));
                    self.bump();
                }
            }
            ProAdminAction::Enable => {
                if self.stage == Stage::Ready
                    && self.pending.is_none()
                    && let (Some(context), Some(record)) = (&self.context, &self.record)
                {
                    self.draft = Some(Draft::Consent {
                        context: context.clone(),
                        prior: Box::new(record.clone()),
                        desired: (*record.consent).clone(),
                    });
                    self.bump();
                }
            }
            ProAdminAction::Revoke => {
                if self.stage == Stage::Ready
                    && self.pending.is_none()
                    && let (Some(context), Some(record)) = (&self.context, &self.record)
                {
                    self.draft = Some(Draft::Revoke {
                        context: context.clone(),
                        prior: Box::new(record.clone()),
                    });
                    self.stop();
                }
            }
            ProAdminAction::Save
            | ProAdminAction::Remove
            | ProAdminAction::Cancel
            | ProAdminAction::More
            | ProAdminAction::Disable
            | ProAdminAction::CloseSecret
            | ProAdminAction::Preview
            | ProAdminAction::Revalidate
            | ProAdminAction::Simulate
            | ProAdminAction::Apply
            | ProAdminAction::Restore
            | ProAdminAction::Rollback
            | ProAdminAction::AddFile
            | ProAdminAction::RemoveFile
            | ProAdminAction::NextFile => {}
        }
    }
    pub fn plan(&mut self, now: Instant) -> Option<Job> {
        if self.exhausted
            || !self.active
            || !self.foreground
            || self.running
            || self.stage == Stage::Invalid
        {
            return None;
        }
        if self.pending.is_some() && self.requested {
            self.requested = false;
            self.running = true;
            if let Some(p) = &mut self.pending {
                p.resolution_read = true;
            }
            return Some(Job::Read);
        }
        if let Some(intent) = self.intent.take() {
            self.running = true;
            self.stage = Stage::Busy;
            return Some(Job::Intent(intent));
        }
        if self.requested {
            self.requested = false;
            self.running = true;
            return Some(Job::Read);
        }
        if self.gate == Gate::Open
            && self.record.as_ref().is_some_and(|r| {
                r.state == wire::InstallationState::Active
                    && (r.consent.battery || r.consent.connectivity || r.consent.activity)
            })
            && now >= self.next_sample
        {
            self.running = true;
            return Some(Job::Sample);
        }
        None
    }
    pub fn accept(
        &mut self,
        context: wire::Context,
        record: wire::Installation,
        resolving: bool,
    ) -> bool {
        let Some(identity) = &self.identity else {
            return false;
        };
        if admit::bind_native(&record, &identity.account, &identity.session).is_err() {
            return false;
        }
        if let (Some(expected_context), Some(expected)) = (&self.context, &self.record)
            && admit::bind_existing(&context, &record, expected_context, expected).is_err()
        {
            return false;
        }
        if self.record.as_ref().is_some_and(|prior| {
            record.revision < prior.revision
                || record.last_sequence < prior.last_sequence
                || (record.revision == prior.revision && record.consent != prior.consent)
        }) {
            return false;
        }
        let changed_revision = self
            .record
            .as_ref()
            .is_none_or(|r| r.revision != record.revision);

        if resolving && let Some(pending) = self.pending.take() {
            if &context != pending.context() {
                return false;
            }
            if let Some(prior) = &pending.prior
                && admit::bind_existing(&context, &record, pending.context(), prior).is_err()
            {
                return false;
            }
            let applied = pending.applied(&record);
            if applied && self.privacy == pending.privacy {
                self.gate = Gate::Open;
                self.notice =
                    "Opération confirmée par le serveur. Aucun renvoi de mutation.".into();
            } else {
                self.gate = Gate::UserStopped;
                self.notice="État relu ; la collecte reste arrêtée localement. Réexaminez le consentement avant de reprendre.".into();
            }
        } else if self.gate == Gate::NeedsRead {
            self.gate = Gate::Open;
        }
        self.stage = match record.state {
            buffa::EnumValue::Known(wire::InstallationState::Active) => Stage::Ready,
            buffa::EnumValue::Known(wire::InstallationState::Revoked) => Stage::Revoked,
            buffa::EnumValue::Known(wire::InstallationState::Expired) => Stage::Expired,
            buffa::EnumValue::Known(wire::InstallationState::Unspecified)
            | buffa::EnumValue::Unknown(_) => return false,
        };
        if self.stage != Stage::Ready {
            self.gate = Gate::UserStopped;
        }
        let cadence = admit::cadence_seconds(record.consent.cadence).expect("admitted cadence");
        let delay = if let Some(at) = record.accepted_at.as_option() {
            let now = chrono::Utc::now();
            let elapsed = now.timestamp().saturating_sub(at.seconds);
            if elapsed < 0 {
                cadence
            } else {
                cadence.saturating_sub(elapsed as u64)
            }
        } else if record.last_sequence > 0 {
            cadence
        } else {
            0
        };
        let mut record = record;
        if self.gate == Gate::UserStopped {
            record.latest = Default::default();
            record.accepted_at = Default::default();
        }
        self.context = Some(context);
        self.record = Some(record);
        self.running = false;
        if changed_revision {
            self.bump();
        }
        // After publication, wait a full cadence from completion; after a
        // consent response, the first sample may run immediately.
        self.next_sample = Instant::now() + std::time::Duration::from_secs(delay);
        true
    }
    pub fn invalid(&mut self) {
        self.interrupt();
        self.context = None;
        self.record = None;
        self.pending = None;
        self.intent = None;
        self.draft = None;
        self.gate = Gate::UserStopped;
        self.stage = Stage::Invalid;
        self.running = false;
        self.notice = "La réponse n’a pas été admise. Aucun échantillon ne sera envoyé.".into();
    }
}

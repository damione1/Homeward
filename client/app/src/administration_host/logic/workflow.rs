//! One explicit attempt, then read-only resolution. No restart/resume queue.
#[cfg(test)]
mod tls_probe;
use super::*;
use crate::administration_host::read::{ReadError, TIMEOUT, admitted_lease, call};
use crate::{ProAdminAction, ProAdminActionRow, ProAdminField, ProAdminFieldKind};
use home_client_session::{Generation, SessionCoordinator};
use home_client_transport::{Connection, FailureKind, RpcCode};
use home_wire_contract::logic_contract::{self as contract, Request, RequestId, Response};
use tokio_util::sync::CancellationToken;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct Permit {
    scope: u64,
    attempt: u64,
}
#[derive(Clone)]
enum Operation {
    Start {
        context: Context,
        definition: Box<wire::DefinitionRef>,
        request: RequestId,
    },
    Cancel {
        context: Context,
        run: Box<wire::Run>,
        request: RequestId,
    },
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Stage {
    Draft,
    Pending,
    Uncertain,
    Resolved,
    Rejected,
}
struct Draft {
    operation: Operation,
    stage: Stage,
    notice: &'static str,
}
enum Current {
    Definition {
        context: Context,
        definition: Box<wire::Definition>,
    },
    Run {
        context: Context,
        run: Box<wire::Run>,
    },
    ReadOnly,
}
pub(in crate::administration_host) struct Job {
    permit: Permit,
    operation: Operation,
    resolve: bool,
}
enum Reply {
    Started(wire::Run),
    Canceled(wire::Run),
    Resolved(wire::Run),
}
pub(in crate::administration_host) struct Completion {
    permit: Permit,
    resolve: bool,
    result: Result<Reply, ReadError>,
}
pub(in crate::administration_host) struct Workflow {
    scope: Option<u64>,
    attempt: u64,
    current: Option<Current>,
    draft: Option<Draft>,
    pending: Option<Job>,
    active: Option<Permit>,
}
impl Default for Workflow {
    fn default() -> Self {
        Self {
            scope: Some(1),
            attempt: 0,
            current: None,
            draft: None,
            pending: None,
            active: None,
        }
    }
}
impl Workflow {
    pub fn retire(&mut self) {
        self.scope = self.scope.and_then(|value| value.checked_add(1));
        self.current = None;
        self.draft = None;
        self.pending = None;
        self.active = None;
    }
    pub fn opened(&self) -> bool {
        self.draft.is_some()
    }
    pub fn observe(&mut self, payload: &Payload) {
        self.current = Some(match payload {
            Payload::Definition {
                catalogue,
                definition,
                ..
            } => Current::Definition {
                context: catalogue.context.clone(),
                definition: definition.clone(),
            },
            Payload::Run { context, run } => Current::Run {
                context: context.clone(),
                run: run.clone(),
            },
            Payload::Catalogue(_) | Payload::Trace { .. } => Current::ReadOnly,
        });
    }
    pub fn actions(&self) -> Vec<ProAdminActionRow> {
        if self.opened() {
            return Vec::new();
        }
        match &self.current {
            Some(Current::Definition { definition, .. }) => vec![
                action(
                    ProAdminAction::Create,
                    "Exécuter · synthétique",
                    definition.can_run,
                    true,
                ),
                action(
                    ProAdminAction::Simulate,
                    "Simuler une source fournie…",
                    definition.can_simulate,
                    false,
                ),
            ],
            Some(Current::Run { run, .. }) => vec![action(
                ProAdminAction::Remove,
                "Annuler les étapes futures…",
                run.can_cancel,
                true,
            )],
            Some(Current::ReadOnly) | None => Vec::new(),
        }
    }
    pub fn open(&mut self, action: ProAdminAction) -> bool {
        if self.opened() || self.scope.is_none() {
            return false;
        }
        let Ok(id) = home_client_actions::CommandId::generate() else {
            return false;
        };
        let Ok(request) = RequestId::parse(id.as_str()) else {
            return false;
        };
        let operation = match action {
            ProAdminAction::Create => {
                let Some(Current::Definition {
                    context,
                    definition,
                }) = &self.current
                else {
                    return false;
                };
                if !definition.can_run {
                    return false;
                }
                Operation::Start {
                    context: context.clone(),
                    definition: Box::new((*definition.reference).clone()),
                    request,
                }
            }
            ProAdminAction::Remove => {
                let Some(Current::Run { context, run }) = &self.current else {
                    return false;
                };
                if !run.can_cancel {
                    return false;
                }
                Operation::Cancel {
                    context: context.clone(),
                    run: run.clone(),
                    request,
                }
            }
            ProAdminAction::Refresh
            | ProAdminAction::Save
            | ProAdminAction::Cancel
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
            | ProAdminAction::Resolve => return false,
        };
        self.draft = Some(Draft {
            operation,
            stage: Stage::Draft,
            notice: "Confirmez cette intention explicite. Une seule tentative sera envoyée au serveur synthétique.",
        });
        true
    }
    pub fn editor_action(&mut self, action: ProAdminAction) -> bool {
        let Some(draft) = &mut self.draft else {
            return false;
        };
        match action {
            ProAdminAction::Cancel if draft.stage != Stage::Pending => {
                self.draft = None;
                true
            }
            ProAdminAction::Save if draft.stage == Stage::Draft => self.queue(false),
            ProAdminAction::Resolve if draft.stage == Stage::Uncertain => self.queue(true),
            ProAdminAction::Refresh
            | ProAdminAction::Create
            | ProAdminAction::Save
            | ProAdminAction::Remove
            | ProAdminAction::Cancel
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
            | ProAdminAction::Resolve => false,
        }
    }
    fn queue(&mut self, resolve: bool) -> bool {
        if self.pending.is_some() || self.active.is_some() {
            return false;
        }
        let (Some(scope), Some(attempt), Some(draft)) =
            (self.scope, self.attempt.checked_add(1), self.draft.as_mut())
        else {
            return false;
        };
        self.attempt = attempt;
        let permit = Permit { scope, attempt };
        self.pending = Some(Job {
            permit,
            operation: draft.operation.clone(),
            resolve,
        });
        self.active = Some(permit);
        draft.stage = Stage::Pending;
        draft.notice = if resolve {
            "Lecture du run uniquement. Aucune exécution ni annulation renvoyée."
        } else {
            "Tentative en cours. Une interruption laissera son issue incertaine."
        };
        true
    }
    pub fn take_pending(&mut self) -> Option<Job> {
        self.pending.take()
    }
    /// True requests that the enclosing owner purge all privileged data.
    pub fn accept(&mut self, result: Completion) -> bool {
        if self.active != Some(result.permit) || self.scope != Some(result.permit.scope) {
            return false;
        }
        self.active = None;
        let Some(draft) = &mut self.draft else {
            return false;
        };
        match result.result {
            Ok(Reply::Started(run)) => {
                draft.stage = Stage::Resolved;
                draft.notice = if run.status == wire::RunStatus::Failed {
                    "Run refusé ou échoué selon le serveur. Aucun effet physique validé."
                } else {
                    "Run synthétique enregistré. Aucun effet physique validé. Fermez puis relisez le catalogue ou les runs."
                };
            }
            Ok(Reply::Canceled(run)) => {
                draft.stage = Stage::Resolved;
                draft.notice = if run.status == wire::RunStatus::Canceled {
                    "Étapes futures arrêtées. Les effets déjà dispatchés ne sont pas inversés."
                } else {
                    "Run déjà terminal selon le serveur. Aucun effet déjà dispatché n’est inversé."
                };
            }
            Ok(Reply::Resolved(run)) => {
                draft.stage = Stage::Resolved;
                draft.notice = match &draft.operation {
                    Operation::Start { .. } => {
                        "Run retrouvé par la tentative d’origine, sans réexécution. Fermez puis relisez ses détails."
                    }
                    Operation::Cancel { .. } => {
                        if run.status == wire::RunStatus::Canceled {
                            "Run relu annulé. Cette lecture ne prouve pas l’attribution de l’annulation à notre tentative."
                        } else {
                            "État du run relu. Aucune annulation renvoyée ; cette lecture n’établit pas l’issue de notre tentative."
                        }
                    }
                };
            }
            Err(ReadError::Transport(FailureKind::Rpc(
                RpcCode::Unauthenticated | RpcCode::PermissionDenied,
            )))
            | Err(ReadError::Session(_)) => {
                self.retire();
                return true;
            }
            Err(ReadError::Transport(FailureKind::Rpc(
                RpcCode::InvalidArgument | RpcCode::FailedPrecondition | RpcCode::Unimplemented,
            ))) if !result.resolve => {
                draft.stage = Stage::Rejected;
                draft.notice = "Tentative refusée par le serveur. Fermez et relisez les références avant une nouvelle confirmation.";
            }
            Err(
                ReadError::Transport(_)
                | ReadError::Invalid
                | ReadError::Unsupported
                | ReadError::Cancelled,
            ) => {
                draft.stage = Stage::Uncertain;
                draft.notice = "Issue incertaine ou run non retrouvé. Aucune répétition automatique. Résoudre relit uniquement le run de cette tentative.";
            }
        }
        false
    }
    pub fn title(&self) -> &'static str {
        match self.draft.as_ref().map(|value| &value.operation) {
            Some(Operation::Start { .. }) => "Exécution synthétique · confirmation",
            Some(Operation::Cancel { .. }) => "Annulation des étapes futures · confirmation",
            None => "Logique",
        }
    }
    pub fn notice(&self) -> &'static str {
        self.draft.as_ref().map_or("", |value| value.notice)
    }
    pub fn editable(&self) -> bool {
        self.draft
            .as_ref()
            .is_some_and(|value| value.stage == Stage::Draft)
    }
    pub fn editor_actions(&self) -> Vec<ProAdminActionRow> {
        let Some(draft) = &self.draft else {
            return Vec::new();
        };
        vec![
            action(
                ProAdminAction::Save,
                "Confirmer une seule tentative",
                draft.stage == Stage::Draft,
                true,
            ),
            action(
                ProAdminAction::Resolve,
                "Résoudre par lecture uniquement",
                draft.stage == Stage::Uncertain,
                false,
            ),
            action(
                ProAdminAction::Cancel,
                "Fermer",
                draft.stage != Stage::Pending,
                false,
            ),
        ]
    }
    pub fn fields(&self) -> Vec<ProAdminField> {
        let Some(draft) = &self.draft else {
            return Vec::new();
        };
        let (context, identity, request) = match &draft.operation {
            Operation::Start {
                context,
                definition,
                request,
            } => (
                context,
                format!(
                    "{} · version {} · hash {}",
                    definition.id,
                    definition.version.revision,
                    crate::administration_host::render::hex(&definition.version.source_sha256)
                ),
                request,
            ),
            Operation::Cancel {
                context,
                run,
                request,
            } => (
                context,
                format!(
                    "Run {} · définition {} · version {}",
                    run.id, run.definition.id, run.definition.version.revision
                ),
                request,
            ),
        };
        vec![
            ProAdminField {
                token: 1,
                label: "Cible capturée".into(),
                description: "Référence serveur exacte ; aucune routine réelle exécutée".into(),
                value: identity.into(),
                kind: ProAdminFieldKind::Plain,
                editable: false,
            },
            ProAdminField {
                token: 2,
                label: "Tentative explicite".into(),
                description: "Une réponse perdue est résolue par lecture de cet identifiant".into(),
                value: request.as_str().into(),
                kind: ProAdminFieldKind::Plain,
                editable: false,
            },
            ProAdminField {
                token: 3,
                label: "Contexte capturé".into(),
                description: "Un changement d’epoch exige une nouvelle lecture".into(),
                value: format!(
                    "{} · catalogue {} · génération {}",
                    context.epoch, context.catalogue, context.generation
                )
                .into(),
                kind: ProAdminFieldKind::Plain,
                editable: false,
            },
        ]
    }
}
fn action(
    action: ProAdminAction,
    label: &str,
    enabled: bool,
    destructive: bool,
) -> ProAdminActionRow {
    ProAdminActionRow {
        action,
        label: label.into(),
        enabled,
        destructive,
    }
}

pub(in crate::administration_host) async fn execute(
    session: &SessionCoordinator,
    connection: &Connection,
    expected: Generation,
    job: Job,
    cancel: &CancellationToken,
) -> Completion {
    let permit = job.permit;
    let resolve = job.resolve;
    let result = tokio::select! {biased;()=cancel.cancelled()=>Err(ReadError::Cancelled),value=execute_inner(session,connection,expected,job,cancel)=>value};
    Completion {
        permit,
        resolve,
        result,
    }
}
async fn execute_inner(
    session: &SessionCoordinator,
    connection: &Connection,
    expected: Generation,
    job: Job,
    cancel: &CancellationToken,
) -> Result<Reply, ReadError> {
    let lease = admitted_lease(session, expected, cancel).await?;
    let authority =
        crate::administration_host::read::probe(session, connection, expected, cancel).await?;
    if !authority.administrator() {
        return Err(ReadError::Transport(FailureKind::Rpc(
            RpcCode::PermissionDenied,
        )));
    }
    let (context, selector) = match &job.operation {
        Operation::Start {
            context, request, ..
        } => (
            context,
            wire::get_run_request::Selector::StartRequestId(request.as_str().into()),
        ),
        Operation::Cancel { context, run, .. } => (
            context,
            wire::get_run_request::Selector::RunId(run.id.clone()),
        ),
    };
    let (response_context, run, reply) = if job.resolve {
        let request = wire::GetRunRequest {
            context: context.to_wire().into(),
            selector: Some(selector),
            ..Default::default()
        };
        contract::admit_request(Request::Get(&request)).map_err(|_| ReadError::Invalid)?;
        let response = call!(session, lease, cancel, connection.logic(), get_run, request);
        contract::admit_response(Response::Get(&response)).map_err(|_| ReadError::Invalid)?;
        (
            response.context.into_option(),
            response.run.into_option(),
            ReadReply::Resolve,
        )
    } else {
        match &job.operation {
            Operation::Start {
                context,
                definition,
                request,
            } => {
                let request = wire::StartRunRequest {
                    expected: context.to_wire().into(),
                    definition: (**definition).clone().into(),
                    request_id: request.as_str().into(),
                    ..Default::default()
                };
                contract::admit_request(Request::Start(&request))
                    .map_err(|_| ReadError::Invalid)?;
                let response = call!(
                    session,
                    lease,
                    cancel,
                    connection.logic(),
                    start_run,
                    request
                );
                contract::admit_response(Response::Start(&response))
                    .map_err(|_| ReadError::Invalid)?;
                (
                    response.context.into_option(),
                    response.run.into_option(),
                    ReadReply::Start,
                )
            }
            Operation::Cancel {
                context,
                run,
                request,
            } => {
                let request = wire::CancelRunRequest {
                    expected: context.to_wire().into(),
                    run_id: run.id.clone(),
                    request_id: request.as_str().into(),
                    ..Default::default()
                };
                contract::admit_request(Request::Cancel(&request))
                    .map_err(|_| ReadError::Invalid)?;
                let response = call!(
                    session,
                    lease,
                    cancel,
                    connection.logic(),
                    cancel_run,
                    request
                );
                contract::admit_response(Response::Cancel(&response))
                    .map_err(|_| ReadError::Invalid)?;
                (
                    response.context.into_option(),
                    response.run.into_option(),
                    ReadReply::Cancel,
                )
            }
        }
    };
    let response_context = response_context.ok_or(ReadError::Invalid)?;
    let run = run.ok_or(ReadError::Invalid)?;
    if Context::capture(&response_context)? != *context {
        return Err(ReadError::Invalid);
    }
    match &job.operation {
        Operation::Start {
            definition,
            request,
            ..
        } => {
            if run.definition.as_option() != Some(definition.as_ref())
                || run.start_request_id != request.as_str()
                || run.generation.as_option() != context.to_wire().generation.as_option()
            {
                return Err(ReadError::Invalid);
            }
        }
        Operation::Cancel { run: previous, .. } => {
            if run.id != previous.id
                || run.definition != previous.definition
                || run.start_request_id != previous.start_request_id
                || run.generation != previous.generation
                || run.actor != previous.actor
            {
                return Err(ReadError::Invalid);
            }
        }
    }
    Ok(match reply {
        ReadReply::Start => Reply::Started(run),
        ReadReply::Cancel => Reply::Canceled(run),
        ReadReply::Resolve => Reply::Resolved(run),
    })
}
enum ReadReply {
    Start,
    Cancel,
    Resolve,
}

#[cfg(test)]
mod tests {
    use super::*;
    fn definition() -> Current {
        Current::Definition {
            context: Context {
                epoch: "synthetic".into(),
                catalogue: 1,
                generation: 1,
                manifest: [1; 32],
            },
            definition: Box::new(wire::Definition {
                can_run: true,
                reference: wire::DefinitionRef {
                    id: "fixture".into(),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            }),
        }
    }
    #[test]
    fn uncertainty_can_only_queue_a_lookup_for_the_original_attempt() {
        let mut workflow = Workflow {
            current: Some(definition()),
            ..Default::default()
        };
        assert!(workflow.open(ProAdminAction::Create));
        assert!(workflow.editor_action(ProAdminAction::Save));
        let job = workflow.take_pending().unwrap();
        assert!(!job.resolve);
        let original = match &job.operation {
            Operation::Start { request, .. } | Operation::Cancel { request, .. } => request.clone(),
        };
        assert!(!workflow.accept(Completion {
            permit: job.permit,
            resolve: false,
            result: Err(ReadError::Transport(FailureKind::NetworkInterrupted))
        }));
        assert!(!workflow.editor_action(ProAdminAction::Save));
        assert!(!workflow.open(ProAdminAction::Create));
        assert!(workflow.pending.is_none());
        assert!(workflow.editor_action(ProAdminAction::Resolve));
        let lookup = workflow.take_pending().unwrap();
        assert!(lookup.resolve);
        match lookup.operation {
            Operation::Start { request, .. } | Operation::Cancel { request, .. } => {
                assert_eq!(request, original)
            }
        }
    }
    #[test]
    fn a_retired_reply_cannot_finish_or_unlock_a_new_confirmation() {
        let mut workflow = Workflow {
            current: Some(definition()),
            ..Default::default()
        };
        assert!(workflow.open(ProAdminAction::Create));
        assert!(workflow.editor_action(ProAdminAction::Save));
        let old = workflow.take_pending().unwrap();
        workflow.retire();
        workflow.current = Some(definition());
        assert!(workflow.open(ProAdminAction::Create));
        assert!(workflow.editor_action(ProAdminAction::Save));
        let current = workflow.active;
        assert!(!workflow.accept(Completion {
            permit: old.permit,
            resolve: false,
            result: Ok(Reply::Started(wire::Run::default()))
        }));
        assert_eq!(workflow.active, current);
        assert_eq!(workflow.draft.as_ref().unwrap().stage, Stage::Pending);
        assert!(!workflow.editor_action(ProAdminAction::Save));
    }
}

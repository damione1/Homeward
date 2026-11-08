//! Real TLS capability-controller proof on isolated synthetic sources.
//! MemoryStore is an explicit test double, never a credential fallback.
#![deny(clippy::wildcard_enum_match_arm)]
use home_client_actions::capabilities::{
    CapabilityController, CapabilityHandle, CapabilitySnapshot, CapabilitySubmission,
    CapabilityTracking,
};
use home_client_actions::{WriteAccess, new_request_id, write_access};
use home_client_model::api::{
    CapabilityName, Command, Deadline, DefinitionEtag, EntityViewName, HomeName, Intent, Power,
};
use home_client_session::{AccessLease, SecureStore, SessionCoordinator, StoreFailure};
use home_client_transport::Connection;
use home_wire_contract::{
    api_command_operations::{CommandCompletion, CommandPhase, admit_command_operation},
    api_contract::parse_power,
    google::longrunning as operations,
    home::api::v2 as wire,
};
use serde::Deserialize;
use std::{
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use tokio_util::sync::CancellationToken;
use zeroize::Zeroizing;
type Result<T> = std::result::Result<T, Box<dyn std::error::Error + Send + Sync>>;
#[derive(Clone, Copy, Deserialize)]
#[serde(rename_all = "kebab-case")]
enum Scenario {
    LostSubmit,
    AckOnly,
    Expire,
    Refusal,
    Withdrawn,
}
#[derive(Deserialize)]
#[serde(rename_all = "kebab-case")]
enum Mode {
    SyntheticOnly,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Fixture {
    mode: Mode,
    origin: String,
    ca_pem: String,
    login_name: String,
    password: String,
    scenario: Scenario,
}
#[derive(Default)]
struct MemoryStore(Mutex<Option<Zeroizing<Vec<u8>>>>);
impl SecureStore for MemoryStore {
    fn load(&self) -> std::result::Result<Option<Zeroizing<Vec<u8>>>, StoreFailure> {
        Ok(self.0.lock().unwrap().clone())
    }
    fn save(&self, data: &[u8]) -> std::result::Result<(), StoreFailure> {
        *self.0.lock().unwrap() = Some(Zeroizing::new(data.to_vec()));
        Ok(())
    }
    fn clear(&self) -> std::result::Result<(), StoreFailure> {
        *self.0.lock().unwrap() = None;
        Ok(())
    }
}
async fn until(
    handle: &CapabilityHandle,
    check: impl Fn(&CapabilitySnapshot) -> bool,
) -> Result<()> {
    let mut updates = handle.subscribe();
    tokio::time::timeout(Duration::from_secs(20), async {
        loop {
            if check(&updates.borrow_and_update()) {
                return Ok(());
            }
            updates
                .changed()
                .await
                .map_err(|_| "capability updates closed")?;
        }
    })
    .await
    .map_err(|_| "capability predicate timed out")?
}

async fn capture(
    connection: &Connection,
    lease: &AccessLease,
    cancel: &CancellationToken,
) -> Result<(wire::EntityView, wire::EntityViewEntry)> {
    let homes = connection
        .home()
        .list_homes(
            wire::ListHomesRequest {
                page_size: 1,
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(10),
            cancel,
        )
        .await?;
    if homes.homes.len() != 1
        || !homes.next_page_token.is_empty()
        || !homes.__buffa_unknown_fields.is_empty()
    {
        return Err("exact home discovery required".into());
    }
    let home = HomeName::try_from(homes.homes[0].name.as_str())?;
    let resource = connection
        .entity_views()
        .create_entity_view(
            wire::CreateEntityViewRequest {
                parent: home.as_str().to_owned(),
                entity_view: wire::EntityView::default().into(),
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(10),
            cancel,
        )
        .await?;
    let name = EntityViewName::try_from(resource.name.as_str())?;
    if name.home() != home || !resource.__buffa_unknown_fields.is_empty() {
        return Err("foreign capture".into());
    }
    let mut page = connection
        .entity_views()
        .query_entity_view(
            wire::QueryEntityViewRequest {
                name: resource.name.clone(),
                page_size: 16,
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(10),
            cancel,
        )
        .await?;
    if page.entries.len() != 1
        || !page.next_page_token.is_empty()
        || page.resume_token.is_empty()
        || !page.__buffa_unknown_fields.is_empty()
    {
        return Err("exact complete isolated capture required".into());
    }
    Ok((resource, page.entries.remove(0)))
}
fn power_slot(entry: &wire::EntityViewEntry) -> Result<(&wire::Capability, Power)> {
    if entry.capabilities.len() != 1 {
        return Err("exact one source slot required".into());
    }
    let slot = &entry.capabilities[0];
    let Some(wire::capability::Trait::Power(power)) = &slot.r#trait else {
        return Err("power source required".into());
    };
    if !power.__buffa_unknown_fields.is_empty() || !power.supports_set {
        return Err("unsupported power definition".into());
    }
    let observation = power
        .observation
        .as_option()
        .ok_or("missing observed power")?;
    let quality = observation
        .quality
        .as_option()
        .ok_or("missing source quality")?;
    if !observation.__buffa_unknown_fields.is_empty()
        || !quality.__buffa_unknown_fields.is_empty()
        || quality.availability != wire::Availability::Available
        || quality.freshness != wire::Freshness::Current
        || quality.provenance != wire::Provenance::Observed
        || observation.observed_time.as_option().is_none()
    {
        return Err("unqualified source observation".into());
    }
    let value = parse_power(observation.value.ok_or("unknown power")?)
        .map_err(|_| "unsupported observed power")?;
    Ok((slot, value))
}
#[tokio::main(flavor = "current_thread")]
async fn main() -> Result<()> {
    let path = std::env::args()
        .nth(1)
        .ok_or("private synthetic fixture required")?;
    let fixture: Fixture = serde_json::from_slice(&std::fs::read(path)?)?;
    let Mode::SyntheticOnly = fixture.mode;
    if !fixture.origin.starts_with("https://127.0.0.1:") {
        return Err("only loopback synthetic fixture accepted".into());
    }
    let connection = Connection::new(&fixture.origin, fixture.ca_pem.as_bytes())?;
    let session = SessionCoordinator::new(connection.auth(), Arc::new(MemoryStore::default()));
    session
        .login(fixture.login_name, Zeroizing::new(fixture.password))
        .await
        .map_err(|_| "native login failed")?;
    let cancel = CancellationToken::new();
    let lease = session.access(&cancel).await.map_err(|_| "no session")?;
    let (view, entry) = capture(&connection, &lease, &cancel).await?;
    let (slot, observed) = power_slot(&entry)?;
    assert_eq!(observed, Power::Off);
    let target = CapabilityName::try_from(slot.name.as_str())?;
    if target.home() != EntityViewName::try_from(view.name.as_str())?.home()
        || write_access(&entry, &target) != WriteAccess::Allowed
    {
        return Err("exact source permission required".into());
    }
    let id = new_request_id().map_err(|e| format!("request identity: {e:?}"))?;
    let lifetime = if matches!(fixture.scenario, Scenario::Expire) {
        3
    } else {
        30
    };
    let now = i64::try_from(SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs())?;
    let etag = if matches!(fixture.scenario, Scenario::Refusal) {
        // Deliberate synthetic stale-CAS fault; no old epoch/config wire fields.
        DefinitionEtag::try_from("W/\"synthetic-stale-definition\"")?
    } else {
        DefinitionEtag::try_from(slot.etag.as_str())?
    };
    let command = Command::new(
        target.clone(),
        Intent::Power(Power::On),
        Some(id.clone()),
        Deadline::new(now + lifetime, 0)?,
    )
    .with_definition_etag(etag);
    let controller = CapabilityController::new(
        session.clone(),
        connection.capabilities(),
        connection.operations(),
        &cancel,
    );
    let valid = Arc::new(AtomicBool::new(true));
    let check = valid.clone();
    let source = lease.source_proof();
    let expiry = view
        .expire_time
        .as_option()
        .ok_or("capture expiry absent")?
        .seconds;
    let handle = controller
        .submit(
            command.clone(),
            Arc::new(move || {
                check.load(Ordering::SeqCst)
                    && source.is_current()
                    && SystemTime::now()
                        .duration_since(UNIX_EPOCH)
                        .is_ok_and(|now| {
                            i64::try_from(now.as_secs()).is_ok_and(|seconds| seconds < expiry)
                        })
            }),
        )
        .map_err(|e| format!("local admission failed: {e:?}"))?;
    if matches!(fixture.scenario, Scenario::Withdrawn) {
        valid.store(false, Ordering::SeqCst);
    }
    match fixture.scenario {
        Scenario::LostSubmit => {
            until(&handle, |value| {
                value.submission == CapabilitySubmission::Uncertain
            })
            .await?;
            assert!(handle.snapshot().operation.is_none());
            // Explicit read reconciliation after a completely lost response.
            // The controller stays uncertain; this never adopts or resends a command.
            let listed = connection
                .operations()
                .list_operations(
                    operations::ListOperationsRequest {
                        name: target.home().as_str().to_owned(),
                        filter: format!("metadata.request_id=\"{}\"", id.as_str()),
                        page_size: 2,
                        ..Default::default()
                    },
                    lease.token(),
                    Duration::from_secs(10),
                    &cancel,
                )
                .await?;
            if listed.operations.len() != 1
                || !listed.next_page_token.is_empty()
                || !listed.__buffa_unknown_fields.is_empty()
                || !listed.unreachable.is_empty()
            {
                return Err("exact original operation discovery required".into());
            }
            let admitted = admit_command_operation(&listed.operations[0], &command, None)
                .map_err(|e| format!("operation provenance: {e:?}"))?;
            let raw = connection
                .operations()
                .get_operation(
                    operations::GetOperationRequest {
                        name: admitted.name().as_str().to_owned(),
                        ..Default::default()
                    },
                    lease.token(),
                    Duration::from_secs(10),
                    &cancel,
                )
                .await?;
            let current = admit_command_operation(&raw, &command, Some(&admitted))
                .map_err(|e| format!("Get provenance: {e:?}"))?;
            let raw = connection
                .operations()
                .wait_operation(
                    operations::WaitOperationRequest {
                        name: current.name().as_str().to_owned(),
                        timeout: buffa_types::google::protobuf::Duration {
                            seconds: 1,
                            ..Default::default()
                        }
                        .into(),
                        ..Default::default()
                    },
                    lease.token(),
                    Duration::from_secs(3),
                    &cancel,
                )
                .await?;
            let next = admit_command_operation(&raw, &command, Some(&current))
                .map_err(|e| format!("Wait provenance: {e:?}"))?;
            assert_eq!(next.phase(), CommandPhase::Acknowledged);
            assert_eq!(next.completion(), &CommandCompletion::Pending);
            assert_eq!(
                handle.snapshot().submission,
                CapabilitySubmission::Uncertain
            );
            assert!(handle.snapshot().operation.is_none());
            handle.stop_tracking();
            assert_eq!(handle.snapshot().tracking, CapabilityTracking::Stopped);
        }
        Scenario::AckOnly | Scenario::Expire => {
            until(&handle, |value| {
                value
                    .operation
                    .as_ref()
                    .is_some_and(|op| op.phase() == CommandPhase::Acknowledged)
            })
            .await?;
            let snapshot = handle.snapshot();
            assert_eq!(snapshot.submission, CapabilitySubmission::Accepted);
            assert_eq!(snapshot.tracking, CapabilityTracking::Live);
            let op = snapshot
                .operation
                .as_ref()
                .ok_or("missing acknowledged operation")?;
            let raw = connection
                .operations()
                .get_operation(
                    operations::GetOperationRequest {
                        name: op.name().as_str().to_owned(),
                        ..Default::default()
                    },
                    lease.token(),
                    Duration::from_secs(10),
                    &cancel,
                )
                .await?;
            let read = admit_command_operation(&raw, &command, Some(op))
                .map_err(|e| format!("Get provenance: {e:?}"))?;
            assert_eq!(read.phase(), CommandPhase::Acknowledged);
            if matches!(fixture.scenario, Scenario::Expire) {
                until(&handle, |value| {
                    value
                        .operation
                        .as_ref()
                        .is_some_and(|op| op.phase() == CommandPhase::Indeterminate)
                })
                .await?;
                let expired = handle.snapshot();
                let Some(CommandCompletion::Failure(fault)) =
                    expired.operation.as_ref().map(|op| op.completion())
                else {
                    return Err("expiry needs typed uncertainty".into());
                };
                assert_eq!(fault.reason(), wire::ApiErrorReason::EffectOutcomeUnknown);
            } else {
                tokio::time::sleep(Duration::from_millis(400)).await;
                assert_eq!(
                    handle
                        .snapshot()
                        .operation
                        .as_ref()
                        .ok_or("ack disappeared")?
                        .phase(),
                    CommandPhase::Acknowledged
                );
                handle.stop_tracking();
                assert_eq!(handle.snapshot().tracking, CapabilityTracking::Stopped);
            }
        }
        Scenario::Refusal => {
            until(&handle, |value| {
                value.submission == CapabilitySubmission::Rejected
            })
            .await?;
            let snapshot = handle.snapshot();
            assert_eq!(
                snapshot
                    .failure
                    .as_ref()
                    .ok_or("missing typed stale-CAS refusal")?
                    .reason(),
                wire::ApiErrorReason::EtagMismatch
            );
            assert!(snapshot.operation.is_none());
        }
        Scenario::Withdrawn => {
            until(&handle, |value| {
                value.submission == CapabilitySubmission::NotSent
            })
            .await?;
            assert!(handle.snapshot().operation.is_none());
        }
    }
    let (after, observed) = capture(&connection, &lease, &cancel).await?;
    let (after_slot, value) = power_slot(&observed)?;
    assert_eq!(after_slot.name, target.as_str());
    assert_eq!(
        value,
        Power::Off,
        "ACK/uncertainty is never an observed effect"
    );
    for name in [view.name, after.name] {
        connection
            .entity_views()
            .delete_entity_view(
                wire::DeleteEntityViewRequest {
                    name,
                    ..Default::default()
                },
                lease.token(),
                Duration::from_secs(10),
                &cancel,
            )
            .await?;
    }
    controller.shutdown().await;
    session.close().await;
    println!(
        "NHF006_RESULT {}",
        serde_json::json!({"pass":true,"request_id":id.as_str(),"observed_unchanged":true})
    );
    Ok(())
}

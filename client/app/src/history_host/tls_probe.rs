//! Actual Rust owner against the ordinary Go Run/Recorder/HistoryService on a
//! fresh synthetic database. Native rendering and cost mocks are separate proofs.
use super::*;
use home_client_session::{SecureStore, StoreFailure};
use home_client_transport::Connection;
use home_wire_contract::home::{history::v1 as wire, state::v1 as state_wire};
use serde::{Deserialize, Serialize};
use std::{io::Write as _, time::Duration};
use zeroize::Zeroizing;

type Result<T> = std::result::Result<T, Box<dyn std::error::Error + Send + Sync>>;
fn admit<T, E: std::fmt::Debug>(value: std::result::Result<T, E>) -> Result<T> {
    value.map_err(|error| format!("history qualification: {error:?}").into())
}
#[derive(Deserialize)]
#[serde(rename_all = "kebab-case")]
enum Mode {
    SyntheticOnly,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Credential {
    login_name: String,
    password: String,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Fixture {
    mode: Mode,
    origin: String,
    ca_pem: String,
    entity_id: String,
    point_count: usize,
    start: String,
    end: String,
    reader: Credential,
    costs_mock: bool,
}
#[derive(Default)]
struct MemoryStore(Mutex<Option<Zeroizing<Vec<u8>>>>);
impl SecureStore for MemoryStore {
    fn load(&self) -> std::result::Result<Option<Zeroizing<Vec<u8>>>, StoreFailure> {
        Ok(self.0.lock().unwrap().clone())
    }
    fn save(&self, value: &[u8]) -> std::result::Result<(), StoreFailure> {
        *self.0.lock().unwrap() = Some(Zeroizing::new(value.to_vec()));
        Ok(())
    }
    fn clear(&self) -> std::result::Result<(), StoreFailure> {
        *self.0.lock().unwrap() = None;
        Ok(())
    }
}
#[derive(Clone, Copy, PartialEq, Eq, Deserialize, Serialize)]
#[serde(rename_all = "kebab-case")]
enum Control {
    PublishStale,
    RevokeHistory,
}
async fn control(sequence: u64, action: Control) -> Result<()> {
    println!(
        "NHF013_CONTROL {}",
        serde_json::json!({"sequence":sequence,"action":action})
    );
    std::io::stdout().flush()?;
    let input = tokio::task::spawn_blocking(|| {
        let mut input = String::new();
        std::io::stdin().read_line(&mut input).map(|_| input)
    })
    .await??;
    #[derive(Deserialize)]
    #[serde(deny_unknown_fields)]
    struct Ack {
        sequence: u64,
        action: Control,
    }
    let ack: Ack = serde_json::from_str(&input)?;
    if ack.sequence != sequence || ack.action != action {
        return Err("unbound history control".into());
    }
    Ok(())
}
async fn until(owner: &History, label: &str, predicate: impl Fn(&State) -> bool) -> Result<()> {
    if tokio::time::timeout(Duration::from_secs(60), async {
        loop {
            if predicate(&owner.0.state.lock().unwrap()) {
                return;
            }
            tokio::time::sleep(Duration::from_millis(10)).await
        }
    })
    .await
    .is_err()
    {
        let state = owner.0.state.lock().unwrap();
        return Err(format!(
            "history owner did not reach {label}: status={:?}, active={}, generation={}, points={}",
            state.status,
            state.active,
            state.generation,
            state.data.as_ref().map_or(0, |data| data.snapshot.points)
        )
        .into());
    }
    Ok(())
}
async fn run(mut fixture: Fixture) -> Result<()> {
    let Mode::SyntheticOnly = fixture.mode;
    let url = url::Url::parse(&fixture.origin)?;
    if url.scheme() != "https"
        || url.host_str() != Some("127.0.0.1")
        || url.port().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.path() != "/"
        || url.query().is_some()
        || url.fragment().is_some()
    {
        return Err("explicit loopback TLS fixture required".into());
    }
    assert_eq!(fixture.point_count, 20_000);
    let entity: EntityId = admit(fixture.entity_id.parse())?;
    let connection = Connection::new(&fixture.origin, fixture.ca_pem.as_bytes())?;
    let session = SessionCoordinator::new(connection.auth(), Arc::new(MemoryStore::default()));
    admit(
        session
            .login(
                std::mem::take(&mut fixture.reader.login_name),
                Zeroizing::new(std::mem::take(&mut fixture.reader.password)),
            )
            .await,
    )?;
    if fixture.costs_mock {
        return run_costs(&fixture, entity, connection, session).await;
    }
    let cancel = CancellationToken::new();
    let lease = admit(session.access(&cancel).await)?;
    let (initial_query, _) = admit(parse_query(
        Some(entity.clone()),
        &fixture.start,
        &fixture.end,
        HistoryStatistic::Observation,
    ))?;
    let capture =
        admit(history::read_history(&connection.history(), &lease, initial_query, &cancel).await)?;
    assert_eq!(capture.points, 20000);
    let owner = History::detached();
    let (running, credential, transport, stop) = (
        owner.clone(),
        session.clone(),
        connection.history(),
        cancel.clone(),
    );
    let task = tokio::spawn(async move { running.run(credential, transport, stop).await });
    until(&owner, "active session", |state| state.active).await?;
    owner.choose(Some(entity.clone()));
    owner.apply(&fixture.start, &fixture.end, HistoryStatistic::Observation);
    until(&owner, "20k original observations", |state| {
        state.status == Status::Ready
    })
    .await?;
    let generation = {
        let state = owner.0.state.lock().unwrap();
        let data = state.data.as_ref().ok_or("history data absent")?;
        assert_eq!(data.snapshot.points, 20_000);
        assert_eq!(data.groups.len(), 1);
        assert_eq!(data.groups[0].series.samples().len(), 20_000);
        assert_eq!(data.groups[0].series.samples()[0].value(), 0.);
        assert_eq!(data.groups[0].value(19_999).unwrap().text(), "19.999");
        assert_eq!(data.groups[0].descriptor.unit, "kWh");
        let Content::Retained(history::Pages::Observations(pages)) = &data.snapshot.content else {
            return Err("raw pages missing".into());
        };
        assert!(pages.len() >= 200);
        let first = &pages[0].points[0].state;
        assert_eq!(first.received_at.seconds - first.observed_at.seconds, 3600);
        let frame = state.frame.as_ref().ok_or("history bitmap absent")?;
        assert_eq!(frame.stats().source_samples, 20_000);
        assert!(frame.stats().projected_points <= 4 * usize::from(state.width) + 2);
        state.generation
    };
    owner.navigate(generation, NavigationStep::Last, false);
    assert_eq!(owner.0.state.lock().unwrap().cursor, Some(19_999));
    owner.navigate(generation, NavigationStep::First, false);
    assert_eq!(owner.0.state.lock().unwrap().cursor, Some(0));
    let lease = admit(session.access(&cancel).await)?;
    let (query, _) = admit(parse_query(
        Some(entity.clone()),
        &fixture.start,
        &fixture.end,
        HistoryStatistic::Observation,
    ))?;
    let first = connection
        .history()
        .query_observations(
            wire::QueryObservationsRequest {
                entity_id: entity.as_str().into(),
                period: query.period().clone().into(),
                resolution: wire::Resolution::Raw.into(),
                page_size: Some(1),
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(3),
            &cancel,
        )
        .await?;
    let Some(wire::query_observations_response::Result::Page(page)) = first.result else {
        return Err("first keyset page absent".into());
    };
    let Some(wire::continuation::Result::NextPageToken(token)) = &page.continuation.result else {
        return Err("keyset continuation absent".into());
    };
    let mut changed = query.period().clone();
    changed.start.get_or_insert_default().nanos = 1;
    let stale = connection
        .history()
        .query_observations(
            wire::QueryObservationsRequest {
                entity_id: entity.as_str().into(),
                period: changed.clone().into(),
                resolution: wire::Resolution::Raw.into(),
                page_size: Some(1),
                page_token: token.clone(),
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(3),
            &cancel,
        )
        .await?;
    assert!(matches!(
        stale.result,
        Some(wire::query_observations_response::Result::StaleCursor(_))
    ));
    let invalid = connection
        .history()
        .query_aggregates(
            wire::QueryAggregatesRequest {
                entity_id: entity.as_str().into(),
                period: changed.into(),
                resolution: wire::Resolution::Hour.into(),
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(3),
            &cancel,
        )
        .await;
    assert!(
        matches!(invalid,Err(error) if error.kind()==FailureKind::Rpc(RpcCode::InvalidArgument))
    );
    let denied = connection
        .history()
        .query_observations(
            wire::QueryObservationsRequest {
                entity_id: "foreign-synthetic-entity".into(),
                period: query.period().clone().into(),
                resolution: wire::Resolution::Raw.into(),
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(3),
            &cancel,
        )
        .await;
    assert!(
        matches!(denied,Err(error) if error.kind()==FailureKind::Rpc(RpcCode::PermissionDenied))
    );
    owner.apply(&fixture.start, &fixture.end, HistoryStatistic::Mean);
    until(&owner, "counter without an invented mean", |state| {
        state.status == Status::Empty
    })
    .await?;
    {
        let state = owner.0.state.lock().unwrap();
        let data = state.data.as_ref().ok_or("counter aggregate absent")?;
        assert!(data.groups.is_empty());
        let Content::Retained(history::Pages::Hourly(pages)) = &data.snapshot.content else {
            return Err("counter aggregate pages absent".into());
        };
        assert!(
            pages
                .iter()
                .flat_map(|page| &page.points)
                .all(|point| point.mean.is_none())
        );
    }
    owner.apply(&fixture.start, &fixture.end, HistoryStatistic::Contribution);
    until(&owner, "real server counter contribution", |state| {
        state.status == Status::Ready
    })
    .await?;
    {
        let state = owner.0.state.lock().unwrap();
        let data = state.data.as_ref().ok_or("aggregate data absent")?;
        assert_eq!(data.statistic, Statistic::Contribution);
        let Content::Retained(history::Pages::Hourly(pages)) = &data.snapshot.content else {
            return Err("aggregate pages absent".into());
        };
        assert!(!pages[0].points.is_empty());
        assert!(
            pages
                .iter()
                .flat_map(|page| &page.points)
                .any(|point| point.contribution.is_some())
        );
    }
    control(1, Control::PublishStale).await?;
    owner.apply(&fixture.start, &fixture.end, HistoryStatistic::Observation);
    until(&owner, "recorded restored quality", |state| {
        state.status == Status::Ready
    })
    .await?;
    {
        let state = owner.0.state.lock().unwrap();
        let data = state.data.as_ref().ok_or("restored history absent")?;
        assert_eq!(data.snapshot.points, 20_001);
        assert!(data.groups.iter().any(|group| group.quality.freshness
            == state_wire::Freshness::Stale
            && group.quality.provenance == state_wire::Provenance::Restored));
    }
    control(2, Control::RevokeHistory).await?;
    owner.apply(&fixture.start, &fixture.end, HistoryStatistic::Observation);
    until(&owner, "revocation purge", |state| {
        state.data.is_none()
            && state.frame.is_none()
            && matches!(
                state.status,
                Status::Failed(Failure::Denied) | Status::Draft
            )
    })
    .await?;
    cancel.cancel();
    task.await?;
    session.close().await;
    assert!(owner.0.state.lock().unwrap().data.is_none());
    println!(
        "NHF013_REPORT {}",
        serde_json::json!({"pass":true,"points":20000,"restored_points":20001,"actual_history_tls":true,"ordinary_run_recorder":true,"server_aggregates":true,"stale_bound_cursor":true,"invalid_hour_rejected":true,"foreign_entity_denied":true,"revocation_purge":true,"owners_joined":true,"costs_mock":false,"native_rendering":false})
    );
    Ok(())
}
async fn run_costs(
    fixture: &Fixture,
    entity: EntityId,
    connection: Connection,
    session: SessionCoordinator,
) -> Result<()> {
    let owner = History::detached();
    let cancel = CancellationToken::new();
    let (running, credential, transport, stop) = (
        owner.clone(),
        session.clone(),
        connection.history(),
        cancel.clone(),
    );
    let task = tokio::spawn(async move { running.run(credential, transport, stop).await });
    until(&owner, "cost mock active session", |state| state.active).await?;
    owner.choose(Some(entity));
    owner.apply(&fixture.start, &fixture.end, HistoryStatistic::Observation);
    until(&owner, "exact cost mock values", |state| {
        state.status == Status::Ready
    })
    .await?;
    {
        let state = owner.0.state.lock().unwrap();
        let data = state.data.as_ref().ok_or("cost mock absent")?;
        assert_eq!(data.snapshot.points, 2);
        assert!(data.incomplete_coverage);
        assert_eq!(data.groups.len(), 1);
        let group = &data.groups[0];
        assert_eq!(group.descriptor.unit, "CAD");
        assert_eq!(group.descriptor.canonical_unit, "CAD");
        assert_eq!(group.value(0).unwrap().text(), "0.00");
        assert_eq!(
            group.value(1).unwrap().text(),
            "123456789012345678901234567890.123456789"
        );
        assert_eq!(group.quality.provenance, state_wire::Provenance::Estimated);
        let Content::Retained(history::Pages::Observations(pages)) = &data.snapshot.content else {
            return Err("cost pages missing".into());
        };
        assert!(pages.len() >= 200);
        assert!(pages.iter().any(|page| page.points.is_empty()));
        assert!(pages.iter().flat_map(|page| &page.points).all(|point| {
            point
                .state
                .source_session
                .starts_with("MOCK coûts synthétiques")
        }));
        assert_eq!(
            state
                .frame
                .as_ref()
                .ok_or("cost raster missing")?
                .stats()
                .source_samples,
            2
        );
    }
    // The mock must preserve actual authorization rather than grant a separate
    // test-only bypass around the ordinary server policy.
    control(1, Control::RevokeHistory).await?;
    owner.apply(&fixture.start, &fixture.end, HistoryStatistic::Observation);
    until(&owner, "cost mock revocation", |state| {
        state.data.is_none()
            && state.frame.is_none()
            && matches!(
                state.status,
                Status::Failed(Failure::Denied) | Status::Draft
            )
    })
    .await?;
    cancel.cancel();
    task.await?;
    session.close().await;
    println!(
        "NHF013_REPORT {}",
        serde_json::json!({"pass":true,"points":2,"costs_mock":true,"currency":"CAD","exact_zero":"0.00","exact_last":"123456789012345678901234567890.123456789","empty_progress_pages":true,"revocation_purge":true,"owners_joined":true,"production_costs":false,"native_rendering":false})
    );
    Ok(())
}
#[tokio::test]
#[ignore = "requires an explicit isolated Go history server and protocol harness"]
async fn real_tls_history_owner_and_source_values() -> Result<()> {
    let path = std::env::var("HOMEWARD_NHF013_READY")?;
    let bytes = std::fs::read(path)?;
    if bytes.len() > 65536 {
        return Err("oversized history fixture".into());
    }
    run(serde_json::from_slice(&bytes)?).await
}

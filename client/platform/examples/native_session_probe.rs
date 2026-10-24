//! Preliminary native macOS session/Keychain proof. The Android proof requires
//! an APK context and does not run this ELF. Input is a private synthetic fixture.

#![deny(clippy::wildcard_enum_match_arm)]

#[cfg(target_os = "macos")]
#[path = "support/session_scenario.rs"]
mod session_scenario;

#[cfg(target_os = "macos")]
#[derive(Clone, Copy)]
enum Completion {
    Logout,
    Persist,
    Rotate,
    LoseResponse,
}

#[cfg(target_os = "macos")]
#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    use home_client_platform::NativeStore;
    use home_client_session::{
        CredentialKey, Phase, SecureStore, ServerRevocation, SessionCoordinator, SessionFailure,
    };
    use home_client_transport::{Connection, FailureKind, RpcCode};
    use home_wire_contract::home::auth::v1 as auth;
    use session_scenario::SessionScenario;
    use sha2::{Digest, Sha256};
    use std::{sync::Arc, time::Duration};
    use tokio_util::sync::CancellationToken;
    use zeroize::Zeroizing;

    let args: Vec<_> = std::env::args().collect();
    if args.len() != 3 {
        return Err("private synthetic fixture and scenario required".into());
    }
    let scenario: SessionScenario = args[2].parse()?;
    let fixture: serde_json::Value = serde_json::from_slice(&std::fs::read(&args[1])?)?;
    if fixture["mode"] != "synthetic-only" {
        return Err("synthetic-only fixture required".into());
    }
    let text = |name: &str| fixture[name].as_str().ok_or("incomplete private fixture");
    let origin = text("origin")?;
    if !origin.starts_with("https://127.0.0.1:") {
        return Err("isolated loopback fixture required".into());
    }
    let connection = Connection::new(origin, text("ca_pem")?.as_bytes())?;
    // Test storage can never select an arbitrary product credential namespace.
    // It is domain separated and bound to this endpoint, trust and fresh nonce.
    let mut digest = Sha256::new();
    for value in [
        "NHF003-synthetic-session-proof-v1",
        origin,
        text("ca_pem")?,
        text("namespace_nonce")?,
    ] {
        digest.update((value.len() as u64).to_le_bytes());
        digest.update(value.as_bytes());
    }
    let store = Arc::new(NativeStore::new(CredentialKey::from_digest(
        digest.finalize().into(),
    ))?);
    let owner = SessionCoordinator::new(connection.auth(), store.clone());
    if !scenario.requires_stored_record() && store.load()?.is_some() {
        return Err("preserve an existing namespace".into());
    }
    let result = async {
        let login = |password: String| owner.login(text("login_name").expect("fixture login").into(), Zeroizing::new(password));
        let completion = match scenario {
            SessionScenario::Cleanup => {
                let _ = owner.restore().await;
                owner.logout_local().await?;
                if store.load()?.is_some() { return Err("synthetic namespace not cleared".into()); }
                return Ok(serde_json::json!({"scenario":scenario,"local_deleted":true}));
            }
            SessionScenario::RestoreUncertain => {
                if owner.restore().await != Err(SessionFailure::NeedsLogin) || store.load()?.is_some() { return Err("consumed refresh survived restart".into()); }
                return Ok(serde_json::json!({"scenario":scenario,"consumed_refresh_refused":true}));
            }
            SessionScenario::WrongPassword => {
                let result = login("synthetic incorrect password".into()).await;
                if result.is_ok() || owner.subscribe().borrow().phase != Phase::CredentialRejected || store.load()?.is_some() { return Err("native password refusal conflated or persisted".into()); }
                return Ok(serde_json::json!({"scenario":scenario,"distinct_refusal":true}));
            }
            SessionScenario::RateLimit => {
                let result = login(text("password")?.into()).await;
                if result.is_ok() || owner.subscribe().borrow().phase != Phase::RateLimited || store.load()?.is_some() { return Err("native rate limit conflated or persisted".into()); }
                return Ok(serde_json::json!({"scenario":scenario,"distinct_refusal":true}));
            }
            SessionScenario::Enrollment => {
                login(text("password")?.into()).await?;
                if owner.subscribe().borrow().phase != Phase::EnrollmentRequired || owner.access(&CancellationToken::new()).await.is_ok() || store.load()?.is_some() { return Err("enrollment grant opened a data session".into()); }
                return Ok(serde_json::json!({"scenario":scenario,"enrollment_blocks_data":true,"client_fido2_qualified":false}));
            }
            SessionScenario::Recovery => {
                login(text("password")?.into()).await?;
                if !matches!(owner.subscribe().borrow().phase, Phase::Challenge { recovery:true, fido2:true }) || owner.access(&CancellationToken::new()).await.is_ok() { return Err("challenge became a session".into()); }
                owner.complete_recovery(Zeroizing::new(text("recovery_code")?.into())).await?;
                Completion::Logout
            }
            SessionScenario::Login => { login(text("password")?.into()).await?; Completion::Logout }
            SessionScenario::Persist => { login(text("password")?.into()).await?; Completion::Persist }
            SessionScenario::Restore => { owner.restore().await?; Completion::Logout }
            SessionScenario::Concurrent => { login(text("password")?.into()).await?; Completion::Rotate }
            SessionScenario::Lose => { login(text("password")?.into()).await?; Completion::LoseResponse }
        };
        let initial = owner.access(&CancellationToken::new()).await?;
        let request = || auth::ListSessionsRequest { account_id: auth::AccountId { value:text("account_id").expect("fixture account").into(), ..Default::default() }.into(), ..Default::default() };
        let page = connection.auth().list_sessions(request(), initial.token(), Duration::from_secs(5), &initial.cancellation).await?;
        if page.sessions.is_empty() { return Err("native protected session read empty".into()); }
        let expect_rotation = match completion {
            Completion::Persist => return Ok(serde_json::json!({"scenario":scenario,"protected_read":true,"secure_record_saved":true})),
            Completion::Logout => None,
            Completion::Rotate => Some(true),
            Completion::LoseResponse => Some(false),
        };
        if let Some(expect_rotation) = expect_rotation {
            tokio::time::sleep(Duration::from_millis(3300)).await;
            let mut readers = Vec::new();
            for _ in 0..10 { let owner=owner.clone(); readers.push(tokio::spawn(async move { owner.access(&CancellationToken::new()).await })); }
            for reader in readers {
                let lease=reader.await?;
                if expect_rotation {
                    if !owner.is_current(lease?.generation) { return Err("refreshed lease obsolete".into()); }
                } else if !matches!(lease, Err(SessionFailure::RefreshUncertain(_))) { return Err("lost refresh did not require new login".into()); }
            }
            if !initial.cancellation.is_cancelled() { return Err("rotation did not cancel old readers".into()); }
            let rejected=connection.auth().list_sessions(request(),initial.token(),Duration::from_secs(5),&CancellationToken::new()).await.expect_err("native old access survived rotation");
            if rejected.kind()!=FailureKind::Rpc(RpcCode::Unauthenticated) { return Err(rejected.into()); }
            if !expect_rotation { return Ok(serde_json::json!({"scenario":scenario,"ten_waiters":true,"old_access_rejected":true,"lost_response_requires_login":true})); }
            let renewed = owner.access(&CancellationToken::new()).await?;
            let page = connection.auth().list_sessions(request(), renewed.token(), Duration::from_secs(5), &renewed.cancellation).await?;
            if page.sessions.is_empty() { return Err("renewed native access failed its protected read".into()); }
        }
        if owner.logout().await? != ServerRevocation::Acknowledged || store.load()?.is_some() { return Err("native revocation/local deletion not acknowledged".into()); }
        Ok(serde_json::json!({"scenario":scenario,"protected_read":true,"native_revocation":true,"local_deleted":true,"ten_waiters":scenario==SessionScenario::Concurrent,"renewed_protected_read":scenario==SessionScenario::Concurrent,"recovery":scenario==SessionScenario::Recovery,"client_fido2_qualified":false}))
    }.await;
    owner.close().await;
    let result: Result<serde_json::Value, Box<dyn std::error::Error>> = result;
    println!("NHF003_RESULT {}", result?);
    Ok(())
}

#[cfg(not(target_os = "macos"))]
fn main() {
    panic!("The Android session proof must run inside the APK.");
}

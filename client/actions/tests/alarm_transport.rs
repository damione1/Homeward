//! Real loopback TLS, the production session/controller and one-shot PIN wire.
//! No real driver, endpoint or credentials. Certificate fixtures are shared with transport.
use base64::Engine as _;
use buffa::Message;
use buffa_types::google::protobuf::{Any, Timestamp};
use connectrpc::{ConnectError, ErrorCode, ErrorDetail};
use home_client_actions::capabilities::{
    AlarmCode, CapabilityController, CapabilityHandle, CapabilitySubmission, CapabilityTracking,
};
use home_client_model::api::{
    AlarmOperation, CapabilityName, Command, Deadline, DefinitionEtag, Intent,
};
use home_client_session::{SecureStore, SessionCoordinator, StoreFailure};
use home_client_transport::Connection;
use home_wire_contract::{
    api_command_operations::{CommandCompletion, CommandOutcome},
    google::{longrunning as lro, rpc},
    home::api::v2 as api,
};
use std::{
    io::{Cursor, Read, Write},
    net::TcpListener,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};
use tokio_util::sync::CancellationToken;
use zeroize::Zeroizing;
const CA: &[u8] = include_bytes!("../../transport/tests/fixtures/synthetic-loopback-ca.pem");
const CERT: &[u8] = include_bytes!("../../transport/tests/fixtures/synthetic-loopback-leaf.pem");
const KEY: &[u8] = include_bytes!("../../transport/tests/fixtures/synthetic-loopback-key.pem");
const CODE: &[u8] = b"058290";
const OPERATION: &str = "homes/synthetic/operations/command-one";
fn now() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_secs() as i64
}
fn any<T: Message>(name: &str, value: T) -> Any {
    Any {
        type_url: format!("type.googleapis.com/{name}"),
        value: value.encode_to_vec().into(),
        ..Default::default()
    }
}
#[derive(Clone, Copy)]
enum Reply {
    Sent,
    Lost,
    Reconcile,
}
#[derive(Default)]
struct Counts {
    executions: AtomicUsize,
    reads: AtomicUsize,
}
struct Fixture {
    origin: String,
    counts: Arc<Counts>,
    stop: Arc<AtomicBool>,
    worker: Option<std::thread::JoinHandle<()>>,
}
impl Drop for Fixture {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        if let Some(worker) = self.worker.take() {
            let result = worker.join();
            if !std::thread::panicking() {
                result.unwrap();
            }
        }
    }
}
fn operation(capability: &str, request_id: &str) -> lro::Operation {
    lro::Operation {
        name: OPERATION.into(),
        done: true,
        metadata: any(
            "home.api.v2.CommandMetadata",
            api::CommandMetadata {
                capability: capability.into(),
                request_id: request_id.into(),
                create_time: Timestamp::from_unix_secs(now()).into(),
                phase: api::CommandPhase::Dispatched.into(),
                ..Default::default()
            },
        )
        .into(),
        result: Some(lro::operation::Result::Response(Box::new(any(
            "home.api.v2.ExecuteCapabilityResponse",
            api::ExecuteCapabilityResponse {
                capability: capability.into(),
                outcome: api::CommandOutcome::SentUnverified.into(),
                ..Default::default()
            },
        )))),
        ..Default::default()
    }
}
fn uncertainty() -> Vec<u8> {
    let mut error = ConnectError::new(
        ErrorCode::Unknown,
        "synthetic submission outcome unavailable",
    );
    for value in [
        any(
            "google.rpc.ErrorInfo",
            rpc::ErrorInfo {
                domain: home_wire_contract::api_errors::DOMAIN.into(),
                reason: "EFFECT_OUTCOME_UNKNOWN".into(),
                ..Default::default()
            },
        ),
        any(
            "home.api.v2.ExecutionUncertainty",
            api::ExecutionUncertainty {
                phase: api::UncertaintyPhase::Effect.into(),
                disposition: api::ReconciliationDisposition::ReconcileOnly.into(),
                ..Default::default()
            },
        ),
        any(
            "google.rpc.ResourceInfo",
            rpc::ResourceInfo {
                resource_type: "longrunning.googleapis.com/Operation".into(),
                resource_name: OPERATION.into(),
                ..Default::default()
            },
        ),
    ] {
        error.details.push(ErrorDetail {
            type_url: value.type_url,
            value: Some(base64::engine::general_purpose::STANDARD.encode(value.value)),
            debug: None,
        });
    }
    serde_json::to_vec(&error).unwrap()
}
fn fixture(reply: Reply) -> Fixture {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    listener.set_nonblocking(true).unwrap();
    let origin = format!("https://{}", listener.local_addr().unwrap());
    let counts = Arc::new(Counts::default());
    let stop = Arc::new(AtomicBool::new(false));
    let observed = counts.clone();
    let stopping = stop.clone();
    let worker = std::thread::spawn(move || {
        let certificates = rustls_pemfile::certs(&mut Cursor::new(CERT))
            .collect::<Result<Vec<_>, _>>()
            .unwrap();
        let key = rustls_pemfile::private_key(&mut Cursor::new(KEY))
            .unwrap()
            .unwrap();
        let mut config = rustls::ServerConfig::builder_with_provider(Arc::new(
            rustls::crypto::aws_lc_rs::default_provider(),
        ))
        .with_protocol_versions(&[&rustls::version::TLS13])
        .unwrap()
        .with_no_client_auth()
        .with_single_cert(certificates, key)
        .unwrap();
        config.alpn_protocols = vec![b"http/1.1".to_vec()];
        let config = Arc::new(config);
        let mut last: Option<lro::Operation> = None;
        while !stopping.load(Ordering::SeqCst) {
            let socket = match listener.accept() {
                Ok((socket, _)) => socket,
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                    std::thread::sleep(Duration::from_millis(5));
                    continue;
                }
                Err(e) => panic!("loopback accept: {e}"),
            };
            socket.set_nonblocking(false).unwrap();
            socket
                .set_read_timeout(Some(Duration::from_secs(3)))
                .unwrap();
            socket
                .set_write_timeout(Some(Duration::from_secs(3)))
                .unwrap();
            let mut stream = rustls::StreamOwned::new(
                rustls::ServerConnection::new(config.clone()).unwrap(),
                socket,
            );
            let mut request = Vec::new();
            let end = loop {
                let mut chunk = [0; 1024];
                let n = stream.read(&mut chunk).unwrap();
                assert!(n > 0);
                request.extend_from_slice(&chunk[..n]);
                assert!(request.len() <= 16384);
                if let Some(end) = request.windows(4).position(|v| v == b"\r\n\r\n") {
                    break end + 4;
                }
            };
            let header = std::str::from_utf8(&request[..end]).unwrap();
            let path = header
                .lines()
                .next()
                .unwrap()
                .split_whitespace()
                .nth(1)
                .unwrap()
                .to_owned();
            let length: usize = header
                .lines()
                .find_map(|line| {
                    let (key, value) = line.split_once(':')?;
                    key.eq_ignore_ascii_case("content-length")
                        .then(|| value.trim().parse().unwrap())
                })
                .unwrap();
            while request.len() < end + length {
                let mut chunk = [0; 1024];
                let n = stream.read(&mut chunk).unwrap();
                assert!(n > 0);
                request.extend_from_slice(&chunk[..n]);
                assert!(request.len() <= 16384);
            }
            let body = &request[end..end + length];
            let (status, content_type, response) = match path.as_str() {
                "/home.api.v2.AuthService/StartLogin" => {
                    let input = api::StartLoginRequest::decode_from_slice(body).unwrap();
                    assert_eq!(input.login_name, "synthetic");
                    let tokens = api::SessionTokens {
                        session: api::Session {
                            name: "homes/synthetic/accounts/test/sessions/test".into(),
                            create_time: Timestamp::from_unix_secs(now() - 1).into(),
                            expire_time: Timestamp::from_unix_secs(now() + 3600).into(),
                            authentication_satisfaction:
                                api::AuthenticationSatisfaction::PasswordOnly.into(),
                            authentication_time: Timestamp::from_unix_secs(now() - 1).into(),
                            ..Default::default()
                        }
                        .into(),
                        audience: api::CredentialAudience::Interactive.into(),
                        access_token: "synthetic-access".into(),
                        refresh_token: "synthetic-refresh".into(),
                        access_expire_time: Timestamp::from_unix_secs(now() + 300).into(),
                        ..Default::default()
                    };
                    (
                        "200 OK",
                        "application/proto",
                        api::StartLoginResponse {
                            decision: api::LoginDecision {
                                outcome: Some(api::login_decision::Outcome::Authenticated(
                                    Box::new(tokens),
                                )),
                                ..Default::default()
                            }
                            .into(),
                            ..Default::default()
                        }
                        .encode_to_vec(),
                    )
                }
                "/home.api.v2.CapabilityService/ExecuteCapability" => {
                    let input = api::ExecuteCapabilityRequest::decode_from_slice(body).unwrap();
                    assert_eq!(
                        input.ephemeral_alarm_code, CODE,
                        "leading zero must survive serialization"
                    );
                    assert!(
                        matches!(input.command,Some(api::execute_capability_request::Command::Alarm(ref value)) if value.operation.as_known()==Some(api::AlarmOperation::Disarm))
                    );
                    observed.executions.fetch_add(1, Ordering::SeqCst);
                    last = Some(operation(&input.name, &input.request_id));
                    match reply {
                        Reply::Sent => (
                            "200 OK",
                            "application/proto",
                            last.as_ref().unwrap().encode_to_vec(),
                        ),
                        Reply::Lost => continue,
                        Reply::Reconcile => (
                            "500 Internal Server Error",
                            "application/json",
                            uncertainty(),
                        ),
                    }
                }
                "/google.longrunning.Operations/GetOperation" => {
                    assert!(!body.windows(CODE.len()).any(|part| part == CODE));
                    let input = lro::GetOperationRequest::decode_from_slice(body).unwrap();
                    assert_eq!(input.name, OPERATION);
                    assert!(input.__buffa_unknown_fields.is_empty());
                    observed.reads.fetch_add(1, Ordering::SeqCst);
                    (
                        "200 OK",
                        "application/proto",
                        last.as_ref().unwrap().encode_to_vec(),
                    )
                }
                _ => panic!("unexpected synthetic RPC route"),
            };
            write!(stream,"HTTP/1.1 {status}\r\nContent-Type: {content_type}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",response.len()).unwrap();
            stream.write_all(&response).unwrap();
            stream.flush().unwrap();
        }
    });
    Fixture {
        origin,
        counts,
        stop,
        worker: Some(worker),
    }
}
#[derive(Default)]
struct MemoryStore(Mutex<Option<Zeroizing<Vec<u8>>>>);
impl SecureStore for MemoryStore {
    fn load(&self) -> Result<Option<Zeroizing<Vec<u8>>>, StoreFailure> {
        Ok(self.0.lock().unwrap().clone())
    }
    fn save(&self, value: &[u8]) -> Result<(), StoreFailure> {
        assert!(!value.windows(CODE.len()).any(|part| part == CODE));
        *self.0.lock().unwrap() = Some(Zeroizing::new(value.to_vec()));
        Ok(())
    }
    fn clear(&self) -> Result<(), StoreFailure> {
        *self.0.lock().unwrap() = None;
        Ok(())
    }
}
fn command() -> Command {
    Command::new(
        CapabilityName::try_from("homes/synthetic/entities/alarm/capabilities/native").unwrap(),
        Intent::Alarm(AlarmOperation::Disarm),
        Some(home_client_actions::new_request_id().unwrap()),
        Deadline::new(now() + 30, 0).unwrap(),
    )
    .with_definition_etag(DefinitionEtag::try_from("synthetic-definition").unwrap())
}
async fn finished(handle: &CapabilityHandle) {
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        let value = handle.snapshot();
        if value.tracking == CapabilityTracking::Complete
            || matches!(
                value.submission,
                CapabilitySubmission::NotSent | CapabilitySubmission::Uncertain
            )
        {
            return;
        };
        assert!(Instant::now() < deadline, "controller did not finish");
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
}
#[tokio::test]
async fn ephemeral_alarm_code_uses_one_tls_post_and_never_enters_recovery_or_snapshots() {
    for reply in [Reply::Sent, Reply::Lost, Reply::Reconcile] {
        let server = fixture(reply);
        let connection = Connection::new(&server.origin, CA).unwrap();
        let session = SessionCoordinator::new(connection.auth(), Arc::new(MemoryStore::default()));
        session
            .login("synthetic".into(), Zeroizing::new("synthetic".into()))
            .await
            .unwrap();
        let cancel = CancellationToken::new();
        let controller = CapabilityController::new(
            session.clone(),
            connection.capabilities(),
            connection.operations(),
            &cancel,
        );
        let checks = Arc::new(AtomicUsize::new(0));
        let guard = checks.clone();
        let denied = controller
            .submit_with_alarm_code(
                command(),
                Arc::new(move || guard.fetch_add(1, Ordering::SeqCst) == 0),
                AlarmCode::new(Zeroizing::new(CODE.to_vec())),
            )
            .unwrap();
        finished(&denied).await;
        assert_eq!(denied.snapshot().submission, CapabilitySubmission::NotSent);
        assert_eq!(server.counts.executions.load(Ordering::SeqCst), 0);
        let logical = command();
        assert!(
            home_wire_contract::api_contract::encode_command(&logical)
                .ephemeral_alarm_code
                .is_empty()
        );
        let handle = controller
            .submit_with_alarm_code(
                logical,
                Arc::new(|| true),
                AlarmCode::new(Zeroizing::new(CODE.to_vec())),
            )
            .unwrap();
        match reply {
            Reply::Reconcile => {
                let deadline = Instant::now() + Duration::from_secs(5);
                while handle.snapshot().tracking != CapabilityTracking::Complete {
                    assert!(Instant::now() < deadline);
                    tokio::time::sleep(Duration::from_millis(5)).await;
                }
            }
            Reply::Sent | Reply::Lost => finished(&handle).await,
        }
        let snapshot = handle.snapshot();
        assert!(!format!("{snapshot:?}").contains(std::str::from_utf8(CODE).unwrap()));
        match reply {
            Reply::Sent | Reply::Reconcile => assert!(matches!(
                snapshot.operation.as_ref().unwrap().completion(),
                CommandCompletion::Success {
                    outcome: CommandOutcome::SentUnverified,
                    ..
                }
            )),
            Reply::Lost => {
                assert_eq!(snapshot.submission, CapabilitySubmission::Uncertain);
                assert!(snapshot.operation.is_none());
            }
        }
        handle.stop_tracking();
        tokio::time::sleep(Duration::from_millis(30)).await;
        assert_eq!(server.counts.executions.load(Ordering::SeqCst), 1);
        assert_eq!(
            server.counts.reads.load(Ordering::SeqCst),
            usize::from(matches!(reply, Reply::Reconcile))
        );
        cancel.cancel();
        controller.shutdown().await;
        session.close().await;
        drop(server);
    }
}

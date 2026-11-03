//! Actual loopback TLS handshakes. These credentials belong only to this fixture.
use std::{
    io::{Cursor, Read, Write},
    net::TcpListener,
    sync::Arc,
    time::{Duration, Instant},
};

use connectrpc::ErrorCode;
use home_client_transport::{Connection, FailureKind};
use home_wire_contract::home::auth::v1::StartLoginRequest;
use tokio_util::sync::CancellationToken;

const CA: &[u8] = include_bytes!("fixtures/synthetic-loopback-ca.pem");
const CERT: &[u8] = include_bytes!("fixtures/synthetic-loopback-leaf.pem");
const KEY: &[u8] = include_bytes!("fixtures/synthetic-loopback-key.pem");

fn server(address: &str) -> (String, std::thread::JoinHandle<()>) {
    let listener = TcpListener::bind((address, 0)).unwrap();
    listener.set_nonblocking(true).unwrap();
    let origin = format!("https://{}", listener.local_addr().unwrap());
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
        let deadline = Instant::now() + Duration::from_secs(5);
        let socket = loop {
            match listener.accept() {
                Ok((socket, _)) => break socket,
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                    if Instant::now() >= deadline {
                        return;
                    }
                    std::thread::sleep(Duration::from_millis(5));
                }
                Err(error) => panic!("loopback accept: {error}"),
            }
        };
        socket.set_nonblocking(false).unwrap();
        socket
            .set_read_timeout(Some(Duration::from_secs(3)))
            .unwrap();
        socket
            .set_write_timeout(Some(Duration::from_secs(3)))
            .unwrap();
        let connection = rustls::ServerConnection::new(Arc::new(config)).unwrap();
        let mut stream = rustls::StreamOwned::new(connection, socket);
        let mut request = [0; 4096];
        // Certificate rejection is an expected terminal handshake outcome.
        if stream.read(&mut request).is_err() {
            return;
        }
        let body = r#"{"code":"unauthenticated","message":"synthetic fixture"}"#;
        let response = format!(
            "HTTP/1.1 401 Unauthorized\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
            body.len(),
        );
        let _ = stream.write_all(response.as_bytes());
        let _ = stream.flush();
    });
    (origin, worker)
}

async fn handshake(address: &str, roots: &[u8]) -> FailureKind {
    let (origin, worker) = server(address);
    let connection = Connection::new(&origin, roots).unwrap();
    let failure = connection
        .auth()
        .start_login(
            StartLoginRequest {
                login_name: "synthetic".into(),
                password: "synthetic".into(),
                ..Default::default()
            },
            Duration::from_secs(4),
            &CancellationToken::new(),
        )
        .await
        .unwrap_err();
    let outcome = failure.kind();
    worker.join().unwrap();
    outcome
}

#[tokio::test]
async fn system_trust_rejects_a_private_ca_while_explicit_trust_verifies_identity() {
    let system = handshake("127.0.0.1", b"").await;
    assert!(
        matches!(
            system,
            FailureKind::UntrustedCertificate | FailureKind::TlsRejected,
        ),
        "unexpected system trust result: {system:?}"
    );
    // Reaching the fixture's RPC denial proves the TLS handshake succeeded.
    assert_eq!(
        handshake("127.0.0.1", CA).await,
        FailureKind::Rpc(ErrorCode::Unauthenticated)
    );
    assert_eq!(handshake("::1", CA).await, FailureKind::WrongServerName);
}

use connectrpc::ConnectError;
use home_client_transport::{Connection, Failure};
use home_wire_contract::home::auth::v1 as auth;
use std::time::Duration;
use tokio_util::sync::CancellationToken;

#[tokio::test]
async fn refused_connection_proves_no_refresh_was_sent_but_remote_errors_and_lost_bodies_do_not() {
    let socket = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let origin = format!("https://{}", socket.local_addr().unwrap());
    drop(socket);
    let result = Connection::new(&origin, &[])
        .unwrap()
        .auth()
        .refresh_session(
            auth::RefreshSessionRequest {
                refresh_token: "synthetic-only".into(),
                ..Default::default()
            },
            Duration::from_secs(2),
            &CancellationToken::new(),
        )
        .await;
    let failure = match result {
        Err(failure) => failure,
        Ok(_) => panic!("unused loopback port must refuse connection"),
    };
    assert!(failure.request_not_dispatched());
    assert!(failure.may_resume_read());
    for error in [
        ConnectError::unavailable("remote unavailable"),
        ConnectError::deadline_exceeded("lost outcome"),
        ConnectError::internal("body lost").with_source(connectrpc::client::ResponseBodyReadError),
        ConnectError::canceled("cancelled"),
    ] {
        let failure = Failure::from(error);
        assert!(!failure.request_not_dispatched());
    }
}

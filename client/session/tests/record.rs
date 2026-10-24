use buffa::Message;
use buffa_types::google::protobuf::Timestamp;
use home_client_session::{RecordFailure, SecretRecord};
use home_wire_contract::home::auth::v1 as auth;

fn record() -> (SecretRecord, Timestamp) {
    let cwd = std::env::current_dir().unwrap();
    let bytes = std::fs::read(cwd.join("../../contracts/auth_golden/login-success.bin")).unwrap();
    let decision = auth::LoginDecision::decode_from_slice(&bytes).unwrap();
    let Some(auth::login_decision::Outcome::Authenticated(tokens)) = decision.outcome else {
        panic!("golden session missing")
    };
    let now = Timestamp {
        seconds: 1_792_314_000,
        ..Default::default()
    };
    (SecretRecord::new(*tokens, now.clone()).unwrap(), now)
}

#[test]
fn expired_access_restores_for_one_refresh_but_a_consumed_refresh_never_restores() {
    let (mut record, now) = record();
    let saved = record.encode();
    let later = Timestamp {
        seconds: now.seconds + 1000,
        ..Default::default()
    };
    let restored = SecretRecord::decode(&saved, &later).unwrap();
    assert!(restored.access_expired(&later));
    record.consume_refresh().unwrap();
    assert_eq!(
        record.consume_refresh(),
        Err(RecordFailure::RefreshConsumed)
    );
    assert!(matches!(
        SecretRecord::decode(&record.encode(), &now),
        Err(RecordFailure::RefreshConsumed)
    ));
}

#[test]
fn corrupt_storage_and_backwards_clock_do_not_expose_a_session() {
    let (record, now) = record();
    let mut bytes = record.encode();
    bytes[8] = 9;
    assert!(matches!(
        SecretRecord::decode(&bytes, &now),
        Err(RecordFailure::Invalid)
    ));
    assert!(matches!(
        SecretRecord::decode(&[0; 20], &now),
        Err(RecordFailure::Invalid)
    ));
    let before = Timestamp {
        seconds: now.seconds - 1,
        ..Default::default()
    };
    assert!(matches!(
        SecretRecord::decode(&record.encode(), &before),
        Err(RecordFailure::ClockMovedBackwards)
    ));
}

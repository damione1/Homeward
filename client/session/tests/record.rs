use buffa_types::google::protobuf::Timestamp;
use home_client_session::{RecordFailure, SecretRecord};
use home_wire_contract::home::api::v2 as auth;

fn record() -> (SecretRecord, Timestamp) {
    let now = Timestamp {
        seconds: 1_792_314_000,
        ..Default::default()
    };
    let tokens = auth::SessionTokens {
        session: auth::Session {
            name: "homes/test/accounts/person/sessions/interactive".into(),
            create_time: Timestamp {
                seconds: now.seconds - 60,
                ..Default::default()
            }
            .into(),
            expire_time: Timestamp {
                seconds: now.seconds + 3600,
                ..Default::default()
            }
            .into(),
            authentication_satisfaction: auth::AuthenticationSatisfaction::NotRecorded.into(),
            ..Default::default()
        }
        .into(),
        audience: auth::CredentialAudience::Interactive.into(),
        access_token: "synthetic-access".into(),
        refresh_token: "synthetic-refresh".into(),
        access_expire_time: Timestamp {
            seconds: now.seconds + 900,
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    (SecretRecord::new(tokens, now.clone()).unwrap(), now)
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

#[test]
fn unsupported_profile_formats_are_rejected_without_legacy_fallback() {
    let (record, now) = record();
    for magic in [b"HWSess01", b"HWSess02"] {
        let mut old = record.encode();
        old[..8].copy_from_slice(magic);
        assert!(matches!(
            SecretRecord::decode(&old, &now),
            Err(RecordFailure::Invalid)
        ));
    }
    assert_eq!(&record.encode()[..8], b"HWSess03");
}

#[test]
fn a_saved_deadline_cannot_discard_a_server_extended_session() {
    let (record, now) = record();
    let later = Timestamp {
        seconds: now.seconds + 365 * 86_400,
        ..Default::default()
    };
    let restored = SecretRecord::decode(&record.encode(), &later).unwrap();
    assert!(restored.access_expired(&later));
    assert!(restored.session_expired(&later));
}

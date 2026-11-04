use super::*;
use buffa_types::google::protobuf::Timestamp;

fn response() -> wire::ConfigurationDocument {
    wire::ConfigurationDocument {
        name: "homes/test/configurationDocuments/operational-main".into(),
        etag: "synthetic-current-etag".into(),
        id: wire::DocumentId {
            value: "operational.main".into(),
            ..Default::default()
        }
        .into(),
        kind: wire::DocumentKind::OperationalYaml.into(),
        schema_version: "homeward.operational/1".into(),
        syntax_profile: "homeward.operational-yaml/1".into(),
        version: wire::DocumentVersion {
            revision: 7,
            source_sha256: vec![1; 32],
            ..Default::default()
        }
        .into(),
        active: true,
        updated_at: Timestamp {
            seconds: 1_700_000_000,
            ..Default::default()
        }
        .into(),
        protected_source_reference: vec![3; 32],
        workspace_state: Some(wire::configuration_document::WorkspaceState::Workspace(
            Box::new(wire::WorkspaceObservation {
                source_sha256: vec![2; 32],
                dirty: wire::DirtyState::Unvalidated.into(),
                updated_at: Timestamp {
                    seconds: 1_700_000_001,
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            }),
        )),
        active_generation: wire::ActiveGeneration {
            revision: 6,
            manifest_sha256: vec![4; 32],
            ..Default::default()
        }
        .into(),
        resume_token: "x".repeat(118),
        ..Default::default()
    }
}

// Reservations and terminal native failures use the current standard envelope;
// they carry no invented commit target and never grant an implicit retry.
fn reservation(
    failure: Option<(wire::ApiErrorReason, home_wire_contract::google::rpc::Code)>,
) -> home_wire_contract::configuration_operations_contract::Publication {
    use buffa::{Enumeration, Message};
    use buffa_types::google::protobuf::Any;
    use home_wire_contract::google::{
        longrunning::{Operation, operation},
        rpc,
    };
    let metadata = wire::ConfigurationOperationMetadata {
        request_id: "11111111-1111-4111-8111-111111111111".into(),
        create_time: Timestamp {
            seconds: 1_700_000_000,
            ..Default::default()
        }
        .into(),
        phase: if failure.is_some() {
            wire::ConfigurationOperationPhase::Failed
        } else {
            wire::ConfigurationOperationPhase::Accepted
        }
        .into(),
        ..Default::default()
    };
    let result = failure.map(|(reason, code)| {
        operation::Result::Error(Box::new(rpc::Status {
            code: code as i32,
            details: vec![Any {
                type_url: "type.googleapis.com/google.rpc.ErrorInfo".into(),
                value: rpc::ErrorInfo {
                    domain: home_wire_contract::api_errors::DOMAIN.into(),
                    reason: reason
                        .proto_name()
                        .strip_prefix("API_ERROR_REASON_")
                        .unwrap()
                        .into(),
                    ..Default::default()
                }
                .encode_to_vec()
                .into(),
                ..Default::default()
            }],
            ..Default::default()
        }))
    });
    let raw = Operation {
        name: "homes/test/operations/config-11111111111111111111111111111111".into(),
        metadata: Any {
            type_url: "type.googleapis.com/home.api.v2.ConfigurationOperationMetadata".into(),
            value: metadata.encode_to_vec().into(),
            ..Default::default()
        }
        .into(),
        done: result.is_some(),
        result,
        ..Default::default()
    };
    home_wire_contract::configuration_operations_contract::admit_response(&raw).unwrap()
}

fn source(value: &[u8]) -> UserSource {
    UserSource::supplied(value.to_vec()).unwrap()
}
fn submitted() -> (Capture, SourceEdit) {
    let capture = Capture::admit(response()).unwrap();
    let mut draft = SourceEdit::open(capture.clone()).unwrap();
    draft.supply(source(b"workday: {}\n")).unwrap();
    draft.prepare_write(&capture).unwrap();
    (capture, draft)
}

#[test]
fn metadata_never_fills_the_editor_and_all_three_references_stay_independent() {
    let capture = Capture::admit(response()).unwrap();
    let mut draft = SourceEdit::open(capture.clone()).unwrap();
    assert!(draft.source().is_none());
    assert!(matches!(
        draft.prepare_write(&capture),
        Err(Refusal::Invalid)
    ));
    let exact = vec![b'#'; 1_048_576];
    draft
        .supply(UserSource::supplied(exact.clone()).unwrap())
        .unwrap();
    let request = draft.prepare_write(&capture).unwrap();
    assert_eq!(request.exact_yaml_source, exact);
    assert_eq!(request.expected.revision, 7);
    assert_eq!(request.expected.source_sha256, vec![1; 32]);
    assert_eq!(request.expected_workspace_sha256, vec![2; 32]);
    assert_eq!(capture.active().revision(), 6);
    assert_eq!(capture.active().manifest().bytes(), &[4; 32]);
    assert!(UserSource::supplied(vec![0; 1_048_577]).is_err());
    assert!(UserSource::supplied(Vec::new()).is_err());
}

#[test]
fn same_source_hash_after_a_revision_change_and_external_workspace_changes_do_not_rebase() {
    let capture = Capture::admit(response()).unwrap();
    let mut changed = response();
    changed.version.as_option_mut().unwrap().revision = 8;
    let changed = Capture::admit(changed).unwrap();
    let mut draft = SourceEdit::open(capture.clone()).unwrap();
    draft.supply(source(b"# exact supplied bytes\n")).unwrap();
    assert!(matches!(draft.prepare_write(&changed), Err(Refusal::Stale)));
    assert_eq!(draft.stage(), EditStage::Conflict);
    assert_eq!(draft.source().unwrap().bytes(), b"# exact supplied bytes\n");
    assert!(draft.prepare_write(&capture).is_err());
    let mut resolved = draft.resolve_explicitly(changed.clone()).unwrap();
    assert_eq!(
        resolved.prepare_write(&changed).unwrap().expected.revision,
        8
    );
    let mut externally_changed = response();
    let Some(wire::configuration_document::WorkspaceState::Workspace(workspace)) =
        &mut externally_changed.workspace_state
    else {
        panic!("workspace")
    };
    workspace.source_sha256 = vec![9; 32];
    let externally_changed = Capture::admit(externally_changed).unwrap();
    let mut draft = SourceEdit::open(capture).unwrap();
    draft.supply(source(b"# correction\n")).unwrap();
    assert!(draft.prepare_write(&externally_changed).is_err());
}

#[test]
fn unobserved_or_reconciling_workspace_never_becomes_a_clean_write_anchor() {
    let mut unknown = response();
    unknown.workspace_state = Some(
        wire::configuration_document::WorkspaceState::WorkspaceUnobserved(Box::new(
            wire::WorkspaceUnobserved {
                reason: wire::WorkspaceUnobservedReason::SourceUnavailable.into(),
                ..Default::default()
            },
        )),
    );
    let capture = Capture::admit(unknown).unwrap();
    assert!(matches!(
        capture.workspace(),
        Workspace::Unobserved(UnobservedReason::SourceUnavailable)
    ));
    assert!(matches!(SourceEdit::open(capture), Err(Refusal::ReadOnly)));
    let mut reconciling = response();
    let Some(wire::configuration_document::WorkspaceState::Workspace(value)) =
        &mut reconciling.workspace_state
    else {
        panic!("workspace")
    };
    value.dirty = wire::DirtyState::Reconciling.into();
    assert!(SourceEdit::open(Capture::admit(reconciling).unwrap()).is_err());
}

#[test]
fn lost_or_ambiguous_response_never_replays_and_preserves_the_old_active_generation() {
    let (capture, mut draft) = submitted();
    assert!(draft.prepare_write(&capture).is_err());
    draft.uncertain();
    assert_eq!(draft.stage(), EditStage::Uncertain);
    assert!(draft.supply(source(b"# later\n")).is_err());
    assert!(draft.prepare_write(&capture).is_err());
    let (capture, mut draft) = submitted();
    let mut newer = response();
    newer.active_generation.as_option_mut().unwrap().revision = 8;
    let receipt = draft.complete(&reservation(None), Some(newer)).unwrap();
    assert_eq!(receipt.outcome, Outcome::CommitUnknown);
    assert_eq!(receipt.displayed_active(), capture.active());
    assert_eq!(receipt.current().unwrap().active().revision(), 8);
    assert!(draft.prepare_write(&capture).is_err());
}

#[test]
fn publication_requires_matching_authoritative_evidence_and_keeps_business_outcomes_distinct() {
    let prior = Capture::admit(response()).unwrap();
    use home_wire_contract::google::rpc::Code;
    for (activation, expected, transaction) in [
        (
            reservation(None),
            Outcome::CommitUnknown,
            Transaction::Unknown,
        ),
        (
            publication_for_test("homes/test", false, 8, 4),
            Outcome::CommittedReconciling,
            Transaction::Committed,
        ),
        (
            reservation(Some((
                wire::ApiErrorReason::AccessDenied,
                Code::PERMISSION_DENIED,
            ))),
            Outcome::RejectedBeforeCommit,
            Transaction::NotCommitted,
        ),
        (
            reservation(Some((
                wire::ApiErrorReason::ConfigRestartRequired,
                Code::FAILED_PRECONDITION,
            ))),
            Outcome::RestartRequired,
            Transaction::NotCommitted,
        ),
    ] {
        let receipt = Receipt::admit(&prior, &activation, None).unwrap();
        assert_eq!(receipt.outcome, expected);
        assert_eq!(receipt.transaction, transaction);
        assert_eq!(receipt.displayed_active(), prior.active());
    }
    let published = publication_for_test("homes/test", true, 8, 4);
    let reported = Receipt::admit(&prior, &published, None).unwrap();
    assert_eq!(reported.outcome, Outcome::Published);
    assert_eq!(
        reported.publication,
        Some(PublicationEvidence::Reported(ActiveGeneration {
            revision: 8,
            manifest: ManifestHash::parse(&[4; 32]).unwrap()
        }))
    );
    assert_eq!(reported.displayed_active(), prior.active());
    let stale_metadata = Receipt::admit(&prior, &published, Some(response())).unwrap();
    assert_eq!(
        stale_metadata.publication,
        Some(PublicationEvidence::Reported(ActiveGeneration {
            revision: 8,
            manifest: ManifestHash::parse(&[4; 32]).unwrap()
        }))
    );
    let mut later = response();
    later.active_generation.as_option_mut().unwrap().revision = 9;
    let later = Receipt::admit(&prior, &published, Some(later)).unwrap();
    assert_eq!(later.outcome, Outcome::Published);
    assert_eq!(
        later.publication,
        Some(PublicationEvidence::Reported(ActiveGeneration {
            revision: 8,
            manifest: ManifestHash::parse(&[4; 32]).unwrap()
        }))
    );
    assert_eq!(later.displayed_active().revision(), 9);
    let mut current = response();
    current.active_generation.as_option_mut().unwrap().revision = 8;
    let receipt = Receipt::admit(&prior, &published, Some(current)).unwrap();
    assert_eq!(receipt.outcome, Outcome::Published);
    assert_eq!(receipt.displayed_active().revision(), 8);
    let (_, mut draft) = submitted();
    let mut malformed = response();
    malformed.resume_token.clear();
    assert!(draft.complete(&published, Some(malformed)).is_err());
    assert_eq!(draft.stage(), EditStage::Uncertain);
}

#[test]
fn plans_keep_explicit_files_bounded_and_accept_every_closed_source_family() {
    for family in SourceFamily::ALL {
        let files = vec![
            SourceFile::supplied("entry.yaml", source(b"# explicit\n")).unwrap(),
            SourceFile::supplied("include.yaml", source(b"# also explicit\n")).unwrap(),
        ];
        let candidate = Candidate::supplied(
            family,
            SourceIdentity::parse("synthetic.source").unwrap(),
            "entry.yaml",
            files,
        )
        .unwrap();
        assert_eq!(candidate.to_wire().files[0].exact_source, b"# explicit\n");
        assert_eq!(candidate.to_wire().family, family.to_wire());
    }
    let file = SourceFile::supplied("entry.yaml", source(b"one")).unwrap();
    assert!(
        Candidate::supplied(
            SourceFamily::Operational,
            SourceIdentity::parse("operational.main").unwrap(),
            "entry.yaml",
            vec![file.clone(), file]
        )
        .is_err()
    );
    let files = vec![
        SourceFile::supplied("a", source(&vec![0; 524_289])).unwrap(),
        SourceFile::supplied("b", source(&vec![0; 524_288])).unwrap(),
    ];
    assert!(
        Candidate::supplied(
            SourceFamily::Script,
            SourceIdentity::parse("synthetic.source").unwrap(),
            "a",
            files
        )
        .is_err()
    );
}

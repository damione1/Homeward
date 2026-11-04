use super::*;
use buffa_types::google::protobuf::Timestamp;

fn account(name: &str, role: auth::AccountRole) -> auth::Account {
    auth::Account {
        name: format!("homes/h/accounts/{name}"),
        etag: "opaque-cas".into(),
        login_name: name.into(),
        display_name: name.into(),
        role: role.into(),
        access: auth::AccountAccess::Enabled.into(),
        create_time: Timestamp {
            seconds: 1_700_000_000,
            ..Default::default()
        }
        .into(),
        update_time: Timestamp {
            seconds: 1_700_000_000,
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
fn authority(name: &str, role: auth::AccountRole) -> Authority {
    Authority::admit(
        Identity {
            account: AccountName::try_from(format!("homes/h/accounts/{name}").as_str()).unwrap(),
            session: SessionName::try_from(
                format!("homes/h/accounts/{name}/sessions/session-{name}").as_str(),
            )
            .unwrap(),
        },
        account(name, role),
    )
    .unwrap()
}
fn instance() -> config::IntegrationInstance {
    config::IntegrationInstance {
        name: "homes/test/integrationInstances/synthetic-broker".into(),
        instance_key: "synthetic-broker".into(),
        integration_type: "homes/test/integrationTypes/mqtt".into(),
        configuration_document: "homes/test/configurationDocuments/integrations".into(),
        etag: "W/\"revision-seven\"".into(),
        desired_state: config::IntegrationDesiredState::Disabled.into(),
        configuration: Some(config::integration_instance::Configuration::Editable(
            Box::new(config::IntegrationConfiguration {
                schema_version: "homeward.integration/mqtt/1".into(),
                yaml_source: b"host: localhost\n".to_vec(),
                secrets: vec![config::IntegrationSecretReference {
                    field: "password_secret".into(),
                    instance_key: "synthetic-broker".into(),
                    secret_key: "synthetic-credential-reference".into(),
                    ..Default::default()
                }],
                ..Default::default()
            }),
        )),
        observed_health: Some(
            config::integration_instance::ObservedHealth::NoHealthReport(Box::default()),
        ),
        observed_supervision: Some(
            config::integration_instance::ObservedSupervision::NotSupervised(Box::default()),
        ),
        ..Default::default()
    }
}

#[test]
fn role_withdrawal_purges_protected_panes_drafts_secrets_and_late_results() {
    let mut state = PrivateState::<String, String>::default();
    let admin = state
        .install(authority("alice", auth::AccountRole::Admin))
        .unwrap();
    state
        .publish(
            admin,
            Section::Accounts,
            Pane {
                stage: Stage::Ready,
                content: Some("private account data".into()),
            },
        )
        .unwrap();
    state
        .edit(admin, Section::Policies, "private draft".into())
        .unwrap();
    state
        .show_secret(
            admin,
            SecretKind::Invitation,
            Zeroizing::new("one-time-invitation".into()),
        )
        .unwrap();
    let user = state
        .install(authority("alice", auth::AccountRole::User))
        .unwrap();
    assert_ne!(admin, user);
    assert!(
        state.pane(Section::Accounts).is_none()
            && state.draft().is_none()
            && state.secret().is_none()
    );
    assert_eq!(
        state.publish(
            admin,
            Section::Accounts,
            Pane {
                stage: Stage::Ready,
                content: Some("late".into())
            }
        ),
        Err(Refusal::Stale)
    );
    assert_eq!(
        state.edit(user, Section::Policies, "denied".into()),
        Err(Refusal::Denied)
    );
    assert_eq!(
        state.show_secret(
            user,
            SecretKind::Invitation,
            Zeroizing::new("denied".into())
        ),
        Err(Refusal::Denied)
    );
    state
        .publish(
            user,
            Section::Account,
            Pane {
                stage: Stage::Ready,
                content: Some("own account".into()),
            },
        )
        .unwrap();
}

#[test]
fn a_failed_subservice_leaves_other_panes_usable_and_retirement_rejects_all_old_work() {
    let mut state = PrivateState::<String, ()>::default();
    let context = state
        .install(authority("alice", auth::AccountRole::Admin))
        .unwrap();
    state
        .publish(
            context,
            Section::Accounts,
            Pane {
                stage: Stage::Ready,
                content: Some("accounts".into()),
            },
        )
        .unwrap();
    state
        .publish(
            context,
            Section::Metrics,
            Pane {
                stage: Stage::Unavailable,
                content: None,
            },
        )
        .unwrap();
    assert_eq!(
        state.pane(Section::Accounts).unwrap().content.as_deref(),
        Some("accounts")
    );
    state.retire().unwrap();
    assert!(state.authority().is_none());
    assert_eq!(
        state.publish(
            context,
            Section::Metrics,
            Pane {
                stage: Stage::Ready,
                content: Some("late metrics".into())
            }
        ),
        Err(Refusal::Stale)
    );
}

#[test]
fn integration_edit_preserves_exact_secret_references_and_resource_cas() {
    let mut source = instance();
    let mut draft = IntegrationDraft::open(&source).unwrap();
    let references = draft.secret_references().to_vec();
    draft.edit_yaml("host: synthetic.local\n").unwrap();
    source.etag = "W/\"revision-eight\"".into();
    let prepared = draft.prepare_update().unwrap();
    let Some(config::integration_instance::Configuration::Editable(configuration)) =
        &prepared.integration_instance.configuration
    else {
        panic!("editable")
    };
    assert_eq!(configuration.secrets, references);
    assert_eq!(configuration.yaml_source, b"host: synthetic.local\n");
    assert_eq!(prepared.integration_instance.etag, "W/\"revision-seven\"");
    assert_eq!(prepared.integration_instance.name, source.name);
    assert!(!draft.yaml().contains("password"));
    let new = IntegrationDraft::open(&source)
        .unwrap()
        .prepare_update()
        .unwrap();
    assert_eq!(new.integration_instance.etag, "W/\"revision-eight\"");
}

#[test]
fn opaque_configuration_is_not_reconstructed_but_explicit_enable_remains_available() {
    let mut source = instance();
    source.integration_type.clear();
    source.configuration = Some(config::integration_instance::Configuration::Opaque(
        Box::default(),
    ));
    assert!(matches!(
        IntegrationDraft::open(&source),
        Err(Refusal::ReadOnly)
    ));
    let enabled = prepare_enabled(&source, config::IntegrationDesiredState::Enabled).unwrap();
    assert_eq!(enabled.integration_instance.name, source.name);
    assert_eq!(enabled.integration_instance.etag, source.etag);
    assert!(prepare_enabled(&source, config::IntegrationDesiredState::Unspecified).is_err());
}

#[test]
fn unsupported_or_foreign_configuration_and_missing_cas_refuse_mutation() {
    let mut source = instance();
    let Some(config::integration_instance::Configuration::Editable(configuration)) =
        source.configuration.as_mut()
    else {
        panic!("editable")
    };
    configuration.secrets[0].instance_key = "another-instance".into();
    assert!(matches!(
        IntegrationDraft::open(&source),
        Err(Refusal::Invalid)
    ));
    source = instance();
    source.desired_state = 999.into();
    assert!(matches!(
        IntegrationDraft::open(&source),
        Err(Refusal::Unsupported)
    ));
    source = instance();
    source.etag.clear();
    assert!(matches!(
        IntegrationDraft::open(&source),
        Err(Refusal::Invalid)
    ));
    assert!(prepare_enabled(&source, config::IntegrationDesiredState::Enabled).is_err());
    source = instance();
    source.configuration_document = "homes/foreign/configurationDocuments/integrations".into();
    assert!(IntegrationDraft::open(&source).is_err());
}

#[test]
fn uncertain_or_inflight_mutation_cannot_be_prepared_again() {
    let outcome = config::ActivationResult {
        outcome: config::ActivationOutcome::CommitUnknown.into(),
        transaction: config::TransactionOutcome::CommitUnknown.into(),
        ..Default::default()
    };
    let mut draft = IntegrationDraft::open(&instance()).unwrap();
    draft.begin_update().unwrap();
    assert!(draft.begin_update().is_err());
    draft
        .complete(MutationState::activation(&outcome).unwrap())
        .unwrap();
    assert_eq!(draft.state, MutationState::Uncertain);
    assert!(matches!(draft.prepare_update(), Err(Refusal::Stale)));
    assert_eq!(draft.edit_yaml("host: retry\n"), Err(Refusal::Stale));
    assert!(draft.complete(MutationState::Draft).is_err());
    draft.state = MutationState::InFlight;
    assert!(
        draft
            .set_enabled(config::IntegrationDesiredState::Enabled)
            .is_err()
    );
}

#[test]
fn received_accounts_are_atomic_bounded_and_bound_to_the_current_identity() {
    let a = account("alice", auth::AccountRole::Admin);
    assert!(
        admit_accounts(auth::ListAccountsResponse {
            accounts: vec![a.clone(), a.clone()],
            ..Default::default()
        })
        .is_err()
    );
    assert!(
        admit_accounts(auth::ListAccountsResponse {
            accounts: vec![a.clone(); MAX_ROWS + 1],
            ..Default::default()
        })
        .is_err()
    );
    assert!(
        Authority::admit(
            Identity {
                account: AccountName::try_from("homes/h/accounts/bob").unwrap(),
                session: SessionName::try_from("homes/h/accounts/bob/sessions/session-bob")
                    .unwrap()
            },
            a.clone()
        )
        .is_err()
    );
    let mut future = a.clone();
    future.role = 999.into();
    assert!(
        admit_accounts(auth::ListAccountsResponse {
            accounts: vec![a, future],
            ..Default::default()
        })
        .is_err()
    );
}

#[test]
fn personal_settings_do_not_require_the_admin_only_account_service() {
    let identity = Identity {
        account: AccountName::try_from("homes/h/accounts/ordinary-account").unwrap(),
        session: SessionName::try_from("homes/h/accounts/ordinary-account/sessions/native-session")
            .unwrap(),
    };
    let mut state = PrivateState::<(), ()>::default();
    let context = state.install(Authority::session_only(identity)).unwrap();
    assert!(state.authority().unwrap().account().is_none());
    for section in [
        Section::Account,
        Section::Sessions,
        Section::Factors,
        Section::McpKeys,
    ] {
        state
            .publish(
                context,
                section,
                Pane {
                    stage: Stage::Ready,
                    content: Some(()),
                },
            )
            .unwrap();
    }
    assert_eq!(
        state.publish(
            context,
            Section::Integrations,
            Pane {
                stage: Stage::Ready,
                content: Some(())
            }
        ),
        Err(Refusal::Denied)
    );
}

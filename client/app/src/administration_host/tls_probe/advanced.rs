//! Rights and integration writes through the actual settings owner and TLS.
use super::*;
use crate::administration::{IntegrationId, MutationState};
use forms::advanced::{
    ChoiceField, Field,
    policy::{PolicyCommand, PolicyDraft, Preset},
};
use home_wire_contract::{auth_contract::AccountId, home::integrationadmin::v1 as integration};
use sha2::{Digest, Sha256};

fn edit(owner: &Administration, field: Field, value: impl Into<String>) {
    owner.edit(1, generation(owner), field as i32, value.into());
}
fn confirm(owner: &Administration) {
    owner.editor_action(1, generation(owner), ProAdminAction::Save);
}
async fn close(owner: &Administration) {
    owner.editor_action(1, generation(owner), ProAdminAction::Cancel);
    until(owner, "explicit fresh read after editor close", |state| {
        ready(state) && state.private.draft().is_none()
    })
    .await;
}
async fn saved(owner: &Administration) {
    until(owner, "acknowledged write", |state| {
        (ready(state) && state.private.draft().is_none())
            || state.private.draft().is_some_and(|draft| {
                matches!(
                    draft.state,
                    MutationState::Reconciling | MutationState::RestartRequired
                )
            })
    })
    .await;
    if owner.0.state.lock().unwrap().private.draft().is_some() {
        close(owner).await;
    }
}
async fn outcome(owner: &Administration, expected: MutationState) {
    until(owner, "explicit write outcome", |state| {
        state
            .private
            .draft()
            .is_some_and(|draft| draft.state == expected)
    })
    .await;
    let before = generation(owner);
    confirm(owner);
    assert_eq!(
        before,
        generation(owner),
        "a failed write is never blindly replayed"
    );
}
async fn policy(owner: &Administration, account: &AccountId) {
    section(owner, Section::Policies).await;
    let token = {
        let state = owner.0.state.lock().unwrap();
        state
            .rows
            .iter()
            .find(|row| row.target == render::Target::Account(account.clone()))
            .unwrap()
            .token
    };
    owner.select(token, generation(owner));
    until(owner, "selected account policy", |state| {
        ready(state) && matches!(state.private.pane(Section::Policies).and_then(|pane|pane.content.as_ref()), Some(Payload::Policy { account: selected, .. }) if selected == account)
    }).await;
}
fn policy_response(owner: &Administration) -> auth::GetPolicyResponse {
    let state = owner.0.state.lock().unwrap();
    let Some(Payload::Policy { response, .. }) = state
        .private
        .pane(Section::Policies)
        .and_then(|pane| pane.content.as_ref())
    else {
        panic!("selected policy")
    };
    response.clone()
}
async fn rights(owner: &Administration, connection: &Connection, session: &SessionCoordinator) {
    section(owner, Section::Policies).await;
    let account = {
        let state = owner.0.state.lock().unwrap();
        let Some(Payload::Accounts(accounts)) = state
            .private
            .pane(Section::Policies)
            .and_then(|pane| pane.content.as_ref())
        else {
            panic!("policy account catalogue")
        };
        AccountId::parse(
            &accounts
                .iter()
                .find(|value| value.login_name == "NHF018 ordinary")
                .unwrap()
                .id
                .value
                .clone(),
        )
        .unwrap()
    };
    policy(owner, &account).await;
    let before = policy_response(owner);
    owner.action(0, generation(owner), ProAdminAction::Save);
    owner.choose(
        1,
        generation(owner),
        ChoiceField::PolicyPreset as i32,
        Preset::ListsRead as i32,
    );
    assert_eq!(
        policy_response(owner),
        before,
        "preset only changes an unsubmitted draft"
    );
    edit(owner, Field::PolicySource, "schema: invalid\n");
    confirm(owner);
    outcome(owner, MutationState::Rejected).await;
    owner.choose(
        1,
        generation(owner),
        ChoiceField::PolicyPreset as i32,
        Preset::ListsReadWrite as i32,
    );
    confirm(owner);
    saved(owner).await;
    let after = policy_response(owner);
    let Some(auth::get_policy_response::Result::Present(value)) = &after.result else {
        panic!("saved rights")
    };
    let source = std::str::from_utf8(&value.yaml_source).unwrap();
    assert!(
        source.contains("family: lists, operation: read")
            && source.contains("family: lists, operation: write")
    );
    assert_eq!(
        value.version.source_sha256.as_slice(),
        &Sha256::digest(&value.yaml_source)[..]
    );

    // The second writer wins with exactly the same admitted snapshot; the open
    // UI keeps its old precondition and must force an explicit reread.
    owner.action(0, generation(owner), ProAdminAction::Save);
    let draft = PolicyDraft::open(&PolicyCommand {
        account: account.clone(),
        response: after,
    })
    .unwrap();
    let forms::advanced::Operation::Policy(request) = draft.submit().unwrap() else {
        unreachable!()
    };
    let cancel = CancellationToken::new();
    let lease = session.access(&cancel).await.unwrap();
    connection
        .policies()
        .replace_policy(request, lease.token(), Duration::from_secs(4), &cancel)
        .await
        .unwrap();
    confirm(owner);
    outcome(owner, MutationState::Conflict).await;
    edit(
        owner,
        Field::PolicySource,
        "must not edit a conflicted snapshot",
    );
    assert_eq!(
        owner.0.state.lock().unwrap().private.draft().unwrap().state,
        MutationState::Conflict
    );
    close(owner).await;
    policy(owner, &account).await;

    // Source reaches the contract's exact 1 MiB maximum; the protobuf envelope
    // is larger. The policy-specific transport limit must allow both directions.
    let mut source = format!(
        "schema: {}\naccount_id: {}\ngrants: []\n#",
        home_wire_contract::authorization_contract::POLICY_SCHEMA,
        serde_json::to_string(account.as_str()).unwrap()
    );
    source.extend(std::iter::repeat_n(
        'x',
        home_wire_contract::authorization_contract::MAX_SOURCE_BYTES - source.len() - 1,
    ));
    source.push('\n');
    assert_eq!(source.len(), 1 << 20);
    owner.action(0, generation(owner), ProAdminAction::Save);
    edit(owner, Field::PolicySource, source.clone());
    confirm(owner);
    saved(owner).await;
    let Some(auth::get_policy_response::Result::Present(value)) = policy_response(owner).result
    else {
        panic!("maximum source returned")
    };
    assert_eq!(value.yaml_source, source.as_bytes());
    // Leave a concise policy behind for later native fixture scenarios.
    owner.action(0, generation(owner), ProAdminAction::Save);
    owner.choose(
        1,
        generation(owner),
        ChoiceField::PolicyPreset as i32,
        Preset::ListsRead as i32,
    );
    confirm(owner);
    saved(owner).await;
    close(owner).await;
    assert!(
        matches!(
            owner
                .0
                .state
                .lock()
                .unwrap()
                .private
                .pane(Section::Policies)
                .unwrap()
                .content,
            Some(Payload::Accounts(_))
        ),
        "closing a read-only policy detail returns to the account catalogue"
    );
}
async fn integration(owner: &Administration, id: &IntegrationId) {
    section(owner, Section::Integrations).await;
    let token = {
        let state = owner.0.state.lock().unwrap();
        state
            .rows
            .iter()
            .find(|row| row.target == render::Target::Integration(id.clone()))
            .unwrap()
            .token
    };
    owner.select(token, generation(owner));
    until(owner, "selected integration", |state| {
        ready(state) && matches!(state.private.pane(Section::Integrations).and_then(|pane|pane.content.as_ref()), Some(Payload::Integration(value)) if value.response.instance.integration_instance_id == id.as_str())
    }).await;
}
fn detail(owner: &Administration) -> read::IntegrationDetail {
    let state = owner.0.state.lock().unwrap();
    let Some(Payload::Integration(value)) = state
        .private
        .pane(Section::Integrations)
        .and_then(|pane| pane.content.as_ref())
    else {
        panic!("integration detail")
    };
    *value.clone()
}
fn public(value: &read::IntegrationDetail) -> &integration::PublicConfiguration {
    let Some(integration::integration_instance::Configuration::Editable(source)) =
        &value.response.instance.configuration
    else {
        panic!("public integration")
    };
    source
}
async fn integrations(
    owner: &Administration,
    connection: &Connection,
    session: &SessionCoordinator,
) {
    let opaque = IntegrationId::parse("nha148-opaque").unwrap();
    integration(owner, &opaque).await;
    let value = detail(owner);
    assert!(value.response.instance.type_id.is_empty());
    assert!(value.kind.is_err());
    {
        let state = owner.0.state.lock().unwrap();
        assert!(
            !forms::available(&state)
                .iter()
                .any(|command| command.action() == ProAdminAction::Save),
            "opaque source has no public editor"
        );
        assert!(state.rows.iter().any(|row| row.detail.contains("opaque")));
    }
    let encoded = format!("{:?}", value.response);
    assert!(!encoded.contains("PRIVATE_OPAQUE_ARGUMENT") && !encoded.contains("/usr/bin/true"));
    owner.action(0, generation(owner), ProAdminAction::Remove);
    confirm(owner);
    saved(owner).await;
    // The worker remains disabled until it is deleted: no executable launches.
    let workday = IntegrationId::parse("homeward-workday-v1").unwrap();
    integration(owner, &workday).await;
    let original = detail(owner);
    owner.action(0, generation(owner), ProAdminAction::Save);
    edit(owner, Field::Fragment, "timezone: NHF018_not_a_timezone\n");
    confirm(owner);
    outcome(owner, MutationState::Rejected).await;
    edit(owner, Field::Fragment, "timezone: UTC\n");
    confirm(owner);
    saved(owner).await;
    integration(owner, &workday).await;
    assert!(
        std::str::from_utf8(&public(&detail(owner)).yaml_source)
            .unwrap()
            .contains("UTC")
    );
    owner.action(0, generation(owner), ProAdminAction::Enable);
    confirm(owner);
    saved(owner).await;
    integration(owner, &workday).await;
    assert_eq!(
        detail(owner).response.instance.desired_state,
        integration::DesiredState::Enabled
    );
    owner.action(0, generation(owner), ProAdminAction::Disable);
    confirm(owner);
    saved(owner).await;
    integration(owner, &workday).await;
    let current = detail(owner);
    owner.action(0, generation(owner), ProAdminAction::Save);
    let cancel = CancellationToken::new();
    let lease = session.access(&cancel).await.unwrap();
    let mut concurrent = crate::administration::IntegrationDraft::open(
        &current.response.instance,
        &current.response.document,
    )
    .unwrap();
    concurrent.edit_yaml("timezone: Europe/Paris\n").unwrap();
    connection
        .integrations()
        .update_instance(
            concurrent.prepare_update().unwrap(),
            lease.token(),
            Duration::from_secs(4),
            &cancel,
        )
        .await
        .unwrap();
    edit(owner, Field::Fragment, "timezone: UTC\n");
    confirm(owner);
    outcome(owner, MutationState::Conflict).await;
    close(owner).await;
    integration(owner, &workday).await;
    owner.action(0, generation(owner), ProAdminAction::Save);
    edit(
        owner,
        Field::Fragment,
        String::from_utf8(public(&original).yaml_source.clone()).unwrap(),
    );
    confirm(owner);
    saved(owner).await;

    owner.action(0, generation(owner), ProAdminAction::Create);
    let type_choice = {
        let state = owner.0.state.lock().unwrap();
        state
            .private
            .draft()
            .unwrap()
            .choices()
            .into_iter()
            .find(|value| value.label == "mqtt")
            .unwrap()
            .token
    };
    owner.choose(
        1,
        generation(owner),
        ChoiceField::IntegrationType as i32,
        type_choice,
    );
    edit(owner, Field::Identity, "nha148-mqtt");
    let mqtt_source = "profile: '5'\nbroker: mqtt://127.0.0.1:1\ninstance: nha148-mqtt\nmode: passive\nsubscriptions: [homeassistant/#]\n";
    edit(owner, Field::Fragment, mqtt_source);
    edit(
        owner,
        Field::References,
        "username_secret=user-ref\npassword_secret=password-ref\n",
    );
    confirm(owner);
    saved(owner).await;
    let mqtt = IntegrationId::parse("nha148-mqtt").unwrap();
    integration(owner, &mqtt).await;
    let original = detail(owner);
    assert_eq!(
        original.response.instance.desired_state,
        integration::DesiredState::Disabled
    );
    let references = public(&original).secrets.clone();
    assert_eq!(references.len(), 2);
    owner.action(0, generation(owner), ProAdminAction::Save);
    edit(
        owner,
        Field::Fragment,
        mqtt_source.replace("homeassistant/#", "nhf018/#"),
    );
    confirm(owner);
    saved(owner).await;
    integration(owner, &mqtt).await;
    let updated = detail(owner);
    assert_eq!(
        public(&updated).secrets,
        references,
        "editing public YAML preserves scoped secret references exactly"
    );
    assert_eq!(
        updated.response.instance.desired_state,
        integration::DesiredState::Disabled,
        "no broker is ever started"
    );
    let encoded = format!("{:?}", updated.response);
    assert!(!encoded.contains("NHF018_PRIVATE_SYNTHETIC"));
    owner.action(0, generation(owner), ProAdminAction::Remove);
    confirm(owner);
    saved(owner).await;
}
pub(super) async fn qualify(
    owner: &Administration,
    connection: &Connection,
    session: &SessionCoordinator,
) {
    rights(owner, connection, session).await;
    integrations(owner, connection, session).await;
}

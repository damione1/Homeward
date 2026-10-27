use super::*;

async fn mfa_login(session: &SessionCoordinator, value: &MfaCredentials, index: usize) {
    session
        .login(
            value.login_name.clone(),
            Zeroizing::new(value.password.clone()),
        )
        .await
        .unwrap();
    assert!(matches!(
        session.subscribe().borrow().phase,
        Phase::Challenge { .. }
    ));
    session
        .complete_recovery(Zeroizing::new(value.recovery_codes[index].clone()))
        .await
        .unwrap();
    assert_eq!(session.subscribe().borrow().phase, Phase::Ready);
}
fn select_session(owner: &Administration, id: &home_wire_contract::auth_contract::SessionId) {
    let token = {
        let state = owner.0.state.lock().unwrap();
        state
            .rows
            .iter()
            .find(|row| row.target == render::Target::Session(id.clone()))
            .unwrap()
            .token
    };
    owner.select(token, generation(owner));
}
async fn sessions(
    owner: &Administration,
    connection: &Connection,
    session: &SessionCoordinator,
    fixture: &Fixture,
) {
    let cancel = CancellationToken::new();
    let current = session.access(&cancel).await.unwrap();
    let other = SessionCoordinator::new(connection.auth(), Arc::new(MemoryStore::default()));
    login(&other, &fixture.admin).await;
    let other_lease = other.access(&cancel).await.unwrap();
    section(owner, Section::Sessions).await;
    select_session(owner, other_lease.session_id());
    owner.action(0, generation(owner), ProAdminAction::Revoke);
    owner.editor_action(1, generation(owner), ProAdminAction::Save);
    until(owner,"other session revocation preserves caller",|state|ready(state)&&state.private.draft().is_none()&&state.private.pane(Section::Sessions).and_then(|pane|pane.content.as_ref()).is_some_and(|payload|matches!(payload,Payload::Sessions(rows) if rows.iter().all(|row|row.id.value!=other_lease.session_id().as_str())))).await;
    assert_eq!(
        session.access(&cancel).await.unwrap().session_id(),
        current.session_id()
    );
    let refused = connection
        .auth()
        .list_sessions(
            auth::ListSessionsRequest {
                account_id: auth::AccountId {
                    value: other_lease.account_id().as_str().into(),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            },
            other_lease.token(),
            Duration::from_secs(4),
            &cancel,
        )
        .await
        .expect_err("revoked session must be refused");
    assert_eq!(
        refused.kind(),
        home_client_transport::FailureKind::Rpc(home_client_transport::RpcCode::Unauthenticated)
    );
    select_session(owner, current.session_id());
    owner.action(0, generation(owner), ProAdminAction::Revoke);
    owner.editor_action(1, generation(owner), ProAdminAction::Save);
    until(
        owner,
        "self revocation clears native credential and private state",
        |state| !state.active && state.private.authority().is_none(),
    )
    .await;
    assert_eq!(session.subscribe().borrow().phase, Phase::SignedOut);
    let stale = current.generation;
    login(session, &fixture.admin).await;
    assert!(
        !session.logout_local_if_current(stale).await.unwrap(),
        "old retirement cannot clear a replacement login"
    );
    until(owner, "login after self revocation", ready).await;
}
async fn factors(
    owner: &Administration,
    connection: &Connection,
    session: &SessionCoordinator,
    fixture: &Fixture,
) {
    session.logout().await.unwrap();
    mfa_login(session, &fixture.factor, 0).await;
    until(owner, "factor account admitted", ready).await;
    section(owner, Section::Factors).await;
    owner.action(0, generation(owner), ProAdminAction::Create);
    owner.editor_action(1, generation(owner), ProAdminAction::Save);
    until(
        owner,
        "recovery result after native credential retirement",
        |state| {
            !state.active
                && state.private.authority().is_none()
                && owner.0.receipts.value().is_some()
        },
    )
    .await;
    assert_eq!(session.subscribe().borrow().phase, Phase::SignedOut);
    let codes = owner.0.receipts.value().unwrap();
    let codes: Vec<_> = codes
        .lines()
        .map(|value| Zeroizing::new(value.to_owned()))
        .collect();
    assert_eq!(codes.len(), 10);
    owner.0.receipts.clear();
    assert!(owner.0.receipts.value().is_none());
    let rejected = SessionCoordinator::new(connection.auth(), Arc::new(MemoryStore::default()));
    rejected
        .login(
            fixture.factor.login_name.clone(),
            Zeroizing::new(fixture.factor.password.clone()),
        )
        .await
        .unwrap();
    assert!(
        rejected
            .complete_recovery(Zeroizing::new(fixture.factor.recovery_codes[1].clone()))
            .await
            .is_err(),
        "old recovery batch must be invalidated"
    );
    session
        .login(
            fixture.factor.login_name.clone(),
            Zeroizing::new(fixture.factor.password.clone()),
        )
        .await
        .unwrap();
    session.complete_recovery(codes[0].clone()).await.unwrap();
    until(owner, "login with newly delivered code", ready).await;
    section(owner, Section::Factors).await;
    let token = {
        let state = owner.0.state.lock().unwrap();
        state
            .rows
            .iter()
            .find(|row| matches!(row.target, render::Target::Factor(_)))
            .unwrap()
            .token
    };
    owner.select(token, generation(owner));
    owner.action(0, generation(owner), ProAdminAction::Revoke);
    owner.editor_action(1, generation(owner), ProAdminAction::Save);
    until(owner, "factor removal retires native session", |state| {
        !state.active && state.private.authority().is_none()
    })
    .await;
    assert_eq!(session.subscribe().borrow().phase, Phase::SignedOut);
    login(
        session,
        &Credentials {
            login_name: fixture.factor.login_name.clone(),
            password: fixture.factor.password.clone(),
        },
    )
    .await;
    assert_eq!(session.subscribe().borrow().phase, Phase::Ready);
    until(
        owner,
        "password login after last ordinary factor removal",
        ready,
    )
    .await;
    section(owner, Section::Factors).await;
    assert!(owner.0.state.lock().unwrap().rows.is_empty());
    session.logout().await.unwrap();
    login(session, &fixture.admin).await;
    until(
        owner,
        "administrator restored after factor scenarios",
        ready,
    )
    .await;
}
async fn policy(owner: &Administration, required: bool) {
    section(owner, Section::MfaPolicy).await;
    owner.action(0, generation(owner), ProAdminAction::Save);
    owner.choose(
        1,
        generation(owner),
        forms::ChoiceField::RequireMfa as i32,
        if required {
            forms::Choice::RequireMfa
        } else {
            forms::Choice::OptionalMfa
        } as i32,
    );
    owner.editor_action(1, generation(owner), ProAdminAction::Save);
    until(owner,"MFA policy applied with continuing session",|state|ready(state)&&state.private.draft().is_none()&&state.private.pane(Section::MfaPolicy).and_then(|pane|pane.content.as_ref()).is_some_and(|payload|matches!(payload,Payload::MfaPolicy(value) if value.require_admin_factor==required))).await;
}
async fn policies(
    owner: &Administration,
    connection: &Connection,
    session: &SessionCoordinator,
    fixture: &Fixture,
) {
    let witness = SessionCoordinator::new(connection.auth(), Arc::new(MemoryStore::default()));
    mfa_login(&witness, &fixture.witness, 0).await;
    let admin = Administration::detached();
    let cancel = CancellationToken::new();
    let (run, source, api, token) = (
        admin.clone(),
        witness.clone(),
        connection.clone(),
        cancel.clone(),
    );
    let task = tokio::spawn(async move { run.run(source, api, token).await });
    admin.visible(true);
    until(&admin, "MFA-qualified administrator", ready).await;
    let lease = witness.access(&cancel).await.unwrap();
    policy(&admin, true).await;
    assert_eq!(
        witness.access(&cancel).await.unwrap().session_id(),
        lease.session_id()
    );
    policy(&admin, false).await;
    // A second valid MFA session changes the policy while this editor retains
    // its exact old version. No database injection or invented conflict.
    admin.action(0, generation(&admin), ProAdminAction::Save);
    let second = SessionCoordinator::new(connection.auth(), Arc::new(MemoryStore::default()));
    mfa_login(&second, &fixture.witness, 1).await;
    let second_lease = second.access(&cancel).await.unwrap();
    let api = connection.accounts();
    let current = api
        .get_second_factor_policy(
            auth::GetSecondFactorPolicyRequest::default(),
            second_lease.token(),
            Duration::from_secs(4),
            &cancel,
        )
        .await
        .unwrap();
    let changed = api
        .set_second_factor_policy(
            auth::SetSecondFactorPolicyRequest {
                expected: current.policy.version.clone(),
                require_admin_factor: true,
                ..Default::default()
            },
            second_lease.token(),
            Duration::from_secs(4),
            &cancel,
        )
        .await
        .unwrap();
    api.set_second_factor_policy(
        auth::SetSecondFactorPolicyRequest {
            expected: changed.policy.version.clone(),
            require_admin_factor: false,
            ..Default::default()
        },
        second_lease.token(),
        Duration::from_secs(4),
        &cancel,
    )
    .await
    .unwrap();
    admin.editor_action(1, generation(&admin), ProAdminAction::Save);
    until(
        &admin,
        "stale policy version is a conflict without replay",
        |state| {
            state
                .private
                .draft()
                .is_some_and(|draft| draft.state == crate::administration::MutationState::Conflict)
        },
    )
    .await;
    admin.action(0, generation(&admin), ProAdminAction::Refresh);
    until(&admin, "explicit policy refresh after conflict", ready).await;
    login(session, &fixture.admin).await;
    until(owner, "unfactored administrator restored", ready).await;
    section(owner, Section::MfaPolicy).await;
    owner.action(0, generation(owner), ProAdminAction::Save);
    owner.choose(
        1,
        generation(owner),
        forms::ChoiceField::RequireMfa as i32,
        forms::Choice::RequireMfa as i32,
    );
    owner.editor_action(1, generation(owner), ProAdminAction::Save);
    until(
        owner,
        "policy continuation requires reauthentication",
        |state| !state.active && state.private.authority().is_none(),
    )
    .await;
    assert_eq!(session.subscribe().borrow().phase, Phase::SignedOut);
    // The enrolled witness can restore the isolated test policy through the
    // same product form. Its factor-qualified native session remains valid.
    admin.action(0, generation(&admin), ProAdminAction::Refresh);
    until(&admin, "fresh required policy", ready).await;
    policy(&admin, false).await;
    cancel.cancel();
    tokio::time::timeout(Duration::from_secs(5), task)
        .await
        .unwrap()
        .unwrap();
    login(session, &fixture.admin).await;
    until(
        owner,
        "administrator restored after MFA continuation",
        ready,
    )
    .await;
}
pub(super) async fn qualify(
    owner: &Administration,
    connection: &Connection,
    session: &SessionCoordinator,
    fixture: &Fixture,
) {
    sessions(owner, connection, session, fixture).await;
    factors(owner, connection, session, fixture).await;
    policies(owner, connection, session, fixture).await;
}

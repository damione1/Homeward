package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Occupy the production aggregate bound with real admitted sources held by
// other pending account mutations. No fake account/grant is published and no
// test-only capacity setting weakens the native handler admission.
func nha050ReserveRemainingCapacity(t *testing.T, authority *authorization.Authority) func() {
	t.Helper()
	remaining := authorization.MaxAuthorityBytes - 256 // one committed empty seed
	const entryCost = 192 + len("budget-0000")
	const maximum = 384 + authorization.MaxEntities*entryCost
	var mutations []*authorization.Mutation
	for index := 0; remaining > 0; index++ {
		cost := min(maximum, remaining)
		if after := remaining - cost; after > 0 && after < 600 {
			cost -= 600
		}
		n := (cost - 384) / entryCost
		padding := cost - 384 - n*entryCost
		id, _ := contracts.ParseAccountID(fmt.Sprintf("pending-capacity-account-%d", index))
		var data strings.Builder
		fmt.Fprintf(&data, "schema: homeward.authorization/1\naccount_id: pending-capacity-account-%d\ngrants:\n  - scope: {family: actions, operation: write}\n    objects:\n      entities:\n        explicit:\n", index)
		for entity := 0; entity < n; entity++ {
			text := fmt.Sprintf("budget-%04d", entity)
			if entity == n-1 {
				text += strings.Repeat("x", padding)
			}
			fmt.Fprintf(&data, "          - entity_id: %q\n            capabilities: {all: {}}\n", text)
		}
		source, err := authorization.AdmitPolicySource(id, []byte(data.String()))
		if err != nil {
			t.Fatal(err)
		}
		mutation, err := authority.BeginMutation(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := mutation.ReservePolicy(source); err != nil {
			mutation.Release()
			t.Fatal(err)
		}
		mutations = append(mutations, mutation)
		remaining -= cost
	}
	release := func() {
		for _, mutation := range mutations {
			mutation.Release()
		}
	}
	t.Cleanup(release)
	return release
}

func TestNHA050NativeHandlersReserveCapacityBeforeSQLAndReconcilePublicationFailure(t *testing.T) {
	cluster := testpg.Start(t)
	settings := nha049Config(t, cluster.Port, shortPrivateDataDir(t))
	pgSettings, err := settings.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, pgSettings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	coordinator, err := prepareLocalAuthorization(ctx, store, realSessionClock{})
	if err != nil {
		t.Fatal(err)
	}
	h := &accountHandler{store: store, coordinator: coordinator}
	const password = "NHA050 capacity synthetic native password"
	admin, err := h.BootstrapAdmin(ctx, connect.NewRequest(&authv1.BootstrapAdminRequest{LoginName: "Capacity Admin", DisplayName: "Capacity Admin", Password: password}))
	if err != nil {
		t.Fatal(err)
	}
	account, _ := contracts.ParseAccountID(admin.Msg.Account.Id.Value)
	session, _ := contracts.ParseSessionID("capacity-native-session")
	principal, _ := authorization.NativePrincipal(account, session)
	actor, _ := principal.Actor()
	nativeCtx, _ := authorization.WithPrincipal(context.WithValue(ctx, actorContextKey{}, actor), principal)
	policy := &authorizationPolicyHandler{store: store, coordinator: coordinator}
	seed, err := policy.GetPolicy(nativeCtx, connect.NewRequest(&authv1.GetPolicyRequest{AccountId: admin.Msg.Account.Id}))
	if err != nil {
		t.Fatal(err)
	}
	invitation, err := h.CreateInvitation(nativeCtx, connect.NewRequest(&authv1.CreateInvitationRequest{LoginName: "Capacity Invited", Role: authv1.AccountRole_ACCOUNT_ROLE_USER, ExpiresAt: timestamppb.New(time.Now().Add(time.Hour))}))
	if err != nil {
		t.Fatal(err)
	}
	count := func() int64 {
		t.Helper()
		var count int64
		if err := store.InTransaction(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM public.auth_accounts`).Scan(&count)
		}); err != nil {
			t.Fatal(err)
		}
		return count
	}
	release := nha050ReserveRemainingCapacity(t, coordinator.authority)
	growth := []byte("schema: homeward.authorization/1\naccount_id: " + admin.Msg.Account.Id.Value + "\ngrants:\n  - scope: {family: actions, operation: write}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n")
	request := &authv1.ReplacePolicyRequest{AccountId: admin.Msg.Account.Id, YamlSource: growth, Precondition: &authv1.ReplacePolicyRequest_Expected{Expected: seed.Msg.GetPresent().Version}}
	if _, err := policy.ReplacePolicy(nativeCtx, connect.NewRequest(request)); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatal("policy capacity rejected only after SQL", err)
	}
	if got, err := policy.GetPolicy(nativeCtx, connect.NewRequest(&authv1.GetPolicyRequest{AccountId: admin.Msg.Account.Id})); err != nil || got.Msg.GetPresent().Version.Revision != seed.Msg.GetPresent().Version.Revision {
		t.Fatal("rejected growth persisted a version", err)
	}
	if _, err := h.CreateAccount(nativeCtx, connect.NewRequest(&authv1.CreateAccountRequest{LoginName: "Capacity Created", DisplayName: "Capacity Created", InitialPassword: password, Role: authv1.AccountRole_ACCOUNT_ROLE_USER})); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatal("creation seed did not reserve before INSERT", err)
	}
	if _, err := h.AcceptInvitation(ctx, connect.NewRequest(&authv1.AcceptInvitationRequest{InvitationToken: invitation.Msg.InvitationToken, DisplayName: "Capacity Invited", Password: password})); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatal("invitation seed did not reserve before INSERT", err)
	}
	if _, err := h.BootstrapAdmin(ctx, connect.NewRequest(&authv1.BootstrapAdminRequest{LoginName: "Capacity Other Admin", DisplayName: "Capacity Other Admin", Password: password})); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatal("bootstrap seed did not reserve before SQL", err)
	}
	if count() != 1 {
		t.Fatal("capacity rejection inserted an account")
	}
	// A source-only edit still fits at the aggregate bound and must not spend
	// another pending mutation's reservation or quarantine this administrator.
	request.YamlSource = append(append([]byte(nil), seed.Msg.GetPresent().YamlSource...), []byte("# capacity no-op\n")...)
	noOp, err := policy.ReplacePolicy(nativeCtx, connect.NewRequest(request))
	if err != nil || noOp.Msg.RightsChange != authv1.PolicyRightsChange_POLICY_RIGHTS_CHANGE_UNCHANGED {
		t.Fatal("capacity prevented no-op policy repair", err)
	}
	release() // other transactions roll back their growth reservations
	// Prove an actual committed growth followed by a failed memory refresh:
	// the reservation stays quarantined until the next primary reconciliation.
	guard, err := h.accountMutationGuard(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	source, _ := authorization.AdmitPolicySource(account, growth)
	if err := guard.mutation.ReservePolicy(source); err != nil {
		t.Fatal(err)
	}
	version := noOp.Msg.Policy.Version
	var hash [32]byte
	copy(hash[:], version.SourceSha256)
	expectedVersion, _ := authorization.NewVersion(version.Revision, hash)
	expected, _ := authorization.ExpectVersion(expectedVersion)
	if _, err := store.ReplaceAuthorizationPolicy(ctx, account, account, source, expected); err != nil {
		t.Fatal(err)
	}
	failedCtx, fail := context.WithCancel(ctx)
	fail()
	if err := finishAccountMutation(guard, failedCtx, nil, false); !errors.Is(err, storage.ErrAccountUnavailable) {
		t.Fatal("failed post-commit projection reported success", err)
	}
	if _, err := coordinator.authority.Current(principal); !errors.Is(err, authorization.ErrUnavailable) {
		t.Fatal("failed post-commit projection admitted stale policy", err)
	}
	reconciled, err := coordinator.enter(ctx, account)
	if err != nil {
		t.Fatal("primary could not reconcile reserved growth", err)
	}
	reconciled.release()
	current, err := coordinator.authority.Current(principal)
	if err != nil {
		t.Fatal(err)
	}
	permission, _ := authorization.NewPermission(authv1.ScopeFamily_SCOPE_FAMILY_ACTIONS, authv1.ScopeOperation_SCOPE_OPERATION_WRITE)
	entity, _ := domain.ParseEntityID("capacity-synthetic-entity")
	slot, _ := authorization.NewCapabilitySlot(contracts.SwitchSchema, "relay")
	object, _ := authorization.EntityObject(entity, slot)
	if err := current.Authorize(permission, object); err != nil {
		t.Fatal("reconciliation did not publish committed growth", err)
	}
	restarted, err := prepareLocalAuthorization(ctx, store, realSessionClock{})
	if err != nil {
		t.Fatal("committed policies prevented restart", err)
	}
	if current, err := restarted.authority.Current(principal); err != nil || current.Authorize(permission, object) != nil {
		t.Fatal("restart lost committed growth", err)
	}
	if _, err := h.AcceptInvitation(ctx, connect.NewRequest(&authv1.AcceptInvitationRequest{InvitationToken: invitation.Msg.InvitationToken, DisplayName: "Capacity Invited", Password: password})); err != nil || count() != 2 {
		t.Fatal("capacity rejection consumed invitation or leaked seed reservation", err)
	}
}

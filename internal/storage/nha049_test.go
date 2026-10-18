package storage

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
)

func nha049AccountInput(t *testing.T, loginName string, role authv1.AccountRole) NewAccount {
	t.Helper()
	id, err := accounts.NewAccountID()
	if err != nil {
		t.Fatal(err)
	}
	login, err := accounts.ParseLoginName(loginName)
	if err != nil {
		t.Fatal(err)
	}
	display, err := accounts.ParseDisplayName(loginName)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := accounts.HashNewPassword(context.Background(), "NHA049 unique test password 🔒")
	if err != nil {
		t.Fatal(err)
	}
	return NewAccount{ID: id, Login: login, Display: display, Role: role, Verifier: verifier}
}

// A nil pointer still satisfies context.Context through its embedded methods.
// The account ports must reject it before pgx can call one of those methods.
type nha049NilContext struct{ context.Context }

func TestNHA049StoreBootstrapInvitationAdminAndRecovery(t *testing.T) {
	if _, err := (AccountRecord{}).Role(); !errors.Is(err, ErrAccountUnavailable) {
		t.Fatalf("zero account record role was admitted: %v", err)
	}
	if _, err := (InvitationRecord{}).AcceptedAt(); !errors.Is(err, ErrAccountUnavailable) {
		t.Fatalf("zero invitation record was admitted: %v", err)
	}
	port := testCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, testSettings(t, port))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if version, err := store.Migrate(ctx); err != nil || version != CurrentVersion() {
		t.Fatalf("account schema migration: %v %v", version, err)
	}

	first := nha049AccountInput(t, "Administrator One", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	second := nha049AccountInput(t, "Administrator Two", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	var typedNil *nha049NilContext
	if _, err := store.BootstrapAdmin(typedNil, first); !errors.Is(err, ErrAccountInvalid) {
		t.Fatalf("typed-nil transaction context: %v", err)
	}
	if _, err := store.CredentialByLogin(typedNil, first.Login); !errors.Is(err, ErrAccountInvalid) {
		t.Fatalf("typed-nil credential context: %v", err)
	}
	if err := store.RecordAuthTransportAudit(typedNil, nil, "/accounts", false); !errors.Is(err, ErrAccountInvalid) {
		t.Fatalf("typed-nil audit context: %v", err)
	}
	inputs := []NewAccount{first, second}
	var wait sync.WaitGroup
	results := make([]error, 2)
	for i := range inputs {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			_, results[i] = store.BootstrapAdmin(ctx, inputs[i])
		}(i)
	}
	wait.Wait()
	winners, denials := 0, 0
	var admin contracts.AccountID
	for i, result := range results {
		if result == nil {
			winners++
			admin = inputs[i].ID
		} else if errors.Is(result, ErrBootstrapCompleted) {
			denials++
		} else {
			t.Fatalf("bootstrap %d: %v", i, result)
		}
	}
	if winners != 1 || denials != 1 {
		t.Fatalf("bootstrap outcomes winners=%d denied=%d", winners, denials)
	}
	if _, err := store.BootstrapAdmin(ctx, first); !errors.Is(err, ErrBootstrapCompleted) {
		t.Fatalf("repeated bootstrap: %v", err)
	}
	if err := store.DeleteAccount(ctx, admin, admin); !errors.Is(err, ErrLastActiveAdmin) {
		t.Fatalf("last admin deletion: %v", err)
	}

	user := nha049AccountInput(t, "Ordinary User", authv1.AccountRole_ACCOUNT_ROLE_USER)
	if _, err := store.CreateAccount(ctx, admin, user); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateAccount(ctx, user.ID, user.ID, AccountChanges{Role: ptrRole(authv1.AccountRole_ACCOUNT_ROLE_ADMIN)}); !errors.Is(err, ErrAccountPermission) {
		t.Fatalf("user self elevation: %v", err)
	}
	if err := store.DeleteAccount(ctx, user.ID, admin); !errors.Is(err, ErrAccountPermission) {
		t.Fatalf("user mutation: %v", err)
	}

	invitationID, err := accounts.NewInvitationID()
	if err != nil {
		t.Fatal(err)
	}
	token, err := accounts.NewInvitationToken()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := token.Digest()
	if err != nil {
		t.Fatal(err)
	}
	invitee, err := accounts.ParseLoginName("Invited Admin")
	if err != nil {
		t.Fatal(err)
	}
	invitation, err := store.CreateInvitation(ctx, admin, NewInvitation{ID: invitationID, Login: invitee,
		Role: authv1.AccountRole_ACCOUNT_ROLE_ADMIN, Digest: digest, ExpiresAt: time.Now().Add(time.Hour)})
	invitationRole, roleErr := invitation.Role()
	if err != nil || roleErr != nil || invitationRole != authv1.AccountRole_ACCOUNT_ROLE_ADMIN {
		t.Fatalf("invitation: %v %+v", err, invitation)
	}
	display, _ := accounts.ParseDisplayName("Invited Admin")
	acceptedID, _ := accounts.NewAccountID()
	accepted, err := store.AcceptInvitation(ctx, digest, acceptedID, display, user.Verifier)
	acceptedRole, roleErr := accepted.Role()
	if err != nil || roleErr != nil || acceptedRole != authv1.AccountRole_ACCOUNT_ROLE_ADMIN {
		t.Fatalf("accept: %v %+v", err, accepted)
	}
	if _, err := store.AcceptInvitation(ctx, digest, acceptedID, display, user.Verifier); !errors.Is(err, ErrInvitationUnavailable) {
		t.Fatalf("reused token: %v", err)
	}
	listed, err := store.ListInvitations(ctx, admin)
	if err != nil || len(listed) != 1 {
		t.Fatalf("accepted invitation listing: %v count=%d", err, len(listed))
	}
	acceptedAt, err := listed[0].AcceptedAt()
	if err != nil || acceptedAt == nil {
		t.Fatalf("accepted invitation timestamp: %v", err)
	}
	originalTime := *acceptedAt
	*acceptedAt = time.Time{}
	again, err := listed[0].AcceptedAt()
	if err != nil || again == nil || !again.Equal(originalTime) {
		t.Fatal("invitation timestamp getter leaked mutable state")
	}
	beforeDisplay, err := accepted.Revision()
	if err != nil {
		t.Fatal(err)
	}
	newDisplay, err := accounts.ParseDisplayName("Updated display")
	if err != nil {
		t.Fatal(err)
	}
	accepted, err = store.UpdateAccount(ctx, admin, acceptedID, AccountChanges{Display: &newDisplay})
	if err != nil {
		t.Fatalf("display-only update: %v", err)
	}
	if current, err := store.RevisionCurrent(ctx, beforeDisplay); err != nil || !current {
		t.Fatalf("display-only update invalidated revision: current=%v err=%v", current, err)
	}
	if _, err := store.UpdateAccount(ctx, admin, acceptedID, AccountChanges{Role: ptrRole(authv1.AccountRole_ACCOUNT_ROLE_ADMIN)}); err != nil {
		t.Fatalf("same-role update: %v", err)
	}
	if current, err := store.RevisionCurrent(ctx, beforeDisplay); err != nil || !current {
		t.Fatalf("same-role update invalidated revision: current=%v err=%v", current, err)
	}
	if _, err := store.RevisionCurrent(typedNil, beforeDisplay); !errors.Is(err, ErrAccountInvalid) {
		t.Fatalf("typed-nil revision context: %v", err)
	}
	adminRecord, err := store.GetAccount(ctx, admin, admin)
	if err != nil {
		t.Fatal(err)
	}
	adminBeforeDemotion, err := adminRecord.Revision()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateAccount(ctx, admin, admin, AccountChanges{Role: ptrRole(authv1.AccountRole_ACCOUNT_ROLE_USER)}); err != nil {
		t.Fatalf("demote with second admin: %v", err)
	}
	if current, err := store.RevisionCurrent(ctx, adminBeforeDemotion); err != nil || current {
		t.Fatalf("role change retained revision: current=%v err=%v", current, err)
	}
	if _, err := store.UpdateAccount(ctx, acceptedID, acceptedID, AccountChanges{Role: ptrRole(authv1.AccountRole_ACCOUNT_ROLE_USER)}); !errors.Is(err, ErrLastActiveAdmin) {
		t.Fatalf("protect remaining admin: %v", err)
	}
	before, err := accepted.Revision()
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := accounts.HashNewPassword(ctx, "NHA049 replacement password ✅")
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := store.RecoverAccount(ctx, acceptedID, replacement)
	if err != nil {
		t.Fatal(err)
	}
	after, err := recovered.Revision()
	if err != nil || !after.NewerThan(before) {
		t.Fatalf("revocation revision: %v", err)
	}
	current, err := store.RevisionCurrent(ctx, before)
	if err != nil || current {
		t.Fatalf("old revision remained current: %v %v", current, err)
	}
	current, err = store.RevisionCurrent(ctx, after)
	if err != nil || !current {
		t.Fatalf("new revision rejected: %v %v", current, err)
	}
	var match bool
	var auditCount int
	if err := store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.auth_accounts WHERE id=$1 AND password_digest=$2)`, mustAccountID(t, acceptedID), []byte("NHA049 replacement password ✅")).Scan(&match); err != nil || match {
		t.Fatalf("plaintext digest persisted: %v %v", match, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.auth_audit WHERE outcome=2`).Scan(&auditCount); err != nil || auditCount < 4 {
		t.Fatalf("denial audit count=%d: %v", auditCount, err)
	}
	var auditDDL string
	if err := store.pool.QueryRow(ctx, `SELECT string_agg(column_name, ',') FROM information_schema.columns WHERE table_schema='public' AND table_name='auth_audit'`).Scan(&auditDDL); err != nil || strings.Contains(auditDDL, "password") || strings.Contains(auditDDL, "token") {
		t.Fatalf("audit schema contains credential: %v", err)
	}
	third := nha049AccountInput(t, "Administrator Three", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	if _, err := store.CreateAccount(ctx, acceptedID, third); err != nil {
		t.Fatal(err)
	}
	admins := []contracts.AccountID{acceptedID, third.ID}
	results = make([]error, 2)
	for i := range admins {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			_, results[i] = store.UpdateAccount(ctx, admins[i], admins[i], AccountChanges{Role: ptrRole(authv1.AccountRole_ACCOUNT_ROLE_USER)})
		}(i)
	}
	wait.Wait()
	winners, denials = 0, 0
	for i, result := range results {
		if result == nil {
			winners++
		} else if errors.Is(result, ErrLastActiveAdmin) {
			denials++
		} else {
			t.Fatalf("concurrent admin demotion %d: %v", i, result)
		}
	}
	if winners != 1 || denials != 1 {
		t.Fatalf("concurrent admin demotions winners=%d denied=%d", winners, denials)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM public.auth_accounts WHERE role=1 AND status=1`).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("active administrator count=%d: %v", auditCount, err)
	}
	remainingAdmin := admins[0]
	if results[0] == nil {
		remainingAdmin = admins[1]
	}
	profile, salt, digest, err := user.Verifier.StorageValues()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO public.auth_accounts
		(id,login_name,login_key,display_name,role,status,password_profile,password_salt,password_digest)
		SELECT 'bulk-'||n,'bulk-'||n,'bulk-'||n,'Bulk '||n,2,1,$1,$2,$3
		FROM generate_series(1,$4) AS n`, profile, salt[:], digest[:], accountListLimit); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListAccounts(ctx, remainingAdmin); !errors.Is(err, ErrAccountListTooLarge) {
		t.Fatalf("oversize listing was silently truncated: %v", err)
	}
}

func TestNHA049AuditInsertFailureRollsBackAccountAndInvitation(t *testing.T) {
	port := testCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, testSettings(t, port))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	admin := nha049AccountInput(t, "Audit Failure Admin", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	if _, err := store.BootstrapAdmin(ctx, admin); err != nil {
		t.Fatal(err)
	}
	// NOT VALID leaves the bootstrap audit intact but rejects every new INSERT.
	// The account/invitation mutation must roll back with its required audit.
	if _, err := store.pool.Exec(ctx, `ALTER TABLE public.auth_audit
		ADD CONSTRAINT nha049_reject_audit_insert CHECK (false) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	account := nha049AccountInput(t, "Audit Failure Account", authv1.AccountRole_ACCOUNT_ROLE_USER)
	if _, err := store.CreateAccount(ctx, admin.ID, account); !errors.Is(err, ErrAccountUnavailable) {
		t.Fatalf("account committed without audit: %v", err)
	}
	invitationID, err := accounts.NewInvitationID()
	if err != nil {
		t.Fatal(err)
	}
	token, err := accounts.NewInvitationToken()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := token.Digest()
	if err != nil {
		t.Fatal(err)
	}
	login, err := accounts.ParseLoginName("Audit Failure Invite")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateInvitation(ctx, admin.ID, NewInvitation{ID: invitationID, Login: login,
		Role: authv1.AccountRole_ACCOUNT_ROLE_USER, Digest: digest, ExpiresAt: time.Now().Add(time.Hour)}); !errors.Is(err, ErrAccountUnavailable) {
		t.Fatalf("invitation committed without audit: %v", err)
	}
	accountID := mustAccountID(t, account.ID)
	inviteID, err := invitationID.Value()
	if err != nil {
		t.Fatal(err)
	}
	_, accountKey, err := account.Login.Values()
	if err != nil {
		t.Fatal(err)
	}
	_, inviteKey, err := login.Values()
	if err != nil {
		t.Fatal(err)
	}
	var accountsLeft, invitationsLeft, claimsLeft, audits int64
	if err := store.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM public.auth_accounts WHERE id=$1),
		(SELECT count(*) FROM public.auth_invitations WHERE id=$2),
		(SELECT count(*) FROM public.auth_login_claims WHERE login_key IN ($3,$4)),
		(SELECT count(*) FROM public.auth_audit)`, accountID, inviteID, accountKey, inviteKey).
		Scan(&accountsLeft, &invitationsLeft, &claimsLeft, &audits); err != nil {
		t.Fatal(err)
	}
	if accountsLeft != 0 || invitationsLeft != 0 || claimsLeft != 0 || audits != 1 {
		t.Fatalf("failed audit left mutations: accounts=%d invitations=%d claims=%d audits=%d",
			accountsLeft, invitationsLeft, claimsLeft, audits)
	}
}

func ptrRole(role authv1.AccountRole) *authv1.AccountRole { return &role }

func mustAccountID(t *testing.T, id contracts.AccountID) string {
	t.Helper()
	value, err := id.Value()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

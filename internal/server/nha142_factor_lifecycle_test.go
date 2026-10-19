package server

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/jackc/pgx/v5"
)

func nha142LifecycleAccountID(t *testing.T, rig *nha142ServerRig) *authv1.AccountId {
	t.Helper()
	id, err := rig.account.Value()
	if err != nil {
		t.Fatal(err)
	}
	return &authv1.AccountId{Value: id}
}

func nha142LifecycleFIDOLogin(t *testing.T, rig *nha142ServerRig, key *nha142Authenticator) *authv1.SessionTokens {
	t.Helper()
	challenge := rig.start(t).GetChallenge()
	if challenge == nil || challenge.Id == nil {
		t.Fatal("enrolled account did not receive a factor challenge")
	}
	result, err := rig.handler.CompleteLogin(rig.ctx, connect.NewRequest(&authv1.CompleteLoginRequest{
		ChallengeId: challenge.Id,
		Proof:       key.assertion(t, challenge, 0x05),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Msg == nil || result.Msg.Decision == nil {
		t.Fatal("FIDO2 completion returned no decision")
	}
	tokens := result.Msg.Decision.GetAuthenticated()
	if tokens == nil || tokens.Session == nil || tokens.Session.Id == nil ||
		tokens.Session.CreatedAt == nil || tokens.Session.AuthenticatedAt == nil ||
		tokens.AccessToken == "" || tokens.RefreshToken == "" ||
		tokens.Session.AuthenticationSatisfaction != authv1.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_FIDO2 ||
		!tokens.Session.CreatedAt.AsTime().Equal(tokens.Session.AuthenticatedAt.AsTime()) {
		t.Fatal("actual FIDO2 proof did not produce typed original authentication")
	}
	return tokens
}

func nha142LifecycleExpectAccessRevoked(t *testing.T, rig *nha142ServerRig, tokens *authv1.SessionTokens) {
	t.Helper()
	if tokens == nil || tokens.AccessToken == "" {
		t.Fatal("missing access token for revocation assertion")
	}
	request := httptest.NewRequest(http.MethodPost, "/nha142/revoked", nil)
	request.TLS = &tls.ConnectionState{}
	request.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	_, _, release, err := (&sessionAuth{store: rig.store, coordinator: rig.coordinator}).AuthenticateWithLease(rig.ctx, request)
	if release != nil {
		release()
	}
	if err == nil {
		t.Fatal("revoked access token was admitted")
	}
}

func nha142LifecycleExpectRevoked(t *testing.T, rig *nha142ServerRig, tokens *authv1.SessionTokens) {
	t.Helper()
	nha142LifecycleExpectAccessRevoked(t, rig, tokens)
	if tokens.RefreshToken == "" {
		t.Fatal("missing refresh token for revocation assertion")
	}
	if _, err := rig.handler.RefreshSession(rig.ctx, connect.NewRequest(&authv1.RefreshSessionRequest{
		RefreshToken: tokens.RefreshToken,
	})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked refresh token returned %v, want unauthenticated", connect.CodeOf(err))
	}
}

func nha142LifecycleAudit(t *testing.T, rig *nha142ServerRig) (int, string) {
	t.Helper()
	account, err := rig.account.Value()
	if err != nil {
		t.Fatal(err)
	}
	var count int
	var serialized string
	err = rig.store.InTransaction(rig.ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(rig.ctx, "SELECT count(*),COALESCE(string_agg(row_to_json(a)::text,E'\\n'),'') FROM public.auth_audit a WHERE target_account_id=$1", account).Scan(&count, &serialized)
	})
	if err != nil {
		t.Fatal(err)
	}
	return count, serialized
}

func nha142LifecycleAssertAudit(t *testing.T, rig *nha142ServerRig, before int, secrets ...string) {
	t.Helper()
	after, serialized := nha142LifecycleAudit(t, rig)
	if after != before+1 {
		t.Fatalf("factor mutation wrote %d audit rows, want one", after-before)
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(serialized, secret) {
			t.Fatal("factor mutation leaked a credential or recovery code into audit")
		}
	}
}

func TestNHA142FactorLifecycleReplaceAndRemove(t *testing.T) {
	rig := nha142NewServerRig(t)
	plain := rig.start(t).GetAuthenticated()
	if plain == nil {
		t.Fatal("default false policy did not issue a password session")
	}
	key := nha142NewAuthenticator(t)
	activated := rig.enroll(t, key, plain.AccessToken, "")
	if activated.Factor == nil || activated.Factor.Id == nil {
		t.Fatal("activation returned no factor metadata")
	}
	nha142LifecycleExpectRevoked(t, rig, plain)

	fido := nha142LifecycleFIDOLogin(t, rig, key)
	ctx, release := rig.session(t, fido)
	listed, err := rig.handler.ListFactors(ctx, connect.NewRequest(&authv1.ListFactorsRequest{
		AccountId: nha142LifecycleAccountID(t, rig),
	}))
	release()
	if err != nil {
		t.Fatal(err)
	}
	if listed == nil || listed.Msg == nil || len(listed.Msg.Factors) != 1 {
		t.Fatal("self factor listing did not return exactly one credential")
	}
	factor := listed.Msg.Factors[0]
	if factor.Id == nil || factor.AccountId == nil ||
		factor.Id.Value != activated.Factor.Id.Value ||
		factor.AccountId.Value != activated.Factor.AccountId.Value ||
		factor.Kind != authv1.FactorKind_FACTOR_KIND_FIDO2 ||
		factor.CreatedAt == nil || factor.LastUsedAt == nil {
		t.Fatal("self listing lost FIDO2 metadata or last-use time")
	}
	// The public metadata has no field for a COSE key, attestation, or codes.
	if len(activated.RecoveryCodes) != authmfa.RecoveryCodeCount {
		t.Fatal("activation did not return the required recovery batch")
	}

	before, _ := nha142LifecycleAudit(t, rig)
	ctx, release = rig.session(t, fido)
	replaced, err := rig.handler.ReplaceRecoveryCodes(ctx, connect.NewRequest(&authv1.ReplaceRecoveryCodesRequest{
		AccountId: nha142LifecycleAccountID(t, rig),
	}))
	release()
	if err != nil {
		t.Fatal(err)
	}
	if replaced == nil || replaced.Msg == nil || len(replaced.Msg.RecoveryCodes) != authmfa.RecoveryCodeCount {
		t.Fatal("replacement did not return exactly ten codes")
	}
	seen := make(map[string]struct{}, len(replaced.Msg.RecoveryCodes))
	for _, code := range replaced.Msg.RecoveryCodes {
		if code == "" {
			t.Fatal("replacement returned an empty code")
		}
		if _, duplicate := seen[code]; duplicate {
			t.Fatal("replacement returned duplicate codes")
		}
		seen[code] = struct{}{}
	}
	for _, old := range activated.RecoveryCodes {
		if _, reused := seen[old]; reused {
			t.Fatal("replacement reused an old code")
		}
	}
	privateValues := append([]string{factor.Id.Value}, activated.RecoveryCodes...)
	privateValues = append(privateValues, replaced.Msg.RecoveryCodes...)
	nha142LifecycleAssertAudit(t, rig, before, privateValues...)
	nha142LifecycleExpectRevoked(t, rig, fido)

	challenge := rig.start(t).GetChallenge()
	if challenge == nil {
		t.Fatal("factor challenge missing after code replacement")
	}
	if _, err := rig.handler.CompleteLogin(rig.ctx, nha142RecoveryRequest(challenge, activated.RecoveryCodes[0])); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("old unused code returned %v, want unauthenticated", connect.CodeOf(err))
	}
	recovered, err := rig.handler.CompleteLogin(rig.ctx, nha142RecoveryRequest(challenge, replaced.Msg.RecoveryCodes[0]))
	if err != nil || recovered == nil || recovered.Msg == nil || recovered.Msg.Decision == nil {
		t.Fatalf("replacement code did not authenticate: %v", err)
	}
	recovery := recovered.Msg.Decision.GetAuthenticated()
	if recovery == nil || recovery.Session == nil ||
		recovery.Session.AuthenticationSatisfaction != authv1.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_RECOVERY_CODE {
		t.Fatal("replacement code lost typed recovery provenance")
	}

	before, _ = nha142LifecycleAudit(t, rig)
	ctx, release = rig.session(t, recovery)
	_, err = rig.handler.RemoveFactor(ctx, connect.NewRequest(&authv1.RemoveFactorRequest{FactorId: factor.Id}))
	release()
	if err != nil {
		t.Fatal(err)
	}
	nha142LifecycleAssertAudit(t, rig, before, privateValues...)
	nha142LifecycleExpectRevoked(t, rig, recovery)

	afterRemoval := rig.start(t).GetAuthenticated()
	if afterRemoval == nil || afterRemoval.Session == nil ||
		afterRemoval.Session.AuthenticationSatisfaction != authv1.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_ONLY {
		t.Fatal("removing the last factor under false policy did not restore password login")
	}
	ctx, release = rig.session(t, afterRemoval)
	empty, err := rig.handler.ListFactors(ctx, connect.NewRequest(&authv1.ListFactorsRequest{
		AccountId: nha142LifecycleAccountID(t, rig),
	}))
	release()
	if err != nil || empty == nil || empty.Msg == nil || len(empty.Msg.Factors) != 0 {
		t.Fatalf("removed factor remained in self listing: %v", err)
	}
}

func TestNHA142RefreshDoesNotRenewFactorRecency(t *testing.T) {
	rig := nha142NewServerRig(t)
	plain := rig.start(t).GetAuthenticated()
	if plain == nil {
		t.Fatal("default false policy did not issue a password session")
	}
	key := nha142NewAuthenticator(t)
	activated := rig.enroll(t, key, plain.AccessToken, "")
	if activated.Factor == nil || activated.Factor.Id == nil {
		t.Fatal("activation returned no factor metadata")
	}
	fido := nha142LifecycleFIDOLogin(t, rig, key)
	original := fido.Session.AuthenticatedAt.AsTime()
	rig.clock.Advance(original.Add(authmfa.RecentAuthenticationLifetime + time.Second))

	rotated, err := rig.handler.RefreshSession(rig.ctx, connect.NewRequest(&authv1.RefreshSessionRequest{
		RefreshToken: fido.RefreshToken,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if rotated == nil || rotated.Msg == nil || rotated.Msg.Tokens == nil || rotated.Msg.Tokens.Session == nil {
		t.Fatal("refresh returned no session")
	}
	refreshed := rotated.Msg.Tokens
	if refreshed.Session.AuthenticatedAt == nil ||
		!refreshed.Session.AuthenticatedAt.AsTime().Equal(original) ||
		refreshed.Session.AuthenticationSatisfaction != authv1.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_FIDO2 ||
		refreshed.Session.Id.Value != fido.Session.Id.Value {
		t.Fatal("refresh changed original typed authentication evidence")
	}
	// Replaying a consumed refresh token revokes its active family, so inspect
	// the old access only while the rotated session is still under test.
	nha142LifecycleExpectAccessRevoked(t, rig, fido)

	ctx, release := rig.session(t, refreshed)
	_, replaceErr := rig.handler.ReplaceRecoveryCodes(ctx, connect.NewRequest(&authv1.ReplaceRecoveryCodesRequest{
		AccountId: nha142LifecycleAccountID(t, rig),
	}))
	_, removeErr := rig.handler.RemoveFactor(ctx, connect.NewRequest(&authv1.RemoveFactorRequest{
		FactorId: activated.Factor.Id,
	}))
	release()
	if connect.CodeOf(replaceErr) != connect.CodeUnauthenticated || connect.CodeOf(removeErr) != connect.CodeUnauthenticated {
		t.Fatalf("stale authentication admitted mutation: replace=%v remove=%v", connect.CodeOf(replaceErr), connect.CodeOf(removeErr))
	}

	fresh := nha142LifecycleFIDOLogin(t, rig, key)
	if !fresh.Session.AuthenticatedAt.AsTime().After(original) {
		t.Fatal("new FIDO2 login did not establish fresh authentication")
	}
	before, _ := nha142LifecycleAudit(t, rig)
	ctx, release = rig.session(t, fresh)
	_, err = rig.handler.RemoveFactor(ctx, connect.NewRequest(&authv1.RemoveFactorRequest{
		FactorId: activated.Factor.Id,
	}))
	release()
	if err != nil {
		t.Fatal(err)
	}
	nha142LifecycleAssertAudit(t, rig, before, activated.Factor.Id.Value)
	nha142LifecycleExpectRevoked(t, rig, fresh)
	nha142LifecycleExpectRevoked(t, rig, refreshed)
}

func TestNHA142OperatorResetDistinctFromPasswordRecovery(t *testing.T) {
	rig := nha142NewServerRig(t)
	plain := rig.start(t).GetAuthenticated()
	if plain == nil {
		t.Fatal("default false policy did not issue a password session")
	}
	rig.requirePolicy(t, plain)
	grant := rig.start(t).GetEnrollmentRequired()
	if grant == nil {
		t.Fatal("required admin received no enrollment grant")
	}
	key := nha142NewAuthenticator(t)
	activated := rig.enroll(t, key, "", grant.EnrollmentToken)
	if activated.Factor == nil || activated.Factor.Id == nil {
		t.Fatal("activation returned no factor metadata")
	}
	fido := nha142LifecycleFIDOLogin(t, rig, key)

	operator := &accountHandler{store: rig.store, coordinator: rig.coordinator}
	const replacementPassword = "NHA142 synthetic operator replacement password"
	recovered, err := operator.RecoverAccount(rig.ctx, connect.NewRequest(&authv1.RecoverAccountRequest{
		AccountId:           nha142LifecycleAccountID(t, rig),
		ReplacementPassword: replacementPassword,
	}))
	if err != nil || recovered == nil || recovered.Msg == nil || recovered.Msg.Account == nil {
		t.Fatalf("password recovery failed: %v", err)
	}
	if !recovered.Msg.Account.SecondFactorEnabled {
		t.Fatal("password recovery removed the factor")
	}
	nha142LifecycleExpectRevoked(t, rig, fido)
	rig.password = replacementPassword
	if rig.start(t).GetChallenge() == nil {
		t.Fatal("password recovery bypassed the retained factor")
	}
	fresh := nha142LifecycleFIDOLogin(t, rig, key)

	before, _ := nha142LifecycleAudit(t, rig)
	reset, err := operator.ResetSecondFactor(rig.ctx, connect.NewRequest(&authv1.ResetSecondFactorRequest{
		AccountId: nha142LifecycleAccountID(t, rig),
	}))
	if err != nil || reset == nil || reset.Msg == nil || reset.Msg.Account == nil {
		t.Fatalf("explicit operator factor reset failed: %v", err)
	}
	if reset.Msg.Account.SecondFactorEnabled {
		t.Fatal("operator factor reset left the account protected")
	}
	secrets := append([]string{activated.Factor.Id.Value}, activated.RecoveryCodes...)
	nha142LifecycleAssertAudit(t, rig, before, secrets...)
	nha142LifecycleExpectRevoked(t, rig, fresh)
	if rig.start(t).GetEnrollmentRequired() == nil {
		t.Fatal("required admin did not return to enrollment after explicit reset")
	}
}

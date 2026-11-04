package server

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
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

func nha142LifecycleFIDOLogin(t *testing.T, rig *nha142ServerRig, key *nha142Authenticator) *api.SessionTokens {
	t.Helper()
	challenge := rig.start(t).GetChallenge()
	if challenge == nil || challenge.ChallengeId == "" {
		t.Fatal("enrolled account did not receive a factor challenge")
	}
	result, err := rig.handler.CompleteLogin(rig.ctx, connect.NewRequest(&api.CompleteLoginRequest{
		ChallengeId: challenge.ChallengeId,
		Proof:       key.assertion(t, challenge, 0x05),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Msg == nil || result.Msg.Decision == nil {
		t.Fatal("FIDO2 completion returned no decision")
	}
	tokens := result.Msg.Decision.GetAuthenticated()
	if tokens == nil || tokens.Session == nil || tokens.Session.Name == "" ||
		tokens.Session.CreateTime == nil || tokens.Session.AuthenticationTime == nil ||
		tokens.AccessToken == "" || tokens.RefreshToken == "" ||
		tokens.Session.AuthenticationSatisfaction != api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_FIDO2 ||
		!tokens.Session.CreateTime.AsTime().Equal(tokens.Session.AuthenticationTime.AsTime()) {
		t.Fatal("actual FIDO2 proof did not produce typed original authentication")
	}
	return tokens
}

func nha142LifecycleExpectAccessRevoked(t *testing.T, rig *nha142ServerRig, tokens *api.SessionTokens) {
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

func nha142LifecycleExpectRevoked(t *testing.T, rig *nha142ServerRig, tokens *api.SessionTokens) {
	t.Helper()
	nha142LifecycleExpectAccessRevoked(t, rig, tokens)
	if tokens.RefreshToken == "" {
		t.Fatal("missing refresh token for revocation assertion")
	}
	if _, err := rig.handler.RefreshSession(rig.ctx, connect.NewRequest(&api.RefreshSessionRequest{
		RefreshToken: tokens.RefreshToken, RenewalKey: []byte("nha198-saved-operation-secret-001"),
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
	if activated.FactorCredential == nil || activated.FactorCredential.Name == "" {
		t.Fatal("activation returned no factor metadata")
	}
	nha142LifecycleExpectRevoked(t, rig, plain)

	fido := nha142LifecycleFIDOLogin(t, rig, key)
	ctx, release := rig.session(t, fido)
	listed, err := rig.handler.ListFactorCredentials(ctx, connect.NewRequest(&api.ListFactorCredentialsRequest{
		Parent: rig.accountName.String(),
	}))
	release()
	if err != nil {
		t.Fatal(err)
	}
	if listed == nil || listed.Msg == nil || len(listed.Msg.FactorCredentials) != 1 {
		t.Fatal("self factor listing did not return exactly one credential")
	}
	factor := listed.Msg.FactorCredentials[0]
	if factor.Name == "" ||
		factor.Name != activated.FactorCredential.Name ||
		factor.Kind != api.FactorKind_FACTOR_KIND_FIDO2 ||
		factor.CreateTime == nil || factor.LastUseTime == nil {
		t.Fatal("self listing lost FIDO2 metadata or last-use time")
	}
	// The public metadata has no field for a COSE key, attestation, or codes.
	if len(activated.RecoveryCodes) != authmfa.RecoveryCodeCount {
		t.Fatal("activation did not return the required recovery batch")
	}

	before, _ := nha142LifecycleAudit(t, rig)
	ctx, release = rig.session(t, fido)
	replaced, err := rig.handler.ReplaceRecoveryCodes(ctx, connect.NewRequest(&api.ReplaceRecoveryCodesRequest{
		Name: rig.accountName.String(),
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
	privateValues := append([]string{factor.Name}, activated.RecoveryCodes...)
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
		recovery.Session.AuthenticationSatisfaction != api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_RECOVERY_CODE {
		t.Fatal("replacement code lost typed recovery provenance")
	}

	before, _ = nha142LifecycleAudit(t, rig)
	ctx, release = rig.session(t, recovery)
	_, err = rig.handler.RemoveFactorCredential(ctx, connect.NewRequest(&api.RemoveFactorCredentialRequest{Name: factor.Name}))
	release()
	if err != nil {
		t.Fatal(err)
	}
	nha142LifecycleAssertAudit(t, rig, before, privateValues...)
	nha142LifecycleExpectRevoked(t, rig, recovery)

	afterRemoval := rig.start(t).GetAuthenticated()
	if afterRemoval == nil || afterRemoval.Session == nil ||
		afterRemoval.Session.AuthenticationSatisfaction != api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_ONLY {
		t.Fatal("removing the last factor under false policy did not restore password login")
	}
	ctx, release = rig.session(t, afterRemoval)
	empty, err := rig.handler.ListFactorCredentials(ctx, connect.NewRequest(&api.ListFactorCredentialsRequest{
		Parent: rig.accountName.String(),
	}))
	release()
	if err != nil || empty == nil || empty.Msg == nil || len(empty.Msg.FactorCredentials) != 0 {
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
	if activated.FactorCredential == nil || activated.FactorCredential.Name == "" {
		t.Fatal("activation returned no factor metadata")
	}
	fido := nha142LifecycleFIDOLogin(t, rig, key)
	original := fido.Session.AuthenticationTime.AsTime()
	rig.clock.Advance(original.Add(authmfa.RecentAuthenticationLifetime + time.Second))

	rotated, err := rig.handler.RefreshSession(rig.ctx, connect.NewRequest(&api.RefreshSessionRequest{
		RefreshToken: fido.RefreshToken, RenewalKey: []byte("nha198-saved-operation-secret-001"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if rotated == nil || rotated.Msg == nil || rotated.Msg.Tokens == nil || rotated.Msg.Tokens.Session == nil {
		t.Fatal("refresh returned no session")
	}
	refreshed := rotated.Msg.Tokens
	if refreshed.Session.AuthenticationTime == nil ||
		!refreshed.Session.AuthenticationTime.AsTime().Equal(original) ||
		refreshed.Session.AuthenticationSatisfaction != api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_FIDO2 ||
		refreshed.Session.Name != fido.Session.Name {
		t.Fatal("refresh changed original typed authentication evidence")
	}
	// Replaying a consumed refresh token revokes its active family, so inspect
	// the old access only while the rotated session is still under test.
	nha142LifecycleExpectAccessRevoked(t, rig, fido)

	ctx, release := rig.session(t, refreshed)
	_, replaceErr := rig.handler.ReplaceRecoveryCodes(ctx, connect.NewRequest(&api.ReplaceRecoveryCodesRequest{
		Name: rig.accountName.String(),
	}))
	_, removeErr := rig.handler.RemoveFactorCredential(ctx, connect.NewRequest(&api.RemoveFactorCredentialRequest{
		Name: activated.FactorCredential.Name,
	}))
	release()
	if connect.CodeOf(replaceErr) != connect.CodeUnauthenticated || connect.CodeOf(removeErr) != connect.CodeUnauthenticated {
		t.Fatalf("stale authentication admitted mutation: replace=%v remove=%v", connect.CodeOf(replaceErr), connect.CodeOf(removeErr))
	}

	fresh := nha142LifecycleFIDOLogin(t, rig, key)
	if !fresh.Session.AuthenticationTime.AsTime().After(original) {
		t.Fatal("new FIDO2 login did not establish fresh authentication")
	}
	before, _ := nha142LifecycleAudit(t, rig)
	ctx, release = rig.session(t, fresh)
	_, err = rig.handler.RemoveFactorCredential(ctx, connect.NewRequest(&api.RemoveFactorCredentialRequest{
		Name: activated.FactorCredential.Name,
	}))
	release()
	if err != nil {
		t.Fatal(err)
	}
	nha142LifecycleAssertAudit(t, rig, before, activated.FactorCredential.Name)
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
	if activated.FactorCredential == nil || activated.FactorCredential.Name == "" {
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
	secrets := append([]string{activated.FactorCredential.Name}, activated.RecoveryCodes...)
	nha142LifecycleAssertAudit(t, rig, before, secrets...)
	nha142LifecycleExpectRevoked(t, rig, fresh)
	if rig.start(t).GetEnrollmentRequired() == nil {
		t.Fatal("required admin did not return to enrollment after explicit reset")
	}
}

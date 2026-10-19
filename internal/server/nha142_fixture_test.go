package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/fxamacker/cbor/v2"
)

// These tests exercise direct handlers with a simulated TLS admission marker.
// They are not a substitute for the separate real Run/TLS contract tests.
type nha142ServerRig struct {
	ctx         context.Context
	store       *storage.Store
	coordinator *sessionCoordinator
	handler     *authHandler
	clock       *nha141Clock
	account     contracts.AccountID
	password    string
	login       string
}

func nha142NewServerRig(t *testing.T) *nha142ServerRig {
	t.Helper()
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	configuration := nha181Config(t, cluster.Port, data, "127.0.0.1:0", strings.Replace(nha181TLSYAML(), "public:\n", "public:\n  url: https://nha142.example.test\n", 1))
	postgres, err := configuration.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	store, err := storage.Open(ctx, postgres)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); store.Close() })
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapMFAPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	protector, err := store.BootstrapSecrets(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := configuration.PublicTransport()
	if err != nil {
		t.Fatal(err)
	}
	clock := newNHA141Clock(time.Now().UTC().Truncate(time.Microsecond))
	coordinator, err := prepareLocalAuthorizationWithSettings(ctx, store, clock, settings)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.mfa, err = newMFARuntime(ctx, coordinator, protector, settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.mfa.requireReady(); err != nil {
		t.Fatal("test canonical HTTPS RP is not configured", err)
	}
	t.Cleanup(func() {
		bounded, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		if err := coordinator.mfa.close(bounded); err != nil {
			t.Error(err)
		}
	})
	login, password := "NHA142 Admin", "NHA142 synthetic password canary"
	accountHandler := &accountHandler{store: store, coordinator: coordinator}
	response, err := accountHandler.BootstrapAdmin(ctx, connect.NewRequest(&authv1.BootstrapAdminRequest{LoginName: login, DisplayName: login, Password: password}))
	if err != nil {
		t.Fatal(err)
	}
	// Operator bootstrap uses its real creation timestamp; logical mutations
	// must not predate it. Keep strict AccountRecord timestamp admission.
	clock.Advance(time.Now().UTC().Truncate(time.Microsecond))
	account, err := contracts.ParseAccountID(response.Msg.Account.Id.Value)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAuthHandler(store, coordinator, coordinator.mfa)
	if err != nil {
		t.Fatal(err)
	}
	return &nha142ServerRig{ctx: ctx, store: store, coordinator: coordinator, handler: handler, clock: clock, account: account, password: password, login: login}
}
func (r *nha142ServerRig) start(t *testing.T) *authv1.LoginDecision {
	t.Helper()
	response, err := r.handler.StartLogin(r.ctx, connect.NewRequest(&authv1.StartLoginRequest{LoginName: r.login, Password: r.password}))
	if err != nil {
		t.Fatal(err)
	}
	return response.Msg.Decision
}
func (r *nha142ServerRig) session(t *testing.T, tokens *authv1.SessionTokens) (context.Context, func()) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/nha142/direct", nil)
	request.TLS = &tls.ConnectionState{}
	request.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	actor, ctx, release, err := (&sessionAuth{store: r.store, coordinator: r.coordinator}).AuthenticateWithLease(r.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	return context.WithValue(ctx, actorContextKey{}, actor), release
}
func (r *nha142ServerRig) tlsContext() context.Context {
	return context.WithValue(r.ctx, nativeTLSContextKey{}, nativeTLSAdmission{})
}
func (r *nha142ServerRig) requirePolicy(t *testing.T, tokens *authv1.SessionTokens) *authv1.SetSecondFactorPolicyResponse {
	t.Helper()
	snapshot, err := r.store.MFAPolicy(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	current, err := policyWire(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	ctx, release := r.session(t, tokens)
	defer release()
	response, err := (&accountHandler{store: r.store, coordinator: r.coordinator}).SetSecondFactorPolicy(ctx, connect.NewRequest(&authv1.SetSecondFactorPolicyRequest{Expected: current.Version, RequireAdminFactor: true}))
	if err != nil {
		t.Fatal(err)
	}
	return response.Msg
}

type nha142Authenticator struct {
	key     *ecdsa.PrivateKey
	id      []byte
	counter uint32
}

func nha142NewAuthenticator(t *testing.T) *nha142Authenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		t.Fatal(err)
	}
	return &nha142Authenticator{key: key, id: id}
}
func nha142ClientData(t *testing.T, kind string, challenge []byte, origin string) []byte {
	t.Helper()
	value, err := json.Marshal(struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
		Origin    string `json:"origin"`
	}{kind, base64.RawURLEncoding.EncodeToString(challenge), origin})
	if err != nil {
		t.Fatal(err)
	}
	return value
}
func (a *nha142Authenticator) attestation(t *testing.T, challenge *authv1.EnrollmentChallenge) *authv1.Fido2Attestation {
	t.Helper()
	options := challenge.Fido2
	data := nha142ClientData(t, "webauthn.create", options.Challenge, options.Origin)
	hash := sha256.Sum256(data)
	rpHash := sha256.Sum256([]byte(options.RpId))
	key, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: a.key.X.FillBytes(make([]byte, 32)), -3: a.key.Y.FillBytes(make([]byte, 32))})
	if err != nil {
		t.Fatal(err)
	}
	auth := append([]byte(nil), rpHash[:]...)
	auth = append(auth, 0x45)
	auth = binary.BigEndian.AppendUint32(auth, 0)
	auth = append(auth, make([]byte, 16)...)
	auth = binary.BigEndian.AppendUint16(auth, uint16(len(a.id)))
	auth = append(auth, a.id...)
	auth = append(auth, key...)
	object, err := cbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": auth})
	if err != nil {
		t.Fatal(err)
	}
	return &authv1.Fido2Attestation{CredentialId: append([]byte(nil), a.id...), ClientDataJson: data, ClientDataHash: hash[:], AttestationObject: object}
}
func (a *nha142Authenticator) assertion(t *testing.T, challenge *authv1.FactorChallenge, flags byte) *authv1.FactorProof {
	t.Helper()
	var options *authv1.Fido2AssertionChallenge
	for _, option := range challenge.Options {
		if value := option.GetFido2(); value != nil {
			options = value
			break
		}
	}
	if options == nil {
		t.Fatal("no actual FIDO2 option")
	}
	data := nha142ClientData(t, "webauthn.get", options.Challenge, options.Origin)
	hash := sha256.Sum256(data)
	rpHash := sha256.Sum256([]byte(options.RpId))
	a.counter++
	auth := append([]byte(nil), rpHash[:]...)
	auth = append(auth, flags)
	auth = binary.BigEndian.AppendUint32(auth, a.counter)
	signed := sha256.Sum256(append(append([]byte(nil), auth...), hash[:]...))
	signature, err := ecdsa.SignASN1(rand.Reader, a.key, signed[:])
	if err != nil {
		t.Fatal(err)
	}
	return &authv1.FactorProof{Proof: &authv1.FactorProof_Fido2{Fido2: &authv1.Fido2Assertion{CredentialId: append([]byte(nil), a.id...), ClientDataJson: data, ClientDataHash: hash[:], AuthenticatorData: auth, Signature: signature}}}
}
func (r *nha142ServerRig) enroll(t *testing.T, a *nha142Authenticator, access, grant string) *authv1.CompleteFactorEnrollmentResponse {
	t.Helper()
	begin := connect.NewRequest(&authv1.BeginFactorEnrollmentRequest{Kind: authv1.FactorKind_FACTOR_KIND_FIDO2, EnrollmentToken: grant})
	if access != "" {
		begin.Header().Set("Authorization", "Bearer "+access)
	}
	response, err := r.handler.BeginFactorEnrollment(r.tlsContext(), begin)
	if err != nil {
		t.Fatal(err)
	}
	complete := connect.NewRequest(&authv1.CompleteFactorEnrollmentRequest{ChallengeId: response.Msg.Challenge.Id, Attestation: a.attestation(t, response.Msg.Challenge), EnrollmentToken: grant})
	if access != "" {
		complete.Header().Set("Authorization", "Bearer "+access)
	}
	activated, err := r.handler.CompleteFactorEnrollment(r.tlsContext(), complete)
	if err != nil {
		t.Fatal(err)
	}
	if len(activated.Msg.RecoveryCodes) != authmfa.RecoveryCodeCount {
		t.Fatal("activation did not return exactly10 codes")
	}
	if _, err := r.handler.CompleteFactorEnrollment(r.tlsContext(), complete); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("activation/codes replayed", err)
	}
	return activated.Msg
}

type nha142CountingProvider struct {
	inner factorProvider
	calls atomic.Int64
}

func (p *nha142CountingProvider) Prepare(ctx context.Context, a contracts.AccountID, id contracts.ChallengeID) (factorPreparation, error) {
	return p.inner.Prepare(ctx, a, id)
}
func (p *nha142CountingProvider) Verify(ctx context.Context, s storage.MFALoginSnapshot, proof *authv1.FactorProof) (authmfa.VerifiedLogin, error) {
	p.calls.Add(1)
	return p.inner.Verify(ctx, s, proof)
}

// The old bool-provider fixture is replaced with a real verified credential
// and real policy. Its reservation/expiry/replay assertions remain meaningful.
func nha142ExerciseFactorReservations(t *testing.T) {
	rig := nha142NewServerRig(t)
	plain := rig.start(t).GetAuthenticated()
	if plain == nil {
		t.Fatal("unconfigured false policy forced a factor")
	}
	result := rig.requirePolicy(t, plain)
	if result.Continuation != authv1.SecondFactorSessionContinuation_SECOND_FACTOR_SESSION_CONTINUATION_REAUTHENTICATION_REQUIRED {
		t.Fatal("self-invalidating policy lost its final response")
	}
	before, err := rig.store.ListSessions(rig.ctx, rig.account, rig.account, rig.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	decision := rig.start(t)
	grant := decision.GetEnrollmentRequired()
	if grant == nil || decision.GetAuthenticated() != nil {
		t.Fatal("unenrolled required admin issued a session")
	}
	access, err := accounts.ParseAccessSecret(grant.EnrollmentToken)
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := access.Digest()
	if _, err := rig.store.LookupAccessOwner(rig.ctx, digest); !errors.Is(err, storage.ErrSessionNotFound) {
		t.Fatal("grant admitted by access index", err)
	}
	after, err := rig.store.ListSessions(rig.ctx, rig.account, rig.account, rig.clock.Now())
	if err != nil || len(after) != len(before) {
		t.Fatal("enrollment decision created a session", err)
	}
	authenticator := nha142NewAuthenticator(t)
	rig.enroll(t, authenticator, "", grant.EnrollmentToken)
	without, err := newAuthHandler(rig.store, rig.coordinator, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := without.StartLogin(rig.ctx, connect.NewRequest(&authv1.StartLoginRequest{LoginName: rig.login, Password: rig.password})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("missing provider downgraded account", err)
	}
	counting := &nha142CountingProvider{inner: rig.coordinator.mfa}
	rig.handler.provider = counting
	challenge := rig.start(t).GetChallenge()
	if challenge == nil {
		t.Fatal("real enrolled account had no challenge")
	}
	malformed := &authv1.CompleteLoginRequest{ChallengeId: challenge.Id, Proof: &authv1.FactorProof{Proof: &authv1.FactorProof_Fido2{Fido2: &authv1.Fido2Assertion{}}}}
	if _, err := rig.handler.CompleteLogin(rig.ctx, connect.NewRequest(malformed)); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("malformed proof admitted", err)
	}
	invalid := &authv1.CompleteLoginRequest{ChallengeId: challenge.Id, Proof: &authv1.FactorProof{Proof: &authv1.FactorProof_RecoveryCode{RecoveryCode: "NHA141 synthetic invalid recovery code"}}}
	for i := 0; i < 4; i++ {
		if _, err := rig.handler.CompleteLogin(rig.ctx, connect.NewRequest(invalid)); connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatal("unverified proof admitted", err)
		}
	}
	if counting.calls.Load() != 4 {
		t.Fatal("malformed proof reached verifier or reservation count changed", counting.calls.Load())
	}
	if _, err := rig.handler.CompleteLogin(rig.ctx, connect.NewRequest(invalid)); connect.CodeOf(err) != connect.CodeUnauthenticated || counting.calls.Load() != 4 {
		t.Fatal("sixth proof reached verifier", err)
	}
	rig.clock.Advance(challenge.ExpiresAt.AsTime())
	if _, err := rig.handler.CompleteLogin(rig.ctx, connect.NewRequest(invalid)); connect.CodeOf(err) != connect.CodeUnauthenticated || counting.calls.Load() != 4 {
		t.Fatal("exact expiry admitted proof", err)
	}
	rig.clock.Advance(rig.clock.Now().Add(16 * time.Minute))
	fresh := rig.start(t).GetChallenge()
	if fresh == nil {
		t.Fatal("fresh challenge missing")
	}
	request := &authv1.CompleteLoginRequest{ChallengeId: fresh.Id, Proof: authenticator.assertion(t, fresh, 0x05)}
	completed, err := rig.handler.CompleteLogin(rig.ctx, connect.NewRequest(request))
	if err != nil || completed == nil || completed.Msg.Decision.GetAuthenticated() == nil {
		t.Fatal("actual verified offered proof did not create session", err)
	}
	if completed.Msg.Decision.GetAuthenticated().Session.AuthenticationSatisfaction != authv1.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_FIDO2 {
		t.Fatal("typed evidence lost at transport")
	}
	if _, err := rig.handler.CompleteLogin(rig.ctx, connect.NewRequest(request)); connect.CodeOf(err) != connect.CodeUnauthenticated || counting.calls.Load() != 5 {
		t.Fatal("consumed challenge replay reached verifier", err)
	}
}

package server

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const loginBucketCapacity = 20

type factorPreparationKind uint8

const (
	factorPreparationUnspecified factorPreparationKind = iota
	factorPreparationAuthenticated
	factorPreparationChallenge
	factorPreparationEnrollment
)

type factorPreparation struct {
	revision storage.RevocationRevision
	policy   authmfa.ExpectedPolicyVersion
	ceremony authmfa.PreparedCeremony
	started  time.Time
	kind     factorPreparationKind
	options  []*authv1.FactorOption
}

func factorAuthenticated() factorPreparation {
	return factorPreparation{kind: factorPreparationAuthenticated}
}
func factorChallenge(options []*authv1.FactorOption) factorPreparation {
	copied := make([]*authv1.FactorOption, 0, len(options))
	for _, option := range options {
		if option == nil {
			return factorPreparation{}
		}
		copied = append(copied, proto.Clone(option).(*authv1.FactorOption))
	}
	return factorPreparation{kind: factorPreparationChallenge, options: copied}
}

// factorProvider is a private NHA-142 seam. Prepare returns exactly one of
// authenticated, challenge or enrollment required. Verify must perform the
// cryptographic proof check; wire admission alone cannot.
type factorProvider interface {
	Prepare(context.Context, contracts.AccountID, contracts.ChallengeID) (factorPreparation, error)
	Verify(context.Context, storage.MFALoginSnapshot, *authv1.FactorProof) (authmfa.VerifiedLogin, error)
}

type loginBucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func (b *loginBucket) take(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.last.IsZero() {
		b.last, b.tokens = now, loginBucketCapacity
	}
	if now.After(b.last) {
		b.tokens += now.Sub(b.last).Seconds() // 60 starts/minute
		if b.tokens > loginBucketCapacity {
			b.tokens = loginBucketCapacity
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

type authHandler struct {
	authv1connect.UnimplementedAuthServiceHandler
	store       *storage.Store
	coordinator *sessionCoordinator
	provider    factorProvider
	bucket      loginBucket
}

var _ authv1connect.AuthServiceHandler = (*authHandler)(nil)

func newAuthHandler(store *storage.Store, coordinator *sessionCoordinator, provider factorProvider) (*authHandler, error) {
	if store == nil || coordinator == nil || typedNilPublic(provider) {
		return nil, storage.ErrSessionInvalid
	}
	return &authHandler{store: store, coordinator: coordinator, provider: provider}, nil
}

func sessionRPCError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, errors.New("request canceled"))
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New("request deadline exceeded"))
	case errors.Is(err, accounts.ErrKDFBusy), errors.Is(err, storage.ErrLoginRateLimited), errors.Is(err, errSessionCoordinatorBusy):
		return connect.NewError(connect.CodeResourceExhausted, errors.New("authentication temporarily limited"))
	case errors.Is(err, accounts.ErrInvalidSessionToken), errors.Is(err, storage.ErrSessionInvalid):
		return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid session request"))
	case errors.Is(err, storage.ErrSessionPermission):
		return connect.NewError(connect.CodePermissionDenied, errors.New("session permission denied"))
	case errors.Is(err, storage.ErrSessionNotFound), errors.Is(err, errSessionUnauthenticated):
		return connect.NewError(connect.CodeUnauthenticated, errors.New("authentication failed"))
	default:
		return connect.NewError(connect.CodeUnavailable, errors.New("session service unavailable"))
	}
}

func (h *authHandler) deniedLogin(ctx context.Context, limited bool) error {
	if err := h.store.RecordLoginDenial(ctx, limited); err != nil {
		return sessionRPCError(err)
	}
	if limited {
		return sessionRPCError(storage.ErrLoginRateLimited)
	}
	return sessionRPCError(errSessionUnauthenticated)
}

type issuedCredentials struct {
	access        accounts.AccessSecret
	refresh       accounts.RefreshSecret
	accessDigest  accounts.AccessDigest
	refreshDigest accounts.RefreshDigest
}

func (issuedCredentials) String() string                 { return "[REDACTED SESSION CREDENTIALS]" }
func (v issuedCredentials) GoString() string             { return v.String() }
func (v issuedCredentials) MarshalJSON() ([]byte, error) { return json.Marshal(v.String()) }

func newIssuedCredentials() (issuedCredentials, error) {
	access, err := accounts.NewAccessSecret()
	if err != nil {
		return issuedCredentials{}, err
	}
	refresh, err := accounts.NewRefreshSecret()
	if err != nil {
		return issuedCredentials{}, err
	}
	accessDigest, err := access.Digest()
	if err != nil {
		return issuedCredentials{}, err
	}
	refreshDigest, err := refresh.Digest()
	if err != nil {
		return issuedCredentials{}, err
	}
	return issuedCredentials{access: access, refresh: refresh, accessDigest: accessDigest, refreshDigest: refreshDigest}, nil
}

func sessionWire(record storage.SessionRecord) (*authv1.Session, error) {
	id, err := record.ID()
	if err != nil {
		return nil, err
	}
	idText, err := id.Value()
	if err != nil {
		return nil, err
	}
	account, err := record.AccountID()
	if err != nil {
		return nil, err
	}
	accountText, err := account.Value()
	if err != nil {
		return nil, err
	}
	result := &authv1.Session{Id: &authv1.SessionId{Value: idText}, AccountId: &authv1.AccountId{Value: accountText},
		CreatedAt: timestamppb.New(record.CreatedAt()), ExpiresAt: timestamppb.New(record.ExpiresAt())}
	satisfaction, authenticated, err := record.Authentication()
	if err != nil {
		return nil, err
	}
	result.AuthenticationSatisfaction = satisfaction
	if !authenticated.IsZero() {
		result.AuthenticatedAt = timestamppb.New(authenticated)
	}
	if revoked := record.RevokedAt(); revoked != nil {
		result.RevokedAt = timestamppb.New(*revoked)
	}
	if used := record.LastUsedAt(); used != nil {
		result.LastUsedAt = timestamppb.New(*used)
	}
	return result, nil
}

func tokensWire(record storage.SessionRecord, accessExpiry time.Time, credentials issuedCredentials) (*authv1.SessionTokens, error) {
	session, err := sessionWire(record)
	if err != nil {
		return nil, err
	}
	access, err := credentials.access.Text()
	if err != nil {
		return nil, err
	}
	refresh, err := credentials.refresh.Text()
	if err != nil {
		return nil, err
	}
	return &authv1.SessionTokens{Session: session, Audience: authv1.CredentialAudience_CREDENTIAL_AUDIENCE_INTERACTIVE,
		AccessToken: access, AccessExpiresAt: timestamppb.New(accessExpiry), RefreshToken: refresh}, nil
}

func (h *authHandler) createSession(ctx context.Context, account contracts.AccountID, revision storage.RevocationRevision, challenge *contracts.ChallengeID) (*authv1.SessionTokens, error) {
	id, err := accounts.NewSessionID()
	if err != nil {
		return nil, err
	}
	credentials, err := newIssuedCredentials()
	if err != nil {
		return nil, err
	}
	ticket, err := h.coordinator.enterMFAPolicy(ctx)
	if err != nil {
		return nil, err
	}
	defer ticket.release()
	guard, err := h.coordinator.enter(ctx, account)
	if err != nil {
		return nil, err
	}
	defer guard.release()
	created, err := h.store.CreateSession(ctx, storage.SessionCreation{ID: id, Account: account, Revision: revision,
		ChallengeID: challenge, AccessDigest: credentials.accessDigest, RefreshDigest: credentials.refreshDigest, Now: h.coordinator.clock.Now()})
	if errors.Is(err, storage.ErrSessionCommitUnknown) {
		guard.quarantine()
		guard.cancelLeases(ctx, nil)
	}
	if err != nil {
		return nil, err
	}
	if created.Evicted != nil {
		guard.cancelLeases(ctx, created.Evicted)
	}
	return tokensWire(created.Session, created.AccessExpiresAt, credentials)
}

func (h *authHandler) StartLogin(ctx context.Context, request *connect.Request[authv1.StartLoginRequest]) (*connect.Response[authv1.StartLoginResponse], error) {
	if request == nil || request.Msg == nil || len(request.Msg.ProtoReflect().GetUnknown()) != 0 {
		return nil, sessionRPCError(storage.ErrSessionInvalid)
	}
	now := h.coordinator.clock.Now()
	if !h.bucket.take(now) {
		return nil, h.deniedLogin(ctx, true)
	}
	login, err := accounts.ParseLoginName(request.Msg.LoginName)
	if err != nil {
		return nil, h.deniedLogin(ctx, false)
	}
	if err := h.store.ReserveLoginAttempt(ctx, login, now); err != nil {
		if errors.Is(err, storage.ErrLoginRateLimited) {
			return nil, h.deniedLogin(ctx, true)
		}
		return nil, sessionRPCError(err)
	}
	record, lookupErr := h.store.CredentialByLogin(ctx, login)
	if lookupErr != nil && !errors.Is(lookupErr, storage.ErrAccountNotFound) {
		return nil, sessionRPCError(lookupErr)
	}
	if errors.Is(lookupErr, storage.ErrAccountNotFound) {
		if err := accounts.VerifyUnknownPassword(ctx, request.Msg.Password); err != nil && !errors.Is(err, accounts.ErrInvalidPassword) {
			return nil, sessionRPCError(err)
		}
		return nil, h.deniedLogin(ctx, false)
	}
	verifier, err := record.Verifier()
	if err != nil {
		return nil, sessionRPCError(err)
	}
	ok, err := accounts.VerifyPassword(ctx, request.Msg.Password, verifier)
	if errors.Is(err, accounts.ErrInvalidPassword) {
		return nil, h.deniedLogin(ctx, false)
	}
	if err != nil {
		return nil, sessionRPCError(err)
	}
	status, err := record.Status()
	if err != nil {
		return nil, sessionRPCError(err)
	}
	if !ok || status != authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE {
		return nil, h.deniedLogin(ctx, false)
	}
	account, err := record.ID()
	if err != nil {
		return nil, sessionRPCError(err)
	}
	revision, err := record.Revision()
	if err != nil {
		return nil, sessionRPCError(err)
	}
	factorEnabled, err := record.SecondFactorEnabled()
	if err != nil {
		return nil, sessionRPCError(err)
	}
	preparation := factorAuthenticated()
	challengeID, err := accounts.NewChallengeID()
	if err != nil {
		return nil, sessionRPCError(err)
	}
	if h.provider == nil {
		if factorEnabled {
			return nil, h.deniedLogin(ctx, false)
		}
	} else {
		if h.coordinator.mfa == nil {
			return nil, mfaRPCError(authmfa.NewError(authmfa.ErrorUnavailable))
		}
		permit, permitErr := h.coordinator.mfa.crypto.Acquire(ctx)
		if permitErr != nil {
			return nil, mfaRPCError(permitErr)
		}
		defer permit.Release()
		preparation, err = h.provider.Prepare(ctx, account, challengeID)
		if err != nil {
			return nil, mfaRPCError(err)
		}
		if preparation.revision != revision {
			return nil, mfaRPCError(authmfa.NewError(authmfa.ErrorConflict))
		}
	}
	switch preparation.kind {
	case factorPreparationAuthenticated:
		if factorEnabled || len(preparation.options) != 0 {
			return nil, h.deniedLogin(ctx, false)
		}
		tokens, err := h.createSession(ctx, account, revision, nil)
		if err != nil {
			return nil, sessionRPCError(err)
		}
		return connect.NewResponse(&authv1.StartLoginResponse{Decision: &authv1.LoginDecision{Outcome: &authv1.LoginDecision_Authenticated{Authenticated: tokens}}}), nil
	case factorPreparationChallenge:
		copied := factorChallenge(preparation.options)
		if copied.kind != factorPreparationChallenge || len(copied.options) == 0 {
			return nil, sessionRPCError(storage.ErrSessionUnavailable)
		}
		expiry, expiryErr := preparation.ceremony.ExpiresAt()
		if expiryErr != nil || preparation.started.IsZero() {
			return nil, mfaRPCError(authmfa.NewError(authmfa.ErrorIntegrity))
		}
		issueNow := h.coordinator.clock.Now()
		idText, _ := challengeID.Value()
		candidate := &authv1.FactorChallenge{Id: &authv1.ChallengeId{Value: idText}, ExpiresAt: timestamppb.New(expiry), Options: copied.options}
		decision := &authv1.LoginDecision{Outcome: &authv1.LoginDecision_Challenge{Challenge: candidate}}
		if _, err := contracts.AdmitLoginDecision(decision, issueNow); err != nil {
			return nil, sessionRPCError(storage.ErrSessionUnavailable)
		}
		payload, err := proto.Marshal(candidate)
		if err != nil {
			return nil, sessionRPCError(err)
		}
		admitted, err := h.coordinator.mfa.repository.PrepareLoginChallenge(storage.ChallengeCreation{ID: challengeID, Account: account, Revision: revision, Payload: payload, Now: preparation.started}, preparation.policy, preparation.ceremony)
		if err != nil {
			return nil, mfaRPCError(err)
		}
		ticket, err := h.coordinator.enterMFAPolicy(ctx)
		if err != nil {
			return nil, mfaRPCError(err)
		}
		defer ticket.release()
		guard, err := h.coordinator.enter(ctx, account)
		if err != nil {
			return nil, sessionRPCError(err)
		}
		stored, issueErr := h.coordinator.mfa.repository.IssueLoginChallenge(ctx, admitted)
		if authmfa.KindOf(issueErr) == authmfa.ErrorCommitUnknown {
			guard.quarantine()
			guard.cancelLeases(ctx, nil)
		}
		guard.release()
		if issueErr != nil {
			return nil, mfaRPCError(issueErr)
		}
		candidate.ExpiresAt = timestamppb.New(stored.ExpiresAt)
		return connect.NewResponse(&authv1.StartLoginResponse{Decision: decision}), nil
	case factorPreparationEnrollment:
		if factorEnabled || len(preparation.options) != 0 {
			return nil, h.deniedLogin(ctx, false)
		}
		grant, err := accounts.NewEnrollmentSecret()
		if err != nil {
			return nil, sessionRPCError(err)
		}
		digest, err := grant.Digest()
		if err != nil {
			return nil, sessionRPCError(err)
		}
		ticket, err := h.coordinator.enterMFAPolicy(ctx)
		if err != nil {
			return nil, mfaRPCError(err)
		}
		defer ticket.release()
		guard, err := h.coordinator.enter(ctx, account)
		if err != nil {
			return nil, sessionRPCError(err)
		}
		expires, issueErr := h.store.IssueEnrollmentGrant(ctx, account, revision, digest, h.coordinator.clock.Now())
		if errors.Is(issueErr, storage.ErrSessionCommitUnknown) {
			guard.quarantine()
			guard.cancelLeases(ctx, nil)
		}
		guard.release()
		if issueErr != nil {
			return nil, sessionRPCError(issueErr)
		}
		accountText, _ := account.Value()
		grantText, err := grant.Text()
		if err != nil {
			return nil, sessionRPCError(err)
		}
		decision := &authv1.LoginDecision{Outcome: &authv1.LoginDecision_EnrollmentRequired{EnrollmentRequired: &authv1.EnrollmentRequired{
			AccountId: &authv1.AccountId{Value: accountText}, EnrollmentToken: grantText, ExpiresAt: timestamppb.New(expires)}}}
		if _, err := contracts.AdmitLoginDecision(decision, h.coordinator.clock.Now()); err != nil {
			return nil, sessionRPCError(storage.ErrSessionUnavailable)
		}
		return connect.NewResponse(&authv1.StartLoginResponse{Decision: decision}), nil
	default:
		return nil, sessionRPCError(storage.ErrSessionUnavailable)
	}
}

func (h *authHandler) CompleteLogin(ctx context.Context, request *connect.Request[authv1.CompleteLoginRequest]) (*connect.Response[authv1.CompleteLoginResponse], error) {
	if request == nil || request.Msg == nil || request.Msg.ChallengeId == nil || len(request.Msg.ProtoReflect().GetUnknown()) != 0 || proto.Size(request.Msg) > authmfa.MaxRequestBytes {
		return nil, sessionRPCError(storage.ErrSessionInvalid)
	}
	id, err := contracts.ParseChallengeID(request.Msg.ChallengeId.Value)
	if err != nil {
		return nil, sessionRPCError(storage.ErrSessionInvalid)
	}
	runtime := h.coordinator.mfa
	if runtime == nil || h.provider == nil {
		return nil, mfaRPCError(authmfa.NewError(authmfa.ErrorUnavailable))
	}
	permit, err := runtime.crypto.Acquire(ctx)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	defer permit.Release()
	now := h.coordinator.clock.Now()
	challenge, err := h.store.LookupChallenge(ctx, id, now)
	if err != nil {
		return nil, sessionRPCError(err)
	}
	snapshot, err := runtime.repository.LoginSnapshot(ctx, challenge, now)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	// Reserve both persistent budgets before offered-proof validation or crypto.
	if err := runtime.repository.ReserveProof(ctx, snapshot, now); err != nil {
		return nil, mfaRPCError(err)
	}
	offered, err := snapshot.Offered()
	if err != nil {
		return nil, mfaRPCError(err)
	}
	if err := contracts.AdmitCompleteLoginRequest(request.Msg, offered, now); err != nil {
		return nil, h.deniedLogin(ctx, false)
	}
	proof, err := h.provider.Verify(ctx, snapshot, request.Msg.Proof)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	finalization, err := runtime.repository.PrepareFinalization(proof, snapshot)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	sessionID, err := accounts.NewSessionID()
	if err != nil {
		return nil, sessionRPCError(err)
	}
	credentials, err := newIssuedCredentials()
	if err != nil {
		return nil, sessionRPCError(err)
	}
	ticket, err := h.coordinator.enterMFAPolicy(ctx)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	defer ticket.release()
	guard, err := h.coordinator.enter(ctx, challenge.Account)
	if err != nil {
		return nil, sessionRPCError(err)
	}
	defer guard.release()
	created, err := runtime.repository.FinalizeLogin(ctx, storage.SessionCreation{ID: sessionID, Account: challenge.Account, Revision: challenge.Revision, ChallengeID: &id, AccessDigest: credentials.accessDigest, RefreshDigest: credentials.refreshDigest, Now: h.coordinator.clock.Now()}, finalization)
	if authmfa.KindOf(err) == authmfa.ErrorCommitUnknown {
		guard.quarantine()
		guard.cancelLeases(ctx, nil)
	}
	if err != nil {
		return nil, mfaRPCError(err)
	}
	if created.Evicted != nil {
		guard.cancelLeases(ctx, created.Evicted)
	}
	tokens, err := tokensWire(created.Session, created.AccessExpiresAt, credentials)
	if err != nil {
		return nil, sessionRPCError(err)
	}
	return connect.NewResponse(&authv1.CompleteLoginResponse{Decision: &authv1.LoginDecision{Outcome: &authv1.LoginDecision_Authenticated{Authenticated: tokens}}}), nil
}

func (h *authHandler) RefreshSession(ctx context.Context, request *connect.Request[authv1.RefreshSessionRequest]) (*connect.Response[authv1.RefreshSessionResponse], error) {
	if request == nil || request.Msg == nil || len(request.Msg.ProtoReflect().GetUnknown()) != 0 {
		return nil, sessionRPCError(storage.ErrSessionInvalid)
	}
	secret, err := accounts.ParseRefreshSecret(request.Msg.RefreshToken)
	if err != nil {
		return nil, sessionRPCError(errSessionUnauthenticated)
	}
	digest, err := secret.Digest()
	if err != nil {
		return nil, sessionRPCError(errSessionUnauthenticated)
	}
	owner, err := h.store.LookupRefreshOwner(ctx, digest)
	if err != nil {
		return nil, sessionRPCError(err)
	}
	credentials, err := newIssuedCredentials()
	if err != nil {
		return nil, sessionRPCError(err)
	}
	ticket, err := h.coordinator.enterMFAPolicy(ctx)
	if err != nil {
		return nil, mfaRPCError(err)
	}
	defer ticket.release()
	guard, err := h.coordinator.enter(ctx, owner.Account)
	if err != nil {
		return nil, sessionRPCError(err)
	}
	defer guard.release()
	rotated, err := h.store.RotateRefresh(ctx, storage.RefreshRotation{Owner: owner, OldDigest: digest,
		NewDigest: credentials.refreshDigest, NewAccessDigest: credentials.accessDigest, Now: h.coordinator.clock.Now()})
	if errors.Is(err, storage.ErrSessionCommitUnknown) {
		guard.quarantine()
		guard.cancelLeases(ctx, nil)
	}
	if err != nil {
		return nil, sessionRPCError(err)
	}
	if rotated.ReplayRevoked() || rotated.GenerationExhausted() {
		guard.cancelLeases(ctx, &owner.Session)
		return nil, sessionRPCError(errSessionUnauthenticated)
	}
	current, accessExpiry, ok := rotated.Rotated()
	if !ok {
		guard.quarantine()
		guard.cancelLeases(ctx, nil)
		return nil, sessionRPCError(storage.ErrSessionUnavailable)
	}
	guard.cancelLeases(ctx, &owner.Session) // old access is invalid after the confirmed commit
	tokens, err := tokensWire(current, accessExpiry, credentials)
	if err != nil {
		return nil, sessionRPCError(err)
	}
	return connect.NewResponse(&authv1.RefreshSessionResponse{Tokens: tokens}), nil
}

func (h *authHandler) ListSessions(ctx context.Context, request *connect.Request[authv1.ListSessionsRequest]) (*connect.Response[authv1.ListSessionsResponse], error) {
	if request == nil || request.Msg == nil || len(request.Msg.ProtoReflect().GetUnknown()) != 0 ||
		request.Msg.AccountId != nil && len(request.Msg.AccountId.ProtoReflect().GetUnknown()) != 0 {
		return nil, sessionRPCError(storage.ErrSessionInvalid)
	}
	actor, err := admittedActor(ctx)
	if err != nil {
		return nil, sessionRPCError(errSessionUnauthenticated)
	}
	target, err := admittedAccountID(request.Msg.AccountId)
	if err != nil {
		return nil, sessionRPCError(storage.ErrSessionInvalid)
	}
	records, err := h.store.ListSessions(ctx, actor, target, h.coordinator.clock.Now())
	if err != nil {
		return nil, sessionRPCError(err)
	}
	response := &authv1.ListSessionsResponse{Sessions: make([]*authv1.Session, 0, len(records))}
	for _, record := range records {
		wire, err := sessionWire(record)
		if err != nil {
			return nil, sessionRPCError(err)
		}
		response.Sessions = append(response.Sessions, wire)
	}
	return connect.NewResponse(response), nil
}

func (h *authHandler) RevokeSession(ctx context.Context, request *connect.Request[authv1.RevokeSessionRequest]) (*connect.Response[authv1.RevokeSessionResponse], error) {
	if request == nil || request.Msg == nil || request.Msg.SessionId == nil || len(request.Msg.ProtoReflect().GetUnknown()) != 0 ||
		len(request.Msg.SessionId.ProtoReflect().GetUnknown()) != 0 {
		return nil, sessionRPCError(storage.ErrSessionInvalid)
	}
	actor, err := admittedActor(ctx)
	if err != nil {
		return nil, sessionRPCError(errSessionUnauthenticated)
	}
	id, err := contracts.ParseSessionID(request.Msg.SessionId.Value)
	if err != nil {
		return nil, sessionRPCError(storage.ErrSessionInvalid)
	}
	owner, err := h.store.LookupSessionOwner(ctx, id)
	if err != nil {
		return nil, sessionRPCError(err)
	}
	guard, err := h.coordinator.enter(ctx, owner.Account)
	if err != nil {
		return nil, sessionRPCError(err)
	}
	defer guard.release()
	err = h.store.RevokeSession(ctx, actor, owner, h.coordinator.clock.Now())
	if errors.Is(err, storage.ErrSessionCommitUnknown) {
		guard.quarantine()
		guard.cancelLeases(ctx, nil)
	}
	if err != nil {
		return nil, sessionRPCError(err)
	}
	guard.cancelLeases(ctx, &owner.Session)
	return connect.NewResponse(&authv1.RevokeSessionResponse{}), nil
}

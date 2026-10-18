package integrations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"reflect"
	"sync"

	"github.com/damione1/Homeward/internal/storage"
)

type CloudAuthorizationAttemptID struct{ value string }

func (id CloudAuthorizationAttemptID) Valid() bool { return id.value != "" }
func (id CloudAuthorizationAttemptID) String() string {
	return "[REDACTED CLOUD AUTHORIZATION ATTEMPT ID]"
}
func (id CloudAuthorizationAttemptID) GoString() string { return id.String() }

// CloudAuthorizationOwner is trusted composition's owner of one pilot client.
// It never opens or replaces a pilot Session.
type CloudAuthorizationOwner struct {
	host    *Manager
	client  *CloudClient
	policy  *CloudPolicy
	handle  storage.SecretHandle
	profile CloudAuthorizationProfileID
	limits  CloudAuthorizationLimits
	mu      sync.Mutex
	active  *CloudAuthorizationAttempt
}

func sameCloudAdmission(a, b CloudAttemptAdmission) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	left, right := reflect.ValueOf(a), reflect.ValueOf(b)
	return left.IsValid() && right.IsValid() && left.Type() == right.Type() && left.Comparable() && left.Interface() == right.Interface()
}

// Optional expectedAdmission checks a configured durable hook by identity;
// omitting it preserves the existing optional quota contract.
func NewCloudAuthorizationOwner(host *Manager, client *CloudClient, expectedPolicy *CloudPolicy, handle storage.SecretHandle, profile CloudAuthorizationProfileID, limits CloudAuthorizationLimits, expectedAdmission ...CloudAttemptAdmission) (*CloudAuthorizationOwner, error) {
	if host == nil || host.active == nil || !client.valid() || client.session.host != host || client.policy != expectedPolicy ||
		expectedPolicy == nil || !profile.Valid() || !limits.Valid() || len(expectedAdmission) > 1 {
		return nil, &CloudError{kind: CloudInvalid}
	}
	instance, _, err := handle.Components()
	if err != nil || instance != client.session.instance {
		return nil, &CloudError{kind: CloudInvalid}
	}
	if len(expectedAdmission) == 1 && (nilInterface(expectedAdmission[0]) || !sameCloudAdmission(client.admission, expectedAdmission[0])) {
		return nil, &CloudError{kind: CloudInvalid}
	}
	if !client.live() {
		return nil, &CloudError{kind: CloudSessionInactive}
	}
	return &CloudAuthorizationOwner{host: host, client: client, policy: expectedPolicy, handle: handle, profile: profile, limits: limits}, nil
}
func (o *CloudAuthorizationOwner) String() string   { return "[REDACTED CLOUD AUTHORIZATION OWNER]" }
func (o *CloudAuthorizationOwner) GoString() string { return o.String() }

type cloudAuthorizationPrecondition uint8

const (
	cloudAuthorizationPreconditionUnspecified cloudAuthorizationPrecondition = iota
	cloudAuthorizationAbsent
	cloudAuthorizationRevision
)

type CloudAuthorizationAttempt struct {
	owner        *CloudAuthorizationOwner
	id           CloudAuthorizationAttemptID
	ctx          context.Context
	cancel       context.CancelFunc
	precondition cloudAuthorizationPrecondition
	revision     storage.SecretRevision
	mu           sync.Mutex
	used         map[CloudAuthorizationExchangeID]struct{}
	exchanging   bool
	publishing   bool
	published    bool
	running      int
	closing      bool
	done         chan struct{}
	jar          cloudAuthorizationJar
}

func (o *CloudAuthorizationOwner) Begin(ctx context.Context) (*CloudAuthorizationAttempt, error) {
	if o == nil || o.client == nil || nilInterface(ctx) {
		return nil, &CloudError{kind: CloudInvalid}
	}
	if ctx.Err() != nil {
		return nil, &CloudError{kind: CloudBudgetExhausted}
	}
	o.mu.Lock()
	if o.active != nil {
		o.mu.Unlock()
		return nil, &CloudError{kind: CloudQueueFull}
	}
	if !o.client.live() {
		o.mu.Unlock()
		return nil, &CloudError{kind: CloudSessionInactive}
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		o.mu.Unlock()
		return nil, &CloudError{kind: CloudInvalid}
	}
	attemptCtx, cancel := context.WithTimeout(ctx, o.limits.value.TotalTimeout)
	a := &CloudAuthorizationAttempt{
		owner: o, id: CloudAuthorizationAttemptID{value: hex.EncodeToString(token[:])},
		ctx: attemptCtx, cancel: cancel, used: make(map[CloudAuthorizationExchangeID]struct{}), done: make(chan struct{}),
		jar: cloudAuthorizationJar{values: make(map[cloudAuthorizationCookieKey]CloudAuthorizationCookie)},
	}
	o.active = a
	o.mu.Unlock()
	go func() {
		select {
		case <-o.client.session.Done():
			cancel()
		case <-a.done:
		}
	}()
	repo, session, err := o.client.secrets.admitCredential(attemptCtx, o.handle)
	if err != nil {
		a.Close()
		return nil, o.client.credentialError(err)
	}
	_, revision, readErr := repo.ReadCredentialVersioned(attemptCtx, o.handle)
	if err := o.client.secrets.checkCredentialAfterSQL(attemptCtx, repo, session, o.handle); err != nil {
		a.Close()
		return nil, o.client.credentialError(err)
	}
	if readErr != nil {
		var storageErr *storage.Error
		if !errors.As(readErr, &storageErr) || storageErr.Kind() != storage.SecretNotFound {
			a.Close()
			return nil, o.client.credentialError(readErr)
		}
		a.precondition = cloudAuthorizationAbsent
	} else {
		a.precondition = cloudAuthorizationRevision
		a.revision = revision
	}
	if attemptCtx.Err() != nil || !o.client.live() {
		a.Close()
		return nil, &CloudError{kind: CloudSessionInactive}
	}
	return a, nil
}

func (a *CloudAuthorizationAttempt) ID() CloudAuthorizationAttemptID {
	if a == nil {
		return CloudAuthorizationAttemptID{}
	}
	return a.id
}
func (a *CloudAuthorizationAttempt) Exchanger() CloudAuthorizationExchanger {
	if a == nil {
		return nil
	}
	return cloudAuthorizationExchangeView{attempt: a}
}
func (a *CloudAuthorizationAttempt) Done() <-chan struct{} {
	if a == nil {
		return nil
	}
	return a.done
}
func (a *CloudAuthorizationAttempt) Cancel() {
	if a != nil && a.cancel != nil {
		a.cancel()
	}
}
func (a *CloudAuthorizationAttempt) Close() {
	if a == nil || a.owner == nil {
		return
	}
	a.mu.Lock()
	if !a.closing {
		a.closing = true
		a.cancel()
		if a.running == 0 {
			close(a.done)
		}
	}
	done := a.done
	a.mu.Unlock()
	<-done
	a.owner.mu.Lock()
	if a.owner.active == a {
		a.owner.active = nil
	}
	a.owner.mu.Unlock()
}

func (a *CloudAuthorizationAttempt) beginExchange(id CloudAuthorizationExchangeID) error {
	if a == nil || a.owner == nil || id.value == "" {
		return &CloudError{kind: CloudInvalid}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctx.Err() != nil {
		return &CloudError{kind: CloudBudgetExhausted}
	}
	if a.closing || a.publishing || a.published || a.exchanging {
		return &CloudError{kind: CloudSessionInactive}
	}
	if _, exists := a.used[id]; exists {
		return &CloudError{kind: CloudInvalid}
	}
	if len(a.used) >= a.owner.limits.value.MaxExchanges {
		return &CloudError{kind: CloudBudgetExhausted}
	}
	a.used[id] = struct{}{}
	a.exchanging = true
	a.running++
	return nil
}
func (a *CloudAuthorizationAttempt) finishExchange() {
	a.mu.Lock()
	a.exchanging = false
	a.running--
	if a.closing && a.running == 0 {
		close(a.done)
	}
	a.mu.Unlock()
}
func (a *CloudAuthorizationAttempt) beginPublish(ctx context.Context) error {
	if a == nil || a.owner == nil || nilInterface(ctx) {
		return &CloudError{kind: CloudInvalid}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctx.Err() != nil || ctx.Err() != nil {
		return &CloudError{kind: CloudBudgetExhausted}
	}
	if a.closing || a.exchanging || a.publishing || a.published {
		return &CloudError{kind: CloudSessionInactive}
	}
	a.published = true // consume once before SQL, including an unknown commit
	a.publishing = true
	a.running++
	return nil
}
func (a *CloudAuthorizationAttempt) finishPublish() {
	a.mu.Lock()
	a.publishing = false
	a.running--
	if a.closing && a.running == 0 {
		close(a.done)
	}
	a.mu.Unlock()
}

// Publish consumes the capability once. It reuses the attached repository and
// the session's before/after SQL fences, never an upsert from a provider.
func (a *CloudAuthorizationAttempt) Publish(ctx context.Context, credential CloudAuthorizedCredential) error {
	if err := a.beginPublish(ctx); err != nil {
		return err
	}
	defer a.finishPublish()
	if !credential.Profile().Equal(a.owner.profile) {
		return &CloudError{kind: CloudInvalid}
	}
	value, err := credential.Secret()
	if err != nil {
		return &CloudError{kind: CloudInvalid}
	}
	raw, err := value.Bytes()
	if err != nil {
		return &CloudError{kind: CloudInvalid}
	}
	owned, err := storage.NewSecretValue(raw)
	if err != nil {
		return &CloudError{kind: CloudInvalid}
	}
	o := a.owner
	repo, session, err := o.client.secrets.admitCredential(a.ctx, o.handle)
	if err != nil {
		return o.client.credentialError(err)
	}
	writeCtx, cancel := context.WithCancel(ctx)
	joined := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-a.ctx.Done():
			cancel()
		case <-session.Done():
			cancel()
		case <-joined:
		}
	}()
	var writeErr error
	switch a.precondition {
	case cloudAuthorizationAbsent:
		var inserted bool
		inserted, writeErr = repo.InitializeCredentialIfAbsent(writeCtx, o.handle, owned)
		if writeErr == nil && !inserted {
			writeErr = &CloudError{kind: CloudCredentialConflict}
		}
	case cloudAuthorizationRevision:
		writeErr = repo.RotateCredentialIfRevision(writeCtx, o.handle, a.revision, owned)
	default:
		writeErr = &CloudError{kind: CloudInvalid}
	}
	close(joined)
	cancel()
	<-watcherDone
	if fenceErr := o.client.secrets.checkCredentialAfterSQL(context.Background(), repo, session, o.handle); fenceErr != nil {
		var storageErr *storage.Error
		if errors.As(writeErr, &storageErr) && storageErr.Kind() == storage.SecretCommitUnknown {
			return &CloudError{kind: CloudCredentialCommitUnknown}
		}
		if writeErr == nil {
			return &CloudError{kind: CloudCredentialCommittedRetired}
		}
		return o.client.credentialError(fenceErr)
	}
	if writeErr != nil {
		return o.client.credentialError(writeErr)
	}
	if ctx.Err() != nil || a.ctx.Err() != nil {
		return &CloudError{kind: CloudCredentialCommittedUnobserved}
	}
	return nil
}

package integrations

import (
	"context"
	"encoding/json"
	"sync"
	"time"
	"unicode/utf8"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/observability"
	"github.com/damione1/Homeward/internal/storage"
)

// CloudFailureKind is the closed result vocabulary of the shared HTTP policy.
// Transport details, URLs, headers, and bodies never enter its diagnostics.
type CloudFailureKind uint8

const (
	CloudFailureUnspecified CloudFailureKind = iota
	CloudInvalid
	CloudSessionInactive
	CloudQueueFull
	CloudBudgetExhausted
	CloudQuotaDeferred
	CloudLocalQuotaExhausted
	CloudQuotaStoreUnavailable
	CloudTransportFailed
	CloudHTTPFailed
	CloudResponseTooLarge
	CloudCredentialFailed
	CloudCredentialConflict
	CloudCredentialCommitUnknown
	CloudCredentialCommittedRetired
	CloudCredentialCommittedUnobserved
	CloudStreamsDisabled
	CloudStreamCapacity
	CloudStreamClosed
	CloudStreamBusy
	CloudStreamProtocolFailed
	CloudStreamIdle
	CloudStreamEnded
	CloudStreamCanceled
)

type CloudError struct {
	kind         CloudFailureKind
	retryAt      time.Time
	retryable    bool
	responseBody []byte
}

// ResponseBytes is an explicit bounded provider-decoding path for failed
// HTTP responses. Error/String diagnostics never include these bytes.
func (e *CloudError) ResponseBytes() []byte {
	if e == nil {
		return nil
	}
	return append([]byte(nil), e.responseBody...)
}

func (e *CloudError) Kind() CloudFailureKind {
	if e == nil {
		return CloudFailureUnspecified
	}
	return e.kind
}

func (e *CloudError) RetryAt() (time.Time, bool) {
	if e == nil || e.retryAt.IsZero() {
		return time.Time{}, false
	}
	return e.retryAt, true
}

func (e *CloudError) Error() string {
	if e == nil {
		return "cloud: unknown failure"
	}
	switch e.kind {
	case CloudInvalid:
		return "cloud: invalid request or policy"
	case CloudSessionInactive:
		return "cloud: integration session inactive"
	case CloudQueueFull:
		return "cloud: request queue full"
	case CloudBudgetExhausted:
		return "cloud: request budget exhausted"
	case CloudQuotaDeferred:
		return "cloud: quota deferred request"
	case CloudLocalQuotaExhausted:
		return "cloud: local account request budget exhausted"
	case CloudQuotaStoreUnavailable:
		return "cloud: local account request budget unavailable"
	case CloudTransportFailed:
		return "cloud: transport outcome uncertain"
	case CloudHTTPFailed:
		return "cloud: upstream response failed"
	case CloudResponseTooLarge:
		return "cloud: response exceeds admitted limit"
	case CloudCredentialFailed:
		return "cloud: credential renewal failed"
	case CloudCredentialConflict:
		return "cloud: credential changed during renewal"
	case CloudCredentialCommitUnknown:
		return "cloud: credential commit outcome unknown"
	case CloudCredentialCommittedRetired:
		return "cloud: credential committed after session admission ended"
	case CloudCredentialCommittedUnobserved:
		return "cloud: credential committed after caller cancellation"
	case CloudStreamsDisabled:
		return "cloud: observation streams are disabled"
	case CloudStreamCapacity:
		return "cloud: observation stream capacity exhausted"
	case CloudStreamClosed:
		return "cloud: observation stream closed"
	case CloudStreamBusy:
		return "cloud: observation stream operation already active"
	case CloudStreamProtocolFailed:
		return "cloud: observation stream protocol failed"
	case CloudStreamIdle:
		return "cloud: observation stream idle limit exceeded"
	case CloudStreamEnded:
		return "cloud: observation stream ended remotely"
	case CloudStreamCanceled:
		return "cloud: observation stream canceled"
	default:
		return "cloud: unknown failure"
	}
}

func (e *CloudError) GoString() string { return e.Error() }

// CloudLimitsInput is admitted as a whole; zero and oversized values never
// silently become defaults. Use DefaultCloudLimits for the shipped policy.
type CloudLimitsInput struct {
	MaxInFlight, MaxQueued, ObservationAttempts, MaxRefreshWaiters       int
	QueueWait, AttemptTimeout, TotalTimeout, BackoffBase, BackoffMaximum time.Duration
	RefreshBudget, RefreshCooldown                                       time.Duration
	ResponseBytes, RequestBytes                                          int64
}

type CloudLimits struct{ value CloudLimitsInput }

// Input returns a copy for provider-specific adjustment and re-admission.
func (l CloudLimits) Input() CloudLimitsInput { return l.value }

func AdmitCloudLimits(in CloudLimitsInput) (CloudLimits, error) {
	if in.MaxInFlight < 1 || in.MaxInFlight > 32 || in.MaxQueued < 1 || in.MaxQueued > 128 ||
		in.ObservationAttempts < 1 || in.ObservationAttempts > 5 || in.MaxRefreshWaiters < 1 || in.MaxRefreshWaiters > 128 ||
		in.QueueWait < time.Millisecond || in.QueueWait > 30*time.Second ||
		in.AttemptTimeout < time.Millisecond || in.AttemptTimeout > 30*time.Second ||
		in.TotalTimeout < in.AttemptTimeout || in.TotalTimeout > 120*time.Second ||
		in.BackoffBase < time.Millisecond || in.BackoffBase > 30*time.Second ||
		in.BackoffMaximum < in.BackoffBase || in.BackoffMaximum > 30*time.Second ||
		in.RefreshBudget < time.Millisecond || in.RefreshBudget > 30*time.Second ||
		in.RefreshCooldown < time.Second || in.RefreshCooldown > 10*time.Minute ||
		in.ResponseBytes < 1 || in.ResponseBytes > 4<<20 || in.RequestBytes < 1 || in.RequestBytes > 4<<20 {
		return CloudLimits{}, &CloudError{kind: CloudInvalid}
	}
	return CloudLimits{value: in}, nil
}

func DefaultCloudLimits() CloudLimits {
	limits, _ := AdmitCloudLimits(CloudLimitsInput{
		MaxInFlight: 4, MaxQueued: 16, ObservationAttempts: 3, MaxRefreshWaiters: 16,
		QueueWait: 2 * time.Second, AttemptTimeout: 5 * time.Second, TotalTimeout: 20 * time.Second,
		BackoffBase: 250 * time.Millisecond, BackoffMaximum: 4 * time.Second,
		RefreshBudget: 10 * time.Second, RefreshCooldown: 30 * time.Second,
		ResponseBytes: 1 << 20, RequestBytes: 1 << 20,
	})
	return limits
}

func (l CloudLimits) Valid() bool {
	_, err := AdmitCloudLimits(l.value)
	return err == nil
}

type CloudAccountID struct{ value string }

func ParseCloudAccountID(raw string) (CloudAccountID, error) {
	if raw == "" || len(raw) > 128 || !utf8.ValidString(raw) {
		return CloudAccountID{}, &CloudError{kind: CloudInvalid}
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return CloudAccountID{}, &CloudError{kind: CloudInvalid}
		}
	}
	return CloudAccountID{value: raw}, nil
}

func (id CloudAccountID) String() string               { return "[REDACTED CLOUD ACCOUNT]" }
func (id CloudAccountID) GoString() string             { return id.String() }
func (id CloudAccountID) MarshalJSON() ([]byte, error) { return json.Marshal(id.String()) }

// A binding is supplied by trusted composition, never by an HTTP request or
// the driver at each call. Instances without a binding receive a private quota.
type CloudAccountBinding struct {
	instance domain.IntegrationInstanceID
	account  CloudAccountID
}

func NewCloudAccountBinding(instance domain.IntegrationInstanceID, account CloudAccountID) (CloudAccountBinding, error) {
	if _, err := instance.Value(); err != nil || account.value == "" {
		return CloudAccountBinding{}, &CloudError{kind: CloudInvalid}
	}
	return CloudAccountBinding{instance: instance, account: account}, nil
}

type cloudScopeKey struct {
	instance domain.IntegrationInstanceID
	account  CloudAccountID
}

type cloudScopeState struct {
	inFlight, queued int
	notBefore        time.Time
	changed          chan struct{}
	streams          map[*CloudObservationStream]struct{}
}

type CloudClock interface{ Now() time.Time }
type RealCloudClock struct{}

func (RealCloudClock) Now() time.Time { return time.Now() }

type CloudWait interface {
	C() <-chan time.Time
	Stop() bool
}
type CloudTimer interface{ NewTimer(time.Duration) CloudWait }
type RealCloudTimer struct{}
type realCloudWait struct{ timer *time.Timer }

func (RealCloudTimer) NewTimer(d time.Duration) CloudWait { return realCloudWait{time.NewTimer(d)} }
func (w realCloudWait) C() <-chan time.Time               { return w.timer.C }
func (w realCloudWait) Stop() bool                        { return w.timer.Stop() }

type CloudPolicy struct {
	observation  *observability.Cells
	mu           sync.Mutex
	limits       CloudLimits
	streamLimits CloudStreamLimits
	bindings     map[domain.IntegrationInstanceID]CloudAccountID
	states       map[cloudScopeKey]*cloudScopeState
	clock        CloudClock
	timer        CloudTimer
	jitter       Jitter
	refreshMu    sync.Mutex
	refresh      map[storage.SecretHandle]*cloudRefreshState
}

func (p *CloudPolicy) String() string   { return "[REDACTED CLOUD POLICY]" }
func (p *CloudPolicy) GoString() string { return p.String() }
func (p *CloudPolicy) MarshalJSON() ([]byte, error) {
	return json.Marshal(p.String())
}

func NewCloudPolicy(limits CloudLimits, bindings []CloudAccountBinding, clock CloudClock, timer CloudTimer, jitter Jitter) (*CloudPolicy, error) {
	if !limits.Valid() || nilInterface(clock) || nilInterface(timer) || nilInterface(jitter) || len(bindings) > 1024 {
		return nil, &CloudError{kind: CloudInvalid}
	}
	p := &CloudPolicy{limits: limits, bindings: make(map[domain.IntegrationInstanceID]CloudAccountID, len(bindings)), states: make(map[cloudScopeKey]*cloudScopeState), clock: clock, timer: timer, jitter: jitter, refresh: make(map[storage.SecretHandle]*cloudRefreshState)}
	for _, binding := range bindings {
		if _, err := binding.instance.Value(); err != nil || binding.account.value == "" {
			return nil, &CloudError{kind: CloudInvalid}
		}
		if _, exists := p.bindings[binding.instance]; exists {
			return nil, &CloudError{kind: CloudInvalid}
		}
		p.bindings[binding.instance] = binding.account
	}
	return p, nil
}

func (p *CloudPolicy) scope(instance domain.IntegrationInstanceID) (cloudScopeKey, *cloudScopeState, error) {
	if p == nil || p.states == nil {
		return cloudScopeKey{}, nil, &CloudError{kind: CloudInvalid}
	}
	if _, err := instance.Value(); err != nil {
		return cloudScopeKey{}, nil, &CloudError{kind: CloudInvalid}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := cloudScopeKey{instance: instance}
	if account, configured := p.bindings[instance]; configured {
		key = cloudScopeKey{account: account}
	}
	state := p.states[key]
	if state == nil {
		if len(p.states) >= 1024 {
			_ = p.observation.AddQueueRejected(obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS, 1)
			return cloudScopeKey{}, nil, &CloudError{kind: CloudQueueFull}
		}
		state = &cloudScopeState{changed: make(chan struct{})}
		p.states[key] = state
		_ = p.observation.ChangeRuntimeQueue(obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS, 1, 0, int64(p.limits.value.MaxQueued), 0, 0)
	}
	return key, state, nil
}

func (p *CloudPolicy) signal(state *cloudScopeState) {
	close(state.changed)
	state.changed = make(chan struct{})
}

func (p *CloudPolicy) acquire(ctx context.Context, sessionDone <-chan struct{}, state *cloudScopeState) (func(), error) {
	if ctx == nil || state == nil || sessionDone == nil {
		return nil, &CloudError{kind: CloudInvalid}
	}
	p.mu.Lock()
	queueUntil := p.clock.Now().Add(p.limits.value.QueueWait)
	queued := false
	for {
		if err := ctx.Err(); err != nil {
			if queued {
				state.queued--
				_ = p.observation.ChangeRuntimeQueue(obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS, 0, -1, 0, 0, 0)
			}
			p.mu.Unlock()
			return nil, &CloudError{kind: CloudBudgetExhausted}
		}
		select {
		case <-sessionDone:
			if queued {
				state.queued--
				_ = p.observation.ChangeRuntimeQueue(obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS, 0, -1, 0, 0, 0)
			}
			p.mu.Unlock()
			return nil, &CloudError{kind: CloudSessionInactive}
		default:
		}
		now := p.clock.Now()
		if state.inFlight < p.limits.value.MaxInFlight && !now.Before(state.notBefore) {
			if queued {
				state.queued--
				_ = p.observation.ChangeRuntimeQueue(obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS, 0, -1, 0, 0, 0)
			}
			state.inFlight++
			_ = p.observation.ChangeRuntimeQueue(obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS, 0, 0, 0, 1, 0)
			p.mu.Unlock()
			return func() {
				p.mu.Lock()
				state.inFlight--
				_ = p.observation.ChangeRuntimeQueue(obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS, 0, 0, 0, -1, 0)
				p.signal(state)
				p.mu.Unlock()
			}, nil
		}
		if !queued {
			if state.queued >= p.limits.value.MaxQueued {
				_ = p.observation.AddQueueRejected(obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS, 1)
				p.mu.Unlock()
				return nil, &CloudError{kind: CloudQueueFull}
			}
			state.queued++
			_ = p.observation.ChangeRuntimeQueue(obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS, 0, 1, 0, 0, 0)
			queued = true
		}
		if !now.Before(queueUntil) {
			state.queued--
			_ = p.observation.ChangeRuntimeQueue(obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS, 0, -1, 0, 0, 0)
			p.mu.Unlock()
			return nil, &CloudError{kind: CloudQuotaDeferred, retryAt: state.notBefore}
		}
		wait := queueUntil.Sub(now)
		if state.inFlight < p.limits.value.MaxInFlight && state.notBefore.After(now) && state.notBefore.Sub(now) < wait {
			wait = state.notBefore.Sub(now)
		}
		changed := state.changed
		p.mu.Unlock()
		timer := p.timer.NewTimer(wait)
		if nilInterface(timer) || timer.C() == nil {
			p.mu.Lock()
			state.queued--
			_ = p.observation.ChangeRuntimeQueue(obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS, 0, -1, 0, 0, 0)
			p.mu.Unlock()
			return nil, &CloudError{kind: CloudInvalid}
		}
		select {
		case <-ctx.Done():
		case <-sessionDone:
		case <-changed:
		case <-timer.C():
		}
		timer.Stop()
		p.mu.Lock()
	}
}

func (p *CloudPolicy) deferScope(state *cloudScopeState, until time.Time) {
	p.mu.Lock()
	if until.After(state.notBefore) {
		state.notBefore = until
		p.signal(state)
	}
	p.mu.Unlock()
}

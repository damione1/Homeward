package authmfa

import (
	"context"
	"sync"
	"time"
)

const (
	CredentialLimit              = 8
	ChallengeBytes               = 32
	UserHandleBytes              = 32
	RecoveryCodeCount            = 10
	RecoveryCodeBytes            = 16
	MaxCredentialIDBytes         = 1024
	MaxPublicKeyBytes            = 4096
	MaxCredentialRecordBytes     = 64 << 10
	MaxClientDataJSONBytes       = 8 << 10
	MaxAuthenticatorDataBytes    = 16 << 10
	MaxSignatureBytes            = 1024
	MaxAttestationObjectBytes    = 64 << 10
	ClientDataHashBytes          = 32
	MaxCeremonyRecordBytes       = 16 << 10
	MaxRequestBytes              = 128 << 10
	CeremonyLifetime             = 5 * time.Minute
	RecentAuthenticationLifetime = 5 * time.Minute
	BudgetWindow                 = 15 * time.Minute
	BeginBudgetLimit             = 3
	ProofBudgetLimit             = 10
	CryptoActiveLimit            = 4
	CryptoWaitingLimit           = 8
	CryptoWaitLimit              = 5 * time.Second
	PolicyAffectedAccountLimit   = 10000
	PolicyBatchLimit             = 128
	PolicyMutationTimeout        = 5 * time.Second
)

// BeginBudget and ProofBudget are detached, bounded proposals for persistence.
// Reserve does not write SQL: the store must CAS/reserve in its own transaction.
// Replacing a ChallengeID or completing a login never constructs an empty
// budget implicitly; callers reload the account's current stored timestamps.
type BeginBudget struct{ timestamps []time.Time }
type ProofBudget struct{ timestamps []time.Time }

func admitWindow(input []time.Time, limit int, now time.Time) ([]time.Time, error) {
	if now.IsZero() || len(input) > limit {
		return nil, errorOf(ErrorIntegrity)
	}
	output := make([]time.Time, 0, limit)
	cutoff := now.Add(-BudgetWindow)
	var previous time.Time
	for _, stamp := range input {
		if stamp.IsZero() || stamp.After(now) || !previous.IsZero() && stamp.Before(previous) {
			return nil, errorOf(ErrorIntegrity)
		}
		previous = stamp
		if stamp.After(cutoff) {
			output = append(output, stamp)
		}
	}
	return output, nil
}

func AdmitBeginBudget(input []time.Time, now time.Time) (BeginBudget, error) {
	values, err := admitWindow(input, BeginBudgetLimit, now)
	if err != nil {
		return BeginBudget{}, err
	}
	return BeginBudget{timestamps: values}, nil
}

func AdmitProofBudget(input []time.Time, now time.Time) (ProofBudget, error) {
	values, err := admitWindow(input, ProofBudgetLimit, now)
	if err != nil {
		return ProofBudget{}, err
	}
	return ProofBudget{timestamps: values}, nil
}

func (v BeginBudget) Reserve(now time.Time) (BeginBudget, error) {
	if v.timestamps == nil {
		return BeginBudget{}, errorOf(ErrorInvalidInput)
	}
	values, err := admitWindow(v.timestamps, BeginBudgetLimit, now)
	if err != nil {
		return BeginBudget{}, err
	}
	if len(values) >= BeginBudgetLimit {
		return BeginBudget{}, errorOf(ErrorRateLimited)
	}
	return BeginBudget{timestamps: append(values, now)}, nil
}

func (v ProofBudget) Reserve(now time.Time) (ProofBudget, error) {
	if v.timestamps == nil {
		return ProofBudget{}, errorOf(ErrorInvalidInput)
	}
	values, err := admitWindow(v.timestamps, ProofBudgetLimit, now)
	if err != nil {
		return ProofBudget{}, err
	}
	if len(values) >= ProofBudgetLimit {
		return ProofBudget{}, errorOf(ErrorRateLimited)
	}
	return ProofBudget{timestamps: append(values, now)}, nil
}

func (v BeginBudget) Timestamps() ([]time.Time, error) {
	if v.timestamps == nil {
		return nil, errorOf(ErrorInvalidInput)
	}
	return append([]time.Time{}, v.timestamps...), nil
}

func (v ProofBudget) Timestamps() ([]time.Time, error) {
	if v.timestamps == nil {
		return nil, errorOf(ErrorInvalidInput)
	}
	return append([]time.Time{}, v.timestamps...), nil
}

// CryptoGate bounds retained snapshots too: its permit must outlive crypto and
// remain held until finalization/refusal has dropped all records/envelopes.
// Waiting requests hold no decoded credential. No goroutine is spawned here.
type CryptoGate struct{ *cryptoGateState }

type cryptoGateState struct {
	active    chan struct{}
	waiting   chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
	mu        sync.Mutex
	changed   chan struct{}
}

type CryptoPermit struct {
	state *cryptoPermitState
}

type cryptoPermitState struct {
	gate *CryptoGate
	once sync.Once
}

func NewCryptoGate() *CryptoGate {
	return &CryptoGate{cryptoGateState: &cryptoGateState{active: make(chan struct{}, CryptoActiveLimit), waiting: make(chan struct{}, CryptoWaitingLimit), closed: make(chan struct{}), changed: make(chan struct{})}}
}

func (g *CryptoGate) valid() bool {
	if g == nil || g.cryptoGateState == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return cap(g.active) == CryptoActiveLimit && cap(g.waiting) == CryptoWaitingLimit && g.closed != nil && g.changed != nil
}

func (g *CryptoGate) notify() {
	g.mu.Lock()
	close(g.changed)
	g.changed = make(chan struct{})
	g.mu.Unlock()
}

func (g *CryptoGate) admitted(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-g.closed:
		return errorOf(ErrorUnavailable)
	default:
		return nil
	}
}

func (g *CryptoGate) Acquire(ctx context.Context) (*CryptoPermit, error) {
	if !g.valid() || !validContext(ctx) {
		return nil, errorOf(ErrorInvalidInput)
	}
	if err := g.admitted(ctx); err != nil {
		return nil, err
	}
	select {
	case g.active <- struct{}{}:
		if err := g.admitted(ctx); err != nil {
			<-g.active
			g.notify()
			return nil, err
		}
		return &CryptoPermit{state: &cryptoPermitState{gate: g}}, nil
	default:
	}
	select {
	case g.waiting <- struct{}{}:
	default:
		return nil, errorOf(ErrorBusy)
	}
	defer func() { <-g.waiting; g.notify() }()
	timer := time.NewTimer(CryptoWaitLimit)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-g.closed:
		return nil, errorOf(ErrorUnavailable)
	case <-timer.C:
		return nil, errorOf(ErrorBusy)
	case g.active <- struct{}{}:
		if err := g.admitted(ctx); err != nil {
			<-g.active
			g.notify()
			return nil, err
		}
		return &CryptoPermit{state: &cryptoPermitState{gate: g}}, nil
	}
}

func (p *CryptoPermit) Release() {
	if p == nil || p.state == nil || p.state.gate == nil {
		return
	}
	p.state.once.Do(func() { <-p.state.gate.active; p.state.gate.notify() })
}

func (g *CryptoGate) CloseAdmissions() {
	if !g.valid() {
		return
	}
	g.closeOnce.Do(func() { close(g.closed); g.notify() })
}

func (g *CryptoGate) Drain(ctx context.Context) error {
	if !g.valid() || !validContext(ctx) {
		return errorOf(ErrorInvalidInput)
	}
	g.CloseAdmissions()
	for {
		g.mu.Lock()
		changed := g.changed
		idle := len(g.active) == 0 && len(g.waiting) == 0
		g.mu.Unlock()
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

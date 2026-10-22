package integrations_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
)

// The barrier is after identity resolution, precisely where Registry.Resolve
// has returned its admission token but Host has not committed declarations.
type nha055DeclarationAuthority struct {
	base               integrations.IdentityAuthority
	calls              atomic.Int32
	resolved, returned chan struct{}
	resume             chan struct{}
}

func (a *nha055DeclarationAuthority) Resolve(ctx context.Context, instance domain.IntegrationInstanceID, keys []integrations.SourceKey) ([]integrations.ResolvedIdentity, error) {
	identities, err := a.base.Resolve(ctx, instance, keys)
	if err != nil {
		return nil, err
	}
	if a.calls.Add(1) == 2 && a.resolved != nil {
		close(a.resolved)
		select {
		case <-a.resume:
			close(a.returned)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return identities, nil
}

type nha055DeclarationSink struct {
	*fake.Fixture
	discoveries, withdrawals atomic.Int32
	withdrawAttempts         atomic.Int32
	withdrawError            error
}

func (s *nha055DeclarationSink) AcceptDiscovery(ctx context.Context, declarations []integrations.DiscoveryDeclaration) error {
	if err := s.Fixture.AcceptDiscovery(ctx, declarations); err != nil {
		return err
	}
	s.discoveries.Add(1)
	return nil
}

func (s *nha055DeclarationSink) WithdrawDiscovery(ctx context.Context, entity domain.EntityID) error {
	s.withdrawAttempts.Add(1)
	if s.withdrawError != nil {
		return s.withdrawError
	}
	if err := s.Fixture.WithdrawDiscovery(ctx, entity); err != nil {
		return err
	}
	s.withdrawals.Add(1)
	return nil
}

type nha055DeclarationRig struct {
	host      *integrations.Manager
	session   *integrations.Session
	authority *nha055DeclarationAuthority
	sink      *nha055DeclarationSink
	binding   integrations.Binding
	key       integrations.SourceKey
}

func nha055DeclarationCandidate(t *testing.T, key integrations.SourceKey, maximum float64) integrations.Candidate {
	t.Helper()
	candidate, err := integrations.NewCandidate(key, []*registry.CapabilityDescription{{
		SchemaId: contracts.LightSchema, Major: 1, DisplayName: "Synthetic declaration", Endpoint: "relay",
		Constraints: &registry.CapabilityDescription_Light{Light: &registry.LightConstraints{SetOn: true, BrightnessPercent: &registry.NumberBounds{Minimum: 0, Maximum: maximum}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return candidate
}

func nha055DeclarationSetup(t *testing.T, barrier bool) nha055DeclarationRig {
	t.Helper()
	_, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	authority := &nha055DeclarationAuthority{base: fixture}
	if barrier {
		authority.resolved, authority.returned, authority.resume = make(chan struct{}), make(chan struct{}), make(chan struct{})
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	sink := &nha055DeclarationSink{Fixture: fixture}
	host, err := integrations.NewHost(authority, sink, catalog)
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(nha016Instance(t, "nha055-declarations"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("controller", "declaration")
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := session.Discover(context.Background(), []integrations.Candidate{nha055DeclarationCandidate(t, key, 100)})
	if err != nil || len(bindings) != 1 {
		t.Fatalf("initial declaration: %v", err)
	}
	return nha055DeclarationRig{host, session, authority, sink, bindings[0], key}
}

func nha055DeclarationWait(t *testing.T, channel <-chan struct{}) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(time.Second):
		t.Fatal("declaration barrier did not complete")
	}
}

func nha055DeclarationResult(t *testing.T, channel <-chan error) error {
	t.Helper()
	select {
	case err := <-channel:
		return err
	case <-time.After(time.Second):
		t.Fatal("declaration operation did not join")
		return nil
	}
}

func TestNHA055DeclarationLeaseClosesResolveGapAndDefersContraction(t *testing.T) {
	rig := nha055DeclarationSetup(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	contracted := nha055DeclarationCandidate(t, rig.key, 50)
	result := make(chan error, 1)
	go func() { _, err := rig.session.Discover(ctx, []integrations.Candidate{contracted}); result <- err }()
	nha055DeclarationWait(t, rig.authority.resolved)
	lease, err := rig.host.ReserveConfigurationDeclarations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	close(rig.authority.resume)
	nha055DeclarationWait(t, rig.authority.returned)
	if err := lease.Confirm(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, entity, _ := rig.binding.IDs()
	descriptions, err := rig.host.Capabilities(entity)
	if err != nil || len(descriptions) != 1 || descriptions[0].GetLight().BrightnessPercent.Maximum != 100 {
		t.Fatalf("contraction escaped reservation: %v", err)
	}
	if rig.sink.discoveries.Load() != 1 {
		t.Fatal("AcceptDiscovery ran while declarations were reserved")
	}
	// Health remains live, proving the waiter did not retain Host.mu.
	report, _ := integrations.NewHealthReport(integrations.HealthReady, "synthetic alive")
	if err := rig.session.ReportHealth(context.Background(), report); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err := nha055DeclarationResult(t, result); err != nil {
		t.Fatal(err)
	}
	descriptions, err = rig.host.Capabilities(entity)
	if err != nil || len(descriptions) != 1 || descriptions[0].GetLight().BrightnessPercent.Maximum != 50 || rig.authority.calls.Load() != 3 {
		t.Fatalf("discovery did not resolve fresh identities after lease: %v", err)
	}
}

func TestNHA055DeclarationLeaseKeepsTechnicalFenceAndFlushesWithdrawal(t *testing.T) {
	rig := nha055DeclarationSetup(t, false)
	lease, err := rig.host.ReserveConfigurationDeclarations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	_, entity, _ := rig.binding.IDs()
	if got, err := rig.session.FenceSource(context.Background(), rig.key); err != nil || got != entity {
		t.Fatalf("source fence waited or failed: %v", err)
	}
	if !errors.Is(lease.Confirm(context.Background()), integrations.ErrConfigurationDeclarationsChanged) {
		t.Fatal("fenced identity remained confirmable during SQL reservation")
	}
	if rig.sink.withdrawals.Load() != 0 {
		t.Fatal("projection withdrawal escaped reservation")
	}
	descriptions, err := rig.host.Capabilities(entity)
	if err != nil || len(descriptions) != 1 {
		t.Fatal("catalogue changed before reservation released")
	}
	if err := rig.session.Publish(context.Background(), rig.binding, nha016Observation(t, time.Now())); !errors.Is(err, integrations.ErrForeignBinding) {
		t.Fatalf("technical fence was deferred: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	descriptions, err = rig.host.Capabilities(entity)
	if err != nil || len(descriptions) != 0 || rig.sink.withdrawals.Load() != 1 {
		t.Fatal("deferred projection did not drain before Release returned")
	}
	if err := lease.Release(); err != nil || rig.sink.withdrawals.Load() != 1 {
		t.Fatal("idempotent release repeated withdrawal")
	}
}

// Done is observed only once the blocked Discover has registered its wait.
type nha055DeclarationWaitContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *nha055DeclarationWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestNHA055DeclarationLeaseWaiterCancellationAndShutdown(t *testing.T) {
	for _, mode := range []string{"cancel", "shutdown", "replace-session"} {
		t.Run(mode, func(t *testing.T) {
			rig := nha055DeclarationSetup(t, false)
			lease, err := rig.host.ReserveConfigurationDeclarations(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &nha055DeclarationWaitContext{Context: base, entered: make(chan struct{})}
			candidate := nha055DeclarationCandidate(t, rig.key, 50)
			result := make(chan error, 1)
			go func() { _, err := rig.session.Discover(ctx, []integrations.Candidate{candidate}); result <- err }()
			nha055DeclarationWait(t, ctx.entered)
			wanted := context.Canceled
			switch mode {
			case "cancel":
				cancel()
			case "shutdown":
				if err := rig.host.CloseAdmissions(); err != nil {
					t.Fatal(err)
				}
				wanted = integrations.ErrAdmissionsClosed
			case "replace-session":
				if _, err := rig.host.OpenSession(nha016Instance(t, "nha055-declarations")); err != nil {
					t.Fatal(err)
				}
				wanted = integrations.ErrInactiveSession
			}
			if err := nha055DeclarationResult(t, result); !errors.Is(err, wanted) {
				t.Fatalf("waiter did not terminate without releasing SQL reservation: %v", err)
			}
			if rig.authority.calls.Load() != 1 || rig.sink.discoveries.Load() != 1 {
				t.Fatal("blocked discovery reached registry or projection")
			}
		})
	}
}

func TestNHA055DeclarationLeaseRejectsZeroTypedNilAndDoubleReservation(t *testing.T) {
	var zero integrations.ConfigurationDeclarationsLease
	if !errors.Is(zero.Confirm(context.Background()), integrations.ErrInvalidInput) || !errors.Is(zero.Release(), integrations.ErrInvalidInput) {
		t.Fatal("zero lease admitted")
	}
	rig := nha055DeclarationSetup(t, false)
	var typedNil *nha055DeclarationWaitContext
	if _, err := rig.host.ReserveConfigurationDeclarations(typedNil); !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatalf("typed nil admitted: %v", err)
	}
	lease, err := rig.host.ReserveConfigurationDeclarations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if _, err := rig.host.ReserveConfigurationDeclarations(context.Background()); !errors.Is(err, integrations.ErrSupervisorState) {
		t.Fatalf("second reservation admitted: %v", err)
	}
	if !errors.Is(lease.Confirm(typedNil), integrations.ErrInvalidInput) {
		t.Fatal("typed nil confirmation admitted")
	}
}

func TestNHA055DeclarationLeasePreservesWithdrawalFailure(t *testing.T) {
	rig := nha055DeclarationSetup(t, false)
	lease, err := rig.host.ReserveConfigurationDeclarations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if err := rig.session.Withdraw(context.Background(), rig.binding); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("synthetic projection withdrawal failure")
	rig.sink.withdrawError = failure
	if err := lease.Release(); !errors.Is(err, failure) {
		t.Fatalf("flush failure was hidden from activation owner: %v", err)
	}
	if err := lease.Release(); !errors.Is(err, failure) || rig.sink.withdrawAttempts.Load() != 1 {
		t.Fatal("repeat Release lost failure or repeated projection mutation")
	}
}

type nha055DeclarationABAAuthority struct {
	current          atomic.Pointer[integrations.ResolvedIdentity]
	calls            atomic.Int32
	resolved, resume chan struct{}
}

func (a *nha055DeclarationABAAuthority) Resolve(ctx context.Context, _ domain.IntegrationInstanceID, keys []integrations.SourceKey) ([]integrations.ResolvedIdentity, error) {
	if len(keys) != 1 {
		return nil, integrations.ErrInvalidInput
	}
	identity := *a.current.Load()
	if a.calls.Add(1) == 1 {
		close(a.resolved)
		select {
		case <-a.resume:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return []integrations.ResolvedIdentity{identity}, nil
}

func TestNHA055DeclarationLeaseReleasedBeforeResolveReturnStillRetries(t *testing.T) {
	_, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	device, err := domain.ParseDeviceID("fixture-aba-device")
	if err != nil {
		t.Fatal(err)
	}
	oldEntity, err := domain.ParseEntityID("Host:nha055-aba-old")
	if err != nil {
		t.Fatal(err)
	}
	newEntity, err := domain.ParseEntityID("Host:nha055-aba-new")
	if err != nil {
		t.Fatal(err)
	}
	oldIdentity := integrations.ResolvedIdentity{Device: device, Entity: oldEntity}
	newIdentity := integrations.ResolvedIdentity{Device: device, Entity: newEntity}
	authority := &nha055DeclarationABAAuthority{resolved: make(chan struct{}), resume: make(chan struct{})}
	authority.current.Store(&oldIdentity)
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(authority, fixture, catalog)
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(nha016Instance(t, "nha055-aba"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("controller", "aba")
	if err != nil {
		t.Fatal(err)
	}
	candidate := nha055DeclarationCandidate(t, key, 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		bindings []integrations.Binding
		err      error
	}
	result := make(chan outcome, 1)
	go func() {
		bindings, err := session.Discover(ctx, []integrations.Candidate{candidate})
		result <- outcome{bindings, err}
	}()
	nha055DeclarationWait(t, authority.resolved)
	lease, err := host.ReserveConfigurationDeclarations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	// Simulated committed registry replacement while the logical reservation
	// is held. Its ENTIRE lifetime ends before the old Resolve may return.
	authority.current.Store(&newIdentity)
	if err := lease.Confirm(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	close(authority.resume)
	select {
	case got := <-result:
		if got.err != nil || len(got.bindings) != 1 {
			t.Fatalf("fresh discovery failed: %v", got.err)
		}
		_, entity, err := got.bindings[0].IDs()
		if err != nil || entity != newEntity || authority.calls.Load() != 2 {
			t.Fatalf("completed reservation allowed old resolved identity: entity=%v calls=%d err=%v", entity, authority.calls.Load(), err)
		}
	case <-time.After(time.Second):
		t.Fatal("ABA discovery did not join")
	}
	if descriptions, err := host.Capabilities(oldEntity); err != nil || len(descriptions) != 0 {
		t.Fatal("old identity was published before the fresh retry")
	}
}

func TestNHA055DeclarationLeaseRetirementSaturationWakesWaiter(t *testing.T) {
	rig := nha055DeclarationSetup(t, false)
	lease, err := rig.host.ReserveConfigurationDeclarations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &nha055DeclarationWaitContext{Context: base, entered: make(chan struct{})}
	candidate := nha055DeclarationCandidate(t, rig.key, 50)
	result := make(chan error, 1)
	go func() { _, err := rig.session.Discover(ctx, []integrations.Candidate{candidate}); result <- err }()
	nha055DeclarationWait(t, ctx.entered)
	// Unconfigured sources are fenced before registry resolution as well.
	// The bounded fake identity table may refuse them; their technical source
	// fences must still saturate Host's existing16384-entry admission bound.
	for i := 0; i <= 16384; i++ {
		key, err := integrations.NewSourceKey("overflow", strconv.Itoa(i))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = rig.session.FenceSource(context.Background(), key)
	}
	if err := nha055DeclarationResult(t, result); !errors.Is(err, integrations.ErrAdmissionsClosed) {
		t.Fatalf("retirement saturation left reserved discovery waiting: %v", err)
	}
	if !errors.Is(lease.Confirm(context.Background()), integrations.ErrConfigurationDeclarationsChanged) {
		t.Fatal("exhausted technical fence remained confirmable")
	}
}

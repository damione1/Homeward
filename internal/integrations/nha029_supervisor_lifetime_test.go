package integrations_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/integrations"
)

func TestNHA029SupervisorReplacementOwnershipRequiresPreStart(t *testing.T) {
	var missing *integrations.Supervisor
	if err := missing.PrepareReplacements(); !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatalf("nil replacement owner admitted: %v", err)
	}
	rig := nha026SetupBare(t)
	supervisor := nha026Supervisor(t, rig.host, nil, 0, &nha026Timer{waits: make(chan nha026TimerRequest, 1)})
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	nha026Done(t, supervisor.Done())
	if err := supervisor.PrepareReplacements(); !errors.Is(err, integrations.ErrSupervisorState) {
		t.Fatalf("late ownership reopened a naturally finished supervisor: %v", err)
	}
	var factories atomic.Int32
	spec := nha026Spec(t, rig.instances[0], func() integrations.Pilot { factories.Add(1); return nil })
	if reservation, err := supervisor.ReserveReplacement([]integrations.PilotSpec{spec}, nil); reservation != nil || !errors.Is(err, integrations.ErrSupervisorState) || factories.Load() != 0 {
		t.Fatalf("autonomous Done admitted future work: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := supervisor.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	// Refused Start has not acquired a runtime owner or frozen Host. Existing
	// prepared-runtime cleanup must still complete without starting any pilot.
	preparedRig := nha026SetupBare(t)
	prepared := nha026Supervisor(t, preparedRig.host, []integrations.PilotSpec{nha026Spec(t, preparedRig.instances[0], func() integrations.Pilot { factories.Add(1); return nil })}, 0, &nha026Timer{waits: make(chan nha026TimerRequest, 1)})
	if err := prepared.PrepareReplacements(); err != nil {
		t.Fatal(err)
	}
	if err := prepared.PrepareReplacements(); !errors.Is(err, integrations.ErrSupervisorState) {
		t.Fatalf("duplicate prepared owner admitted: %v", err)
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if err := prepared.Start(cancelled); !errors.Is(err, context.Canceled) || factories.Load() != 0 {
		t.Fatalf("refused Start launched a pilot: %v", err)
	}
	if done, err := preparedRig.host.ClosePreparedRuntime(ctx); err != nil || done != nil {
		t.Fatalf("refused activation-owned Start could not clean prepared Host: %v", err)
	}
}

func TestNHA029SupervisorEmptyActivationOwnerRepairsDegradedPilot(t *testing.T) {
	rig := nha026SetupBare(t)
	supervisor := nha026Supervisor(t, rig.host, nil, 0, &nha026Timer{waits: make(chan nha026TimerRequest, 1)})
	if err := supervisor.PrepareReplacements(); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := supervisor.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	select {
	case <-supervisor.Done():
		t.Fatal("actual activation owner lost its empty runtime")
	default:
	}
	var failedFactories, repairedFactories atomic.Int32
	bad := nha026Spec(t, rig.instances[0], func() integrations.Pilot { failedFactories.Add(1); panic("synthetic first activation factory failure") })
	first, err := supervisor.ReserveReplacement([]integrations.PilotSpec{bad}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Quiesce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if failedFactories.Load() != 0 {
		t.Fatal("replacement preparation invoked the failed factory")
	}
	if err := first.Publish(); err != nil {
		t.Fatal(err)
	}
	status := nha026AwaitPhase(t, supervisor, rig.instances[0], integrations.PilotDegraded)
	if status.Tasks != 0 || status.Attempts != 1 || failedFactories.Load() != 1 {
		t.Fatalf("factory degradation did not finish its real work: %+v", status)
	}
	select {
	case <-supervisor.Done():
		t.Fatal("degradation consumed the actual future-admission owner")
	default:
	}
	repaired := &nha026Pilot{host: rig.host, started: make(chan struct{})}
	worker, joined := make(chan struct{}), make(chan struct{})
	repaired.task = func(_ context.Context, tasks integrations.Tasks) error {
		return tasks.Go(func(ctx context.Context) error { close(worker); <-ctx.Done(); close(joined); return nil })
	}
	next := nha026Spec(t, rig.instances[0], func() integrations.Pilot { repairedFactories.Add(1); return repaired })
	second, err := supervisor.ReserveReplacement([]integrations.PilotSpec{next}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Quiesce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := second.Publish(); err != nil {
		t.Fatal(err)
	}
	nha026Done(t, repaired.started)
	nha026Done(t, worker)
	nha026AwaitPhase(t, supervisor, rig.instances[0], integrations.PilotRunning)
	if repairedFactories.Load() != 1 || failedFactories.Load() != 1 || !rig.host.AdmissionsOpen() {
		t.Fatal("repair reopened a factory twice or closed unrelated Host admissions")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := supervisor.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	nha026Done(t, joined)
	nha026Done(t, supervisor.Done())
	if status, _ := supervisor.Status(rig.instances[0]); status.Tasks != 0 || repaired.stops.Load() != 1 || rig.host.AdmissionsOpen() {
		t.Fatalf("global stop did not release owner and join repaired pilot: %+v", status)
	}
	if reservation, err := supervisor.ReserveReplacement([]integrations.PilotSpec{next}, nil); reservation != nil || !errors.Is(err, integrations.ErrSupervisorState) || repairedFactories.Load() != 1 {
		t.Fatalf("global stop readmitted a replacement: %v", err)
	}
}

func TestNHA029SupervisorQuiesceSupersedeKeepsAdmissionUntilC(t *testing.T) {
	rig := nha026SetupBare(t)
	workerA, joinedA := make(chan struct{}), make(chan struct{})
	pilotA := &nha026Pilot{host: rig.host, started: make(chan struct{})}
	pilotA.task = func(_ context.Context, tasks integrations.Tasks) error {
		return tasks.Go(func(ctx context.Context) error { close(workerA); <-ctx.Done(); close(joinedA); return nil })
	}
	supervisor := nha026Supervisor(t, rig.host, []integrations.PilotSpec{nha026Spec(t, rig.instances[0], func() integrations.Pilot { return pilotA })}, 0, &nha026Timer{waits: make(chan nha026TimerRequest, 1)})
	if err := supervisor.PrepareReplacements(); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := supervisor.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	nha026Done(t, workerA)
	nha026AwaitPhase(t, supervisor, rig.instances[0], integrations.PilotRunning)
	var factoriesB, factoriesC atomic.Int32
	candidateB := nha026Spec(t, rig.instances[0], func() integrations.Pilot { factoriesB.Add(1); return nil })
	b, err := supervisor.ReserveReplacement([]integrations.PilotSpec{candidateB}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := b.Quiesce(ctx); err != nil {
		t.Fatal(err)
	}
	nha026Done(t, joinedA)
	if pilotA.stops.Load() != 1 || factoriesB.Load() != 0 || !rig.host.AdmissionsOpen() {
		t.Fatal("quiesce did not join only A before invoking any replacement")
	}
	forced, err := b.Supersede(ctx)
	if err != nil || len(forced) == 0 {
		t.Fatalf("joined B could not hand its retired instances to C: %v", err)
	}
	for _, instance := range forced {
		if instance != rig.instances[0] {
			t.Fatal("supersede reported an unrelated pilot")
		}
	}
	if err := b.Publish(); !errors.Is(err, integrations.ErrSupervisorState) || factoriesB.Load() != 0 {
		t.Fatalf("superseded B still invoked a factory: %v", err)
	}
	select {
	case <-supervisor.Done():
		t.Fatal("zero-worker supersede gap discarded the actual activation owner")
	default:
	}
	pilotC := &nha026Pilot{host: rig.host, started: make(chan struct{})}
	workerC, joinedC := make(chan struct{}), make(chan struct{})
	pilotC.task = func(_ context.Context, tasks integrations.Tasks) error {
		return tasks.Go(func(ctx context.Context) error { close(workerC); <-ctx.Done(); close(joinedC); return nil })
	}
	candidateC := nha026Spec(t, rig.instances[0], func() integrations.Pilot { factoriesC.Add(1); return pilotC })
	c, err := supervisor.ReserveReplacement([]integrations.PilotSpec{candidateC}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Quiesce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Publish(); err != nil {
		t.Fatal(err)
	}
	nha026Done(t, workerC)
	nha026AwaitPhase(t, supervisor, rig.instances[0], integrations.PilotRunning)
	if factoriesC.Load() != 1 || factoriesB.Load() != 0 || pilotA.stops.Load() != 1 || !rig.host.AdmissionsOpen() {
		t.Fatal("C did not use the original Supervisor's surviving admission owner")
	}
	if err := pilotA.session.Publish(context.Background(), pilotA.binding, nha016Observation(t, time.Now())); !errors.Is(err, integrations.ErrInactiveSession) {
		t.Fatalf("retired A session published through C: %v", err)
	}
	if err := supervisor.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	nha026Done(t, joinedC)
	nha026Done(t, supervisor.Done())
	if status, _ := supervisor.Status(rig.instances[0]); status.Tasks != 0 || pilotC.stops.Load() != 1 {
		t.Fatalf("C retained supervised work after global Stop: %+v", status)
	}
}

package integrations

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// Go has passed its first context check and waits for the supervisor lock.
// A concurrent Stop wins that authority before Go may reserve a task.
func TestNHA026TaskGroupRechecksShutdownAfterSupervisorLockWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := &Supervisor{phase: SupervisorRunning, limits: TaskLimits{perPilot: 1, global: 1}}
	run := &pilotRun{}
	group := newTaskGroup(ctx, owner, run)
	owner.mu.Lock()
	ran := make(chan struct{})
	result := make(chan error, 1)
	go func() { result <- group.Go(func(context.Context) error { close(ran); return nil }) }()
	deadline := time.After(time.Second)
	for group.mu.TryLock() {
		group.mu.Unlock()
		select {
		case <-time.After(time.Millisecond):
		case <-deadline:
			t.Fatal("Go did not reach lock barrier")
		}
	}
	owner.phase = SupervisorStopping
	cancel()
	owner.mu.Unlock()
	if err := <-result; !errors.Is(err, ErrSupervisorState) {
		t.Fatalf("late task admission = %v", err)
	}
	group.Close()
	select {
	case <-group.Done():
	case <-time.After(time.Second):
		t.Fatal("rejected Go was retained")
	}
	select {
	case <-ran:
		t.Fatal("late task executed")
	default:
	}
	if owner.tasksActive != 0 || run.tasks != 0 {
		t.Fatal("rejected task changed counts")
	}
}

func TestNHA026IgnoredInvalidTaskCannotLeavePilotReady(t *testing.T) {
	owner := &Supervisor{phase: SupervisorRunning, limits: TaskLimits{perPilot: 1, global: 1}}
	group := newTaskGroup(context.Background(), owner, &pilotRun{})
	if err := group.Go(nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid task = %v", err)
	}
	select {
	case <-group.fail:
	default:
		t.Fatal("ignored task rejection left pilot healthy")
	}
	group.Close()
	<-group.Done()
}

func TestNHA026RandomJitterFullDurationDomain(t *testing.T) {
	jitter := RandomJitter{}
	for _, limit := range []time.Duration{0, -1, time.Duration(1<<63 - 1)} {
		for i := 0; i < 32; i++ {
			got := jitter.Offset(limit)
			if limit <= 0 && got != 0 || limit > 0 && (got < 0 || got > limit) {
				t.Fatalf("Offset(%v) = %v", limit, got)
			}
		}
	}
}

func TestNHA026PilotTaskCapabilityExposesOnlyGo(t *testing.T) {
	var tasks Tasks = pilotTaskView{}
	methods := reflect.TypeOf(tasks)
	if methods.NumMethod() != 1 || methods.Method(0).Name != "Go" {
		t.Fatalf("pilot task methods = %v", methods)
	}
	if err := tasks.Go(func(context.Context) error { return nil }); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("zero task view = %v", err)
	}
}

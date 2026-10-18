package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

func TestNHA170CrashWaitsForAdmittedCommandStream(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "nha170-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	markerPath := filepath.Join(directory, "worker.log")
	gate, err := net.Listen("unix", filepath.Join(directory, "commands.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.(*net.UnixListener).SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		_ = gate.Close()
		t.Fatal(err)
	}
	type gateResult struct {
		before string
		err    error
	}
	controllerDone := make(chan gateResult, 1)
	forceRelease := make(chan struct{})
	waitingForStream := make(chan struct{})
	joined := false
	defer func() {
		close(forceRelease)
		_ = gate.Close()
		if !joined {
			<-controllerDone
		}
	}()
	go func() {
		workerGate, err := gate.Accept()
		if err != nil {
			controllerDone <- gateResult{err: err}
			return
		}
		defer workerGate.Close()
		if err := workerGate.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			controllerDone <- gateResult{err: err}
			return
		}
		var arrived [1]byte
		if _, err := io.ReadFull(workerGate, arrived[:]); err != nil || arrived[0] != 0 {
			controllerDone <- gateResult{err: fmt.Errorf("worker gate arrival: %v, byte %d", err, arrived[0])}
			return
		}
		before, err := os.ReadFile(markerPath)
		if err != nil {
			controllerDone <- gateResult{err: err}
			return
		}
		select {
		case <-waitingForStream:
		case <-forceRelease:
		}
		if _, err := workerGate.Write([]byte{1}); err != nil {
			controllerDone <- gateResult{err: err}
			return
		}
		controllerDone <- gateResult{before: string(before)}
	}()
	// This call must remain blocked by the real Python worker's gate until
	// Commands is admitted. With the old initial-only rig, it returns early.
	rig := startWorkerRigWithGate(t, "crash", true, gate.Addr().String(), markerPath,
		func() { close(waitingForStream) })
	select {
	case <-waitingForStream:
	default:
		t.Fatal("rig returned before the command stream was admitted")
	}
	gateState := <-controllerDone
	joined = true
	if gateState.err != nil {
		t.Fatal(gateState.err)
	}
	before := gateState.before
	if !strings.Contains(before, "initial:2\n") || !strings.Contains(before, "event_emitted\n") ||
		strings.Contains(before, "second_stream_rejected\n") {
		t.Fatalf("gate did not isolate initial observations from command admission: %s", before)
	}
	streamMarker := waitMarker(t, markerPath, "second_stream_rejected", 5*time.Second)
	position := -1
	for _, step := range []string{"initial:2\n", "command_gate_waiting\n", "command_gate_released\n", "second_stream_rejected\n"} {
		next := strings.Index(streamMarker, step)
		if next <= position {
			t.Fatalf("readiness step %q missing or out of order: %s", step, streamMarker)
		}
		position = next
	}
	if len(rig.admitted) != 0 {
		t.Fatal("test dispatched before command readiness")
	}
	actor, err := domain.ParseActorID("trusted-actor")
	if err != nil {
		t.Fatal(err)
	}
	const id = "nha170-crash-command"
	request := commandRequest(t, rig.fixture, rig.entities[0], id)
	response, err := rig.service.Dispatch(context.Background(), actor, request)
	if !errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) || response == nil ||
		response.CommandId != id || response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
		t.Fatalf("worker crash outcome: %v %v", response, err)
	}
	waitMarker(t, markerPath, "crash_on_command:"+id, 5*time.Second)
	select {
	case <-rig.pilot.waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Python worker did not exit after receiving the command")
	}
	rig.pilot.mu.Lock()
	exitErr := rig.pilot.exitErr
	rig.pilot.mu.Unlock()
	var crashed *exec.ExitError
	if !errors.As(exitErr, &crashed) || crashed.ExitCode() != 17 {
		t.Fatalf("Python worker exit after command: %v, want status 17", exitErr)
	}
	first, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(first), "command:1:"+id+"\n") != 1 ||
		strings.Count(string(first), "crash_on_command:"+id+"\n") != 1 ||
		strings.Count("\n"+string(first), "\ncommand:") != 1 {
		t.Fatalf("Python did not receive exactly the expected command once: %s", first)
	}
	if len(rig.admitted) != 1 {
		t.Fatalf("first route reached the driver %d times, want 1", len(rig.admitted))
	}
	<-rig.admitted
	replayed, replayErr := rig.service.Dispatch(context.Background(), actor, request)
	if !errors.Is(replayErr, integrations.ErrCommandOutcomeIndeterminate) || replayed == nil ||
		replayed.CommandId != id || replayed.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
		t.Fatalf("repeated command changed its indeterminate result: %v %v", replayed, replayErr)
	}
	if len(rig.admitted) != 0 {
		t.Fatal("repeated command reached the driver again")
	}
	after, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(first) {
		t.Fatalf("worker marker changed after deduplicated command: %s", after)
	}
}

package nha045

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/automationruns"
)

func TestNHA045NativeObservationReleasedOnlyAfterJoinedQualification(t *testing.T) {
	r, observation := joinedNativeOrderFixture(t, 0)
	if !r.observationHeld {
		t.Fatal("postjoin native storage has no explicit observation owner")
	}
	if err := r.qualifyNative(observation, true); err != nil {
		t.Fatal(err)
	}
	decisions, decisionLoss := r.controller.Traces()
	nodes, nodeLoss := r.interpreter.NodeTraces()
	if len(nodes) == 0 {
		t.Fatal("joined interpreter ring was not retained")
	}
	order := r.controller.TraceOrderState()
	alias := r.observation
	if err := r.dispose(); err != nil {
		t.Fatal(err)
	}
	after, afterNodeLoss := r.interpreter.NodeTraces()
	afterDecisions, afterDecisionLoss := r.controller.Traces()
	if r.observationHeld || len(after) != 0 || afterNodeLoss != nodeLoss || afterDecisionLoss != decisionLoss ||
		!reflect.DeepEqual(decisions, afterDecisions) || r.controller.TraceOrderState() != order {
		t.Fatal("postqualification release retained the ring or changed native evidence")
	}
	if err := alias.Release(); !errors.Is(err, automationruns.ErrInvalidPlan) {
		t.Fatalf("probe copied handle released the same image twice: %v", err)
	}
	if err := r.qualifyNative(observation, true); !issueIs(err, InvalidObservation) {
		t.Fatalf("released producer was replaced by the saved pre-release snapshot: %v", err)
	}
	if err := r.dispose(); err != nil {
		t.Fatalf("final probe cleanup was not idempotent: %v", err)
	}
}

func TestNHA045NativeObservationCaptureRefusalStillDisposesOwner(t *testing.T) {
	fixtures, err := Corpus()
	if err != nil || len(fixtures) != 10 {
		t.Fatalf("corpus: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r, err := buildRig(ctx, fixtures[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.dispose(); err != nil {
			t.Error(err)
		}
	})
	if !r.observationHeld {
		t.Fatal("constructor did not retain the real native image")
	}
	// Only admit the first real phase: capture must refuse unfinished business.
	if err := r.step(ctx, r.f.phases[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := r.capture(NetworkEvidence{}); !issueIs(err, InvalidObservation) {
		t.Fatalf("unfinished capture did not refuse before cleanup: %v", err)
	}
	alias := r.observation
	if err := r.dispose(); err != nil {
		t.Fatal(err)
	}
	if !channelJoined(r.controller.Done()) || !channelJoined(r.triggers.Done()) ||
		!channelJoined(r.reactive.Done()) || !channelJoined(r.session.Done()) || r.observationHeld {
		t.Fatal("capture refusal left an execution or native observation owner")
	}
	if nodes, _ := r.interpreter.NodeTraces(); len(nodes) != 0 {
		t.Fatal("capture refusal kept native storage after cleanup")
	}
	if err := alias.Release(); !errors.Is(err, automationruns.ErrInvalidPlan) {
		t.Fatalf("error cleanup did not consume its exact native owner: %v", err)
	}
}

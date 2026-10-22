package automationruns

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/observability"
	"github.com/damione1/Homeward/internal/sequencer"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestNHA114StateOccurrenceJoinsPublicRunTrace(t *testing.T) {
	ctx := context.Background()
	clock := &nha042ManualClock{wall: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	host, fixture, err := fake.NewCommandFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha114-trace")
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := fake.NewDriver(clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(ctx, session); err != nil {
		t.Fatal(err)
	}
	binding, err := driver.Binding()
	if err != nil {
		t.Fatal(err)
	}
	device, entity, err := binding.IDs()
	if err != nil {
		t.Fatal(err)
	}
	key, _ := integrations.NewSourceKey("fixture-controller", "relay")
	alias, _ := domain.ParseHAAlias("switch.fixture")
	record, err := integrations.NewRegistryRecord(device, entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	ref, _ := ir.NewEntity("switch.fixture", ir.ReferencePresent, ir.SupportObserveState)
	catalogue, err := ir.NewCatalogue([]ir.Entity{ref}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err := parser.NewSourceFile("nha114-trace.yaml", []byte("id: nha114_trace\ntriggers:\n- trigger: state\n  entity_id: switch.fixture\n  to: 'on'\n  id: NHA114_PRIVATE_TRIGGER_KEY\nactions:\n- stop: done\n"))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := ir.CompileSources("automation.nha114_trace", "nha114-trace.yaml", []parser.SourceFile{source}, catalogue)
	if err != nil || len(compiled.Accepted()) != 1 {
		t.Fatalf("compile: %v %+v", err, compiled.Rejected())
	}
	programs := compiled.Accepted()
	triggerPlan, err := automationtriggers.NewPlan(automationtriggers.PlanInput{Automations: programs, Zone: "UTC", Resolver: nha042Resolver{entity}})
	if err != nil {
		t.Fatal(err)
	}
	triggers, err := automationtriggers.NewService(triggerPlan, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := triggers.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = triggers.Close() })
	plan, err := NewPlan(programs)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan Run, 1)
	controller, err := NewController(plan, triggers,
		nha041PrepareFunc(func(context.Context, ir.Automation, Trigger) (Preparation, error) {
			return Preparation{Outcome: PreparationReady}, nil
		}),
		nha041ExecuteFunc(func(ctx context.Context, _ ir.Automation, run Run) error {
			entered <- run
			<-ctx.Done()
			return ctx.Err()
		}))
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := observability.NewProcessObservationEpoch()
	if err != nil {
		t.Fatal(err)
	}
	cells, err := observability.NewCells(epoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.AttachObservation(cells); err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close(ctx) })
	before, err := fixture.Sequencer().Capture()
	if err != nil {
		t.Fatal(err)
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	value, at := domain.NewSwitchValue(true), clock.Now()
	state, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Publish(ctx, binding, state); err != nil {
		t.Fatal(err)
	}
	run := nha041Receive(t, entered)
	entries, _, err := fixture.Sequencer().ReadAfter(before.Cursor, 16)
	if err != nil || len(entries) != 1 || entries[0].Kind() != sequencer.KindState {
		t.Fatalf("one admitted state occurrence required: %d %v", len(entries), err)
	}
	wantEpoch, wantSequence, err := entries[0].Cursor().Components()
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Close(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _, truncated, err := controller.ObservationTrace(run.ID())
	if err != nil || truncated || len(rows) < 2 {
		t.Fatal("initial and terminal public decisions required", err)
	}
	positions := 0
	for _, row := range rows {
		wire, err := row.Wire()
		if err != nil || wire.RunId != uint64(run.ID()) {
			t.Fatal("public trace changed the real run identity", err)
		}
		position := wire.GetController().GetTriggerPosition()
		if position != nil {
			positions++
			if wire.GetController().Decision != obs.TraceDecision_TRACE_DECISION_ADMITTED || hex.EncodeToString(position.Epoch) != wantEpoch || position.Sequence != wantSequence {
				t.Fatal("initial public admission lost the exact state occurrence")
			}
		}
		encoded, err := protojson.Marshal(wire)
		if err != nil || bytes.Contains(encoded, []byte("NHA114_PRIVATE_TRIGGER_KEY")) || bytes.Contains(encoded, []byte("switch.fixture")) || bytes.Contains(encoded, []byte("nha114-trace.yaml")) {
			t.Fatal("projection exposed private trigger/source metadata", err)
		}
	}
	if positions != 1 {
		t.Fatalf("expected one initial source position, got %d", positions)
	}
}

func TestNHA114TraceCursorAbsenceEpochAndCopy(t *testing.T) {
	for _, decision := range []Decision{DecisionAdmitted, DecisionQueued, DecisionRestartPending, DecisionFinished} {
		row, err := observationControllerRow(Trace{Decision: decision, RunID: 7, ordinal: TraceOrdinal{value: 1}}, TraceOrderHealthy)
		if err != nil {
			t.Fatal(err)
		}
		wire, _ := row.Wire()
		if wire.GetController().GetTriggerPosition() != nil {
			t.Fatal("absent source was fabricated")
		}
	}
	for _, epoch := range []string{"01000000000000000000000000000000", "02000000000000000000000000000000"} {
		cursor, err := sequencer.ParseCursor(epoch, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decision := range []Decision{DecisionAdmitted, DecisionQueued, DecisionRestartPending} {
			row, err := observationControllerRow(Trace{Decision: decision, RunID: 7, ordinal: TraceOrdinal{value: 1}, triggerCursor: cursor}, TraceOrderHealthy)
			if err != nil {
				t.Fatal(err)
			}
			wire, _ := row.Wire()
			position := wire.GetController().GetTriggerPosition()
			if position == nil || hex.EncodeToString(position.Epoch) != epoch || position.Sequence != 0 {
				t.Fatal("valid initial boundary or source epoch changed")
			}
			position.Epoch[0] = 255
			fresh, _ := row.Wire()
			if hex.EncodeToString(fresh.GetController().TriggerPosition.Epoch) != epoch {
				t.Fatal("public wire mutation changed the admitted trace")
			}
		}
		if _, err := observationControllerRow(Trace{Decision: DecisionFinished, RunID: 7, ordinal: TraceOrdinal{value: 1}, triggerCursor: cursor}, TraceOrderHealthy); err == nil {
			t.Fatal("noninitial decision admitted a source position")
		}
	}
}

func TestNHA114ScriptInvocationHasNoSourcePosition(t *testing.T) {
	controller, gate, script, _, _ := nha056ObservedController(t, "single")
	handle := nha188Invoke(t, controller, script)
	if got := nha041Receive(t, gate.entered); got != handle.ID() {
		t.Fatal("wrong script run entered")
	}
	rows, _, truncated, err := controller.ObservationTrace(handle.ID())
	if err != nil || truncated || len(rows) == 0 {
		t.Fatal("script trace unavailable", err)
	}
	for _, row := range rows {
		wire, err := row.Wire()
		if err != nil || wire.RunId != uint64(handle.ID()) || wire.GetController().GetTriggerPosition() != nil {
			t.Fatal("script invocation fabricated an event position", err)
		}
	}
}

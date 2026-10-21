package observability

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
)

func nha056Fixture(t *testing.T) (ProcessObservationEpoch, *Cells, *CursorCodec) {
	t.Helper()
	epoch, err := NewProcessObservationEpoch()
	if err != nil {
		t.Fatal(err)
	}
	cells, err := NewCells(epoch)
	if err != nil {
		t.Fatal(err)
	}
	codec, err := NewCursorCodec(epoch)
	if err != nil {
		t.Fatal(err)
	}
	return epoch, cells, codec
}

func nha056Binding(t *testing.T, value byte) ReadBinding {
	t.Helper()
	var digest [32]byte
	digest[0] = value
	binding, err := NewReadBinding(1, digest)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func nha056Event(t *testing.T) Event {
	t.Helper()
	event, err := NewEvent(obs.LogCode_LOG_CODE_PROCESS_READY, obs.LogLevel_LOG_LEVEL_INFO,
		obs.LogComponent_LOG_COMPONENT_PROCESS, time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC), NewProcessAuthor())
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func TestNHA056DomainAdmissionAndSafeFormatting(t *testing.T) {
	if _, err := NewCells(ProcessObservationEpoch{}); err == nil {
		t.Fatal("zero epoch admitted")
	}
	if _, err := NewReadBinding(1, [32]byte{}); err == nil {
		t.Fatal("zero binding admitted")
	}
	if _, err := NewEvent(0, 0, 0, time.Time{}, Author{}); err == nil {
		t.Fatal("zero event admitted")
	}
	account, _ := contracts.ParseAccountID(strings.Repeat("x", 129))
	if _, err := NewHumanAuthor(account); err == nil {
		t.Fatal("oversized author admitted")
	}
	_, _, codec := nha056Fixture(t)
	for _, rendered := range []string{fmt.Sprintf("%v", codec), fmt.Sprintf("%+v", codec), fmt.Sprintf("%#v", codec)} {
		if rendered != "observability: cursor codec" {
			t.Fatalf("unsafe codec formatting %q", rendered)
		}
	}
	for _, value := range []any{ReadBinding{}, ControllerHandle{}, CaptureReservation{}, CaptureHandle{}} {
		if rendered := fmt.Sprintf("%+v", value); !strings.HasPrefix(rendered, "observability: ") {
			t.Fatalf("unsafe opaque value rendering %q", rendered)
		}
	}
	if (&Error{}).Code() != obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_INVALID_ARGUMENT || (&Error{}).Wire().Code == obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_UNSPECIFIED {
		t.Fatal("zero error became an operational unknown")
	}
}

func TestNHA056HealthDoesNotInventReadyGeneration(t *testing.T) {
	epoch, _, _ := nha056Fixture(t)
	health, err := NewHealthCell(epoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := health.PublishReady(ActiveRevision{}); err == nil {
		t.Fatal("READY without generation")
	}
	snapshot, err := health.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	server, err := snapshot.Server()
	if err != nil {
		t.Fatal(err)
	}
	if server.GetReadiness() != obs.ServerReadiness_SERVER_READINESS_STARTING || server.GetGeneration().GetNotLoaded() == nil {
		t.Fatal("initial health invents a loaded generation")
	}
	revision, _ := NewActiveRevision(7)
	if err := health.PublishReady(revision); err != nil {
		t.Fatal(err)
	}
	if err := health.PublishDurableFact(obs.DurableAvailability_DURABLE_AVAILABILITY_UNAVAILABLE, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	snapshot, err = health.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	server, err = snapshot.Server()
	if err != nil {
		t.Fatal(err)
	}
	if server.Readiness != obs.ServerReadiness_SERVER_READINESS_READY {
		t.Fatal("durable fact closed volatile readiness")
	}
	if err := health.PublishDraining(); err != nil {
		t.Fatal(err)
	}
	if err := health.PublishReady(revision); err == nil {
		t.Fatal("draining reopened")
	}
}

func TestNHA056HealthFactsAreAdmittedAndImmutable(t *testing.T) {
	var zero HealthFacts
	if _, err := zero.Epoch(); err == nil {
		t.Fatal("zero facts epoch admitted")
	}
	if _, err := zero.Server(); err == nil {
		t.Fatal("zero facts server admitted")
	}
	if _, err := zero.Durable(); err == nil {
		t.Fatal("zero facts durable admitted")
	}
	epoch, _, _ := nha056Fixture(t)
	health, err := NewHealthCell(epoch)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := health.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	gotEpoch, err := facts.Epoch()
	if err != nil || gotEpoch != epoch {
		t.Fatal("facts lost the process identity", err)
	}
	server, err := facts.Server()
	if err != nil || contracts.AdmitObservabilityValue(server) != nil {
		t.Fatal("server facts not admitted", err)
	}
	durable, err := facts.Durable()
	if err != nil || durable.Availability != obs.DurableAvailability_DURABLE_AVAILABILITY_UNKNOWN || durable.GetNotObserved() == nil {
		t.Fatal("durable absence invented", err)
	}
	server.Readiness = obs.ServerReadiness_SERVER_READINESS_UNSPECIFIED
	server.Generation = nil
	durable.Availability = obs.DurableAvailability_DURABLE_AVAILABILITY_AVAILABLE
	durable.Evidence = nil
	server, err = facts.Server()
	if err != nil || server.Readiness != obs.ServerReadiness_SERVER_READINESS_STARTING || server.Generation.GetNotLoaded() == nil {
		t.Fatal("server accessor aliases its facts", err)
	}
	durable, err = facts.Durable()
	if err != nil || durable.Availability != obs.DurableAvailability_DURABLE_AVAILABILITY_UNKNOWN || durable.GetNotObserved() == nil {
		t.Fatal("durable accessor aliases its facts", err)
	}
	revision, _ := NewActiveRevision(7)
	if err := health.PublishReady(revision); err != nil {
		t.Fatal(err)
	}
	server, err = facts.Server()
	if err != nil || server.Readiness != obs.ServerReadiness_SERVER_READINESS_STARTING {
		t.Fatal("snapshot aliases later publication", err)
	}
	invalidFacts := facts
	invalidFacts.server = &obs.ServerHealth{}
	if _, err := invalidFacts.Durable(); err == nil {
		t.Fatal("partially invalid facts admitted")
	}
}

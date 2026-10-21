package integrations_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/damione1/Homeward/gen/home/integrations/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/observability"
	"google.golang.org/protobuf/encoding/protojson"
)

const nha056CompositionPanicCanary = "NHA056_PROVIDER_ATTACH_PANIC_CANARY"

type nha056CompositionPanicPilot struct{ starts atomic.Int32 }

func (*nha056CompositionPanicPilot) AttachObservation(*observability.Cells, *observability.Journal) error {
	panic(nha056CompositionPanicCanary)
}
func (p *nha056CompositionPanicPilot) Start(context.Context, integrations.SessionHost, integrations.Tasks) error {
	p.starts.Add(1)
	return nil
}
func (*nha056CompositionPanicPilot) Stop(context.Context) error { return nil }

func TestNHA056SupervisorCompositionPanicIsContainedInChild(t *testing.T) {
	const childFlag = "HOMEWARD_NHA056_ATTACH_PANIC_CHILD"
	const completed = "NHA056_COMPOSITION_PANIC_CONTAINED"
	if os.Getenv(childFlag) != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal("test executable unavailable")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, executable, "-test.run=^TestNHA056SupervisorCompositionPanicIsContainedInChild$", "-test.v")
		command.Env = append(os.Environ(), childFlag+"=1")
		command.WaitDelay = 2 * time.Second
		output, err := command.CombinedOutput()
		// Never echo failed child output: the regression itself could contain
		// a provider-controlled panic and stack from the supervisor goroutine.
		if err != nil {
			t.Fatal("composition panic child did not exit successfully")
		}
		text := string(output)
		if !strings.Contains(text, completed) || strings.Contains(text, nha056CompositionPanicCanary) || strings.Contains(text, "panic:") || strings.Contains(text, "\ngoroutine ") {
			t.Fatal("composition panic escaped containment or lacked a positive completion witness")
		}
		return
	}

	host, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	cells, err := host.ObservationCells()
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := cells.Epoch()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := observability.NewJournal(epoch, cells)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.AttachObservation(cells, journal); err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha056-composition-panic")
	if err != nil {
		t.Fatal(err)
	}
	pilot := &nha056CompositionPanicPilot{}
	spec, err := integrations.NewPilotSpec(instance, func() integrations.Pilot { return pilot })
	if err != nil {
		t.Fatal(err)
	}
	policy, err := integrations.NewRetryPolicy(0, 20*time.Millisecond, time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := integrations.NewSupervisor(host, []integrations.PilotSpec{spec}, policy, integrations.RealTimer{}, integrations.RandomJitter{})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := supervisor.Stop(ctx); err != nil {
			t.Error("supervisor cleanup did not join")
		}
	})
	status := nha026AwaitPhase(t, supervisor, instance, integrations.PilotDegraded)
	if status.Tasks != 0 || status.Attempts != 1 || pilot.starts.Load() != 0 || !host.AdmissionsOpen() {
		t.Fatal("composition panic did not degrade before pilot work while retaining the host")
	}
	health, err := fixture.Sequencer().IntegrationHealth(instance)
	if err != nil || health.GetPresent().GetStatus() != wire.HealthStatus_HEALTH_STATUS_DEGRADED {
		t.Fatal("real supervisor degradation did not reach the health projection")
	}
	snapshot, err := journal.Snapshot()
	if err != nil || snapshot.Through() == 0 {
		t.Fatal("supervisor log witness absent")
	}
	codec, err := observability.NewCursorCodec(epoch)
	if err != nil {
		t.Fatal(err)
	}
	var digest [32]byte
	digest[0] = 1
	binding, err := observability.NewReadBinding(1, digest)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := codec.BeginLogs(binding, snapshot.Through())
	if err != nil {
		t.Fatal(err)
	}
	size, err := observability.NewPageSize(100)
	if err != nil {
		t.Fatal(err)
	}
	request, err := observability.NewLogPageRequest(size, cursor)
	if err != nil {
		t.Fatal(err)
	}
	page, err := journal.Read(request)
	if err != nil {
		t.Fatal(err)
	}
	response, err := page.Wire("")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range response.GetPage().GetRows() {
		event := row.GetEvent()
		if event.GetCode() == obs.LogCode_LOG_CODE_PILOT_FACTORY_FAILED && event.GetComponent() == obs.LogComponent_LOG_COMPONENT_SUPERVISOR && event.GetOccurredAt() != nil && event.GetAuthor().GetProcess() != nil {
			found = true
		}
	}
	encoded, err := protojson.Marshal(response)
	if err != nil || !found || strings.Contains(string(encoded), nha056CompositionPanicCanary) {
		t.Fatal("closed factory failure event was absent or exposed provider panic data")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := supervisor.Stop(ctx); err != nil {
		t.Fatal("supervisor did not stop after controlled degradation")
	}
	select {
	case <-supervisor.Done():
	default:
		t.Fatal("supervisor work did not join")
	}
	fmt.Fprintln(os.Stdout, completed)
}

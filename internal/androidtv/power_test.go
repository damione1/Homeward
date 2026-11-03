package androidtv

import (
	"bufio"
	"context"
	"crypto/tls"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/storage"
)

type capturedTasks struct{ tasks []integrations.Task }

func (t *capturedTasks) Go(task integrations.Task) error { t.tasks = append(t.tasks, task); return nil }

type powerHost struct {
	*integrations.Session
	value  storage.SecretValue
	health chan struct{}
}

func (h *powerHost) Secrets() (integrations.SecretProvider, error) { return h, nil }
func (h *powerHost) Read(context.Context, storage.SecretHandle) (storage.SecretValue, error) {
	return h.value, nil
}
func (h *powerHost) ReportHealth(ctx context.Context, report integrations.HealthReport) error {
	if e := h.Session.ReportHealth(ctx, report); e != nil {
		return e
	}
	select {
	case h.health <- struct{}{}:
	default:
	}
	return nil
}

// Exercise the composed driver through a real Host, capability catalogue and
// sequencer; only its pinned network transport is replaced by an in-memory pipe.
func TestCorePowerIsObservedReadOnlyAndInvalidatedOnDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, e := NewRuntime()
	if e != nil {
		t.Fatal(e)
	}
	settings, e := ParseSettings(Input{Instance: "demo-tv", Players: []PlayerInput{testPlayer()}})
	if e != nil {
		t.Fatal(e)
	}
	instance, _ := settings.Instance()
	manager, fixture, e := fake.NewFixture()
	if e != nil {
		t.Fatal(e)
	}
	session, e := manager.OpenSession(instance)
	if e != nil {
		t.Fatal(e)
	}
	v, _ := testCredential(t, "atvremote/test/"+testMAC, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), nil)
	host := &powerHost{Session: session, value: v, health: make(chan struct{}, 8)}
	sendPing := make(chan struct{})
	drop := make(chan struct{})
	serverErrors := make(chan error, 1)
	var dials atomic.Int32
	r.dial = func(context.Context, string, *tls.Config) (net.Conn, error) {
		if dials.Add(1) > 1 {
			return nil, ErrUnavailable
		}
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			reader := bufio.NewReader(server)
			if e := writeFrame(server, message(1, varint(1, 1023))); e != nil {
				serverErrors <- e
				return
			}
			if _, e := readFrame(reader); e != nil {
				serverErrors <- e
				return
			}
			if e := writeFrame(server, message(40, nil)); e != nil {
				serverErrors <- e
				return
			} // explicitly observed false
			select {
			case <-sendPing:
			case <-ctx.Done():
				return
			}
			if e := writeFrame(server, message(8, varint(1, 42))); e != nil {
				serverErrors <- e
				return
			}
			if _, e := readFrame(reader); e != nil {
				serverErrors <- e
				return
			}
			select {
			case <-drop:
			case <-ctx.Done():
			}
		}()
		return client, nil
	}
	p := &pilot{settings: settings, runtime: r}
	tasks := &capturedTasks{}
	if e = p.Start(ctx, host, tasks); e != nil {
		t.Fatal(e)
	}
	defer p.Stop(context.Background())
	if len(tasks.tasks) != 1 {
		t.Fatal("expected one task for the shared media/power binding")
	}
	identities, e := fixture.Resolve(ctx, instance, []integrations.SourceKey{testPlayer().SourceKey()})
	if e != nil || len(identities) != 1 {
		t.Fatal(e)
	}
	id := identities[0].Entity
	idText, _ := id.Value()
	descriptions, e := manager.Capabilities(id)
	if e != nil || len(descriptions) != 2 {
		t.Fatalf("expected exactly two capabilities: %v", e)
	}
	catalog, e := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if e != nil {
		t.Fatal(e)
	}
	powerFound, mediaFound := false, false
	for _, description := range descriptions {
		if e = catalog.RegisterDriverCapability(id, description); e != nil {
			t.Fatal(e)
		}
		switch description.SchemaId {
		case contracts.SwitchSchema:
			powerFound = true
			if description.Endpoint != PowerEndpoint || description.GetSwitch() == nil || description.GetSwitch().SetOn {
				t.Fatal("power control advertised")
			}
			if e = catalog.AdmitReadOnlyDriverCapability(description); e != nil {
				t.Fatal(e)
			}
		case Schema:
			mediaFound = true
		}
	}
	if !powerFound || !mediaFound {
		t.Fatal("missing power or media capability")
	}
	for _, on := range []bool{false, true} {
		command := &actions.CommandRequest{CommandId: "synthetic", AdmissionEpoch: "epoch-a", ConfigRevision: 1, Target: &registry.EntityRef{Reference: &registry.EntityRef_EntityId{EntityId: idText}}, CapabilitySchemaId: contracts.SwitchSchema, Endpoint: PowerEndpoint, Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: on}}}}
		if rejected := catalog.AdmitCapabilityCommand(command, "epoch-a", 1); rejected == nil || rejected.Code != actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT || rejected.FieldPath != "arguments" {
			t.Fatalf("core power action was not rejected by its read-only constraints: %v", rejected)
		}
	}
	if _, actionable := any(p).(integrations.CommandDriver); actionable {
		t.Fatal("read-only driver implements commands")
	}
	done := make(chan error, 1)
	go func() { done <- tasks.tasks[0](ctx) }()
	defer func() {
		cancel()
		select {
		case e := <-done:
			if e != nil {
				t.Error(e)
			}
		case <-time.After(time.Second):
			t.Error("driver did not stop")
		}
	}()
	waitState := func(availability domain.Availability, freshness domain.Freshness) domain.ObservedState {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if state, ok := fixture.State(id); ok {
				_, quality, _, _, _, e := state.Components()
				if e != nil {
					t.Fatal(e)
				}
				a, f, p, _ := quality.Components()
				if a == availability && f == freshness && p == domain.ProvenanceObserved {
					return state
				}
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("expected core power quality missing")
		return domain.ObservedState{}
	}
	off := waitState(domain.AvailabilityAvailable, domain.FreshnessFresh)
	value, present := off.CurrentValue()
	on, valid := value.Switch()
	if !present || !valid || on {
		t.Fatal("off observation missing or fabricated on")
	}
	_, _, times, _, revision, e := off.Components()
	if e != nil || times.ObservedAt == nil {
		t.Fatal("power lacks an actual observation timestamp")
	}
	select {
	case <-host.health:
	case <-time.After(time.Second):
		t.Fatal("start processing missing")
	}
	close(sendPing)
	select {
	case <-host.health:
	case <-time.After(time.Second):
		t.Fatal("ping processing missing")
	}
	afterPing, ok := fixture.State(id)
	if !ok {
		t.Fatal("power disappeared on heartbeat")
	}
	_, _, pingTimes, _, pingRevision, e := afterPing.Components()
	if e != nil || revision != pingRevision || !times.LastReported.Equal(pingTimes.LastReported) {
		t.Fatal("heartbeat published a fabricated power observation")
	}
	close(drop)
	stale := waitState(domain.AvailabilityUnavailable, domain.FreshnessStale)
	if _, present = stale.CurrentValue(); present {
		t.Fatal("disconnected power remained a current value")
	}
	if _, ok = r.Snapshot(id); ok {
		t.Fatal("disconnected media remained fresh")
	}
	select {
	case e := <-serverErrors:
		t.Fatal(e)
	default:
	}
}

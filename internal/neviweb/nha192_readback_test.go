package neviweb

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	wire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func nha192Commands(t *testing.T, rig nha091SessionRig) (*integrations.VolatileCommandService, domain.ActorID, func(string, string, *actions.ActionArgs) *actions.CommandRequest) {
	t.Helper()
	ctx := context.Background()
	snapshot := cycleSnapshot{frames: make(map[string]attributeFrame)}
	for _, binding := range rig.settings.bindings {
		if binding.kind != BindingClimate && binding.kind != BindingSwitch {
			continue
		}
		rig.pilot.listed[binding.deviceID] = true
		body := `{"roomTemperature":{"value":20},"roomSetpoint":22,"roomSetpointMin":5,"roomSetpointMax":30,"systemMode":"heat"}`
		signature := []byte(`{"model":1123}`)
		if binding.kind == BindingSwitch {
			body, signature = `{"onOff":"off"}`, []byte(`{"model":2151}`)
		}
		rig.pilot.signatures[binding.deviceID] = signature
		frame, err := frameFor(binding.kind, []byte(body), signature)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.frames[binding.alias] = frame
	}
	if err := rig.pilot.discover(ctx, rig.session, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := rig.pilot.publish(ctx, rig.session, snapshot); err != nil {
		t.Fatal(err)
	}
	rig.pilot.adoptSession(sessionState{phase: sessionActive, id: "fixture-session", account: "123"})
	if err := rig.host.AttachCommandDriver(rig.session, rig.pilot); err != nil {
		t.Fatal(err)
	}
	service, err := integrations.NewVolatileCommandService(rig.host, nha091Generation{}, integrations.DefaultVolatileCommandLimits())
	if err != nil {
		t.Fatal(err)
	}
	actor, err := domain.ParseActorID("nha192-fixture")
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := rig.sequence.CurrentAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	return service, actor, func(alias, id string, args *actions.ActionArgs) *actions.CommandRequest {
		t.Helper()
		_, entity, err := rig.pilot.bindings[alias].IDs()
		if err != nil {
			t.Fatal(err)
		}
		entityID, err := entity.Value()
		if err != nil {
			t.Fatal(err)
		}
		schema, endpoint := contracts.ClimateSchema, "climate"
		if args.GetSetSwitch() != nil {
			schema, endpoint = contracts.SwitchSchema, "relay"
		}
		return &actions.CommandRequest{CommandId: id, AdmissionEpoch: epoch, ConfigRevision: 7, Target: &wire.EntityRef{Reference: &wire.EntityRef_EntityId{EntityId: entityID}}, CapabilitySchemaId: schema, Endpoint: endpoint, Arguments: args, ExpiresAt: timestamppb.New(time.Now().Add(time.Minute))}
	}
}

func nha192ClimateArgs() *actions.ActionArgs {
	setpoint := 23.0
	mode := wire.ClimateMode_CLIMATE_MODE_OFF
	return &actions.ActionArgs{Arguments: &actions.ActionArgs_SetClimate{SetClimate: &actions.SetClimate{SetpointCelsius: &setpoint, Mode: &mode}}}
}

func nha192Status(t *testing.T, rig nha091SessionRig, request *actions.CommandRequest) actions.ActionStatus {
	t.Helper()
	id, err := domain.ParseCommandID(request.CommandId)
	if err != nil {
		t.Fatal(err)
	}
	status, err := rig.host.ActionStatus(id)
	if err != nil {
		t.Fatal(err)
	}
	return status.Status
}

func TestNHA192TargetedReadbackRequiresActualMatchingAttributes(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     actions.ActionStatus
		wantError  bool
		wantTarget float64
	}{
		{"confirmed", `{"roomTemperature":{"value":21.25},"roomSetpoint":23,"roomSetpointMin":5,"roomSetpointMax":30,"systemMode":"off"}`, actions.ActionStatus_ACTION_STATUS_CONFIRMED, false, 23},
		{"delayed", `{"roomTemperature":{"value":21.25},"roomSetpoint":22,"roomSetpointMin":5,"roomSetpointMax":30,"systemMode":"heat"}`, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED, false, 22},
		{"missing requested mode", `{"roomTemperature":{"value":21.25},"roomSetpoint":23,"roomSetpointMin":5,"roomSetpointMax":30}`, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED, false, 23},
		{"malformed", `{"roomTemperature":{"value":"broken"},"roomSetpoint":23,"systemMode":"off"}`, actions.ActionStatus_ACTION_STATUS_INDETERMINATE, true, 23},
		{"device unavailable", `{"error":{"code":"DVCUNVLB"}}`, actions.ActionStatus_ACTION_STATUS_INDETERMINATE, true, 0},
		{"provider quota", `{"error":{"code":"ACCDAYREQMAX"}}`, actions.ActionStatus_ACTION_STATUS_INDETERMINATE, true, 0},
		{"session expired", `{"error":{"code":"USRSESSEXP"}}`, actions.ActionStatus_ACTION_STATUS_INDETERMINATE, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var writes, reads, unexpected atomic.Int32
			rig := newNHA091SessionRig(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/device/device-00/attribute" || r.Header.Get("Session-Id") != "fixture-session" {
					unexpected.Add(1)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				switch r.Method {
				case http.MethodPut:
					writes.Add(1)
					_, _ = w.Write([]byte(`{}`))
				case http.MethodGet:
					if writes.Load() != 1 || r.URL.Query().Get("attributes") != requestedAttributes(BindingClimate, modelInfo{family: familyZigbee}) {
						unexpected.Add(1)
					}
					reads.Add(1)
					_, _ = w.Write([]byte(tc.body))
				default:
					unexpected.Add(1)
					w.WriteHeader(http.StatusBadRequest)
				}
			})
			service, actor, request := nha192Commands(t, rig)
			_, neighbour, _ := rig.pilot.bindings[retainedAliases[1]].IDs()
			before, _, err := rig.sequence.State(neighbour)
			if err != nil {
				t.Fatal(err)
			}
			command := request(retainedAliases[0], "nha192-readback", nha192ClimateArgs())
			response, err := service.Dispatch(context.Background(), actor, command)
			if (err != nil) != tc.wantError || response == nil || nha192Status(t, rig, command) != tc.status {
				t.Fatalf("readback result=%v err=%v live=%v", response, err, nha192Status(t, rig, command))
			}
			_, entity, _ := rig.pilot.bindings[retainedAliases[0]].IDs()
			state, present, err := rig.sequence.State(entity)
			if err != nil || !present {
				t.Fatalf("target state missing: %v", err)
			}
			value, current := state.CurrentValue()
			if tc.wantTarget == 0 {
				if current {
					t.Fatal("unavailable target retained a current reading")
				}
			} else {
				climate, ok := value.Climate()
				parts, err := climate.Components()
				if !current || !ok || err != nil || parts.Setpoint == nil {
					t.Fatalf("observed climate absent: %v", err)
				}
				temperature, single := parts.Setpoint.Single()
				if !single {
					t.Fatal("readback setpoint was not single")
				}
				target, err := temperature.Value()
				if err != nil || target != tc.wantTarget {
					t.Fatalf("target=%v, wanted actual readback %v", target, tc.wantTarget)
				}
			}
			_, _ = service.Dispatch(context.Background(), actor, command)
			if writes.Load() != 1 || reads.Load() != 1 || unexpected.Load() != 0 {
				t.Fatalf("write/read/other=%d/%d/%d: replay, global poll or reconnect", writes.Load(), reads.Load(), unexpected.Load())
			}
			after, _, err := rig.sequence.State(neighbour)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("targeted observation changed another device")
			}
			count, err := rig.quota.Today(context.Background())
			if err != nil || count != 2 || rig.settings.PollInterval() != 540*time.Second {
				t.Fatalf("local attempts=%d err=%v or cadence changed", count, err)
			}
		})
	}
}

func TestNHA192CancellationDuringReadbackNeverConfirmsOrReplays(t *testing.T) {
	var writes, reads atomic.Int32
	entered := make(chan struct{})
	rig := newNHA091SessionRig(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			writes.Add(1)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		reads.Add(1)
		close(entered)
		<-r.Context().Done()
	})
	service, actor, request := nha192Commands(t, rig)
	command := request(retainedAliases[0], "nha192-cancel-readback", nha192ClimateArgs())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := service.Dispatch(ctx, actor, command); finished <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("readback did not enter")
	}
	if nha192Status(t, rig, command) != actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED {
		t.Fatal("ACK alone confirmed the action")
	}
	cancel()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("readback cancellation ignored")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readback did not stop")
	}
	_, _ = service.Dispatch(context.Background(), actor, command)
	if nha192Status(t, rig, command) != actions.ActionStatus_ACTION_STATUS_INDETERMINATE || writes.Load() != 1 || reads.Load() != 1 {
		t.Fatal("cancelled readback confirmed or replayed")
	}
}

func TestNHA192OperationWaitIsCancellableWithoutLaterTransport(t *testing.T) {
	var attempts atomic.Int32
	rig := newNHA091SessionRig(t, func(w http.ResponseWriter, r *http.Request) { attempts.Add(1); _, _ = w.Write([]byte(`{}`)) })
	service, actor, request := nha192Commands(t, rig)
	if err := rig.pilot.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	command := request(retainedAliases[0], "nha192-cancel-wait", nha192ClimateArgs())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := service.Dispatch(ctx, actor, command)
	rig.pilot.release()
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) {
		t.Fatalf("operation wait=%v", err)
	}
	_, _ = service.Dispatch(context.Background(), actor, command)
	if attempts.Load() != 0 {
		t.Fatal("cancelled operation waited and later emitted transport")
	}
}

func TestNHA192RelayRequiresObservedReadback(t *testing.T) {
	var writes, reads atomic.Int32
	rig := newNHA091SessionRig(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/device/device-08/attribute" {
			t.Error("relay refresh queried another device")
		}
		if r.Method == http.MethodPut {
			writes.Add(1)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		reads.Add(1)
		if r.URL.Query().Get("attributes") != "onOff" {
			t.Error("relay refresh did not use model-qualified attributes")
		}
		_, _ = w.Write([]byte(`{"onOff":"on"}`))
	})
	service, actor, request := nha192Commands(t, rig)
	command := request(retainedAliases[8], "nha192-relay", &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: true}}})
	if _, err := service.Dispatch(context.Background(), actor, command); err != nil {
		t.Fatal(err)
	}
	if nha192Status(t, rig, command) != actions.ActionStatus_ACTION_STATUS_CONFIRMED || writes.Load() != 1 || reads.Load() != 1 {
		t.Fatal("relay was not confirmed by one observed readback")
	}
}

func TestNHA192PollCannotOverwriteCommandWithAnOlderRead(t *testing.T) {
	entered, resume := make(chan struct{}), make(chan struct{})
	var targetReads, writes atomic.Int32
	rig := newNHA091SessionRig(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			writes.Add(1)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		switch {
		case r.URL.Path == "/api/device/device-00/attribute":
			if targetReads.Add(1) == 1 {
				close(entered)
				select {
				case <-resume:
				case <-r.Context().Done():
					return
				}
				_, _ = w.Write([]byte(`{"roomSetpoint":22,"roomSetpointMin":5,"roomSetpointMax":30,"systemMode":"heat"}`))
			} else {
				_, _ = w.Write([]byte(`{"roomSetpoint":23,"roomSetpointMin":5,"roomSetpointMax":30,"systemMode":"off"}`))
			}
		case r.URL.Path == "/api/device/device-08/attribute":
			_, _ = w.Write([]byte(`{"onOff":"off"}`))
		case strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"status":"online"}`))
		case strings.HasSuffix(r.URL.Path, "/notifications"):
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`{"roomSetpoint":22,"roomSetpointMin":5,"roomSetpointMax":30,"systemMode":"heat"}`))
		}
	})
	service, actor, request := nha192Commands(t, rig)
	value, err := activeSessionValue("fixture-session", "", "123")
	if err != nil || rig.secrets.RotateCredential(context.Background(), rig.settings.session, value) != nil {
		t.Fatal("fixture encrypted session")
	}
	for _, binding := range rig.settings.bindings {
		rig.pilot.listed[binding.deviceID] = true
	}
	rig.pilot.networkAt = time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	polled := make(chan error, 1)
	go func() { polled <- rig.pilot.poll(ctx, rig.session) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("normal poll did not enter its old read")
	}
	command := request(retainedAliases[0], "nha192-poll-order", nha192ClimateArgs())
	finished := make(chan error, 1)
	go func() { _, err := service.Dispatch(ctx, actor, command); finished <- err }()
	select {
	case err := <-finished:
		t.Fatalf("command escaped the poll fence: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if writes.Load() != 0 {
		t.Fatal("PUT raced the previous poll publication")
	}
	close(resume)
	if err := <-polled; err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	_, entity, _ := rig.pilot.bindings[retainedAliases[0]].IDs()
	state, _, err := rig.sequence.State(entity)
	if err != nil {
		t.Fatal(err)
	}
	current, _ := state.CurrentValue()
	climate, _ := current.Climate()
	parts, err := climate.Components()
	if err != nil || parts.Setpoint == nil {
		t.Fatal("latest readback absent")
	}
	temperature, _ := parts.Setpoint.Single()
	target, _ := temperature.Value()
	if target != 23 || nha192Status(t, rig, command) != actions.ActionStatus_ACTION_STATUS_CONFIRMED || writes.Load() != 1 || targetReads.Load() != 2 {
		t.Fatal("old poll overwrote the confirmed targeted readback")
	}
}

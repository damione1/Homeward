package wled

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type transport struct {
	mu     sync.Mutex
	d      document
	writes int
	bodies []map[string]any
	mode   string
}

func (f *transport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Context().Err() != nil {
		return nil, r.Context().Err()
	}
	var out []byte
	switch r.URL.Path {
	case "/presets.json":
		out = []byte(`{"2":{"n":"Evening"}}`)
	case "/json":
		if f.mode == "wrong-identity" {
			f.d.Info.MAC = "112233445566"
		}
		out, _ = json.Marshal(f.d)
	case "/json/state":
		if r.Method != "POST" || r.GetBody != nil {
			return nil, ErrUnavailable
		}
		f.writes++
		if f.mode == "write-failure" {
			return nil, io.ErrUnexpectedEOF
		}
		type change struct {
			On         *bool      `json:"on"`
			Brightness *uint32    `json:"bri"`
			Preset     *int       `json:"ps"`
			Colors     [][]uint32 `json:"col"`
		}
		var a struct {
			change
			Segments []struct {
				change
				ID uint32 `json:"id"`
			} `json:"seg"`
		}
		raw, readErr := io.ReadAll(r.Body)
		var body map[string]any
		if readErr != nil || json.Unmarshal(raw, &body) != nil || json.Unmarshal(raw, &a) != nil {
			return nil, ErrUnavailable
		}
		f.bodies = append(f.bodies, body)
		if f.mode != "ack-only" {
			apply := func(change change, on **bool, bri **uint32) {
				if change.Brightness != nil {
					q := *change.Brightness
					*on = proto.Bool(q > 0)
					if q > 0 {
						*bri = proto.Uint32(q)
					}
				}
				if change.On != nil {
					*on = proto.Bool(*change.On)
				}
			}
			apply(a.change, &f.d.State.On, &f.d.State.Brightness)
			for _, update := range a.Segments {
				for i := range f.d.State.Segments {
					seg := &f.d.State.Segments[i]
					if *seg.ID == update.ID {
						apply(update.change, &seg.On, &seg.Brightness)
						if update.Colors != nil {
							seg.Colors = update.Colors
						}
					}
				}
			}
			if a.Preset != nil {
				f.d.State.Preset = a.Preset
			}
		}

		out = []byte(`{"success":true}`)
	default:
		return nil, ErrUnavailable
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(out)))}, nil
}

type generation struct{}

func (generation) WithCurrent(ctx context.Context, f func(uint64) error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return f(7)
}

type rig struct {
	p         *pilot
	c         *controller
	transport *transport
	host      *integrations.Manager
	service   *integrations.VolatileCommandService
	actor     domain.ActorID
	request   *actions.CommandRequest
}

func newRig(t *testing.T) rig { return newRouteRig(t, nil) }
func newRouteRig(t *testing.T, segmentID *uint32) rig {
	return newRoutesRig(t, []RouteInput{{Alias: "light.demo", Label: "Demo", Segment: segmentID}})
}
func newRoutesRig(t *testing.T, routes []RouteInput) rig {
	t.Helper()
	host, fixture, e := fake.NewCommandFixture()
	if e != nil {
		t.Fatal(e)
	}
	settings, e := ParseSettings(Input{Instance: "demo-wled", Controllers: []ControllerInput{{Address: "192.168.50.20", MAC: "aabbccddeeff", Routes: routes}}})
	if e != nil {
		t.Fatal(e)
	}
	instance, _ := settings.Instance()
	input := settings.Controllers()[0]
	keys := make([]integrations.SourceKey, len(routes))
	for i, r := range routes {
		keys[i] = r.SourceKey(input.MAC)
	}
	resolved, e := fixture.Resolve(context.Background(), instance, keys)
	if e != nil {
		t.Fatal(e)
	}
	records := make([]integrations.RegistryRecord, len(routes))
	for i, r := range routes {
		alias, _ := domain.ParseHAAlias(r.Alias)
		records[i], e = integrations.NewRegistryRecord(resolved[i].Device, resolved[i].Entity, instance, keys[i], alias, alias, []domain.HAAlias{alias}, false)
		if e != nil {
			t.Fatal(e)
		}
	}
	if e = fixture.PublishRegistry(context.Background(), records); e != nil {
		t.Fatal(e)
	}
	session, e := host.OpenSession(instance)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f := &transport{d: doc(t)}
	f.d.State.Brightness = proto.Uint32(200)
	c := &controller{input: input, gate: make(chan struct{}, 1), routes: map[domain.EntityID]route{}}
	p := &pilot{settings: settings, ctx: ctx, cancel: cancel, host: session, client: &client{http: &http.Client{Transport: f}}, controllers: []*controller{c}}
	if e = host.AttachCommandDriver(session, p); e != nil {
		t.Fatal(e)
	}
	p.refresh(c)
	if len(c.routes) != len(routes) {
		t.Fatal("no admitted native route")
	}
	service, e := integrations.NewVolatileCommandService(host, generation{}, integrations.DefaultVolatileCommandLimits())
	if e != nil {
		t.Fatal(e)
	}
	actor, _ := domain.ParseActorID("wled-test")
	epoch, _ := fixture.CurrentAdmissionEpoch()
	entity, _ := resolved[0].Entity.Value()
	request := &actions.CommandRequest{CommandId: "wled-test-command", AdmissionEpoch: epoch, ConfigRevision: 7, Target: &registry.EntityRef{Reference: &registry.EntityRef_EntityId{EntityId: entity}}, CapabilitySchemaId: "core.light.v1", Endpoint: c.routes[resolved[0].Entity].description.Endpoint, ExpiresAt: timestamppb.New(time.Now().Add(time.Second)), Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetLight{SetLight: &actions.SetLight{BrightnessPercent: proto.Float64(50)}}}}
	return rig{p, c, f, host, service, actor, request}
}
func TestNHA083HostLedgerQuantizedReadbackAndNoReplay(t *testing.T) {
	r := newRig(t)
	if _, e := r.service.Dispatch(context.Background(), r.actor, r.request); e != nil {
		t.Fatal(e)
	}
	id, _ := domain.ParseCommandID(r.request.CommandId)
	status, e := r.host.ActionStatus(id)
	if e != nil || status.Status != actions.ActionStatus_ACTION_STATUS_CONFIRMED {
		t.Fatalf("status %v %v", status, e)
	}
	if _, e = r.service.Dispatch(context.Background(), r.actor, r.request); e != nil {
		t.Fatal(e)
	}
	r.p.refresh(r.c)
	if r.transport.writes != 1 {
		t.Fatal("replayed physical mutation")
	}
}
func TestNHA083ACKAndIdentityNeverInventConfirmation(t *testing.T) {
	for _, mode := range []string{"ack-only", "write-failure", "wrong-identity"} {
		t.Run(mode, func(t *testing.T) {
			r := newRig(t)
			r.transport.mode = mode
			r.request.Arguments.GetSetLight().BrightnessPercent = proto.Float64(10)
			r.request.ExpiresAt = timestamppb.New(time.Now().Add(60 * time.Millisecond))
			_, _ = r.service.Dispatch(context.Background(), r.actor, r.request)
			id, _ := domain.ParseCommandID(r.request.CommandId)
			status, e := r.host.ActionStatus(id)
			if e != nil {
				t.Fatal(e)
			}
			if status.Status == actions.ActionStatus_ACTION_STATUS_CONFIRMED {
				t.Fatal("false physical confirmation")
			}
			_, _ = r.service.Dispatch(context.Background(), r.actor, r.request)
			want := 1
			if mode == "wrong-identity" {
				want = 0
			}
			if r.transport.writes != want {
				t.Fatalf("writes %d want %d", r.transport.writes, want)
			}
		})
	}
}

func TestNHA083SegmentOffACKCannotBorrowMasterOff(t *testing.T) {
	r := newRouteRig(t, proto.Uint32(0))
	r.transport.d.State.On = proto.Bool(false)
	r.transport.d.State.Segments[0].On = proto.Bool(true)
	r.transport.mode = "ack-only"
	r.request.Arguments.GetSetLight().BrightnessPercent = nil
	r.request.Arguments.GetSetLight().On = proto.Bool(false)
	r.request.ExpiresAt = timestamppb.New(time.Now().Add(60 * time.Millisecond))
	_, _ = r.service.Dispatch(context.Background(), r.actor, r.request)
	id, _ := domain.ParseCommandID(r.request.CommandId)
	status, e := r.host.ActionStatus(id)
	if e != nil || status.Status == actions.ActionStatus_ACTION_STATUS_CONFIRMED {
		t.Fatalf("ACK borrowed master OFF: %v %v", status, e)
	}
	if r.transport.writes != 1 {
		t.Fatal("wrong number of writes")
	}
}
func TestNHA083ZeroBrightnessConfirmsNativeOffWithRetainedLevel(t *testing.T) {
	for _, segmentID := range []*uint32{nil, proto.Uint32(0)} {
		r := newRouteRig(t, segmentID)
		r.request.Arguments.GetSetLight().BrightnessPercent = proto.Float64(0)
		_, e := r.service.Dispatch(context.Background(), r.actor, r.request)
		if e != nil {
			t.Fatal(e)
		}
		id, _ := domain.ParseCommandID(r.request.CommandId)
		status, e := r.host.ActionStatus(id)
		if e != nil || status.Status != actions.ActionStatus_ACTION_STATUS_CONFIRMED {
			t.Fatalf("zero brightness %v %v", status, e)
		}
		if segmentID == nil {
			if *r.transport.d.State.On || *r.transport.d.State.Brightness == 0 {
				t.Fatal("master fixture did not preserve last level")
			}
		} else {
			if *r.transport.d.State.Segments[0].On || *r.transport.d.State.Segments[0].Brightness == 0 {
				t.Fatal("segment fixture did not preserve last level")
			}
		}
		if r.transport.writes != 1 {
			t.Fatal("replayed")
		}
	}
}

func TestNHA083PositiveBrightnessACKCannotBorrowRetainedOffLevel(t *testing.T) {
	r := newRig(t)
	r.transport.d.State.On = proto.Bool(false)
	r.transport.d.State.Brightness = proto.Uint32(128)
	r.transport.mode = "ack-only"
	r.request.ExpiresAt = timestamppb.New(time.Now().Add(60 * time.Millisecond))
	_, _ = r.service.Dispatch(context.Background(), r.actor, r.request)
	id, _ := domain.ParseCommandID(r.request.CommandId)
	status, e := r.host.ActionStatus(id)
	if e != nil || status.Status == actions.ActionStatus_ACTION_STATUS_CONFIRMED {
		t.Fatalf("retained off level confirmed positive brightness: %v %v", status, e)
	}
}

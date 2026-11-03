package sonos

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	media "github.com/damione1/Homeward/gen/home/media/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Every request stays in this RoundTripper. No device or listening socket is
// involved, including in the tests of the real Host/action ledger below.
type commandTransport struct {
	mu                                       sync.Mutex
	volume                                   uint32
	playback                                 string
	topology                                 string
	wrongUID                                 bool
	ackOnly, failWrite, failReadback, badACK bool
	writes                                   []string
	afterTopology                            func()
	transitionReads, transportReads          int
	transitionForever                        bool
	afterTransport                           func(int)
}

func (f *commandTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Context().Err() != nil {
		return nil, req.Context().Err()
	}
	if req.URL.Host != "192.168.50.10:1400" {
		return nil, ErrUnavailable
	}
	f.mu.Lock()
	action := req.Header.Get("SOAPACTION")
	name := strings.TrimSuffix(action[strings.LastIndex(action, "#")+1:], `"`)
	mutation := name == "Play" || name == "Pause" || name == "Stop" || name == "SetVolume"
	if mutation {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			f.mu.Unlock()
			return nil, err
		}
		if req.GetBody != nil {
			f.mu.Unlock()
			return nil, fmt.Errorf("mutation is replayable")
		}
		f.writes = append(f.writes, name+" "+string(body))
		if f.failWrite {
			f.mu.Unlock()
			return nil, io.ErrUnexpectedEOF
		}
		service := "AVTransport"
		if name == "SetVolume" {
			service = "RenderingControl"
			fields, err := fields(body, "DesiredVolume", "Channel", "InstanceID")
			v, e := strconv.ParseUint(fields["DesiredVolume"], 10, 32)
			if err != nil || e != nil || fields["Channel"] != "Master" || fields["InstanceID"] != "0" || v > 100 {
				f.mu.Unlock()
				return nil, ErrUnavailable
			}
			if !f.ackOnly {
				f.volume = uint32(v)
			}
		} else if !f.ackOnly {
			f.playback = map[string]string{"Play": "PLAYING", "Pause": "PAUSED_PLAYBACK", "Stop": "STOPPED"}[name]
		}
		if req.URL.Path != "/MediaRenderer/"+service+"/Control" {
			f.mu.Unlock()
			return nil, ErrUnavailable
		}
		raw := `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:` + name + `Response xmlns:u="urn:schemas-upnp-org:service:` + service + `:1"/></s:Body></s:Envelope>`
		if f.badACK {
			raw = `<Envelope><Body><Fault/></Body></Envelope>`
		}
		f.mu.Unlock()
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw))}, nil
	}
	if f.failReadback && len(f.writes) > 0 {
		f.mu.Unlock()
		return nil, io.ErrUnexpectedEOF
	}
	wrong, volume, playback, topology, callback := f.wrongUID, f.volume, f.playback, f.topology, f.afterTopology
	readNumber := 0
	if name == "GetTransportInfo" && len(f.writes) > 0 {
		f.transportReads++
		readNumber = f.transportReads
		if f.transitionForever || f.transitionReads > 0 {
			playback = "TRANSITIONING"
			if f.transitionReads > 0 {
				f.transitionReads--
			}
		}
	}
	transportCallback := f.afterTransport
	f.mu.Unlock()
	response, err := (&readTransport{wrongUID: wrong}).RoundTrip(req)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return nil, err
	}
	body := strings.ReplaceAll(string(raw), "<CurrentVolume>7</CurrentVolume>", fmt.Sprintf("<CurrentVolume>%d</CurrentVolume>", volume))
	body = strings.ReplaceAll(body, "<CurrentTransportState>PLAYING</CurrentTransportState>", "<CurrentTransportState>"+playback+"</CurrentTransportState>")
	if name == "GetZoneGroupState" {
		if topology != "" {
			body = "<Response><ZoneGroupState>" + html.EscapeString(topology) + "</ZoneGroupState></Response>"
		}
		if callback != nil {
			callback()
		}
	}
	if readNumber != 0 && transportCallback != nil {
		transportCallback(readNumber)
	}
	response.Body = io.NopCloser(strings.NewReader(body))
	return response, nil
}

func (f *commandTransport) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.writes) }

type commandGeneration struct{}

func (commandGeneration) WithCurrent(ctx context.Context, fn func(uint64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(7)
}

type commandRig struct {
	pilot     *pilot
	transport *commandTransport
	host      *integrations.Manager
	fixture   *fake.Fixture
	service   *integrations.VolatileCommandService
	entity    domain.EntityID
	actor     domain.ActorID
	epoch     string
}

func newCommandRig(t *testing.T) commandRig {
	t.Helper()
	_, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if err = runtime.RegisterCodec(catalog); err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(fixture, fixture, catalog)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("test-sonos")
	player := testPlayer()
	resolved, err := fixture.Resolve(context.Background(), instance, []integrations.SourceKey{player.SourceKey()})
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := domain.ParseHAAlias(player.Alias)
	record, err := integrations.NewRegistryRecord(resolved[0].Device, resolved[0].Entity, instance, player.SourceKey(), alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := integrations.NewCandidate(player.SourceKey(), []*registry.CapabilityDescription{runtime.description("Demo"), volumeDescription("Demo")})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := session.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil {
		t.Fatal(err)
	}
	_, entity, _ := bindings[0].IDs()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	transport := &commandTransport{volume: 7, playback: "PLAYING"}
	p := &pilot{runtime: runtime, ctx: ctx, cancel: cancel, reader: &reader{client: &http.Client{Transport: transport}}, host: session, players: map[domain.EntityID]PlayerInput{entity: player}, bindings: map[domain.EntityID]integrations.Binding{entity: bindings[0]}}
	if err = host.AttachCommandDriver(session, p.CommandDriver()); err != nil {
		t.Fatal(err)
	}
	p.poll(ctx, p.reader, []PlayerInput{player}, map[string]domain.EntityID{player.UID: entity})
	if _, ok := runtime.Snapshot(entity); !ok {
		t.Fatal("missing initial observation")
	}
	service, err := integrations.NewVolatileCommandService(host, commandGeneration{}, integrations.DefaultVolatileCommandLimits())
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := fixture.CurrentAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	actor, _ := domain.ParseActorID("sonos-test")
	return commandRig{p, transport, host, fixture, service, entity, actor, epoch}
}

func (r commandRig) request(t *testing.T, command any) *actions.CommandRequest {
	t.Helper()
	snapshot, ok := r.pilot.runtime.Snapshot(r.entity)
	if !ok {
		t.Fatal("snapshot unavailable")
	}
	action := &media.MediaAction{Route: proto.Clone(snapshot.Route).(*media.Route)}
	action.Route.AdmissionEpoch, action.Route.ConfigRevision = r.epoch, 7
	switch v := command.(type) {
	case media.Operation:
		action.Command = &media.MediaAction_Operation{Operation: v}
	case uint32:
		action.Command = &media.MediaAction_VolumeBasisPoints{VolumeBasisPoints: v}
	default:
		t.Fatal("bad test command")
	}
	payload, err := proto.Marshal(action)
	if err != nil {
		t.Fatal(err)
	}
	entity, _ := r.entity.Value()
	return &actions.CommandRequest{CommandId: "sonos-command-1", AdmissionEpoch: r.epoch, ConfigRevision: 7, Target: &registry.EntityRef{Reference: &registry.EntityRef_EntityId{EntityId: entity}}, CapabilitySchemaId: Schema, Endpoint: Endpoint, ExpiresAt: timestamppb.New(time.Now().Add(time.Minute)), Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_Extension{Extension: &actions.ExtensionPayload{SchemaId: Schema, Major: 1, DescriptorSha256: append([]byte(nil), r.pilot.runtime.digest[:]...), ProtobufPayload: payload}}}}
}

func (r commandRig) status(t *testing.T, want actions.ActionStatus) {
	t.Helper()
	id, _ := domain.ParseCommandID("sonos-command-1")
	s, err := r.host.ActionStatus(id)
	if err != nil || s.Status != want {
		t.Fatalf("status=%v err=%v want=%v", s.Status, err, want)
	}
}

func TestSonosCommandsUseHostLedgerAndObservedReadback(t *testing.T) {
	for _, command := range []any{media.Operation_OPERATION_PLAY, media.Operation_OPERATION_PAUSE, media.Operation_OPERATION_STOP, uint32(0), uint32(10000), uint32(1800)} {
		t.Run(fmt.Sprint(command), func(t *testing.T) {
			r := newCommandRig(t)
			request := r.request(t, command)
			if _, err := r.service.Dispatch(context.Background(), r.actor, request); err != nil {
				t.Fatal(err)
			}
			r.status(t, actions.ActionStatus_ACTION_STATUS_CONFIRMED)
			if r.transport.count() != 1 {
				t.Fatal("expected exactly one physical emission")
			}
			if _, err := r.service.Dispatch(context.Background(), r.actor, request); err != nil {
				t.Fatal(err)
			}
			r.pilot.poll(r.pilot.ctx, r.pilot.reader, []PlayerInput{testPlayer()}, map[string]domain.EntityID{testUID: r.entity})
			if r.transport.count() != 1 {
				t.Fatal("ledger replay or polling emitted a mutation")
			}
			snapshot, _ := r.pilot.runtime.Snapshot(r.entity)
			if v, volume := command.(uint32); volume && snapshot.GetVolumeBasisPoints() != v {
				t.Fatal("snapshot did not reflect actual readback")
			}
		})
	}
}

func TestSonosACKAloneNeverConfirmsAndFailureNeverReplays(t *testing.T) {
	for _, mode := range []string{"ack-only", "readback-failure", "write-failure", "invalid-ack"} {
		t.Run(mode, func(t *testing.T) {
			r := newCommandRig(t)
			r.transport.ackOnly = mode == "ack-only"
			r.transport.failReadback = mode == "readback-failure"
			r.transport.failWrite = mode == "write-failure"
			r.transport.badACK = mode == "invalid-ack"
			request := r.request(t, uint32(1800))
			_, _ = r.service.Dispatch(context.Background(), r.actor, request)
			want := actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED
			if mode == "write-failure" || mode == "invalid-ack" {
				want = actions.ActionStatus_ACTION_STATUS_INDETERMINATE
			}
			r.status(t, want)
			_, _ = r.service.Dispatch(context.Background(), r.actor, request)
			r.pilot.clear()
			r.pilot.poll(r.pilot.ctx, r.pilot.reader, []PlayerInput{testPlayer()}, map[string]domain.EntityID{testUID: r.entity})
			if r.transport.count() != 1 {
				t.Fatal("failed or acknowledged command replayed")
			}
		})
	}
}

func TestSonosTransportTransitionWaitsForObservedStableStateWithoutAnotherWrite(t *testing.T) {
	for _, operation := range []media.Operation{media.Operation_OPERATION_PLAY, media.Operation_OPERATION_PAUSE, media.Operation_OPERATION_STOP} {
		t.Run(operation.String(), func(t *testing.T) {
			r := newCommandRig(t)
			r.transport.transitionReads = 1
			bufferingObserved := false
			r.transport.afterTransport = func(read int) {
				if read != 2 {
					return
				}
				snapshot, ok := r.pilot.runtime.Snapshot(r.entity)
				if !ok || snapshot.Playback != media.PlaybackState_PLAYBACK_STATE_BUFFERING {
					t.Fatal("transition was replaced by an invented stable state")
				}
				r.status(t, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
				bufferingObserved = true
			}
			request := r.request(t, operation)
			if _, err := r.service.Dispatch(context.Background(), r.actor, request); err != nil {
				t.Fatal(err)
			}
			r.status(t, actions.ActionStatus_ACTION_STATUS_CONFIRMED)
			if !bufferingObserved || r.transport.count() != 1 || r.transport.transportReads != 2 {
				t.Fatal("transport transition did not finish with two observations and one write")
			}
			_, _ = r.service.Dispatch(context.Background(), r.actor, request)
			if r.transport.count() != 1 {
				t.Fatal("confirmed transition replayed")
			}
		})
	}
}

func TestSonosTransportTransitionTimeoutCancellationAndFencesNeverReplay(t *testing.T) {
	for _, mode := range []string{"timeout", "intent-expiry", "cancel", "session-stop", "host-fence", "uid-change", "regroup"} {
		t.Run(mode, func(t *testing.T) {
			r := newCommandRig(t)
			r.transport.transitionReads = 1
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "timeout" {
				var timeoutCancel context.CancelFunc
				ctx, timeoutCancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer timeoutCancel()
				r.transport.transitionForever = true
			}
			r.transport.afterTransport = func(read int) {
				if read != 2 {
					return
				}
				switch mode {
				case "cancel":
					cancel()
				case "session-stop":
					_ = r.pilot.Stop(context.Background())
				case "host-fence":
					_ = r.host.CloseAdmissions()
				case "uid-change":
					r.transport.mu.Lock()
					r.transport.wrongUID = true
					r.transport.mu.Unlock()
				case "regroup":
					r.transport.mu.Lock()
					r.transport.topology = `<ZoneGroupState><ZoneGroups><ZoneGroup Coordinator="` + testUID + `"><ZoneGroupMember UUID="` + testUID + `"/><ZoneGroupMember UUID="RINCON_00112233446601400"/></ZoneGroup></ZoneGroups></ZoneGroupState>`
					r.transport.mu.Unlock()
				}
			}
			request := r.request(t, media.Operation_OPERATION_PLAY)
			if mode == "intent-expiry" {
				request.ExpiresAt = timestamppb.New(time.Now().Add(100 * time.Millisecond))
			}
			started := time.Now()
			_, _ = r.service.Dispatch(ctx, r.actor, request)
			if time.Since(started) > 2*time.Second {
				t.Fatal("transition wait ignored cancellation or timeout")
			}
			id, _ := domain.ParseCommandID(request.CommandId)
			status, err := r.host.ActionStatus(id)
			if err != nil || status.Status == actions.ActionStatus_ACTION_STATUS_CONFIRMED {
				t.Fatal("transition confirmed after timeout, cancellation or a lost fence", err)
			}
			_, _ = r.service.Dispatch(context.Background(), r.actor, request)
			if r.transport.count() != 1 {
				t.Fatal("transition failure replayed the physical write")
			}
		})
	}
}

func TestSonosTransitionWaitDoesNotExtendVolumeOrStableMismatch(t *testing.T) {
	for _, volume := range []bool{false, true} {
		r := newCommandRig(t)
		var request *actions.CommandRequest
		if volume {
			r.transport.transitionForever = true
			request = r.request(t, uint32(1800))
		} else {
			r.transport.ackOnly = true
			request = r.request(t, media.Operation_OPERATION_PAUSE)
		}
		if _, err := r.service.Dispatch(context.Background(), r.actor, request); err != nil {
			t.Fatal(err)
		}
		if r.transport.count() != 1 || r.transport.transportReads != 1 {
			t.Fatal("readback wait escaped its transport TRANSITIONING scope")
		}
	}
}

func TestSonosMismatchedReadbackPublishesActualValueWithoutConfirmation(t *testing.T) {
	r := newCommandRig(t)
	request := r.request(t, uint32(1800))
	r.transport.volume, r.transport.ackOnly = 9, true
	if _, err := r.service.Dispatch(context.Background(), r.actor, request); err != nil {
		t.Fatal(err)
	}
	r.status(t, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
	snapshot, ok := r.pilot.runtime.Snapshot(r.entity)
	if !ok || snapshot.GetVolumeBasisPoints() != 900 {
		t.Fatal("actual mismatched readback was replaced by desired or old volume")
	}
}

func TestSonosRegroupingDuringReadbackNeverConfirms(t *testing.T) {
	r := newCommandRig(t)
	topologyReads := 0
	r.transport.afterTopology = func() {
		topologyReads++
		if topologyReads == 2 {
			r.transport.mu.Lock()
			r.transport.topology = `<ZoneGroupState><ZoneGroups><ZoneGroup Coordinator="` + testUID + `"><ZoneGroupMember UUID="` + testUID + `"/><ZoneGroupMember UUID="RINCON_00112233446601400"/></ZoneGroup></ZoneGroups></ZoneGroupState>`
			r.transport.mu.Unlock()
		}
	}
	if _, err := r.service.Dispatch(context.Background(), r.actor, r.request(t, uint32(1800))); err != nil {
		t.Fatal(err)
	}
	r.status(t, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
	if r.transport.count() != 1 {
		t.Fatal("regrouping triggered another emission")
	}
	if _, ok := r.pilot.runtime.Snapshot(r.entity); ok {
		t.Fatal("regrouped readback retained standalone snapshot")
	}
}

func TestSonosRejectsUnsupportedAndNonIntegralVolume(t *testing.T) {
	for _, command := range []any{media.Operation_OPERATION_NEXT, media.Operation_OPERATION_PREVIOUS, uint32(10001), uint32(1), uint32(9999)} {
		r := newCommandRig(t)
		response, err := r.service.Dispatch(context.Background(), r.actor, r.request(t, command))
		if err == nil && response.Error == nil {
			t.Fatalf("accepted unsupported command %v", command)
		}
		if r.transport.count() != 0 {
			t.Fatal("invalid command reached transport")
		}
	}
}

func TestSonosTopologyIdentityCancellationAndHostFenceBeforeWrite(t *testing.T) {
	const other = "RINCON_00112233446601400"
	for _, mode := range []string{"group", "follower", "invisible", "satellite", "wrong-uid", "stale-route", "route-epoch", "route-config", "closed-host", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			r := newCommandRig(t)
			request := r.request(t, media.Operation_OPERATION_PLAY)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			member := `<ZoneGroupMember UUID="` + testUID + `"/>`
			coordinator := testUID
			switch mode {
			case "group":
				member += `<ZoneGroupMember UUID="` + other + `"/>`
			case "follower":
				coordinator = other
				member += `<ZoneGroupMember UUID="` + other + `"/>`
			case "invisible":
				member += `<ZoneGroupMember UUID="` + other + `" Invisible="1"/>`
			case "satellite":
				member = `<ZoneGroupMember UUID="` + testUID + `"><Satellite UUID="` + other + `"/></ZoneGroupMember>`
			case "wrong-uid":
				r.transport.wrongUID = true
			case "stale-route", "route-epoch", "route-config":
				action := &media.MediaAction{}
				if err := proto.Unmarshal(request.GetArguments().GetExtension().ProtobufPayload, action); err != nil {
					t.Fatal(err)
				}
				if mode == "stale-route" {
					action.Route.TopologyRevision++
				}
				if mode == "route-epoch" {
					action.Route.AdmissionEpoch = "another-epoch"
				}
				if mode == "route-config" {
					action.Route.ConfigRevision++
				}
				request.GetArguments().GetExtension().ProtobufPayload, _ = proto.Marshal(action)
			case "closed-host":
				r.transport.afterTopology = func() { _ = r.host.CloseAdmissions() }
			case "cancel":
				r.transport.afterTopology = cancel
			}
			r.transport.topology = `<ZoneGroupState><ZoneGroups><ZoneGroup Coordinator="` + coordinator + `">` + member + `</ZoneGroup></ZoneGroups></ZoneGroupState>`
			_, _ = r.service.Dispatch(ctx, r.actor, request)
			if r.transport.count() != 0 {
				t.Fatal("unsafe command reached speaker")
			}
		})
	}
}

func TestSonosVolumeCoreStateUsesObservedPercentAndStalesOnLoss(t *testing.T) {
	r := newCommandRig(t)
	state, ok := r.fixture.State(r.entity)
	if !ok {
		t.Fatal("no core state for observed volume")
	}
	value, ok := state.CurrentValue()
	measurement, measurementOK := value.Measurement()
	components, err := measurement.Components()
	if !ok || !measurementOK || err != nil || components.Value != 7 {
		t.Fatal("volume state is not observed percent")
	}
	r.transport.wrongUID = true
	r.pilot.poll(r.pilot.ctx, r.pilot.reader, []PlayerInput{testPlayer()}, map[string]domain.EntityID{testUID: r.entity})
	state, _ = r.fixture.State(r.entity)
	_, quality, _, _, _, _ := state.Components()
	availability, freshness, _, _ := quality.Components()
	if availability != domain.AvailabilityUnavailable || freshness != domain.FreshnessStale {
		t.Fatal("loss retained an available core volume")
	}
}

func TestSonosTopologyRevisionDoesNotReuseAfterUnavailable(t *testing.T) {
	r := newCommandRig(t)
	before, _ := r.pilot.runtime.Snapshot(r.entity)
	r.pilot.clear()
	r.pilot.poll(r.pilot.ctx, r.pilot.reader, []PlayerInput{testPlayer()}, map[string]domain.EntityID{testUID: r.entity})
	after, ok := r.pilot.runtime.Snapshot(r.entity)
	if !ok || after.Route.TopologyRevision <= before.Route.TopologyRevision {
		t.Fatal("stale topology revision was reused")
	}
}

func TestSonosSOAPACKRequiresExactEmptyResponse(t *testing.T) {
	for _, raw := range []string{`<x/>`, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><Fault/></s:Body></s:Envelope>`, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:PlayResponse xmlns:u="urn:schemas-upnp-org:service:AVTransport:1"><Fault/></u:PlayResponse></s:Body></s:Envelope>`, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><Body><u:PlayResponse xmlns:u="urn:schemas-upnp-org:service:AVTransport:1"/></Body></s:Envelope>`, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:PlayResponse xmlns:u="urn:schemas-upnp-org:service:AVTransport:1"/></s:Body></s:Envelope><extra/>`} {
		if validACK([]byte(raw), "AVTransport", "Play") {
			t.Fatal("invalid SOAP response acknowledged")
		}
	}
}

func TestSonosTransportHasNoUnfencedOrUnsupportedWriteEntry(t *testing.T) {
	fake := &commandTransport{}
	r := &reader{client: &http.Client{Transport: fake}}
	for _, action := range []string{"Play", "Pause", "Stop", "SetVolume", "SetAVTransportURI", "SetMute", "AddURIToQueue", "Seek"} {
		if _, err := r.request(context.Background(), testPlayer().Address, "/MediaRenderer/AVTransport/Control", "AVTransport", action, ""); err == nil {
			t.Fatalf("unfenced or unsupported operation %s reached transport", action)
		}
	}
	if fake.count() != 0 {
		t.Fatal("unfenced mutation emitted")
	}
}

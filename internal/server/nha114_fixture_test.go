package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/gen/home/actions/v1/actionsv1connect"
	configuration "github.com/damione1/Homeward/gen/home/configuration/v1"
	applywire "github.com/damione1/Homeward/gen/home/configurationapply/v1"
	planwire "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	"github.com/damione1/Homeward/gen/home/observability/v1/observabilityv1connect"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/gen/home/state/v1/statev1connect"
	"github.com/damione1/Homeward/internal/cli"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
	"github.com/eclipse/paho.golang/paho"
	mochi "github.com/mochi-mqtt/server/v2"
	mochipackets "github.com/mochi-mqtt/server/v2/packets"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const nha114Instance = "nha114-isolated-mqtt"

// These frozen manifest aliases are explicitly bound only to the disposable
// source below. No production source, credentials, broker or driver is loaded.
var nha114Aliases = []string{
	"switch.arc_soundbar_crossfade", "switch.arc_soundbar_loudness",
	"switch.arc_soundbar_night_sound", "switch.arc_soundbar_speech_enhancement",
	"switch.arc_soundbar_subwoofer_enabled",
	"switch.arc_soundbar_surround_music_full_volume", "switch.arc_soundbar_tv_autoplay",
	"switch.arc_soundbar_ungroup_on_autoplay", "switch.backyard_detect", "switch.backyard_motion",
}

type nha114CommandCounter struct {
	mochi.HookBase
	count atomic.Int64
}

func (*nha114CommandCounter) ID() string               { return "nha114-command-packets" }
func (*nha114CommandCounter) Provides(event byte) bool { return event == mochi.OnPacketRead }
func (c *nha114CommandCounter) OnPacketRead(_ *mochi.Client, p mochipackets.Packet) (mochipackets.Packet, error) {
	if p.FixedHeader.Type == mochipackets.Publish && strings.HasPrefix(p.TopicName, "zigbee2mqtt/nha114-") && strings.HasSuffix(p.TopicName, "/set") {
		c.count.Add(1)
	}
	return p, nil
}

type nha114Fixture struct {
	*nha055PublicationFixture
	cluster        testpg.Cluster
	broker         *testbroker.Broker
	count          *nha114CommandCounter
	packets        <-chan *paho.Publish
	producer       *testbroker.Peer
	client         *http.Client
	actions        actionsv1connect.ActionServiceClient
	states         statev1connect.StateServiceClient
	observation    observabilityv1connect.ObservabilityServiceClient
	data, clientID string
	ids            []string
	mqttWire       *nha114TCPFault
	publicView     *nha114PublicView
	policySource   []byte
}

type nha114Projection struct {
	Cursor   *state.ViewCursor
	Entities []*state.ViewEntity
}
type nha114PublicView struct {
	mu         sync.Mutex
	projection nha114Projection
	resync     state.ResyncReason
	err        error
	close      func()
	done       chan struct{}
}

func nha114Start(t *testing.T) *nha114Fixture {
	t.Helper()
	b := testbroker.Start(t)
	counter := new(nha114CommandCounter)
	if err := b.Server.AddHook(counter, nil); err != nil {
		t.Fatal(err)
	}
	observer := b.Connect(t, testbroker.ObserverUser, testbroker.ObserverPassword, "nha114-observer", true, 0, nil)
	packets := observer.Subscribe(t, "zigbee2mqtt/+/set")
	cluster := testpg.Start(t)
	mqttWire := nha114StartTCPFault(t, b.Address)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	raw := []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\n%spostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\nmqtt:\n  profile: '5'\n  broker: 'mqtt://%s'\n  instance: '%s'\n  mode: active\n  username_secret: mqtt-username\n  password_secret: mqtt-password\n  subscriptions: ['homeassistant/#', 'zigbee2mqtt/+/state']\n  command_topics: ['zigbee2mqtt/+/set']\n  will:\n    topic: 'nha068/presence/pilot'\n    payload: offline\n", data, nha181TLSYAML(), cluster.Port, mqttWire.address(), nha114Instance))
	cfg := nha029RunFixtureFromRaw(t, raw)
	pg, err := cfg.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), pg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	secrets, err := store.BootstrapSecrets(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"mqtt-username": testbroker.PilotUser, "mqtt-password": testbroker.PilotPassword} {
		secret, err := storage.NewSecretValue([]byte(value))
		if err != nil {
			t.Fatal(err)
		}
		if err := secrets.RotateCredential(context.Background(), nha051ServerHandle(t, nha114Instance, name), secret); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := registry.ParseManifest(scope.ManifestV1())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := registry.NewRepository(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID(nha114Instance)
	if err != nil {
		t.Fatal(err)
	}
	f := &nha114Fixture{cluster: cluster, broker: b, count: counter, packets: packets, data: data, mqttWire: mqttWire}
	for i, alias := range nha114Aliases {
		// A device/schema/endpoint identifies one command route. Distinct
		// endpoints named "mqtt" therefore require distinct synthetic devices.
		key, err := integrations.NewSourceKey(fmt.Sprintf("nha114-synthetic-device-%d", i), fmt.Sprintf("nha114-relay-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		identity, err := repository.ConfigureBinding(context.Background(), instance, key, alias)
		if err != nil {
			t.Fatal(err)
		}
		id, err := identity.Entity.Value()
		if err != nil {
			t.Fatal(err)
		}
		f.ids = append(f.ids, id)
		profile := fmt.Sprintf(`{"unique_id":"nha114-relay-%d","device":{"identifiers":["nha114-synthetic-device-%d"]},"state_topic":"zigbee2mqtt/nha114-%d/state","command_topic":"zigbee2mqtt/nha114-%d/set"}`, i, i, i, i)
		b.Publish(t, fmt.Sprintf("homeassistant/switch/nha114-%d/config", i), []byte(profile), true)
		b.Publish(t, nha114StateTopic(i), []byte("OFF"), true)
	}
	path := filepath.Join(t.TempDir(), "configuration.yaml")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	t.Cleanup(cancel)
	f.nha055PublicationFixture = &nha055PublicationFixture{startup: startup, roots: roots}
	f.path, f.raw, f.ctx = path, raw, ctx
	f.startRun(t)
	nha162RunBootstrap(t, ctx, data, "NHA055 publication admin", "NHA055 publication isolated password")
	f.login(t)
	grants := ""
	for _, permission := range []string{"state_registry, operation: read", "actions, operation: read", "actions, operation: write"} {
		grants += "  - scope: {family: " + permission + "}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n"
	}
	nha162RunPolicy(t, ctx, f.policies, f.tokens.Session.AccountId, f.token, grants)
	f.policySource = []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n%s", f.tokens.Session.AccountId.Value, grants))
	f.refreshClients(t)
	f.producer = b.Connect(t, testbroker.ProducerUser, testbroker.ProducerPassword, "nha114-producer", true, 0, nil)
	digest := sha256.Sum256([]byte(nha114Instance))
	f.clientID = "homeward-" + hex.EncodeToString(digest[:12])
	// Supply a real suffix before any wait that may consume the new watch.
	// Discovery can still be finishing; after all public bindings are present,
	// the second receipt below establishes fresh state rather than cached replay.
	for i := range f.ids {
		f.publish(t, nha114StateTopic(i), []byte("OFF"), nil)
	}
	f.waitBindings(t)
	f.noCommands(t, 0)
	for i := range f.ids {
		f.publish(t, nha114StateTopic(i), []byte("OFF"), nil)
		f.waitSwitch(t, i, false)
	}
	f.noCommands(t, 0)
	return f
}

func nha114StateTopic(i int) string { return fmt.Sprintf("zigbee2mqtt/nha114-%d/state", i) }
func (f *nha114Fixture) refreshClients(t *testing.T) {
	t.Helper()
	f.client = nha162RunClient(t, f.roots, f.run.address)
	base := "https://" + f.run.address.String()
	f.actions = actionsv1connect.NewActionServiceClient(f.client, base)
	f.states = statev1connect.NewStateServiceClient(f.client, base)
	f.observation = observabilityv1connect.NewObservabilityServiceClient(f.client, base)
	f.refreshPublicView(t)
}
func nha114Eventually(t *testing.T, reason string, predicate func() bool) {
	t.Helper()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		if predicate() {
			return
		}
		select {
		case <-poll.C:
		case <-deadline.C:
			t.Fatal(reason)
		}
	}
}
func (f *nha114Fixture) refreshPublicView(t *testing.T) {
	t.Helper()
	if f.publicView != nil {
		f.publicView.close()
		select {
		case <-f.publicView.done:
		case <-f.ctx.Done():
			t.Fatal("public state watch did not close", f.ctx.Err())
		}
	}
	ctx, cancel := context.WithCancel(f.ctx)
	r, err := f.states.OpenView(ctx, nha056SessionRequest(&state.OpenViewRequest{PageSize: 100, Filter: &state.ViewFilter{}}, f.token))
	if err != nil {
		cancel()
		t.Fatal("authenticated state snapshot", err)
	}
	p := &nha114PublicView{projection: nha114Projection{Cursor: r.Msg.Cursor, Entities: r.Msg.Entities}, done: make(chan struct{})}
	var once sync.Once
	p.close = func() { once.Do(cancel) }
	t.Cleanup(func() {
		p.close()
		select {
		case <-p.done:
		case <-f.ctx.Done():
			t.Error("public state watch cleanup did not join", f.ctx.Err())
		}
	})
	f.publicView = p
	states, token := f.states, f.token
	go func() {
		defer close(p.done)
		// Opening the Connect call can wait for response headers. The server
		// sends its first headers with a delta/resync, so the caller must remain
		// free to publish the real OFF receipt used as the initial barrier.
		stream, err := states.WatchView(ctx, nha056SessionRequest(&state.WatchViewRequest{Cursor: r.Msg.Cursor, Filter: &state.ViewFilter{}}, token))
		if err != nil {
			p.mu.Lock()
			if ctx.Err() == nil {
				p.err = err
			}
			p.mu.Unlock()
			return
		}
		// This goroutine owns opening, Receive and Close. Cancellation also
		// interrupts an opening call that has not received headers yet.
		defer stream.Close()
		for stream.Receive() {
			frame := stream.Msg()
			p.mu.Lock()
			if resync := frame.GetResyncRequired(); resync != nil {
				p.resync = resync.Reason
				p.mu.Unlock()
				return
			}
			if frame.Cursor == nil || frame.GetDelta() == nil {
				p.err = errors.New("public watch omitted cursor/delta")
				p.mu.Unlock()
				return
			}
			p.projection.Cursor = proto.Clone(frame.Cursor).(*state.ViewCursor)
			for _, value := range frame.GetDelta().States {
				found := false
				for _, entity := range p.projection.Entities {
					if entity.EntityId == value.GetEntity().GetEntityId() {
						entity.State = proto.Clone(value).(*state.ObservedState)
						found = true
						break
					}
				}
				if !found {
					p.err = errors.New("public state delta has no snapshot entity")
				}
			}
			p.mu.Unlock()
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if ctx.Err() == nil {
			p.err = stream.Err()
			if p.err == nil {
				p.err = errors.New("public state watch ended without resync")
			}
		}
	}()
}
func (f *nha114Fixture) view(t *testing.T) *nha114Projection {
	t.Helper()
	for {
		p := f.publicView
		p.mu.Lock()
		resync, err := p.resync, p.err
		result := &nha114Projection{Cursor: proto.Clone(p.projection.Cursor).(*state.ViewCursor)}
		for _, entity := range p.projection.Entities {
			result.Entities = append(result.Entities, proto.Clone(entity).(*state.ViewEntity))
		}
		p.mu.Unlock()
		if err != nil {
			t.Fatal("public state watch failed", err)
		}
		if resync != state.ResyncReason_RESYNC_REASON_UNSPECIFIED {
			f.refreshPublicView(t)
			continue
		}
		return result
	}
}
func (f *nha114Fixture) waitBindings(t *testing.T) {
	t.Helper()
	nha114Eventually(t, "ten actual MQTT bindings/states were not admitted", func() bool {
		v := f.view(t)
		found := 0
		for _, e := range v.Entities {
			for _, id := range f.ids {
				if e.EntityId == id && len(e.Capabilities) == 1 && e.Capabilities[0].Endpoint == "mqtt" && e.State != nil {
					found++
				}
			}
		}
		return found == len(nha114Aliases)
	})
}
func (f *nha114Fixture) noCommands(t *testing.T, want int64) {
	t.Helper()
	if got := f.count.count.Load(); got != want {
		t.Fatalf("actual command-topic packets=%d, want=%d", got, want)
	}
}
func (f *nha114Fixture) drainAndCount(t *testing.T, want int64) {
	t.Helper()
	f.publicView.close()
	select {
	case <-f.publicView.done:
	case <-f.ctx.Done():
		t.Fatal("public state watch did not join before Run drain", f.ctx.Err())
	}
	if err := f.run.join(t); err != nil {
		t.Fatal("actual Run drain before terminal command count", err)
	}
	// Client deletion follows packet processing and unsubscribe on the actual
	// broker. No late startup/reconnect/refresh command can escape this count.
	f.broker.WaitSessionRemoved(t, f.clientID)
	f.noCommands(t, want)
}
func (f *nha114Fixture) nextCommand(t *testing.T) *paho.Publish {
	t.Helper()
	select {
	case p := <-f.packets:
		return p
	case <-f.ctx.Done():
		t.Fatal("no command broker packet", f.ctx.Err())
		return nil
	}
}
func (f *nha114Fixture) command(t *testing.T, id, alias string, on bool) *actions.CommandRequest {
	t.Helper()
	cursor := f.view(t).Cursor
	return &actions.CommandRequest{CommandId: id, AdmissionEpoch: cursor.Epoch, ConfigRevision: cursor.ConfigRevision,
		Target: &registrywire.EntityRef{Reference: &registrywire.EntityRef_HaAlias{HaAlias: alias}}, CapabilitySchemaId: contracts.SwitchSchema, Endpoint: "mqtt",
		Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: on}}}, ExpiresAt: timestamppb.New(time.Now().Add(time.Minute))}
}
func (f *nha114Fixture) submit(t *testing.T, command *actions.CommandRequest) {
	t.Helper()
	result, err := f.actions.SubmitAction(f.ctx, nha056SessionRequest(&actions.SubmitActionRequest{Command: command}, f.token))
	if err != nil {
		t.Fatal("actual API durable dispatch", err)
	}
	if result == nil || result.Msg == nil {
		t.Fatal("actual API durable dispatch returned no response")
	}
	response := result.Msg.GetResult()
	if response.GetCommandId() != command.CommandId || response.GetStatus() != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("actual API durable dispatch: request=%s result_id=%s status=%s code=%s field=%s", command.CommandId, response.GetCommandId(), response.GetStatus(), response.GetError().GetCode(), response.GetError().GetFieldPath())
	}
}
func (f *nha114Fixture) progress(t *testing.T, id string) *actions.GetActionResponse {
	t.Helper()
	r, err := f.actions.GetAction(f.ctx, nha056SessionRequest(&actions.GetActionRequest{CommandId: id}, f.token))
	if err != nil {
		t.Fatal("read existing command", err)
	}
	return r.Msg
}
func (f *nha114Fixture) publish(t *testing.T, topic string, payload []byte, correlation []byte) {
	t.Helper()
	p := &paho.Publish{Topic: topic, Payload: payload, QoS: 1, Retain: false}
	if correlation != nil {
		p.Properties = &paho.PublishProperties{CorrelationData: bytes.Clone(correlation)}
	}
	ack, err := f.producer.Client.Publish(f.ctx, p)
	if err != nil || ack == nil || ack.ReasonCode >= 0x80 {
		t.Fatal("synthetic endpoint publish", err)
	}
}
func (f *nha114Fixture) applySource(t *testing.T, family planwire.SourceFamily, identity, id, entry string, raw []byte) {
	t.Helper()
	before := f.read(t)
	candidate := &planwire.Candidate{Family: family, Identity: identity, Entry: entry, Files: []*planwire.SourceFile{{LogicalPath: entry, ExactSource: raw}}}
	plan := f.build(t, candidate)
	if plan.State != planwire.PlanState_PLAN_STATE_ADMITTED || plan.Receipt == "" {
		t.Fatalf("source plan was not admitted: family=%s state=%s diagnostics=%v", family, plan.State, plan.Diagnostics)
	}
	request := nha055Request(before, candidate, &plan.Receipt)
	request.Sources = []*applywire.SourcePrecondition{nha055PublicationExpected(id, entry, nil, nil)}
	if err := contracts.AdmitConfigurationApplyRequest(request); err != nil {
		t.Fatal("fixture produced an invalid Apply request", family, err)
	}
	r, err := f.apply.Apply(f.ctx, nha056SessionRequest(request, f.token))
	if err != nil || r.Msg.Activation.Outcome != configuration.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED {
		t.Fatal("actual source publication", err)
	}
	f.refreshPublicView(t)
}
func (f *nha114Fixture) installAutomation(t *testing.T) {
	t.Helper()
	f.applySource(t, planwire.SourceFamily_SOURCE_FAMILY_DECLARATIVE_V2, "nha114.definitions", "nha114.definitions", "definitions.yaml", []byte("input_boolean:\n  away_mode: {name: NHA114 synthetic helper, initial: false}\ninput_button:\n  trash_out: {name: NHA114 synthetic button}\n"))
	raw := fmt.Sprintf("id: nha114_vertical\ntriggers:\n - trigger: state\n   entity_id: %s\n   from: 'off'\n   to: 'on'\nactions:\n - stop: complete\n", nha114Aliases[0])
	f.applySource(t, planwire.SourceFamily_SOURCE_FAMILY_AUTOMATION, "automation.turn_on_lights_at_sunset", "nha114.automation", "automation.yaml", []byte(raw))
	f.waitBindings(t)
}

type nha114Terminal struct{}

func (nha114Terminal) ReadPassword(string) ([]byte, error) {
	return []byte("NHA055 publication isolated password"), nil
}
func (f *nha114Fixture) cli(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"profiles", "add", "isolated", "--url", "https://" + f.run.address.String(), "--ca", filepath.Join(f.data, nha181CertificateName)},
		{"--profile", "isolated", "login", "NHA055 publication admin"},
		{"--profile", "isolated", "integrations", "health", nha114Instance},
	} {
		var output, diagnostics bytes.Buffer
		argv := append([]string{"--config-dir", dir}, args...)
		if code := cli.Run(f.ctx, argv, &output, &diagnostics, nha114Terminal{}); code != cli.ExitOK {
			t.Fatalf("actual CLI %s exit=%d diagnostic=%s", args[0], code, diagnostics.String())
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(output.Bytes(), &envelope); err != nil || len(envelope["result"]) == 0 {
			t.Fatal("actual CLI returned no JSON result", err)
		}
	}
}

func (f *nha114Fixture) operatorHealth(t *testing.T) (bool, bool) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(f.data, operatorSocketName))
	}}}
	defer client.CloseIdleConnections()
	r, err := client.Get("http://operator/health")
	if err != nil {
		t.Fatal("same-UID health", err)
	}
	defer r.Body.Close()
	var facts struct{ Live, Ready, Degraded bool }
	if err := json.NewDecoder(r.Body).Decode(&facts); err != nil || r.StatusCode != http.StatusOK || !facts.Live {
		t.Fatal("same-UID health facts unavailable", err)
	}
	return facts.Ready, facts.Degraded
}

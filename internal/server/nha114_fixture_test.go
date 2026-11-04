package server

import (
	"bytes"
	"connectrpc.com/connect"
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

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/gen/google/longrunning/longrunningpbconnect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	applywire "github.com/damione1/Homeward/gen/home/api/v2"
	planwire "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/cli"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
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
	capabilities   apiv2connect.CapabilityServiceClient
	views          apiv2connect.EntityViewServiceClient
	homes          apiv2connect.HomeServiceClient
	operations     longrunningpbconnect.OperationsClient
	home           string
	operationNames map[string]string
	observation    apiv2connect.ObservabilityServiceClient
	data, clientID string
	ids            []string
	mqttWire       *nha114TCPFault
	publicView     *nha114PublicView
	policySource   []byte
}

type nha114Projection struct {
	Name, Resume string
	Entities     []*api.EntityViewEntry
}
type nha114PublicView struct {
	mu         sync.Mutex
	projection nha114Projection
	resync     bool
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
	nha162RunPolicy(t, ctx, f.policies, nha203SessionAccount(t, f.tokens.Session).String(), f.token, grants)
	f.policySource = []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n%s", nha203AccountText(t, nha203NativeAccount(t, ctx, f.run.observed.sessions.store, nha203SessionAccount(t, f.tokens.Session).String())), grants))
	f.producer = b.Connect(t, testbroker.ProducerUser, testbroker.ProducerPassword, "nha114-producer", true, 0, nil)
	digest := sha256.Sum256([]byte(nha114Instance))
	f.clientID = "homeward-" + hex.EncodeToString(digest[:12])
	// MQTT connection/subscription readiness does not join asynchronous native
	// discovery. Publish the suffix, then join all ten real capability/state
	// admissions before asking the public authorizer for a stable snapshot.
	// The second receipt below still proves fresh state on the actual watch.
	for i := range f.ids {
		f.publish(t, nha114StateTopic(i), []byte("OFF"), nil)
	}
	f.refreshClients(t)
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
	f.capabilities = apiv2connect.NewCapabilityServiceClient(f.client, base)
	f.views = apiv2connect.NewEntityViewServiceClient(f.client, base)
	f.homes = apiv2connect.NewHomeServiceClient(f.client, base)
	f.operations = longrunningpbconnect.NewOperationsClient(f.client, base)
	if f.operationNames == nil {
		f.operationNames = make(map[string]string)
	}
	homes, err := f.homes.ListHomes(f.ctx, nha056SessionRequest(&api.ListHomesRequest{}, f.token))
	if err != nil || len(homes.Msg.GetHomes()) != 1 {
		t.Fatal("authenticated installation discovery", err)
	}
	f.home = homes.Msg.Homes[0].Name
	f.observation = apiv2connect.NewObservabilityServiceClient(f.client, base)
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
			t.Fatal("public entity watch did not join", f.ctx.Err())
		}
		// Explicitly release superseded captures; teardown/restart may already have invalidated them.
		_, _ = f.views.DeleteEntityView(f.ctx, nha056SessionRequest(&api.DeleteEntityViewRequest{Name: f.publicView.projection.Name}, f.token))
	}
	f.waitNativeBindingSession(t, "")
	ctx, cancel := context.WithCancel(f.ctx)
	created, err := f.views.CreateEntityView(ctx, nha056SessionRequest(&api.CreateEntityViewRequest{Parent: f.home, EntityView: &api.EntityView{}}, f.token))
	if err != nil {
		cancel()
		t.Fatal("authenticated entity capture", err)
	}
	projection := nha114Projection{Name: created.Msg.Name}
	pageToken := ""
	for {
		page, err := f.views.QueryEntityView(ctx, nha056SessionRequest(&api.QueryEntityViewRequest{Name: created.Msg.Name, PageSize: 100, PageToken: pageToken}, f.token))
		if err != nil {
			cancel()
			t.Fatal("complete authenticated entity snapshot", err)
		}
		projection.Entities = append(projection.Entities, page.Msg.Entries...)
		pageToken = page.Msg.NextPageToken
		if pageToken == "" {
			projection.Resume = page.Msg.ResumeToken
			break
		}
		if page.Msg.ResumeToken != "" {
			cancel()
			t.Fatal("partial page supplied a checkpoint")
		}
	}
	if projection.Resume == "" {
		cancel()
		t.Fatal("completed snapshot omitted checkpoint")
	}
	p := &nha114PublicView{projection: projection, done: make(chan struct{})}
	var once sync.Once
	p.close = func() { once.Do(cancel) }
	t.Cleanup(func() {
		p.close()
		select {
		case <-p.done:
		case <-f.ctx.Done():
			t.Error("public entity watch cleanup did not join", f.ctx.Err())
		}
	})
	f.publicView = p
	views, token := f.views, f.token
	go func() {
		defer close(p.done)
		stream, err := views.WatchEntityView(ctx, nha056SessionRequest(&api.WatchEntityViewRequest{Name: projection.Name, ResumeToken: projection.Resume}, token))
		if err == nil {
			defer stream.Close()
			for stream.Receive() {
				frame := stream.Msg()
				p.mu.Lock()
				if frame.ResumeToken == "" {
					p.err = errors.New("public watch omitted checkpoint")
					p.mu.Unlock()
					return
				}
				p.projection.Resume = frame.ResumeToken
				if entry := frame.GetUpsert(); entry != nil {
					found := false
					for i, old := range p.projection.Entities {
						if old.Entity.GetName() == entry.Entity.GetName() {
							p.projection.Entities[i] = proto.Clone(entry).(*api.EntityViewEntry)
							found = true
							break
						}
					}
					if !found {
						p.projection.Entities = append(p.projection.Entities, proto.Clone(entry).(*api.EntityViewEntry))
					}
				} else if removed := frame.GetRemove(); removed != "" {
					for i, old := range p.projection.Entities {
						if old.Entity.GetName() == removed {
							p.projection.Entities = append(p.projection.Entities[:i], p.projection.Entities[i+1:]...)
							break
						}
					}
				} else {
					p.err = errors.New("public entity watch omitted membership change")
				}
				p.mu.Unlock()
			}
			err = stream.Err()
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if ctx.Err() == nil {
			if connect.CodeOf(err) == connect.CodeFailedPrecondition {
				p.resync = true
			} else if err != nil {
				p.err = err
			} else {
				p.err = errors.New("public entity watch ended without invalidation")
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
		result := &nha114Projection{Name: p.projection.Name, Resume: p.projection.Resume}
		for _, entry := range p.projection.Entities {
			result.Entities = append(result.Entities, proto.Clone(entry).(*api.EntityViewEntry))
		}
		p.mu.Unlock()
		if err != nil {
			t.Fatal("public entity watch failed", err)
		}
		if resync {
			f.refreshPublicView(t)
			continue
		}
		return result
	}
}
func (f *nha114Fixture) entityName(t *testing.T, native string) string {
	t.Helper()
	home, err := apitypes.ParseHomeName(f.home)
	if err != nil {
		t.Fatal(err)
	}
	entity, err := domain.ParseEntityID(native)
	if err != nil {
		t.Fatal(err)
	}
	name, err := apitypes.EntityNameForIdentity(home, entity)
	if err != nil {
		t.Fatal(err)
	}
	return name.String()
}
func (f *nha114Fixture) aliasName(t *testing.T, alias string) string {
	t.Helper()
	parsed, err := registry.ParseHAAlias(alias)
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.actual.sequence.ResolveCommandAlias(parsed)
	if err != nil {
		t.Fatal("fixture alias has no native binding", err)
	}
	text, err := id.Value()
	if err != nil {
		t.Fatal(err)
	}
	return f.entityName(t, text)
}

// waitNativeBindingSession is a fixture readiness barrier, not a public view
// substitute. It reads the SAME Run-owned sequencer; all assertions below still
// use native authenticated EntityViews and the real MQTT packet count.
// A catalogue change during CurrentGrant correctly fails closed. Do not mask
// that error by retrying OpenView while discovery is still publishing metadata.
func (f *nha114Fixture) waitNativeBindingSession(t *testing.T, previous string) string {
	t.Helper()
	var admitted string
	nha114Eventually(t, "ten MQTT bindings/states did not join one current native session", func() bool {
		snapshot, err := f.actual.sequence.Capture()
		if errors.Is(err, sequencer.ErrRegistryUnavailable) {
			return false
		}
		if err != nil {
			t.Fatal("native MQTT publication barrier", err)
		}
		if !snapshot.RegistryReady {
			return false
		}
		current := ""
		for _, text := range f.ids {
			id, err := domain.ParseEntityID(text)
			if err != nil {
				t.Fatal(err)
			}
			record, present := snapshot.Registry[id]
			capabilities := snapshot.Capabilities[id]
			observed, observedPresent := snapshot.States[id]
			if !present || record.Retired() || !observedPresent || len(capabilities) != 1 || capabilities[0] == nil || capabilities[0].SchemaId != contracts.SwitchSchema || capabilities[0].Endpoint != "mqtt" {
				return false
			}
			_, _, _, stamp, _, err := observed.Components()
			if err != nil {
				t.Fatal("native MQTT state stamp", err)
			}
			session, _, err := stamp.Components()
			if err != nil {
				t.Fatal("native MQTT source session", err)
			}
			if session == previous || (current != "" && current != session) {
				return false
			}
			current = session
		}
		admitted = current
		return admitted != ""
	})
	return admitted
}

func (f *nha114Fixture) waitBindings(t *testing.T) {
	t.Helper()
	nha114Eventually(t, "ten actual MQTT bindings/states were not admitted", func() bool {
		v := f.view(t)
		found := 0
		for _, e := range v.Entities {
			for _, id := range f.ids {
				if e.Entity.GetName() == f.entityName(t, id) && len(e.Capabilities) == 1 && e.Capabilities[0].GetPower().GetObservation() != nil {
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
func nha114RequestID(label string) string {
	digest := sha256.Sum256([]byte(label))
	digest[6] = (digest[6] & 15) | 64
	digest[8] = (digest[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", digest[:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16])
}
func (f *nha114Fixture) command(t *testing.T, label, alias string, on bool) *api.ExecuteCapabilityRequest {
	t.Helper()
	entity := f.aliasName(t, alias)
	for _, entry := range f.view(t).Entities {
		if entry.Entity.GetName() != entity {
			continue
		}
		for _, capability := range entry.Capabilities {
			if capability.GetPower() == nil && capability.GetHelper() == nil {
				continue
			}
			value := api.PowerValue_POWER_VALUE_OFF
			if on {
				value = api.PowerValue_POWER_VALUE_ON
			}
			return &api.ExecuteCapabilityRequest{Name: capability.Name, Etag: capability.Etag, RequestId: nha114RequestID(label), ExpireTime: timestamppb.New(time.Now().Add(time.Minute)), Command: &api.ExecuteCapabilityRequest_Power{Power: &api.SetPowerCommand{Value: value}}}
		}
	}
	t.Fatal("fixture capability not publicly advertised", alias)
	return nil
}
func (f *nha114Fixture) submit(t *testing.T, command *api.ExecuteCapabilityRequest) *longrunningpb.Operation {
	t.Helper()
	result, err := f.capabilities.ExecuteCapability(f.ctx, nha056SessionRequest(command, f.token))
	if err != nil {
		t.Fatal("actual API durable dispatch", err)
	}
	if result == nil || result.Msg == nil || result.Msg.Name == "" {
		t.Fatal("actual API durable dispatch returned no operation")
	}
	metadata := nha114Metadata(t, result.Msg)
	if metadata.RequestId != command.RequestId || metadata.Capability != command.Name || result.Msg.GetError() != nil {
		t.Fatal("operation changed admitted intention", result.Msg)
	}
	f.operationNames[command.RequestId] = result.Msg.Name
	return result.Msg
}
func nha114Metadata(t *testing.T, operation *longrunningpb.Operation) *api.CommandMetadata {
	t.Helper()
	metadata := new(api.CommandMetadata)
	if operation == nil || operation.Metadata == nil {
		t.Fatal("operation metadata absent")
	}
	if err := operation.Metadata.UnmarshalTo(metadata); err != nil {
		t.Fatal("operation metadata", err)
	}
	return metadata
}
func (f *nha114Fixture) progress(t *testing.T, id string) *longrunningpb.Operation {
	t.Helper()
	name := f.operationNames[id]
	if name == "" {
		t.Fatal("no admitted operation for request", id)
	}
	response, err := f.operations.GetOperation(f.ctx, nha056SessionRequest(&longrunningpb.GetOperationRequest{Name: name}, f.token))
	if err != nil {
		t.Fatal("read existing operation", err)
	}
	return response.Msg
}
func (f *nha114Fixture) durableResponse(t *testing.T, id string) []byte {
	t.Helper()
	name, err := apitypes.ParseOperationName(f.operationNames[id])
	if err != nil {
		t.Fatal(err)
	}
	native, err := domain.ParseCommandID(strings.TrimPrefix(name.ID(), "cmd-"))
	if err != nil {
		t.Fatal(err)
	}
	var response []byte
	nha114Eventually(t, "original durable dispatch result did not commit", func() bool {
		record, found, err := f.actual.store.CommandIntent(f.ctx, native)
		if err != nil || !found {
			t.Fatal("original durable occurrence unavailable", err)
		}
		if record.State() == storage.IntentAccepted || record.State() == storage.IntentDispatching {
			return false
		}
		if record.State() != storage.IntentResult || record.ErrorKind() != storage.IntentNoError {
			t.Fatal("original durable dispatch failed", record.State(), record.ErrorKind())
		}
		response = record.Response()
		return len(response) > 0
	})
	return response

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
func (f *nha114Fixture) applySource(t *testing.T, family planwire.ConfigurationPlanSourceFamily, identity, id, entry string, raw []byte) {
	t.Helper()
	before := f.read(t)
	candidate := &planwire.ConfigurationPlanCandidate{Family: family, Identity: identity, Entry: entry, Files: []*planwire.ConfigurationPlanSourceFile{{LogicalPath: entry, ExactSource: raw}}}
	plan := f.build(t, candidate)
	if plan.State != planwire.ConfigurationPlanState_CONFIGURATION_PLAN_STATE_ADMITTED || plan.Receipt == "" {
		t.Fatalf("source plan was not admitted: family=%s state=%s diagnostics=%v", family, plan.State, plan.Diagnostics)
	}
	request := nha055Request(before, candidate, plan)
	request.Sources = []*applywire.SourcePrecondition{nha055PublicationExpected(id, entry, nil, nil)}
	if err := contracts.AdmitConfigurationApplyRequest(request); err != nil {
		t.Fatal("fixture produced an invalid Apply request", family, err)
	}
	r, err := f.apply.ApplyConfigurationPlan(f.ctx, nha056SessionRequest(request, f.token))
	if err != nil || !r.Msg.Done || r.Msg.GetError() != nil || f.read(t).ActiveGeneration.Revision != before.ActiveGeneration.Revision+1 {
		t.Fatal("actual source publication", err)
	}
	f.refreshPublicView(t)
}
func (f *nha114Fixture) installAutomation(t *testing.T) {
	t.Helper()
	f.applySource(t, planwire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DECLARATIVE_V2, "nha114.definitions", "nha114.definitions", "definitions.yaml", []byte("input_boolean:\n  away_mode: {name: NHA114 synthetic helper, initial: false}\ninput_button:\n  trash_out: {name: NHA114 synthetic button}\n"))
	raw := fmt.Sprintf("id: nha114_vertical\ntriggers:\n - trigger: state\n   entity_id: %s\n   from: 'off'\n   to: 'on'\nactions:\n - stop: complete\n", nha114Aliases[0])
	f.applySource(t, planwire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_AUTOMATION, "automation.turn_on_lights_at_sunset", "nha114.automation", "automation.yaml", []byte(raw))
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

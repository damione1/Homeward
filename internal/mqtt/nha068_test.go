package mqtt

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/eclipse/paho.golang/paho"
)

type nha068Provider struct {
	instance domain.IntegrationInstanceID
	password string
	username string
}

func (p nha068Provider) Read(_ context.Context, handle storage.SecretHandle) (storage.SecretValue, error) {
	instance, name, err := handle.Components()
	if err != nil || instance != p.instance {
		return storage.SecretValue{}, ErrInvalidSettings
	}
	value, err := name.Value()
	if err != nil {
		return storage.SecretValue{}, err
	}
	switch value {
	case "mqtt-username":
		username := p.username
		if username == "" {
			username = testbroker.PilotUser
		}
		return storage.NewSecretValue([]byte(username))
	case "mqtt-password":
		return storage.NewSecretValue([]byte(p.password))
	default:
		return storage.SecretValue{}, ErrInvalidSettings
	}
}

type nha068Host struct {
	provider nha068Provider
	health   chan integrations.HealthReport
}

func (h *nha068Host) Discover(context.Context, []integrations.Candidate) ([]integrations.Binding, error) {
	return nil, ErrInvalidSettings
}
func (h *nha068Host) Publish(context.Context, integrations.Binding, integrations.StateObservation) error {
	return ErrInvalidSettings
}
func (h *nha068Host) Emit(context.Context, integrations.Binding, integrations.DeviceEvent) error {
	return ErrInvalidSettings
}
func (h *nha068Host) ReportHealth(_ context.Context, r integrations.HealthReport) error {
	select {
	case h.health <- r:
	default:
	}
	return nil
}
func (h *nha068Host) Secrets() (integrations.SecretProvider, error) { return h.provider, nil }

type nha068Tasks struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	errors chan error
}

func newNHA068Tasks() *nha068Tasks {
	ctx, cancel := context.WithCancel(context.Background())
	return &nha068Tasks{ctx: ctx, cancel: cancel, errors: make(chan error, 8)}
}
func (g *nha068Tasks) Go(task integrations.Task) error {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if err := task(g.ctx); err != nil {
			select {
			case g.errors <- err:
			default:
			}
		}
	}()
	return nil
}
func (g *nha068Tasks) close(t *testing.T, p *Pilot) {
	t.Helper()
	g.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := p.Stop(ctx); err != nil {
		t.Errorf("pilot stop: %v", err)
	}
	g.wg.Wait()
}

type nha068Capture struct{ observations chan Observation }

func (h nha068Capture) Observe(ctx context.Context, _ integrations.SessionHost, observation Observation) error {
	select {
	case h.observations <- observation:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func nha068Settings(t *testing.T, address string, mode Mode) Settings {
	t.Helper()
	label := "passive"
	input := Input{Profile: "5", Broker: "mqtt://" + address, Instance: "nha068-test-instance", Mode: label, UsernameSecret: "mqtt-username", PasswordSecret: "mqtt-password", Subscriptions: []string{"nha068/state/#"}}
	if mode == ModeActive {
		input.Mode = "active"
		input.CommandTopics = []string{"nha068/command/#"}
		input.Will = &WillInput{Topic: "nha068/presence/pilot", Payload: "offline"}
	}
	settings, err := ParseSettings(input)
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

func nha068Start(t *testing.T, settings Settings, handler ObservationHandler) (*Pilot, *nha068Tasks) {
	t.Helper()
	p, err := NewPilot(settings, handler)
	if err != nil {
		t.Fatal(err)
	}
	tasks := newNHA068Tasks()
	host := &nha068Host{provider: nha068Provider{instance: settings.instance, password: testbroker.PilotPassword}, health: make(chan integrations.HealthReport, 4)}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := p.Start(ctx, host, tasks); err != nil {
		tasks.cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { tasks.close(t, p) })
	return p, tasks
}

func nha068Observation(t *testing.T, ch <-chan Observation) Observation {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(3 * time.Second):
		t.Fatal("observation absent")
		return Observation{}
	}
}

func TestNHA068RetainedSnapshotIsStaleAndLiveRetainedPublishIsFresh(t *testing.T) {
	broker := testbroker.Start(t)
	broker.Publish(t, "nha068/state/device", []byte("snapshot"), true)
	settings := nha068Settings(t, broker.Address, ModePassive)
	h := nha068Capture{observations: make(chan Observation, 8)}
	pilot, _ := nha068Start(t, settings, h)
	commandTopic, _ := ParseTopic("nha068/command/relay")
	command, _ := NewCommandPublication(commandTopic, []byte("blocked"))
	if err := pilot.WriteCommand(context.Background(), command); !errors.Is(err, ErrPassiveMode) {
		t.Fatalf("passive command admitted: %v", err)
	}
	old := nha068Observation(t, h.observations)
	_, payload, q, err := old.Record()
	if err != nil || string(payload) != "snapshot" {
		t.Fatalf("snapshot = %q %v", payload, err)
	}
	a, f, p, err := q.Components()
	if err != nil || a != domain.AvailabilityAvailable || f != domain.FreshnessStale || p != domain.ProvenanceRestored {
		t.Fatalf("retained quality = %v/%v/%v %v", a, f, p, err)
	}
	if _, _, err := old.Event(); !errors.Is(err, ErrNotAnEvent) {
		t.Fatalf("retained edge admitted: %v", err)
	}
	broker.Publish(t, "nha068/state/device", []byte("live"), true)
	live := nha068Observation(t, h.observations)
	_, payload, q, err = live.Record()
	if err != nil || string(payload) != "live" {
		t.Fatalf("live = %q %v", payload, err)
	}
	_, f, p, err = q.Components()
	if err != nil || f != domain.FreshnessFresh || p != domain.ProvenanceObserved {
		t.Fatalf("live quality = %v/%v %v", f, p, err)
	}
	if _, payload, err := live.Event(); err != nil || string(payload) != "live" {
		t.Fatalf("live edge = %q %v", payload, err)
	}
}

func TestNHA068CleanStartDropsRealOfflineSessionBacklogAndRestoresSubscriptions(t *testing.T) {
	broker := testbroker.Start(t)
	settings := nha068Settings(t, broker.Address, ModePassive)
	old := broker.Connect(t, testbroker.PilotUser, testbroker.PilotPassword, settings.clientID, false, 60, nil)
	_ = old.Subscribe(t, "nha068/state/#")
	if err := old.Client.Disconnect(&paho.Disconnect{ReasonCode: 0}); err != nil {
		t.Fatal(err)
	}
	broker.Publish(t, "nha068/state/device", []byte("offline-old-appui"), false)
	deadline := time.Now().Add(2 * time.Second)
	for {
		stored, ok := broker.Server.Clients.Get(settings.clientID)
		if ok && stored.State.Inflight.Len() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("broker did not retain offline QoS1 publication")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h := nha068Capture{observations: make(chan Observation, 8)}
	p, tasks := nha068Start(t, settings, h)
	broker.Publish(t, "nha068/state/device", []byte("live-one"), false)
	got := nha068Observation(t, h.observations)
	_, payload, _, _ := got.Record()
	if string(payload) != "live-one" {
		t.Fatalf("offline backlog replayed: %q", payload)
	}
	if _, edge, err := got.Event(); err != nil || string(edge) != "live-one" {
		t.Fatalf("first live edge: %q %v", edge, err)
	}
	broker.Publish(t, "nha068/state/device", []byte("barrier-one"), false)
	got = nha068Observation(t, h.observations)
	_, payload, _, _ = got.Record()
	if string(payload) != "barrier-one" {
		t.Fatalf("pre-barrier backlog replayed: %q", payload)
	}
	select {
	case extra := <-h.observations:
		t.Fatalf("unexpected old observation after barrier: %#v", extra)
	default:
	}
	// A network break loses the zero-expiry session. The next pilot instance
	// resubscribes, and no stale QoS1 message is delivered as an edge.
	p.fail(ErrBrokerConnection)
	select {
	case <-p.client.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Paho did not join after loss")
	}
	tasks.close(t, p)
	broker.Publish(t, "nha068/state/device", []byte("offline-after-loss"), false)
	h2 := nha068Capture{observations: make(chan Observation, 8)}
	_, _ = nha068Start(t, settings, h2)
	broker.Publish(t, "nha068/state/device", []byte("live-two"), false)
	got = nha068Observation(t, h2.observations)
	_, payload, _, _ = got.Record()
	if string(payload) != "live-two" {
		t.Fatalf("old message after reconnect: %q", payload)
	}
	broker.Publish(t, "nha068/state/device", []byte("barrier-two"), false)
	got = nha068Observation(t, h2.observations)
	_, payload, _, _ = got.Record()
	if string(payload) != "barrier-two" {
		t.Fatalf("old message before reconnect barrier: %q", payload)
	}
	select {
	case extra := <-h2.observations:
		t.Fatalf("unexpected old observation after reconnect barrier: %#v", extra)
	default:
	}
}

func TestNHA068CredentialsAndBrokerACL(t *testing.T) {
	broker := testbroker.Start(t)
	settings := nha068Settings(t, broker.Address, ModePassive)
	p, err := NewPilot(settings, nha068Capture{observations: make(chan Observation, 1)})
	if err != nil {
		t.Fatal(err)
	}
	host := &nha068Host{provider: nha068Provider{instance: settings.instance, password: "bad-canary"}, health: make(chan integrations.HealthReport, 1)}
	tasks := newNHA068Tasks()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Start(ctx, host, tasks); !errors.Is(err, ErrBrokerConnection) {
		t.Fatalf("bad credential: %v", err)
	}
	tasks.close(t, p)
	peer := broker.Connect(t, testbroker.PilotUser, testbroker.PilotPassword, "homeward-acl-probe", true, 0, nil)
	pubCtx, pubCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer pubCancel()
	response, err := peer.Client.Publish(pubCtx, &paho.Publish{Topic: "nha068/forbidden/device", Payload: []byte("blocked"), QoS: 1})
	if response == nil || response.ReasonCode < 0x80 {
		t.Fatalf("broker ACL must reject at PUBACK: %#v %v", response, err)
	}
	if _, err := ParseSettings(Input{Profile: "3.1.1"}); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("3.1.1 silently admitted: %v", err)
	}
}

func TestNHA068ActiveWillAndPhysicalCommandWirePolicy(t *testing.T) {
	broker := testbroker.Start(t)
	observer := broker.Connect(t, testbroker.ObserverUser, testbroker.ObserverPassword, "nha068-observer-wire", true, 0, nil)
	messages := observer.SubscribeRAP(t, true, "nha068/presence/#", "nha068/command/#")
	settings := nha068Settings(t, broker.Address, ModeActive)
	p, _ := nha068Start(t, settings, nha068Capture{observations: make(chan Observation, 2)})
	topic, err := ParseTopic("nha068/command/relay")
	if err != nil {
		t.Fatal(err)
	}
	command, err := NewCommandPublication(topic, []byte("synthetic-command"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.WriteCommand(ctx, command); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-messages:
		if got.Topic != "nha068/command/relay" || got.QoS != 0 || got.Retain || string(got.Payload) != "synthetic-command" {
			t.Fatalf("physical command wire packet: %#v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("command not received by synthetic observer")
	}
	if _, retained := broker.Server.Topics.Retained.Get("nha068/command/relay"); retained {
		t.Fatal("physical command stored as retained")
	}
	if count := broker.PublishCount(settings.clientID); count != 1 {
		t.Fatalf("physical command publication count %d", count)
	}
	// Prove the oracle would catch a retained physical command. This separate
	// synthetic client deliberately sends the forbidden flag to the same ACL.
	probe := broker.Connect(t, testbroker.PilotUser, testbroker.PilotPassword, "nha068-retain-probe", true, 0, nil)
	response, err := probe.Client.Publish(ctx, &paho.Publish{Topic: "nha068/command/retain-probe", Payload: []byte("probe"), QoS: 1, Retain: true})
	if err != nil || response == nil || response.ReasonCode >= 0x80 {
		t.Fatalf("oracle mutation publish: %#v %v", response, err)
	}
	select {
	case got := <-messages:
		if got.Topic != "nha068/command/retain-probe" || !got.Retain {
			t.Fatalf("RAP oracle missed retained mutation: %#v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("retained mutation not observed")
	}
	if _, retained := broker.Server.Topics.Retained.Get("nha068/command/retain-probe"); !retained {
		t.Fatal("mutation missing in retained table")
	}
	p.fail(ErrBrokerConnection)
	select {
	case got := <-messages:
		if got.Topic != "nha068/presence/pilot" || got.QoS != 1 || got.Retain || string(got.Payload) != "offline" {
			t.Fatalf("active LWT packet: %#v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("active LWT missing on forced socket loss")
	}
	if _, retained := broker.Server.Topics.Retained.Get("nha068/presence/pilot"); retained {
		t.Fatal("active LWT retained")
	}
}

func TestNHA068SettingsRejectUnsafeProfilesAndTopics(t *testing.T) {
	input := Input{Profile: "5", Broker: "mqtt://127.0.0.1:1883", Instance: "test", Mode: "passive", UsernameSecret: "user", PasswordSecret: "pass", Subscriptions: []string{"nha068/state/#"}}
	for name, mutate := range map[string]func(*Input){
		"url credentials": func(i *Input) { i.Broker = "mqtt://user:pass@127.0.0.1:1883" },
		"unsafe wildcard": func(i *Input) { i.Subscriptions = []string{"nha068/state#"} },
		"will passive":    func(i *Input) { i.Will = &WillInput{Topic: "nha068/presence/pilot", Payload: "offline"} },
		"command passive": func(i *Input) { i.CommandTopics = []string{"nha068/command/#"} },
		"zero instance":   func(i *Input) { i.Instance = "" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := input
			mutate(&bad)
			if _, err := ParseSettings(bad); err == nil {
				t.Fatal("unsafe config admitted")
			}
		})
	}
	if _, err := NewPilot(Settings{}, nha068Capture{}); err == nil {
		t.Fatal("zero settings admitted")
	}
	var nilHandler *nha068Capture
	if _, err := NewPilot(nha068Settings(t, "127.0.0.1:1883", ModePassive), nilHandler); err == nil {
		t.Fatal("typed nil handler admitted")
	}
	global, _ := ParseTopicFilter("#")
	sys, _ := ParseTopic("$SYS/broker/uptime")
	if global.Matches(sys) {
		t.Fatal("root wildcard matched reserved $ topic")
	}
	specific, _ := ParseTopicFilter("$SYS/#")
	if !specific.Matches(sys) {
		t.Fatal("specific $ topic filter rejected")
	}
}

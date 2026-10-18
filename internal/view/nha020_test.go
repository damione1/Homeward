package view_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/internal/view"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *testClock) Advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

type testAuthority struct {
	mu     sync.Mutex
	grant  view.Grant
	config view.ConfigStamp
}

type blockingAuthorizer struct {
	entered chan struct{}
	release chan struct{}
	grant   view.Grant
}

func (a *blockingAuthorizer) CurrentGrant(ctx context.Context, _ view.ActorID) (view.Grant, error) {
	select {
	case a.entered <- struct{}{}:
	case <-ctx.Done():
		return view.Grant{}, ctx.Err()
	}
	select {
	case <-a.release:
		return a.grant, nil
	case <-ctx.Done():
		return view.Grant{}, ctx.Err()
	}
}

func (a *testAuthority) CurrentGrant(_ context.Context, _ view.ActorID) (view.Grant, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.grant, nil
}
func (a *testAuthority) CurrentConfig(_ context.Context) (view.ConfigStamp, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.config, nil
}
func (a *testAuthority) SetGrant(g view.Grant)        { a.mu.Lock(); a.grant = g; a.mu.Unlock() }
func (a *testAuthority) SetConfig(c view.ConfigStamp) { a.mu.Lock(); a.config = c; a.mu.Unlock() }

type projectedSink struct {
	sequence      *sequencer.Sequencer
	mu            sync.Mutex
	failDiscovery bool
	failRegistry  bool
}

func (s *projectedSink) AcceptState(ctx context.Context, id domain.EntityID, state integrations.AcceptedState) error {
	return s.sequence.AcceptState(ctx, id, state)
}
func (s *projectedSink) AcceptEvent(ctx context.Context, id domain.EntityID, event integrations.AcceptedEvent) error {
	return s.sequence.AcceptEvent(ctx, id, event)
}
func (s *projectedSink) AcceptHealth(ctx context.Context, id domain.IntegrationInstanceID, health integrations.HealthReport) error {
	return s.sequence.AcceptHealth(ctx, id, health)
}
func (s *projectedSink) AcceptDiscovery(ctx context.Context, items []integrations.DiscoveryDeclaration) error {
	s.mu.Lock()
	fail := s.failDiscovery
	s.failDiscovery = false
	s.mu.Unlock()
	if fail {
		return errors.New("injected discovery failure")
	}
	return s.sequence.AcceptDiscovery(ctx, items)
}
func (s *projectedSink) PublishRegistry(ctx context.Context, items []integrations.RegistryRecord) error {
	s.mu.Lock()
	fail := s.failRegistry
	s.failRegistry = false
	s.mu.Unlock()
	if fail {
		return errors.New("injected post-commit publication failure")
	}
	return s.sequence.PublishRegistry(ctx, items)
}
func (s *projectedSink) InvalidateRegistry() { s.sequence.InvalidateRegistry() }

func setup(t *testing.T) (*storage.Store, context.Context, *registry.Repository, *sequencer.Sequencer, *testClock, registry.Manifest, []string) {
	t.Helper()
	data, err := os.ReadFile("../../scope/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := registry.ParseManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Entities []struct {
			ID string `json:"entity_id"`
		} `json:"entities"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	aliases := make([]string, len(raw.Entities))
	for i, e := range raw.Entities {
		aliases[i] = e.ID
	}
	cluster := testpg.Start(t)
	settings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	repo, err := registry.NewRepository(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	clock := &testClock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	seq, err := sequencer.New(clock, 4096)
	if err != nil {
		t.Fatal(err)
	}
	return store, ctx, repo, seq, clock, manifest, aliases
}

func testConfig(t *testing.T, revision uint64, text string) view.ConfigStamp {
	t.Helper()
	c, err := view.NewConfigStamp(revision, sha256.Sum256([]byte(text)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func testActor(t *testing.T, text string) view.ActorID {
	t.Helper()
	a, err := view.ParseActorID(text)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func testGrant(t *testing.T, a view.ActorID, revision uint64, ids []domain.EntityID) view.Grant {
	t.Helper()
	g, err := view.NewGrant(a, revision, ids)
	if err != nil {
		t.Fatal(err)
	}
	return g
}
func testCandidate(t *testing.T, key integrations.SourceKey, name string) integrations.Candidate {
	t.Helper()
	c, err := integrations.NewCandidate(key, []*wireregistry.CapabilityDescription{{SchemaId: contracts.SwitchSchema, Major: 1, DisplayName: name, Endpoint: "relay", Constraints: &wireregistry.CapabilityDescription_Switch{Switch: &wireregistry.SwitchConstraints{SetOn: false}}}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNHA020EightHundredFiftyBindingsAndTwentyMutations(t *testing.T) {
	_, ctx, repo, seq, clock, _, aliases := setup(t)
	instance, _ := domain.ParseIntegrationInstanceID("fixture-850")
	candidates := make([]integrations.Candidate, 850)
	for i := 0; i < 850; i++ {
		key, err := integrations.NewSourceKey(fmt.Sprintf("fixture-device-%03d", i), "relay")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.ConfigureBinding(ctx, instance, key, aliases[i]); err != nil {
			t.Fatalf("configure %d: %v", i, err)
		}
		candidates[i] = testCandidate(t, key, "Fixture relay")
	}
	if err := repo.AttachProjection(ctx, seq); err != nil {
		t.Fatal(err)
	}
	catalog, _ := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	host, err := integrations.NewHost(repo, seq, catalog)
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	bindings := make([]integrations.Binding, 0, 850)
	for start := 0; start < 850; start += 250 {
		end := start + 250
		if end > 850 {
			end = 850
		}
		batch, err := session.Discover(ctx, candidates[start:end])
		if err != nil {
			t.Fatalf("discover %d: %v", start, err)
		}
		bindings = append(bindings, batch...)
	}
	initialCursor, _ := seq.Start()
	metadataEntries, _, err := seq.ReadAfter(initialCursor, 5)
	if err != nil || len(metadataEntries) != 5 {
		t.Fatalf("registry/capability journal: %d %v", len(metadataEntries), err)
	}
	if metadataEntries[0].Kind() != sequencer.KindRegistry {
		t.Fatal("registry publication missing")
	}
	for i, entry := range metadataEntries {
		if _, _, _, present := entry.Origin(); present {
			t.Fatalf("metadata entry %d invented integration origin", i)
		}
	}
	ids := make([]domain.EntityID, 850)
	for i, b := range bindings {
		_, id, err := b.IDs()
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	initial := domain.NewSwitchValue(false)
	for _, binding := range bindings {
		at := clock.Now()
		observation, err := integrations.NewStateObservation("relay", quality, &initial, &at, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Publish(ctx, binding, observation); err != nil {
			t.Fatal(err)
		}
	}
	actor := testActor(t, "test-user")
	authority := &testAuthority{grant: testGrant(t, actor, 1, ids), config: testConfig(t, 1, "generation-1")}
	service, err := view.NewService(seq, authority, authority, clock)
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.OpenView(ctx, actor, view.All(), 100)
	if err != nil {
		t.Fatal(err)
	}
	wirePage, err := view.EncodePage(page)
	if err != nil || len(wirePage.GetEntities()) != 100 || len(wirePage.GetCursor().GetConfigSha256()) != 32 {
		t.Fatalf("wire page: %d %v", len(wirePage.GetEntities()), err)
	}
	for _, entity := range wirePage.GetEntities() {
		if entity.GetEntityId() == "" || len(entity.GetCapabilities()) != 1 {
			t.Fatal("wire identity/capability association lost")
		}
	}
	startCursor := page.Cursor
	seen := make(map[domain.EntityID]bool, 850)
	published := 0
	for {
		if page.Cursor != startCursor {
			t.Fatal("mixed snapshot cursors")
		}
		for _, item := range page.Items {
			_, id, err := item.Registry().IDs()
			if err != nil {
				t.Fatal(err)
			}
			if seen[id] {
				t.Fatal("duplicate entity in snapshot pages")
			}
			seen[id] = true
			state, ok := item.State()
			if !ok {
				t.Fatal("seeded state absent from snapshot")
			}
			stateID, _, _, _, revision, err := state.Components()
			if err != nil || stateID != id || revision != 1 {
				t.Fatalf("mixed state identity/revision: %v %d %v", stateID, revision, err)
			}
			value, ok := state.CurrentValue()
			if !ok {
				t.Fatal("seeded state value absent")
			}
			on, isSwitch := value.Switch()
			if !isSwitch || on {
				t.Fatal("post-capture mutation changed frozen state")
			}
			if len(item.Capabilities()) != 1 {
				t.Fatal("capability missing from captured binding")
			}
		}
		if page.NextPageToken == "" {
			break
		}
		for j := 0; j < 3 && published < 20; j++ {
			changed := domain.NewSwitchValue(true)
			at := clock.Now()
			observation, err := integrations.NewStateObservation("relay", quality, &changed, &at, at, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := session.Publish(ctx, bindings[published], observation); err != nil {
				t.Fatal(err)
			}
			published++
		}
		page, err = service.ReadPage(ctx, actor, page.SnapshotToken, page.NextPageToken)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 850 || published != 20 {
		t.Fatalf("seen=%d mutations-during-pagination=%d", len(seen), published)
	}
	for _, id := range ids {
		if !seen[id] {
			t.Fatal("expected registry identity absent from pages")
		}
	}
	if len(seen) != len(ids) {
		t.Fatal("unexpected registry identity in pages")
	}
	for client := 1; client < 10; client++ {
		parallelPage, err := service.OpenView(ctx, actor, view.All(), 100)
		if err != nil || len(parallelPage.Items) != 100 {
			t.Fatalf("client %d view: %d %v", client, len(parallelPage.Items), err)
		}
	}
	sequenceCursor, _, _, _, err := startCursor.Components()
	if err != nil {
		t.Fatal(err)
	}
	entries, end, err := seq.ReadAfter(sequenceCursor, 32)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 20 {
		t.Fatalf("post-snapshot suffix: %d entries", len(entries))
	}
	_, startSeq, _ := sequenceCursor.Components()
	_, endSeq, _ := end.Components()
	if endSeq-startSeq != 20 {
		t.Fatalf("cursor hole %d..%d", startSeq, endSeq)
	}
	for i, entry := range entries {
		if entry.Kind() != sequencer.KindState {
			t.Fatalf("suffix entry %d kind %v", i, entry.Kind())
		}
		id, state, _, ok := entry.State()
		if !ok || id != ids[i] {
			t.Fatalf("suffix identity %d", i)
		}
		_, _, _, _, revision, err := state.Components()
		if err != nil || revision != 2 {
			t.Fatalf("suffix revision %d: %d %v", i, revision, err)
		}
	}
}

func TestNHA020AuthorityExpiryAndProjectionFailures(t *testing.T) {
	store, ctx, repo, seq, clock, _, aliases := setup(t)
	instance, _ := domain.ParseIntegrationInstanceID("fixture-one")
	key, _ := integrations.NewSourceKey("fixture-device", "relay")
	identity, err := repo.ConfigureBinding(ctx, instance, key, aliases[0])
	if err != nil {
		t.Fatal(err)
	}
	sink := &projectedSink{sequence: seq}
	if err := repo.AttachProjection(ctx, sink); err != nil {
		t.Fatal(err)
	}
	catalog, _ := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	host, err := integrations.NewHost(repo, sink, catalog)
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	candidate := testCandidate(t, key, "First")
	sink.mu.Lock()
	sink.failDiscovery = true
	sink.mu.Unlock()
	if _, err := session.Discover(ctx, []integrations.Candidate{candidate}); err == nil {
		t.Fatal("injected discovery failure admitted")
	}
	if descriptions, err := catalog.Descriptions(identity.Entity); err != nil || len(descriptions) != 0 {
		t.Fatalf("catalog changed on failed discovery: %d %v", len(descriptions), err)
	}
	if capture, err := seq.Capture(); err != nil || len(capture.Capabilities) != 0 {
		t.Fatalf("sequencer changed on failed discovery: %v", err)
	}
	bindings, err := session.Discover(ctx, []integrations.Candidate{candidate})
	if err != nil {
		t.Fatal(err)
	}
	actor := testActor(t, "user-one")
	other := testActor(t, "other-user")
	authority := &testAuthority{grant: testGrant(t, actor, 1, []domain.EntityID{identity.Entity}), config: testConfig(t, 1, "config-one")}
	service, err := view.NewService(seq, authority, authority, clock)
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.OpenView(ctx, actor, view.All(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || len(page.Items[0].Capabilities()) != 1 {
		t.Fatal("initial projection incomplete")
	}
	if _, err := service.ReadPage(ctx, other, page.SnapshotToken, "bogus"); !errors.Is(err, view.ErrSnapshotUnknown) {
		t.Fatalf("cross-actor token: %v", err)
	}
	// A pre-commit alias collision leaves the old projection available.
	if err := repo.Rename(ctx, identity.Entity, aliases[1]); !errors.Is(err, registry.ErrAliasConflict) {
		t.Fatalf("expected reserved alias collision: %v", err)
	}
	if _, err := seq.Capture(); err != nil {
		t.Fatalf("pre-commit failure invalidated projection: %v", err)
	}
	// A committed rename whose publication fails closes both new captures and
	// further identity admission until the SQL authority is reloaded.
	sink.mu.Lock()
	sink.failRegistry = true
	sink.mu.Unlock()
	if err := repo.Rename(ctx, identity.Entity, "switch.new_alias"); err == nil {
		t.Fatal("publication failure accepted")
	}
	if _, err := seq.Capture(); !errors.Is(err, sequencer.ErrRegistryUnavailable) {
		t.Fatalf("old projection still active: %v", err)
	}
	if _, err := repo.Resolve(ctx, instance, []integrations.SourceKey{key}); !errors.Is(err, registry.ErrProjectionUnavailable) {
		t.Fatalf("dirty resolve: %v", err)
	}
	if err := repo.ReloadProjection(ctx); err != nil {
		t.Fatal(err)
	}
	newPage, err := service.OpenView(ctx, actor, view.All(), 1)
	if err != nil {
		t.Fatal(err)
	}
	_, current, _, err := newPage.Items[0].Registry().Aliases()
	if err != nil {
		t.Fatal(err)
	}
	label, _ := current.Value()
	if label != "switch.new_alias" {
		t.Fatalf("reconciled alias: %s", label)
	}
	// Discovery replacement advances the same cursor and keeps old page immutable.
	changed := testCandidate(t, key, "Changed")
	if _, err := session.Discover(ctx, []integrations.Candidate{changed}); err != nil {
		t.Fatal(err)
	}
	latest, err := service.OpenView(ctx, actor, view.All(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if page.Items[0].Capabilities()[0].DisplayName != "First" || latest.Items[0].Capabilities()[0].DisplayName != "Changed" {
		t.Fatal("capability generation mixed")
	}
	if err := repo.Tombstone(ctx, identity.Entity); err != nil {
		t.Fatal(err)
	}
	retired, err := service.OpenView(ctx, actor, view.All(), 1)
	if err != nil || len(retired.Items) != 0 {
		t.Fatalf("tombstone leaked into new view: %d %v", len(retired.Items), err)
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	value := domain.NewSwitchValue(true)
	at := clock.Now()
	observation, _ := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err := session.Publish(ctx, bindings[0], observation); !errors.Is(err, sequencer.ErrIdentityConflict) {
		t.Fatalf("retired publication: %v", err)
	}
	// Page token order, rights/config changes and TTL are checked independently
	// with a second active binding.
	key2, _ := integrations.NewSourceKey("fixture-device-2", "relay")
	identity2, err := repo.ConfigureBinding(ctx, instance, key2, aliases[2])
	if err != nil {
		t.Fatal(err)
	}
	bindings2, err := session.Discover(ctx, []integrations.Candidate{testCandidate(t, key2, "Second")})
	if err != nil {
		t.Fatal(err)
	}
	authority.SetGrant(testGrant(t, actor, 2, []domain.EntityID{identity2.Entity}))
	filtered, err := service.OpenView(ctx, actor, view.All(), 1)
	if err != nil || len(filtered.Items) != 1 {
		t.Fatalf("rights filter: %d %v", len(filtered.Items), err)
	}
	filter, err := view.Only([]domain.EntityID{identity.Entity})
	if err != nil {
		t.Fatal(err)
	}
	excluded, err := service.OpenView(ctx, actor, filter, 1)
	if err != nil || len(excluded.Items) != 0 {
		t.Fatalf("filter leaked: %d %v", len(excluded.Items), err)
	}
	// Add a third active entity to force a second page.
	key3, _ := integrations.NewSourceKey("fixture-device-3", "relay")
	identity3, err := repo.ConfigureBinding(ctx, instance, key3, aliases[3])
	if err != nil {
		t.Fatal(err)
	}
	_, err = session.Discover(ctx, []integrations.Candidate{testCandidate(t, key3, "Third")})
	if err != nil {
		t.Fatal(err)
	}
	authority.SetGrant(testGrant(t, actor, 3, []domain.EntityID{identity2.Entity}))
	restricted, err := service.OpenView(ctx, actor, view.All(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(restricted.Items) != 1 || restricted.NextPageToken != "" {
		t.Fatalf("active unauthorized entity leaked: %d", len(restricted.Items))
	}
	_, restrictedID, _ := restricted.Items[0].Registry().IDs()
	if restrictedID != identity2.Entity {
		t.Fatal("wrong authorized entity")
	}
	onlyThird, err := view.Only([]domain.EntityID{identity3.Entity})
	if err != nil {
		t.Fatal(err)
	}
	intersection, err := service.OpenView(ctx, actor, onlyThird, 1)
	if err != nil || len(intersection.Items) != 0 {
		t.Fatalf("filter-rights intersection leaked: %d %v", len(intersection.Items), err)
	}
	authority.SetGrant(testGrant(t, actor, 4, []domain.EntityID{identity2.Entity, identity3.Entity}))
	first, err := service.OpenView(ctx, actor, view.All(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if first.NextPageToken == "" {
		t.Fatal("expected another page")
	}
	if _, err := service.ReadPage(ctx, actor, first.SnapshotToken, "wrong"); !errors.Is(err, view.ErrPageToken) {
		t.Fatalf("wrong page token: %v", err)
	}
	second, err := service.ReadPage(ctx, actor, first.SnapshotToken, first.NextPageToken)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := service.ReadPage(ctx, actor, first.SnapshotToken, first.NextPageToken)
	if err != nil {
		t.Fatal(err)
	}
	if second.Cursor != retry.Cursor || second.NextPageToken != retry.NextPageToken || len(second.Items) != len(retry.Items) || !second.Items[0].Registry().Equal(retry.Items[0].Registry()) {
		t.Fatal("page retry changed result")
	}
	authority.SetConfig(testConfig(t, 2, "config-two"))
	if _, err := service.ReadPage(ctx, actor, first.SnapshotToken, first.NextPageToken); !errors.Is(err, view.ErrViewChanged) {
		t.Fatalf("changed config: %v", err)
	}
	first, err = service.OpenView(ctx, actor, view.All(), 1)
	if err != nil {
		t.Fatal(err)
	}
	authority.SetGrant(testGrant(t, actor, 5, []domain.EntityID{identity2.Entity}))
	if _, err := service.ReadPage(ctx, actor, first.SnapshotToken, first.NextPageToken); !errors.Is(err, view.ErrViewChanged) {
		t.Fatalf("changed rights: %v", err)
	}
	authority.SetGrant(testGrant(t, actor, 6, []domain.EntityID{identity2.Entity, identity3.Entity}))
	first, err = service.OpenView(ctx, actor, view.All(), 1)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(61 * time.Second)
	if _, err := service.ReadPage(ctx, actor, first.SnapshotToken, first.NextPageToken); !errors.Is(err, view.ErrSnapshotExpired) {
		t.Fatalf("expired page: %v", err)
	}
	if _, err := service.OpenView(ctx, actor, view.All(), 1); err != nil {
		t.Fatalf("new capture after expiry: %v", err)
	}
	store.Close()
	if err := repo.Rename(ctx, identity2.Entity, "switch.offline_alias"); err == nil {
		t.Fatal("rename unexpectedly committed after store close")
	}
	if _, err := seq.Capture(); err != nil {
		t.Fatalf("known pre-commit outage invalidated live projection: %v", err)
	}
	if err := session.Publish(ctx, bindings2[0], observation); err != nil {
		t.Fatalf("known pre-commit outage disabled live publication: %v", err)
	}
	if _, err := service.OpenView(ctx, actor, view.All(), 1); err != nil {
		t.Fatalf("memory projection required PostgreSQL: %v", err)
	}
}

func TestNHA020FilterEncodingAndZeroBoundaries(t *testing.T) {
	a := testActor(t, "actor")
	if reflect.TypeOf(a).ConvertibleTo(reflect.TypeOf(domain.EntityID{})) {
		t.Fatal("actor ID converts to entity ID")
	}
	if _, err := view.NewGrant(view.ActorID{}, 1, nil); !errors.Is(err, view.ErrInvalidInput) {
		t.Fatal(err)
	}
	if _, err := view.NewConfigStamp(0, sha256.Sum256([]byte("x"))); !errors.Is(err, view.ErrInvalidInput) {
		t.Fatal(err)
	}
	if _, err := view.NewConfigStamp(1, [32]byte{}); !errors.Is(err, view.ErrInvalidInput) {
		t.Fatal(err)
	}
	if _, err := view.ParseActorID(""); !errors.Is(err, view.ErrInvalidInput) {
		t.Fatal(err)
	}
	leftA, _ := domain.ParseEntityID("a\nb")
	leftB, _ := domain.ParseEntityID("c")
	rightA, _ := domain.ParseEntityID("a")
	rightB, _ := domain.ParseEntityID("b\nc")
	left, err := view.Only([]domain.EntityID{leftA, leftB})
	if err != nil {
		t.Fatal(err)
	}
	right, err := view.Only([]domain.EntityID{rightA, rightB})
	if err != nil {
		t.Fatal(err)
	}
	leftHash, _ := left.Digest()
	rightHash, _ := right.Digest()
	if leftHash == rightHash {
		t.Fatal("ambiguous filter encoding")
	}
	var nilAuthority *testAuthority
	clock := &testClock{now: time.Now()}
	seq, err := sequencer.New(clock, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := view.NewService(seq, nilAuthority, nilAuthority, clock); !errors.Is(err, view.ErrInvalidInput) {
		t.Fatal(err)
	}
	var nilClock *testClock
	if _, err := view.NewService(seq, &testAuthority{}, &testAuthority{}, nilClock); !errors.Is(err, view.ErrInvalidInput) {
		t.Fatal(err)
	}
	zero := &view.Service{}
	if _, err := zero.OpenView(context.Background(), a, view.All(), 1); !errors.Is(err, view.ErrInvalidInput) {
		t.Fatalf("zero service OpenView: %v", err)
	}
	if _, err := zero.ReadPage(context.Background(), a, "token", "page"); !errors.Is(err, view.ErrInvalidInput) {
		t.Fatalf("zero service ReadPage: %v", err)
	}
}

func TestNHA020BoundsConcurrentPreparations(t *testing.T) {
	clock := &testClock{now: time.Now()}
	seq, err := sequencer.New(clock, 8)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := seq.PublishRegistry(ctx, nil); err != nil {
		t.Fatal(err)
	}
	actor := testActor(t, "bounded-actor")
	config := &testAuthority{config: testConfig(t, 1, "bounded")}
	auth := &blockingAuthorizer{entered: make(chan struct{}, 4), release: make(chan struct{}), grant: testGrant(t, actor, 1, nil)}
	service, err := view.NewService(seq, auth, config, clock)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, err := service.OpenView(ctx, actor, view.All(), 1); results <- err }()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-auth.entered:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if _, err := service.OpenView(ctx, actor, view.All(), 1); !errors.Is(err, view.ErrResourceExhausted) {
		t.Fatalf("unbounded third preparation: %v", err)
	}
	close(auth.release)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

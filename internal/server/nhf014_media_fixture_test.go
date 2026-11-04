package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	imported "github.com/damione1/Homeward/compat/ha/dashboard"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	media "github.com/damione1/Homeward/gen/home/media/v1"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mediasessions"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type nhf014PlayerKind string

const (
	nhf014Minimal nhf014PlayerKind = "minimal"
	nhf014Group   nhf014PlayerKind = "group"
)

func (k *nhf014PlayerKind) UnmarshalJSON(raw []byte) error {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	switch nhf014PlayerKind(value) {
	case nhf014Minimal, nhf014Group:
		*k = nhf014PlayerKind(value)
		return nil
	default:
		return errors.New("unknown synthetic player kind")
	}
}

type nhf014PlayerSource struct {
	Kind       nhf014PlayerKind `json:"kind"`
	Alias      string           `json:"alias"`
	Title      string           `json:"title"`
	Artist     string           `json:"artist"`
	Collection string           `json:"collection"`
}

func nhf014ReadPlayers(root string) ([]nhf014PlayerSource, error) {
	file, err := os.Open(filepath.Join(root, "players.json"))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 8193))
	decoder.DisallowUnknownFields()
	var value struct {
		SyntheticOnly bool                 `json:"synthetic_only"`
		Players       []nhf014PlayerSource `json:"players"`
	}
	if err = decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err = decoder.Decode(new(any)); err != io.EOF || !value.SyntheticOnly || len(value.Players) != 2 {
		return nil, errors.New("bounded synthetic media configuration required")
	}
	kinds := make(map[nhf014PlayerKind]bool)
	aliases := make(map[string]bool)
	for _, player := range value.Players {
		if _, err = domain.ParseHAAlias(player.Alias); err != nil || aliases[player.Alias] || kinds[player.Kind] || player.Title == "" || len(player.Title) > 1024 || len(player.Artist) > 1024 || len(player.Collection) > 1024 {
			return nil, errors.New("invalid synthetic player")
		}
		aliases[player.Alias], kinds[player.Kind] = true, true
	}
	if !kinds[nhf014Minimal] || !kinds[nhf014Group] {
		return nil, errors.New("both synthetic media configurations required")
	}
	return value.Players, nil
}

func nhf014Descriptor() ([]byte, []byte, error) {
	// Match the native compiled descriptor fingerprint: dependencies precede
	// their importer in declared import order, as in Sonos/AndroidTV.
	seen := make(map[string]bool)
	set := new(descriptorpb.FileDescriptorSet)
	var visit func(protoreflect.FileDescriptor)
	visit = func(file protoreflect.FileDescriptor) {
		if seen[file.Path()] {
			return
		}
		seen[file.Path()] = true
		for i := 0; i < file.Imports().Len(); i++ {
			visit(file.Imports().Get(i).FileDescriptor)
		}
		set.File = append(set.File, protodesc.ToFileDescriptorProto(file))
	}
	visit(media.File_home_media_v1_media_proto)
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(set)
	if err != nil {
		return nil, nil, err
	}
	digest := sha256.Sum256(raw)
	return raw, digest[:], nil
}
func nhf014DescriptorHash() ([]byte, error) { _, hash, err := nhf014Descriptor(); return hash, err }
func nhf014Description(label string) (*registrywire.CapabilityDescription, error) {
	raw, hash, err := nhf014Descriptor()
	if err != nil {
		return nil, err
	}
	return &registrywire.CapabilityDescription{SchemaId: nhf014MediaSchema, Major: 1, Endpoint: "media", DisplayName: label, DescriptorSet: raw, DescriptorSha256: hash, Constraints: &registrywire.CapabilityDescription_Extension{Extension: &registrywire.ExtensionConstraints{ActionMessage: proto.String("home.media.v1.MediaAction")}}}, nil
}
func nhf014NewMock(players []nhf014PlayerSource, resolved map[string]domain.EntityID, cursor *state.ViewCursor) (*nhf014MediaMock, error) {
	digest, err := nhf014DescriptorHash()
	if err != nil {
		return nil, err
	}
	if cursor == nil || cursor.Epoch == "" || cursor.ConfigRevision == 0 {
		return nil, errors.New("actual view cursor required")
	}
	m := &nhf014MediaMock{snapshots: make(map[domain.EntityID]*media.Snapshot), records: make(map[domain.CommandID]*nhf014MediaRecord), unavailable: make(map[domain.EntityID]bool), now: time.Now, artworkMode: nhf014ArtworkAllowed, groups: make(map[domain.EntityID]map[nhf014GroupID]nhf014GroupRoute)}
	ids := make(map[nhf014PlayerKind]string)
	for _, player := range players {
		entity, ok := resolved[player.Alias]
		if !ok {
			return nil, errors.New("actual registry binding required")
		}
		ids[player.Kind], err = entity.Value()
		if err != nil {
			return nil, err
		}
	}
	for _, player := range players {
		name := ids[player.Kind]
		entity := resolved[player.Alias]
		value := &media.Snapshot{Route: &media.Route{EntityId: name, Endpoint: "media", CoordinatorEntityId: name, TopologyRevision: 1, AdmissionEpoch: cursor.Epoch, ConfigRevision: cursor.ConfigRevision}, Capabilities: &media.Capabilities{Operations: []media.Operation{media.Operation_OPERATION_PLAY, media.Operation_OPERATION_PAUSE}}, Playback: media.PlaybackState_PLAYBACK_STATE_PAUSED, Title: proto.String(player.Title), Artist: proto.String(player.Artist), Collection: proto.String(player.Collection), Quality: &state.Quality{Availability: state.Availability_AVAILABILITY_AVAILABLE, Freshness: state.Freshness_FRESHNESS_FRESH, Provenance: state.Provenance_PROVENANCE_ESTIMATED}, ReceivedAt: timestamppb.Now(), Provenance: "MOCK média synthétique — aucun appareil réel", DescriptorSha256: bytes.Clone(digest)}
		switch player.Kind {
		case nhf014Minimal:
		case nhf014Group:
			value.Route.CoordinatorEntityId = ids[nhf014Minimal]
			value.Capabilities.Operations = append(value.Capabilities.Operations, media.Operation_OPERATION_STOP, media.Operation_OPERATION_PREVIOUS, media.Operation_OPERATION_NEXT)
			value.Capabilities.VolumeBasisPoints = &media.QuantityBounds{Maximum: 10_000, Step: 100}
			value.Capabilities.PositionMillis = &media.QuantityBounds{Maximum: 240_000, Step: 1000}
			value.Capabilities.Sources = []*media.Option{{Id: "synthetic-library", Label: "Bibliothèque"}, {Id: "synthetic-radio", Label: "Radio synthétique"}}
			value.Capabilities.Groups = []*media.Option{{Id: "synthetic-duet", Label: "Duo synthétique"}, {Id: "synthetic-solo", Label: "Lecture individuelle"}}
			value.SourceId = proto.String("synthetic-library")
			value.GroupId = proto.String("synthetic-duet")
			value.VolumeBasisPoints = proto.Uint32(0)
			value.PositionMillis = proto.Uint64(0)
			value.DurationMillis = proto.Uint64(240_000)
			value.Members = []*media.Member{{EntityId: ids[nhf014Minimal], Label: "Lecteur minimal"}, {EntityId: name, Label: "Lecteur de groupe"}}
			coordinator, err := domain.ParseEntityID(ids[nhf014Minimal])
			if err != nil {
				return nil, err
			}
			m.groups[entity] = map[nhf014GroupID]nhf014GroupRoute{
				"synthetic-duet": {coordinator: coordinator, members: []*media.Member{proto.Clone(value.Members[0]).(*media.Member), proto.Clone(value.Members[1]).(*media.Member)}},
				"synthetic-solo": {coordinator: entity, members: []*media.Member{proto.Clone(value.Members[1]).(*media.Member)}},
			}
			ref := make([]byte, 32)
			if _, err = rand.Read(ref); err != nil {
				return nil, err
			}
			value.Artwork = &media.ArtworkReference{Reference: ref, ExpiresAt: timestamppb.New(time.Now().Add(30 * time.Second))}
		default:
			return nil, errors.New("unparsed media player kind")
		}
		m.snapshots[entity] = value
	}
	bitmap := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			bitmap.SetNRGBA(x, y, color.NRGBA{R: uint8(30 + x*2), G: uint8(80 + y), B: 155, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err = png.Encode(&encoded, bitmap); err != nil {
		return nil, err
	}
	m.png = encoded.Bytes()
	return m, nil
}

type nhf014ControlAction string

const (
	nhf014SwitchCoordinator    nhf014ControlAction = "switch-coordinator"
	nhf014RemoveCoordinator    nhf014ControlAction = "remove-coordinator"
	nhf014RestoreCoordinator   nhf014ControlAction = "restore-coordinator"
	nhf014WithdrawCapabilities nhf014ControlAction = "withdraw-capabilities"
	nhf014RestoreCapabilities  nhf014ControlAction = "restore-capabilities"
	nhf014LoseReply            nhf014ControlAction = "lose-next-reply"
	nhf014Confirm              nhf014ControlAction = "confirm"
	nhf014InspectActions       nhf014ControlAction = "inspect-actions"
	nhf014DenyArtwork          nhf014ControlAction = "deny-artwork"
	nhf014ExpireArtwork        nhf014ControlAction = "expire-artwork"
	nhf014AbsentArtwork        nhf014ControlAction = "absent-artwork"
	nhf014RestoreArtwork       nhf014ControlAction = "restore-artwork"
	nhf014RevokeWrite          nhf014ControlAction = "revoke-write"
	nhf014AllowWrite           nhf014ControlAction = "allow-write"
	nhf014RevokeRead           nhf014ControlAction = "revoke-read"
	nhf014AllowRead            nhf014ControlAction = "allow-read"
	nhf014Outage               nhf014ControlAction = "outage"
	nhf014RestoreNetwork       nhf014ControlAction = "restore-network"
)

func (a *nhf014ControlAction) UnmarshalJSON(raw []byte) error {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	switch action := nhf014ControlAction(value); action {
	case nhf014InspectActions, nhf014SwitchCoordinator, nhf014RemoveCoordinator, nhf014RestoreCoordinator, nhf014WithdrawCapabilities, nhf014RestoreCapabilities, nhf014LoseReply, nhf014Confirm, nhf014DenyArtwork, nhf014ExpireArtwork, nhf014AbsentArtwork, nhf014RestoreArtwork, nhf014RevokeWrite, nhf014AllowWrite, nhf014RevokeRead, nhf014AllowRead, nhf014Outage, nhf014RestoreNetwork:
		*a = action
		return nil
	default:
		return errors.New("unknown media fixture control")
	}
}

type nhf014Control struct {
	Sequence uint64              `json:"sequence"`
	Action   nhf014ControlAction `json:"action"`
	Command  string              `json:"command_id,omitempty"`
}

func nhf014ReadControl(root string, previous uint64) (*nhf014Control, error) {
	file, err := os.Open(filepath.Join(root, "command.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 4097))
	decoder.DisallowUnknownFields()
	var value nhf014Control
	if err = decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err = decoder.Decode(new(any)); err != io.EOF || value.Sequence == 0 || value.Sequence > 128 || value.Action == "" {
		return nil, errors.New("invalid media fixture control")
	}
	if value.Sequence == previous {
		return nil, nil
	}
	if value.Sequence != previous+1 {
		return nil, errors.New("out of order media fixture control")
	}
	switch value.Action {
	case nhf014Confirm:
		if _, err = apitypes.ParseRequestID(value.Command); err != nil {
			return nil, err
		}
	default:
		if value.Command != "" {
			return nil, errors.New("unexpected command identity")
		}
	}
	return &value, nil
}

// A loopback proxy models an explicitly lost command response. All authority,
// admission, durable effects and Operations remain in the one Run owner.
func nhf014StartMock(t *testing.T, m *nhf014MediaMock, client *http.Client, upstream, data string) (string, func()) {
	t.Helper()
	target, err := url.Parse(upstream)
	if err != nil || target.Scheme != "https" || target.Hostname() != "127.0.0.1" {
		t.Fatal("synthetic loopback upstream required", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = client.Transport
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != apiv2connect.CapabilityServiceExecuteCapabilityProcedure {
			proxy.ServeHTTP(w, r)
			return
		}
		m.mu.Lock()
		m.submitCalls++
		lose := m.loseReply
		m.loseReply = false
		m.mu.Unlock()
		if !lose {
			proxy.ServeHTTP(w, r)
			return
		}
		captured := httptest.NewRecorder()
		proxy.ServeHTTP(captured, r)
		if captured.Code == http.StatusOK {
			_ = connect.NewErrorWriter().Write(w, r, connect.NewError(connect.CodeUnavailable, errors.New("synthetic response lost after upstream execution")))
			return
		}
		for name, values := range captured.Header() {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(captured.Code)
		_, _ = w.Write(captured.Body.Bytes())
	}))
	pair, err := tls.LoadX509KeyPair(filepath.Join(data, nha181CertificateName), filepath.Join(data, nha181PrivateKeyName))
	if err != nil {
		t.Fatal(err)
	}
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}
	server.EnableHTTP2 = true
	server.StartTLS()
	return server.URL, func() { server.CloseClientConnections(); server.Close() }
}

// Starts only through the explicit isolated qualification executable. No real
// pilot is configured, even if a source alias resembles an installed alias.
func TestNHF014MediaServer(t *testing.T) { nhf014RunFixture(t, false) }
func nhf014RunFixture(t *testing.T, smoke bool) {
	out, root := os.Getenv("HOMEWARD_NHF014_FIXTURE"), os.Getenv("HOMEWARD_NHF014_SOURCES")
	if out == "" {
		t.Skip("explicit isolated NHF014 fixture only")
	}
	if os.Getenv("HOMEWARD_NHF014_SYNTHETIC_ONLY") != "1" || !filepath.IsAbs(out) || filepath.Clean(out) != out || !filepath.IsAbs(root) {
		t.Fatal("fresh absolute synthetic-only fixture required")
	}
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	players, err := nhf014ReadPlayers(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, signals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer signals()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	settings := nha049Config(t, cluster.Port, data)
	manifest, err := registry.LoadManifest(scope.BaselineV1())
	if err != nil {
		t.Fatal(err)
	}
	dashboard := &nhf005Dashboard{file: filepath.Join(root, "dashboard.yaml"), compilation: imported.CompilationRecipes}
	instance, _ := domain.ParseIntegrationInstanceID("nhf014-synthetic-only")
	var host *integrations.Manager
	var sessions *sessionCoordinator
	var epochs integrations.CommandEpochAuthority
	var identity *registry.Repository
	var fixtureStore *storage.Store
	mock := &nhf014MediaMock{now: time.Now, artwork: make(map[domain.EntityID]*nha202MediaOwner)}
	keys := make(map[string]integrations.SourceKey)
	var mediaSources []MediaResourceSource
	factory := RuntimeFactory(func(factoryCtx context.Context, actual *storage.Store) (RuntimePlan, error) {
		fixtureStore = actual
		identities, err := registry.NewRepository(actual, manifest)
		if err != nil {
			return RuntimePlan{}, err
		}
		identity = identities
		for _, player := range players {
			key, err := integrations.NewSourceKey("synthetic-media-"+string(player.Kind), "media")
			if err != nil {
				return RuntimePlan{}, err
			}
			keys[player.Alias] = key
			resolved, err := identities.ConfigureBinding(factoryCtx, instance, key, player.Alias)
			if err != nil {
				return RuntimePlan{}, err
			}
			if player.Kind == nhf014Group {
				id, err := resolved.Entity.Value()
				if err != nil {
					return RuntimePlan{}, err
				}
				owner := nha202NewMediaOwner(t, id, mediasessions.Artwork, color.NRGBA{R: 64, G: 128, B: 155, A: 255})
				mock.artwork[resolved.Entity] = owner
				source, err := NewMediaResourceSource(resolved.Entity, owner.binding.Slot(), owner)
				if err != nil {
					return RuntimePlan{}, err
				}
				mediaSources = append(mediaSources, source)
			}
		}
		public, err := NewPublicPlan(nil, nil, accountTransportAudit{store: actual})
		if err != nil {
			return RuntimePlan{}, err
		}
		return RuntimePlan{Identity: identities, Public: public, MediaSources: mediaSources, ConfigureAuthorization: func(_ context.Context, c AuthorizationComposition) error {
			dashboard.authority = c.ViewAuthorizer().(*policyViewAuthorizer).authority
			mock.authority = dashboard.authority
			return nil
		}, ConfigureActivation: func(_ context.Context, owner *configactivation.Owner) error {
			if owner == nil {
				return errors.New("actual activation owner required")
			}
			dashboard.activation = owner
			return nil
		}, Configure: func(_ context.Context, manager *integrations.Manager, actual *storage.Store) error {
			host = manager
			return nil
		}}, nil
	})
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done, ready := make(chan error, 1), make(chan net.Addr, 1)
	go func() {
		done <- runWithDependencies(runCtx, settings.startup, func(listenCtx context.Context, kind, _ string) (net.Listener, error) {
			return (&net.ListenConfig{}).Listen(listenCtx, kind, "127.0.0.1:0")
		}, func(address net.Addr) { ready <- address }, factory, runDependencies{observeLists: func(_ *listComposition, actual *sessionCoordinator) { sessions = actual }, mediaProjector: mock, observe: func(_ context.Context, sequence *sequencer.Sequencer) error { epochs = sequence; return nil }, configureCapabilityCatalog: func(catalog *contracts.CapabilityCatalog) error {
			description, err := nhf014Description("Synthetic media")
			if err != nil {
				return err
			}
			if err = catalog.RegisterExtensionCodec(description, nhf014MediaCodec{}); err != nil {
				return err
			}
			return nil
		}})
	}()
	joined := false
	defer func() {
		stop()
		if !joined {
			select {
			case err := <-done:
				joined = true
				if err != nil {
					t.Error(err)
				}
			case <-time.After(20 * time.Second):
				t.Error("media Run did not join")
			}
		}
		entries := uint64(0)
		if host != nil {
			entries = host.CommandDriverEntries()
		}
		calls, mutations, records := uint64(0), uint64(0), 0
		if mock != nil {
			mock.mu.Lock()
			calls, mutations, records = mock.submitCalls, mock.mutations, len(mock.records)
			mock.mu.Unlock()
		}
		nhf003WritePrivate(t, out, "result.json", map[string]any{"server_joined": joined, "command_driver_entries": entries, "real_device_writes": 0, "synthetic_submit_calls": calls, "synthetic_mutations": mutations, "synthetic_action_records": records})
		if entries != mutations {
			t.Error("native driver entries differ from synthetic committed effects")
		}
	}()
	var address net.Addr
	select {
	case address = <-ready:
	case err := <-done:
		joined = true
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	source, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	candidates := make([]integrations.Candidate, 0, len(players))
	for _, player := range players {
		description, err := nhf014Description(player.Title)
		if err != nil {
			t.Fatal(err)
		}
		candidate, err := integrations.NewCandidate(keys[player.Alias], []*registrywire.CapabilityDescription{description})
		if err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, candidate)
	}
	bindings, err := source.Discover(ctx, candidates)
	if err != nil || len(bindings) != len(players) {
		t.Fatal("synthetic media discovery", err)
	}
	resolved := make(map[string]domain.EntityID)
	for i, binding := range bindings {
		_, entity, err := binding.IDs()
		if err != nil {
			t.Fatal(err)
		}
		resolved[players[i].Alias] = entity

	}
	const adminName, adminPassword = "NHF014 isolated admin", "NHF014 synthetic admin password"
	adminID := nha162RunBootstrap(t, ctx, data, adminName, adminPassword)
	client := nha181Client(roots)
	defer client.CloseIdleConnections()
	base := "https://" + address.String()
	authAPI := apiv2connect.NewAuthServiceClient(client, base)
	admin := nha162RunLogin(t, ctx, authAPI, adminName, adminPassword)
	if err = dashboard.install(adminID.Value, resolved); err != nil {
		t.Fatal(err)
	}
	accounts := apiv2connect.NewAccountServiceClient(client, base)
	const readerName, readerPassword = "NHF014 Reader", "NHF014 synthetic reader password"
	created, err := accounts.CreateAccount(ctx, nha162Bearer(&api.CreateAccountRequest{Parent: nha203SessionAccount(t, admin.Session).Home().String(), Account: &api.Account{LoginName: readerName, DisplayName: readerName, InitialPassword: readerPassword, Role: api.AccountRole_ACCOUNT_ROLE_USER}}, admin.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	dashboard.mu.Lock()
	readerAccount := nha203NativeAccount(t, ctx, sessions.store, created.Msg.Name)
	dashboard.owners = map[contracts.AccountID]contracts.AccountID{readerAccount: readerAccount}
	dashboard.mu.Unlock()
	dashboard.start(t, ctx, client, base, admin.AccessToken)
	policies := apiv2connect.NewAuthorizationPolicyServiceClient(client, base)
	readAllowed, writeAllowed := true, true
	setPolicy := func() {
		grants := `  - scope: {family: state_registry, operation: read}
    objects: {entities: {all: {capabilities: {all: {}}}}}
  - scope: {family: dashboards, operation: read}
    objects: {dashboards: {all: {}}}
  - scope: {family: actions, operation: read}
    objects: {entities: {all: {capabilities: {all: {}}}}}
`
		if readAllowed {
			grants += `  - scope: {family: media, operation: read}
    objects: {entities: {all: {capabilities: {all: {}}}}}
`
		}
		if writeAllowed {
			grants += `  - scope: {family: actions, operation: write}
    objects: {entities: {all: {capabilities: {all: {}}}}}
`
		}
		nha162RunPolicy(t, ctx, policies, created.Msg.Name, admin.AccessToken, grants)
	}
	setPolicy()
	reader := nha162RunLogin(t, ctx, authAPI, readerName, readerPassword)
	if epochs == nil || identity == nil || sessions == nil {
		t.Fatal("actual sequence, identity and sessions required")
	}
	epoch, err := epochs.CurrentAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := dashboard.activation.CurrentConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	revision, _, err := stamp.Components()
	if err != nil {
		t.Fatal(err)
	}
	seeded, err := nhf014NewMock(players, resolved, &state.ViewCursor{Epoch: epoch, ConfigRevision: revision})
	if err != nil {
		t.Fatal(err)
	}
	mock.mu.Lock()
	mock.snapshots = seeded.snapshots
	mock.records = seeded.records
	mock.unavailable = seeded.unavailable
	mock.groups = seeded.groups
	mock.png = seeded.png
	mock.mu.Unlock()
	if err = host.AttachCommandDriver(source, mock); err != nil {
		t.Fatal(err)
	}
	readerSession := nha203NativeSession(t, ctx, sessions.store, reader.Session.Name)
	readerPrincipal, err := authorization.NativePrincipal(readerAccount, readerSession)
	if err != nil {
		t.Fatal(err)
	}
	readerActor, err := readerPrincipal.Actor()
	if err != nil {
		t.Fatal(err)
	}

	var group domain.EntityID
	for _, player := range players {
		if player.Kind == nhf014Group {
			group = resolved[player.Alias]
		}
	}
	original := proto.Clone(mock.snapshots[group]).(*media.Snapshot)
	origin, closeMock := nhf014StartMock(t, mock, client, base, data)
	defer closeMock()
	certificate, err := os.ReadFile(filepath.Join(data, nha181CertificateName))
	if err != nil {
		t.Fatal(err)
	}
	ids := make(map[string]string)
	home, err := identity.HomeName(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, player := range players {
		name, err := apitypes.EntityNameForIdentity(home, resolved[player.Alias])
		if err != nil {
			t.Fatal(err)
		}
		ids[string(player.Kind)] = name.String()
	}
	mediaAPI := apiv2connect.NewHomeServiceClient(client, origin)
	for _, kind := range []nhf014PlayerKind{nhf014Minimal, nhf014Group} {
		response, err := mediaAPI.ListCapabilities(ctx, nha162Bearer(&api.ListCapabilitiesRequest{Parent: ids[string(kind)]}, reader.AccessToken))
		if err != nil || len(response.Msg.Capabilities) != 1 || response.Msg.Capabilities[0].GetMediaPlayback() == nil || response.Msg.Capabilities[0].GetMediaPlayback().ActionAccess != api.ActionAccess_ACTION_ACCESS_ALLOWED {
			t.Fatal("native authenticated capability projection", err)
		}
	}
	if _, err = mediaAPI.ListCapabilities(ctx, connect.NewRequest(&api.ListCapabilitiesRequest{Parent: ids[string(nhf014Group)]})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("anonymous capability read not refused", err)
	}
	nhf003WritePrivate(t, out, "ready.json", map[string]any{"mode": "synthetic-only", "origin": origin, "ca_pem": string(certificate), "entities": ids, "reader": map[string]string{"login_name": readerName, "password": readerPassword}, "production_media": false})
	if smoke {
		nhf014SmokeCore(t, ctx, client, origin, reader.AccessToken, ids[string(nhf014Group)], mock, fixtureStore, host)
		return
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var controlSequence uint64
	for {
		select {
		case err := <-done:
			joined = true
			if err != nil {
				t.Fatal(err)
			}
			return
		case <-ctx.Done():
			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatal(ctx.Err())
			}
			return
		case <-ticker.C:
			if _, err = os.Stat(filepath.Join(out, "stop")); err == nil {
				return
			}
			control, err := nhf014ReadControl(out, controlSequence)
			if err != nil {
				t.Fatal(err)
			}
			if control == nil {
				continue
			}
			switch control.Action {
			case nhf014InspectActions:
				mock.mu.Lock()
				records := make([]map[string]string, 0, len(mock.records))
				for id, record := range mock.records {
					commandID, _ := id.Value()
					entityID, _ := record.entity.Value()
					records = append(records, map[string]string{"command_id": commandID, "entity_id": entityID})
				}
				stats := map[string]any{"records": records, "synthetic_submit_calls": mock.submitCalls, "synthetic_mutations": mock.mutations}
				mock.mu.Unlock()
				nhf003WritePrivate(t, out, fmt.Sprintf("stats-%02d.json", control.Sequence), stats)
			case nhf014RevokeRead:
				readAllowed = false
				setPolicy()
			case nhf014AllowRead:
				readAllowed = true
				setPolicy()
			case nhf014RevokeWrite:
				writeAllowed = false
				setPolicy()
			case nhf014AllowWrite:
				writeAllowed = true
				setPolicy()
			default:
				mock.mu.Lock()
				value := mock.snapshots[group]
				switch control.Action {
				case nhf014SwitchCoordinator:
					value.Route.CoordinatorEntityId = value.Route.EntityId
					value.Route.TopologyRevision++
				case nhf014RemoveCoordinator:
					mock.unavailable[group] = true
				case nhf014RestoreCoordinator:
					mock.unavailable[group] = false
					value.Route.CoordinatorEntityId = value.Route.EntityId
					value.Route.TopologyRevision++
				case nhf014WithdrawCapabilities:
					value.Capabilities = &media.Capabilities{}
				case nhf014RestoreCapabilities:
					value.Capabilities = proto.Clone(original.Capabilities).(*media.Capabilities)
				case nhf014LoseReply:
					mock.loseReply = true
				case nhf014Confirm:
					key, err := apitypes.ParseRequestID(control.Command)
					if err != nil {
						mock.mu.Unlock()
						t.Fatal(err)
					}
					id, err := publicCommandID(readerActor, &key)
					if err != nil {
						mock.mu.Unlock()
						t.Fatal(err)
					}
					record := mock.records[id]
					if record == nil {
						mock.mu.Unlock()
						t.Fatal("unbound synthetic effect confirmation")
					}
					admitted, entity := record.command, record.entity
					mock.mu.Unlock()
					err = admitted.ConfirmProtocol(nhf014SyntheticProof{id, entity})
					mock.mu.Lock()
					if err != nil {
						mock.mu.Unlock()
						t.Fatal(err)
					}
				case nhf014DenyArtwork:
					mock.artworkMode = nhf014ArtworkDenied
					// Publish a new reference so an already cached allowed image
					// cannot hide this explicitly simulated authorization change.
					reference := make([]byte, 32)
					if _, err = rand.Read(reference); err != nil {
						mock.mu.Unlock()
						t.Fatal(err)
					}
					value.Artwork = &media.ArtworkReference{Reference: reference, ExpiresAt: timestamppb.New(time.Now().Add(30 * time.Second))}
					if owner := mock.artwork[group]; owner != nil {
						owner.mu.Lock()
						copy(owner.key[:], reference)
						owner.mu.Unlock()
					}
				case nhf014ExpireArtwork:
					mock.artworkMode = nhf014ArtworkExpired
					if value.Artwork != nil {
						value.Artwork.ExpiresAt = timestamppb.New(time.Now().Add(-time.Second))
					}
				case nhf014AbsentArtwork:
					value.Artwork = nil
				case nhf014RestoreArtwork:
					mock.artworkMode = nhf014ArtworkAllowed
					reference := make([]byte, 32)
					if _, err = rand.Read(reference); err != nil {
						mock.mu.Unlock()
						t.Fatal(err)
					}
					value.Artwork = &media.ArtworkReference{Reference: reference, ExpiresAt: timestamppb.New(time.Now().Add(30 * time.Second))}
					if owner := mock.artwork[group]; owner != nil {
						owner.mu.Lock()
						copy(owner.key[:], reference)
						owner.mu.Unlock()
					}
				case nhf014Outage:
					mock.offline = true
				case nhf014RestoreNetwork:
					mock.offline = false
				case nhf014InspectActions, nhf014RevokeRead, nhf014AllowRead, nhf014RevokeWrite, nhf014AllowWrite:
					mock.mu.Unlock()
					t.Fatal("policy control reached mock mutation")
				default:
					mock.mu.Unlock()
					t.Fatal("unparsed media fixture control")
				}
				for entity, owner := range mock.artwork {
					owner.mu.Lock()
					owner.artworkDenied = mock.artworkMode == nhf014ArtworkDenied
					owner.offline = mock.offline
					value := mock.snapshots[entity]
					if value != nil && value.Artwork != nil {
						owner.artworkExpiry = value.Artwork.ExpiresAt.AsTime()
					} else {
						owner.artworkExpiry = time.Now().Add(-time.Second)
					}
					owner.mu.Unlock()
				}
				mock.mu.Unlock()
			}
			controlSequence = control.Sequence
			nhf003WritePrivate(t, out, fmt.Sprintf("ack-%02d.json", controlSequence), control)
		}
	}
}

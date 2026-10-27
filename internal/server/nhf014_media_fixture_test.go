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
	"sort"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	imported "github.com/damione1/Homeward/compat/ha/dashboard"
	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/gen/home/actions/v1/actionsv1connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	media "github.com/damione1/Homeward/gen/home/media/v1"
	"github.com/damione1/Homeward/gen/home/media/v1/mediav1connect"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/gen/home/state/v1/statev1connect"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
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

func nhf014DescriptorHash() ([]byte, error) {
	files := make(map[string]protoreflect.FileDescriptor)
	var visit func(protoreflect.FileDescriptor)
	visit = func(file protoreflect.FileDescriptor) {
		if files[file.Path()] != nil {
			return
		}
		files[file.Path()] = file
		for i := 0; i < file.Imports().Len(); i++ {
			visit(file.Imports().Get(i).FileDescriptor)
		}
	}
	visit(media.File_home_media_v1_media_proto)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	set := new(descriptorpb.FileDescriptorSet)
	for _, name := range names {
		set.File = append(set.File, protodesc.ToFileDescriptorProto(files[name]))
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(set)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(raw)
	return digest[:], nil
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
		if _, err = domain.ParseCommandID(value.Command); err != nil {
			return nil, err
		}
	default:
		if value.Command != "" {
			return nil, errors.New("unexpected command identity")
		}
	}
	return &value, nil
}

// A sidecar transport reuses Run's exact native session/authority and certificate.
// Only auth/state/dashboard calls are forwarded to its loopback origin.
func nhf014StartMock(t *testing.T, m *nhf014MediaMock, sessions *sessionCoordinator, store *storage.Store, client *http.Client, upstream, data string) (string, func()) {
	t.Helper()
	authenticator := &sessionAuth{store: store, coordinator: sessions}
	plan, err := NewPublicPlan(authenticator, authenticator, accountTransportAudit{store: store})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.register(plan); err != nil {
		t.Fatal(err)
	}
	settings := config.DefaultPublicSettings()
	compiled, err := compilePublicPlan(plan, settings)
	if err != nil {
		t.Fatal(err)
	}
	protected := compiled.handler(settings, newPublicAdmissions())
	target, err := url.Parse(upstream)
	if err != nil || target.Scheme != "https" || target.Hostname() != "127.0.0.1" {
		t.Fatal("synthetic loopback upstream required", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = client.Transport
	mux := http.NewServeMux()
	mux.Handle("/"+mediav1connect.MediaServiceName+"/", protected)
	mux.Handle("/"+actionsv1connect.ActionServiceName+"/", protected)
	mux.Handle("/", proxy)
	server := httptest.NewUnstartedServer(mux)
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
func TestNHF014MediaServer(t *testing.T) {
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
	var store *storage.Store
	var sessions *sessionCoordinator
	keys := make(map[string]integrations.SourceKey)
	factory := RuntimeFactory(func(factoryCtx context.Context, actual *storage.Store) (RuntimePlan, error) {
		identities, err := registry.NewRepository(actual, manifest)
		if err != nil {
			return RuntimePlan{}, err
		}
		for _, player := range players {
			key, err := integrations.NewSourceKey("synthetic-media", string(player.Kind))
			if err != nil {
				return RuntimePlan{}, err
			}
			keys[player.Alias] = key
			if _, err = identities.ConfigureBinding(factoryCtx, instance, key, player.Alias); err != nil {
				return RuntimePlan{}, err
			}
		}
		public, err := NewPublicPlan(nil, nil, accountTransportAudit{store: actual})
		if err != nil {
			return RuntimePlan{}, err
		}
		if err = dashboard.register(public); err != nil {
			return RuntimePlan{}, err
		}
		return RuntimePlan{Identity: identities, Public: public, ConfigureAuthorization: func(_ context.Context, c AuthorizationComposition) error {
			dashboard.authority = c.ViewAuthorizer().(*policyViewAuthorizer).authority
			return nil
		}, ConfigureActivation: func(_ context.Context, owner *configactivation.Owner) error {
			if owner == nil {
				return errors.New("actual activation owner required")
			}
			dashboard.activation = owner
			return nil
		}, Configure: func(_ context.Context, manager *integrations.Manager, actual *storage.Store) error {
			host, store = manager, actual
			return nil
		}}, nil
	})
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done, ready := make(chan error, 1), make(chan net.Addr, 1)
	go func() {
		done <- runWithDependencies(runCtx, settings.startup, func(listenCtx context.Context, kind, _ string) (net.Listener, error) {
			return (&net.ListenConfig{}).Listen(listenCtx, kind, "127.0.0.1:0")
		}, func(address net.Addr) { ready <- address }, factory, runDependencies{observeLists: func(_ *listComposition, actual *sessionCoordinator) { sessions = actual }})
	}()
	joined := false
	var mock *nhf014MediaMock
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
		if entries != 0 {
			t.Error("synthetic media entered real command driver")
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
		candidate, err := integrations.NewCandidate(keys[player.Alias], []*registrywire.CapabilityDescription{{SchemaId: contracts.BinarySchema, Major: 1, Endpoint: "media", DisplayName: player.Title, Constraints: &registrywire.CapabilityDescription_Binary{Binary: &registrywire.BinaryConstraints{}}}})
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
		quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		value := domain.NewSwitchValue(false)
		observation, err := integrations.NewStateObservation("media", quality, &value, nil, time.Now(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = source.Publish(ctx, binding, observation); err != nil {
			t.Fatal(err)
		}
	}
	const adminName, adminPassword = "NHF014 isolated admin", "NHF014 synthetic admin password"
	adminID := nha162RunBootstrap(t, ctx, data, adminName, adminPassword)
	client := nha181Client(roots)
	defer client.CloseIdleConnections()
	base := "https://" + address.String()
	authAPI := authv1connect.NewAuthServiceClient(client, base)
	admin := nha162RunLogin(t, ctx, authAPI, adminName, adminPassword)
	if err = dashboard.install(adminID.Value, resolved); err != nil {
		t.Fatal(err)
	}
	accounts := authv1connect.NewAccountServiceClient(client, base)
	const readerName, readerPassword = "NHF014 Reader", "NHF014 synthetic reader password"
	created, err := accounts.CreateAccount(ctx, nha162Bearer(&auth.CreateAccountRequest{LoginName: readerName, DisplayName: readerName, InitialPassword: readerPassword, Role: auth.AccountRole_ACCOUNT_ROLE_USER}, admin.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	dashboard.mu.Lock()
	readerAccount, err := contracts.ParseAccountID(created.Msg.Account.Id.Value)
	if err != nil {
		dashboard.mu.Unlock()
		t.Fatal(err)
	}
	dashboard.owners = map[contracts.AccountID]contracts.AccountID{readerAccount: readerAccount}
	dashboard.mu.Unlock()
	policies := authv1connect.NewAuthorizationPolicyServiceClient(client, base)
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
		nha162RunPolicy(t, ctx, policies, created.Msg.Account.Id, admin.AccessToken, grants)
	}
	setPolicy()
	reader := nha162RunLogin(t, ctx, authAPI, readerName, readerPassword)
	view, err := statev1connect.NewStateServiceClient(client, base).OpenView(ctx, nha162Bearer(&state.OpenViewRequest{Filter: &state.ViewFilter{}, PageSize: 16}, reader.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	mock, err = nhf014NewMock(players, resolved, view.Msg.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	mock.authority = sessions.authority
	var group domain.EntityID
	for _, player := range players {
		if player.Kind == nhf014Group {
			group = resolved[player.Alias]
		}
	}
	original := proto.Clone(mock.snapshots[group]).(*media.Snapshot)
	origin, closeMock := nhf014StartMock(t, mock, sessions, store, client, base, data)
	defer closeMock()
	certificate, err := os.ReadFile(filepath.Join(data, nha181CertificateName))
	if err != nil {
		t.Fatal(err)
	}
	ids := make(map[string]string)
	for _, player := range players {
		ids[string(player.Kind)], _ = resolved[player.Alias].Value()
	}
	mediaAPI := mediav1connect.NewMediaServiceClient(client, origin)
	for _, kind := range []nhf014PlayerKind{nhf014Minimal, nhf014Group} {
		response, err := mediaAPI.ReadSnapshot(ctx, nha162Bearer(&media.ReadSnapshotRequest{EntityId: ids[string(kind)]}, reader.AccessToken))
		if err != nil || response.Msg.GetSnapshot() == nil || response.Msg.GetSnapshot().GetRoute().GetEntityId() != ids[string(kind)] || response.Msg.GetSnapshot().ActionAccess != state.ActionAccess_ACTION_ACCESS_ALLOWED {
			t.Fatal("native authenticated media read failed", err)
		}
	}
	if _, err = mediaAPI.ReadSnapshot(ctx, connect.NewRequest(&media.ReadSnapshotRequest{EntityId: ids[string(nhf014Group)]})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("anonymous media read was not refused", err)
	}
	nhf003WritePrivate(t, out, "ready.json", map[string]any{"mode": "synthetic-only", "origin": origin, "ca_pem": string(certificate), "entities": ids, "reader": map[string]string{"login_name": readerName, "password": readerPassword}, "production_media": false})
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var sequence uint64
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
			control, err := nhf014ReadControl(out, sequence)
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
					id, _ := domain.ParseCommandID(control.Command)
					record := mock.records[id]
					if record == nil {
						mock.mu.Unlock()
						t.Fatal("unbound synthetic confirmation")
					}
					record.response = proto.Clone(record.response).(*actions.CommandResponse)
					record.progress = proto.Clone(record.progress).(*actions.ActionProgress)
					record.response.Status = actions.ActionStatus_ACTION_STATUS_CONFIRMED
					record.progress.Status = actions.ActionStatus_ACTION_STATUS_CONFIRMED
					record.progress.Sequence++
					close(record.changed)
					record.changed = make(chan struct{})
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
				mock.mu.Unlock()
			}
			sequence = control.Sequence
			nhf003WritePrivate(t, out, fmt.Sprintf("ack-%02d.json", sequence), control)
		}
	}
}

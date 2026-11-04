package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func nha201SyntheticManifest(t *testing.T) registry.Manifest {
	t.Helper()
	type entry struct {
		ID     string `json:"entity_id"`
		Source string `json:"source"`
	}
	entries := make([]entry, 850)
	ids := make([]string, len(entries))
	for i := range entries {
		ids[i] = fmt.Sprintf("light.synthetic_%04d", i)
		entries[i] = entry{ids[i], "synthetic"}
	}
	digest := sha256.Sum256([]byte(strings.Join(ids, "\n") + "\n"))
	data, err := json.Marshal(struct {
		Version int     `json:"version"`
		Digest  string  `json:"ids_sha256"`
		Entries []entry `json:"entities"`
	}{1, hex.EncodeToString(digest[:]), entries})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := registry.ParseManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func nha201PrincipalContext(t *testing.T, ctx context.Context, account contracts.AccountID) context.Context {
	t.Helper()
	session, _ := contracts.ParseSessionID("synthetic-metadata-session")
	principal, err := authorization.NativePrincipal(account, session)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = authorization.WithPrincipal(ctx, principal)
	if err != nil {
		t.Fatal(err)
	}
	actor, _ := principal.Actor()
	return context.WithValue(ctx, actorContextKey{}, actor)
}

// This uses actual registry/account/policy/dedup SQL owners and admitted server
// handlers. It is not a transport credential or physical-device qualification.
func TestNHA201EntityMetadataCASReplayAndSQLAuthorizationFence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cluster := testpg.Start(t)
	settings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapMFAPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	admin, err := newAccountInput(ctx, "Synthetic Metadata Admin", "Synthetic Metadata Admin", "Synthetic metadata password alpha", auth.AccountRole_ACCOUNT_ROLE_ADMIN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapAdmin(ctx, admin); err != nil {
		t.Fatal(err)
	}
	second, err := newAccountInput(ctx, "Synthetic Other Admin", "Synthetic Other Admin", "Synthetic metadata password beta", auth.AccountRole_ACCOUNT_ROLE_ADMIN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAccount(ctx, admin.ID, second); err != nil {
		t.Fatal(err)
	}
	user, err := newAccountInput(ctx, "Synthetic Metadata User", "Synthetic Metadata User", "Synthetic metadata password gamma", auth.AccountRole_ACCOUNT_ROLE_USER)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAccount(ctx, admin.ID, user); err != nil {
		t.Fatal(err)
	}
	authority := authorization.NewAuthority()
	states := make([]authorization.AccountState, 0, 3)
	for _, id := range []contracts.AccountID{admin.ID, second.ID, user.ID} {
		snapshot, err := store.AuthorizationPolicy(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		state, err := snapshot.State()
		if err != nil {
			t.Fatal(err)
		}
		states = append(states, state)
	}
	if err := authority.Hydrate(states); err != nil {
		t.Fatal(err)
	}
	manifest := nha201SyntheticManifest(t)
	repository, err := registry.NewRepository(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("synthetic-metadata")
	source, _ := integrations.NewSourceKey("synthetic-device", "light")
	identity, err := repository.ConfigureBinding(ctx, instance, source, "light.synthetic_0000")
	if err != nil {
		t.Fatal(err)
	}
	home, err := repository.HomeName(ctx)
	if err != nil {
		t.Fatal(err)
	}
	name, err := apitypes.EntityNameForIdentity(home, identity.Entity)
	if err != nil {
		t.Fatal(err)
	}
	records, err := repository.List(ctx)
	if err != nil || len(records) != 1 {
		t.Fatal(err)
	}
	initial, err := encodeEntity(home, records[0].Metadata())
	if err != nil {
		t.Fatal(err)
	}
	handler := &homeResourceHandler{owner: repository, authority: authority}
	adminCtx := nha201PrincipalContext(t, ctx, admin.ID)
	userCtx := nha201PrincipalContext(t, ctx, user.ID)
	if _, err := handler.UpdateEntity(userCtx, connect.NewRequest(&api.UpdateEntityRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("detailed validation preceded admin authorization", err)
	}
	request := &api.UpdateEntityRequest{Entity: &api.Entity{Name: name.String(), Etag: initial.Etag, DisplayName: "Synthetic label"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}, RequestId: "3feaa45a-75bb-4ecd-9cf5-5a88ae8bb811"}
	first, err := handler.UpdateEntity(adminCtx, connect.NewRequest(request))
	if err != nil {
		t.Fatal(err)
	}
	if first.Msg.Etag == initial.Etag || first.Msg.DisplayName != "Synthetic label" || first.Msg.UpdateTime == nil {
		t.Fatal("metadata change did not advance its independent revision")
	}
	replay := proto.CloneOf(request)
	replay.Entity.CreateTime = &timestamppb.Timestamp{Seconds: 1 << 62}
	replay.UpdateMask.Paths = append(replay.UpdateMask.Paths, "create_time")
	got, err := handler.UpdateEntity(adminCtx, connect.NewRequest(replay))
	if err != nil || !proto.Equal(first.Msg, got.Msg) {
		t.Fatal("exact result replay or output-only exclusion failed", err)
	}
	conflict := proto.CloneOf(request)
	conflict.Entity.DisplayName = "Another label"
	if _, err := handler.UpdateEntity(adminCtx, connect.NewRequest(conflict)); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatal("request_id intent conflict", err)
	}
	stale := proto.CloneOf(request)
	stale.RequestId = ""
	if _, err := handler.UpdateEntity(adminCtx, connect.NewRequest(stale)); connect.CodeOf(err) != connect.CodeAborted {
		t.Fatal("stale etag did not reach CAS", err)
	}
	noop := &api.UpdateEntityRequest{Entity: proto.CloneOf(first.Msg), UpdateMask: &fieldmaskpb.FieldMask{}}
	noop.Entity.DisplayName = " ignored by an explicit empty mask "
	unchanged, err := handler.UpdateEntity(adminCtx, connect.NewRequest(noop))
	if err != nil || !proto.Equal(first.Msg, unchanged.Msg) {
		t.Fatal("empty mask changed data or revision", err)
	}
	clear := &api.UpdateEntityRequest{Entity: proto.CloneOf(first.Msg), UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}
	clear.Entity.DisplayName = ""
	cleared, err := handler.UpdateEntity(adminCtx, connect.NewRequest(clear))
	if err != nil || cleared.Msg.DisplayName != "" || cleared.Msg.Etag == first.Msg.Etag {
		t.Fatal("explicit clear lost presence", err)
	}
	// Keep the in-memory authority deliberately stale, simulating the interval
	// between a committed account change and its publication. SQL must still deny.
	role := auth.AccountRole_ACCOUNT_ROLE_USER
	if _, err := store.UpdateAccount(ctx, second.ID, admin.ID, storage.AccountChanges{Role: &role}); err != nil {
		t.Fatal(err)
	}
	clear.Entity = proto.CloneOf(cleared.Msg)
	clear.Entity.DisplayName = "Must not be written"
	if _, err := handler.UpdateEntity(adminCtx, connect.NewRequest(clear)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("stale memory evidence escaped SQL fence", err)
	}
	records, err = repository.List(ctx)
	if err != nil || len(records) != 1 {
		t.Fatal(err)
	}
	if records[0].Metadata().Label().String() != "" || records[0].Metadata().ETag().String() != cleared.Msg.Etag {
		t.Fatal("denied write changed registry metadata")
	}
}

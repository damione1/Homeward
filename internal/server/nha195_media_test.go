package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	media "github.com/damione1/Homeward/gen/home/media/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/androidtv"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/sonos"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func nha195Snapshot() *media.Snapshot {
	return &media.Snapshot{
		Route:        &media.Route{EntityId: "garage-player", CoordinatorEntityId: "garage-player", Endpoint: sonos.Endpoint, AdmissionEpoch: "private-runtime-epoch", TopologyRevision: 3},
		Capabilities: &media.Capabilities{Operations: []media.Operation{media.Operation_OPERATION_PLAY, media.Operation_OPERATION_PAUSE, media.Operation_OPERATION_STOP}, VolumeBasisPoints: &media.QuantityBounds{Minimum: 0, Maximum: 10000, Step: 100}},
		Members:      []*media.Member{{EntityId: "garage-player", Label: "Garage"}},
		Quality:      &state.Quality{Availability: state.Availability_AVAILABILITY_AVAILABLE, Freshness: state.Freshness_FRESHNESS_FRESH, Provenance: state.Provenance_PROVENANCE_OBSERVED},
		ReceivedAt:   timestamppb.Now(), DescriptorSha256: bytes.Repeat([]byte{1}, 32),
	}
}

func nha195Source(value *media.Snapshot, schema string, commands bool) nativeMediaSource {
	return nativeMediaSource{schema: schema, endpoint: "player", commands: commands, snapshot: func(domain.EntityID) (*media.Snapshot, bool) { return value, true }}
}

func nha195Policy(t *testing.T, actions string, mediaRead, stateRead bool) authorization.PolicySource {
	t.Helper()
	account, _ := contracts.ParseAccountID("nha195-media")
	grants := ""
	for family, enabled := range map[string]bool{"media": mediaRead, "state_registry": stateRead} {
		if enabled {
			grants += "  - scope: {family: " + family + ", operation: read}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n"
		}
	}
	grants += actions
	if grants == "" {
		grants = "    []\n"
	}
	source, err := authorization.AdmitPolicySource(account, []byte("schema: homeward.authorization/1\naccount_id: nha195-media\ngrants:\n"+grants))
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func nha195WriteGrant(entity, schema, endpoint string) string {
	return fmt.Sprintf("  - scope: {family: actions, operation: write}\n    objects:\n      entities:\n        explicit:\n          - entity_id: %s\n            capabilities:\n              explicit:\n                - {schema_id: %s, endpoint: %s}\n", entity, schema, endpoint)
}

func nha195Authority(t *testing.T, source authorization.PolicySource) (*authorization.Authority, authorization.Principal) {
	t.Helper()
	authority := authorization.NewAuthority()
	if err := authority.Hydrate([]authorization.AccountState{nha164AuthorityState(t, source, auth.AccountRole_ACCOUNT_ROLE_USER, 1)}); err != nil {
		t.Fatal(err)
	}
	account, _ := contracts.ParseAccountID("nha195-media")
	session, _ := contracts.ParseSessionID("nha195-session")
	principal, err := authorization.NativePrincipal(account, session)
	if err != nil {
		t.Fatal(err)
	}
	return authority, principal
}

func nha195Project(t *testing.T, authority *authorization.Authority, principal authorization.Principal, sources []nativeMediaSource) (*media.Snapshot, error) {
	t.Helper()
	id, _ := domain.ParseEntityID("garage-player")
	var result *media.Snapshot
	err := authority.WithCurrent(context.Background(), principal, func(current authorization.Current) error {
		var err error
		result, err = projectNativeMediaCurrent(current, id, sources, 42, nha195Epoch{"host-admission-epoch", nil})
		return err
	})
	return result, err
}

type nha195Epoch struct {
	value string
	err   error
}

func (e nha195Epoch) CurrentAdmissionEpoch() (string, error) { return e.value, e.err }

func TestNHA195MediaUsesSequencerAdmissionEpochAndFailsClosedWithoutIt(t *testing.T) {
	authority, principal := nha195Authority(t, nha195Policy(t, nha195WriteGrant("garage-player", sonos.Schema, "player"), true, true))
	_, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	want, err := fixture.CurrentAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	id, _ := domain.ParseEntityID("garage-player")
	probes := 0
	source := nha195Source(nha195Snapshot(), sonos.Schema, true)
	source.snapshot = func(domain.EntityID) (*media.Snapshot, bool) { probes++; return nha195Snapshot(), true }
	for _, test := range []struct {
		epoch   integrations.CommandEpochAuthority
		allowed bool
	}{
		{fixture.Sequencer(), true}, {nha195Epoch{"", nil}, false}, {nha195Epoch{"", errors.New("epoch unavailable")}, false}, {nil, false},
	} {
		before := probes
		err := authority.WithCurrent(context.Background(), principal, func(current authorization.Current) error {
			got, err := projectNativeMediaCurrent(current, id, []nativeMediaSource{source}, 42, test.epoch)
			if err == nil && (got.Route.AdmissionEpoch != want || got.Route.AdmissionEpoch == "private-runtime-epoch") {
				t.Fatal("private media epoch escaped global sequencer projection")
			}
			return err
		})
		if test.allowed != (err == nil) || !test.allowed && probes != before {
			t.Fatal("invalid admission epoch probed provider or produced a route", err)
		}
	}
}

func TestNHA195MediaProjectionUsesCurrentExactActionSlotAndHostEpoch(t *testing.T) {
	for _, test := range []struct {
		name, entity, schema, endpoint string
		allowed                        bool
	}{
		{"exact", "garage-player", sonos.Schema, "player", true},
		{"other entity", "other-player", sonos.Schema, "player", false},
		{"other schema", "garage-player", androidtv.Schema, "player", false},
		{"other endpoint", "garage-player", sonos.Schema, "volume", false},
		{"no action grant", "", "", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			grant := ""
			if test.entity != "" {
				grant = nha195WriteGrant(test.entity, test.schema, test.endpoint)
			}
			authority, principal := nha195Authority(t, nha195Policy(t, grant, true, true))
			value := nha195Snapshot()
			got, err := nha195Project(t, authority, principal, []nativeMediaSource{nha195Source(value, sonos.Schema, true)})
			if err != nil {
				t.Fatal(err)
			}
			if (got.ActionAccess == state.ActionAccess_ACTION_ACCESS_ALLOWED) != test.allowed {
				t.Fatalf("access=%v wanted allowed=%v", got.ActionAccess, test.allowed)
			}
			if got.Route.AdmissionEpoch != "host-admission-epoch" || got.Route.ConfigRevision != 42 {
				t.Fatal("projection retained private epoch or stale configuration")
			}
			if value.Route.AdmissionEpoch != "private-runtime-epoch" || value.Route.ConfigRevision != 0 {
				t.Fatal("projection mutated provider cache")
			}
		})
	}
}

func TestNHA195MediaWriteRevocationAndAndroidTVRemainReadOnly(t *testing.T) {
	granted := nha195Policy(t, nha195WriteGrant("garage-player", sonos.Schema, "player"), true, true)
	authority, principal := nha195Authority(t, granted)
	sources := []nativeMediaSource{nha195Source(nha195Snapshot(), sonos.Schema, true)}
	before, err := nha195Project(t, authority, principal, sources)
	if err != nil || before.ActionAccess != state.ActionAccess_ACTION_ACCESS_ALLOWED {
		t.Fatal("initial write grant missing", err)
	}
	withdrawn := nha195Policy(t, "", true, true)
	if err := authority.Publish(nha164AuthorityState(t, withdrawn, auth.AccountRole_ACCOUNT_ROLE_USER, 2)); err != nil {
		t.Fatal(err)
	}
	after, err := nha195Project(t, authority, principal, sources)
	if err != nil || after.ActionAccess != state.ActionAccess_ACTION_ACCESS_DENIED {
		t.Fatal("withdrawn write grant remained cached", err)
	}
	authority, principal = nha195Authority(t, nha195Policy(t, nha195WriteGrant("garage-player", androidtv.Schema, "player"), true, true))
	tv := nha195Snapshot()
	tv.ActionAccess = state.ActionAccess_ACTION_ACCESS_ALLOWED
	got, err := nha195Project(t, authority, principal, []nativeMediaSource{nha195Source(tv, androidtv.Schema, false)})
	if err != nil || got.ActionAccess != state.ActionAccess_ACTION_ACCESS_DENIED {
		t.Fatal("read-only TV acquired command access", err)
	}
}

func TestNHA195MediaRequiresBothReadFamiliesBeforeProviderProbe(t *testing.T) {
	for _, mediaRead := range []bool{false, true} {
		authority, principal := nha195Authority(t, nha195Policy(t, "", mediaRead, !mediaRead))
		calls := 0
		source := nha195Source(nha195Snapshot(), sonos.Schema, true)
		source.snapshot = func(domain.EntityID) (*media.Snapshot, bool) { calls++; return nha195Snapshot(), true }
		if _, err := nha195Project(t, authority, principal, []nativeMediaSource{source}); !errors.Is(err, authorization.ErrDenied) || calls != 0 {
			t.Fatal("provider probed without both current read grants", err)
		}
	}
}

func TestNHA195MediaCommandsRequireFreshSupportedStandaloneObservation(t *testing.T) {
	id, _ := domain.ParseEntityID("garage-player")
	allow := func(domain.EntityID, string, string) error { return nil }
	for _, test := range []struct {
		name   string
		mutate func(*media.Snapshot)
	}{
		{"stale", func(s *media.Snapshot) { s.Quality.Freshness = state.Freshness_FRESHNESS_STALE }},
		{"unavailable", func(s *media.Snapshot) { s.Quality.Availability = state.Availability_AVAILABILITY_UNAVAILABLE }},
		{"unknown provenance", func(s *media.Snapshot) { s.Quality.Provenance = state.Provenance_PROVENANCE_UNSPECIFIED }},
		{"expired", func(s *media.Snapshot) { s.ReceivedAt = timestamppb.New(time.Now().Add(-46 * time.Second)) }},
		{"future", func(s *media.Snapshot) { s.ReceivedAt = timestamppb.New(time.Now().Add(time.Minute)) }},
		{"missing topology", func(s *media.Snapshot) { s.Route.TopologyRevision = 0 }},
		{"missing descriptor", func(s *media.Snapshot) { s.DescriptorSha256 = nil }},
		{"unsupported operation", func(s *media.Snapshot) {
			s.Capabilities = &media.Capabilities{Operations: []media.Operation{media.Operation_OPERATION_NEXT}}
		}},
		{"wrong volume scale", func(s *media.Snapshot) {
			s.Capabilities = &media.Capabilities{VolumeBasisPoints: &media.QuantityBounds{Maximum: 100, Step: 1}}
		}},
		{"grouped", func(s *media.Snapshot) { s.Members = append(s.Members, &media.Member{EntityId: "other-player"}) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := nha195Snapshot()
			test.mutate(value)
			got, err := projectNativeMediaActions(id, []nativeMediaSource{nha195Source(value, sonos.Schema, true)}, 42, "host-epoch", allow, allow)
			if err != nil || got.ActionAccess != state.ActionAccess_ACTION_ACCESS_DENIED {
				t.Fatal("unsafe observation offered command access", err)
			}
		})
	}
}

func TestNHA195MediaFilteringCannotTurnGroupIntoStandalone(t *testing.T) {
	id, _ := domain.ParseEntityID("garage-player")
	value := nha195Snapshot()
	value.Members = append(value.Members, &media.Member{EntityId: "private-member", Label: "Hidden"})
	read := func(entity domain.EntityID, schema, endpoint string) error {
		text, _ := entity.Value()
		if text == "private-member" {
			return authorization.ErrDenied
		}
		return nil
	}
	writes := 0
	write := func(domain.EntityID, string, string) error { writes++; return nil }
	got, err := projectNativeMediaActions(id, []nativeMediaSource{nha195Source(value, sonos.Schema, true)}, 42, "host-epoch", read, write)
	if err != nil || len(got.Members) != 1 || got.ActionAccess != state.ActionAccess_ACTION_ACCESS_DENIED || len(got.Capabilities.Operations) != 0 || writes != 0 {
		t.Fatal("filtered group offered standalone writes", err)
	}
	if len(value.Members) != 2 {
		t.Fatal("filter changed provider cache")
	}
	value.Route.CoordinatorEntityId = "private-member"
	if _, err = projectNativeMediaActions(id, []nativeMediaSource{nha195Source(value, sonos.Schema, true)}, 42, "host-epoch", read, write); !errors.Is(err, authorization.ErrDenied) {
		t.Fatal("private coordinator leaked", err)
	}
	value.Route.CoordinatorEntityId = "garage-player"
	read = func(entity domain.EntityID, schema, endpoint string) error {
		text, _ := entity.Value()
		if text == "private-member" {
			return authorization.ErrUnavailable
		}
		return nil
	}
	if _, err = projectNativeMediaActions(id, []nativeMediaSource{nha195Source(value, sonos.Schema, true)}, 42, "host-epoch", read, write); !errors.Is(err, authorization.ErrUnavailable) {
		t.Fatal("member authorization failure was silently hidden", err)
	}
}

type nha195HeldTasks struct{ tasks []integrations.Task }

func (h *nha195HeldTasks) Go(task integrations.Task) error {
	h.tasks = append(h.tasks, task)
	return nil
}

func TestNHA195SonosCompositionRegistersCodecBeforeAnyPilotOrHotEnable(t *testing.T) {
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := newNativeSonosRuntime(catalog)
	if err != nil {
		t.Fatal(err)
	}
	// The runtime is composed before there is a configured Sonos instance.
	// Later discovery succeeds through the real catalog; held tasks cannot
	// perform any polling, network I/O or physical command.
	_, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(fixture, fixture, catalog)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := sonos.ParseSettings(sonos.Input{Instance: "nha195-composition", Players: []sonos.PlayerInput{{Address: "192.168.50.10", UID: "RINCON_00112233445501400", Alias: "media_player.demo", Label: "Demo"}}})
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := settings.Instance()
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	factory, err := runtime.Factory(settings)
	if err != nil {
		t.Fatal(err)
	}
	pilot, tasks := factory(), &nha195HeldTasks{}
	if err = pilot.Start(context.Background(), session, tasks); err != nil {
		t.Fatal("composed codec did not admit native discovery", err)
	}
	t.Cleanup(func() { _ = pilot.Stop(context.Background()) })
	resolved, err := fixture.Resolve(context.Background(), instance, []integrations.SourceKey{settings.Players()[0].SourceKey()})
	if err != nil {
		t.Fatal(err)
	}
	descriptions, err := host.Capabilities(resolved[0].Entity)
	if err != nil || len(descriptions) != 2 || len(tasks.tasks) != 1 {
		t.Fatal("native discovery composition incomplete", err)
	}
	codecFound := false
	for _, description := range descriptions {
		codecFound = codecFound || description.SchemaId == sonos.Schema && description.GetExtension().GetActionMessage() == "home.media.v1.MediaAction"
	}
	if !codecFound {
		t.Fatal("common media codec not discovered")
	}
	if _, ok := runtime.Snapshot(resolved[0].Entity); ok {
		t.Fatal("composition fabricated an observation")
	}
}

func TestNHA195MediaProjectionRejectsTwoReadableOwners(t *testing.T) {
	authority, principal := nha195Authority(t, nha195Policy(t, nha195WriteGrant("garage-player", sonos.Schema, "player"), true, true))
	sources := []nativeMediaSource{nha195Source(nha195Snapshot(), sonos.Schema, true), nha195Source(nha195Snapshot(), androidtv.Schema, false)}
	if _, err := nha195Project(t, authority, principal, sources); !errors.Is(err, sonos.ErrUnavailable) {
		t.Fatal("ambiguous native ownership admitted", err)
	}
}

func TestNHA195ProjectionKeepsOnlySupportedMediaControls(t *testing.T) {
	value := nha195Snapshot()
	value.Capabilities.Operations = append(value.Capabilities.Operations, media.Operation_OPERATION_NEXT, media.Operation_OPERATION_PLAY)
	value.Capabilities.PositionMillis = &media.QuantityBounds{Maximum: 10000, Step: 1}
	value.Capabilities.Sources = []*media.Option{{Id: "opaque-source"}}
	value.Capabilities.Groups = []*media.Option{{Id: "opaque-group"}}
	id, _ := domain.ParseEntityID("garage-player")
	allow := func(domain.EntityID, string, string) error { return nil }
	got, err := projectNativeMediaActions(id, []nativeMediaSource{nha195Source(value, sonos.Schema, true)}, 42, "host-epoch", allow, allow)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Capabilities.Operations) != 3 || got.Capabilities.PositionMillis != nil || len(got.Capabilities.Sources) != 0 || len(got.Capabilities.Groups) != 0 || !proto.Equal(got.Capabilities.VolumeBasisPoints, value.Capabilities.VolumeBasisPoints) {
		t.Fatal("unsupported media control reached projection")
	}
}

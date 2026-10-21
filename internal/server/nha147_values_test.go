package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	helperv1 "github.com/damione1/Homeward/gen/home/helpers/v1"
	registryv1 "github.com/damione1/Homeward/gen/home/registry/v1"
	solarv1 "github.com/damione1/Homeward/gen/home/solar/v1"
	statev1 "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/gen/home/state/v1/statev1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/view"
	"google.golang.org/protobuf/proto"
)

type nha147ValueGrant struct {
	grant view.Grant
	nha050ViewConfig
}

func (a nha147ValueGrant) CurrentGrant(context.Context, view.ActorID) (view.Grant, error) {
	return a.grant, nil
}
func (a nha147ValueGrant) SubscribeGrantChanges(context.Context, view.ActorID) (<-chan struct{}, func(), error) {
	return make(chan struct{}), func() {}, nil
}

func nha147FixtureRequest[T any](message *T) *connect.Request[T] {
	request := connect.NewRequest(message)
	request.Header().Set("Authorization", "Bearer fixture")
	return request
}

// This uses the existing in-memory Host fixture, with no command dispatch,
// and the production handlers over loopback TLS. Native auth/Owner/PG are
// exercised independently by TestNHA147RunNoJobsSnapshotDispatchRestartAndRevocation.
func TestNHA147ConnectValuesKeepFalseZeroSolarAbsenceAndPartialSlots(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha147-values")
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	min, max, step := int64(0), int64(10), int64(1)
	descriptions := [][]*registryv1.CapabilityDescription{
		{{SchemaId: contracts.HelperSchema, Major: 1, Endpoint: "value", DisplayName: "Synthetic false", Constraints: &registryv1.CapabilityDescription_Helper{Helper: &helperv1.HelperConstraints{Kind: helperv1.HelperKind_HELPER_KIND_BOOLEAN, Operations: []helperv1.HelperOperation{helperv1.HelperOperation_HELPER_OPERATION_SET}}}}},
		{{SchemaId: contracts.HelperSchema, Major: 1, Endpoint: "value", DisplayName: "Synthetic zero", Constraints: &registryv1.CapabilityDescription_Helper{Helper: &helperv1.HelperConstraints{Kind: helperv1.HelperKind_HELPER_KIND_COUNTER, Operations: []helperv1.HelperOperation{helperv1.HelperOperation_HELPER_OPERATION_SET}, CounterMinimum: &min, CounterMaximum: &max, CounterStep: &step}}}},
		{{SchemaId: contracts.SolarSchema, Major: 1, Endpoint: "value", DisplayName: "Synthetic sun", Constraints: &registryv1.CapabilityDescription_Solar{Solar: &solarv1.SolarConstraints{}}}},
		{{SchemaId: contracts.SolarEventSchema, Major: 1, Endpoint: "value", DisplayName: "Synthetic present dawn", Constraints: &registryv1.CapabilityDescription_SolarEvent{SolarEvent: &solarv1.SolarEventConstraints{Event: solarv1.SolarEventKind_SOLAR_EVENT_KIND_DAWN}}}},
		{{SchemaId: contracts.SolarEventSchema, Major: 1, Endpoint: "value", DisplayName: "Synthetic absent dawn", Constraints: &registryv1.CapabilityDescription_SolarEvent{SolarEvent: &solarv1.SolarEventConstraints{Event: solarv1.SolarEventKind_SOLAR_EVENT_KIND_DAWN}}}},
		{{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "value", DisplayName: "Public composite slot", Constraints: &registryv1.CapabilityDescription_Switch{Switch: &registryv1.SwitchConstraints{SetOn: true}}},
			{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "private", DisplayName: "NHA147-FORBIDDEN-SLOT-CANARY", Constraints: &registryv1.CapabilityDescription_Switch{Switch: &registryv1.SwitchConstraints{SetOn: true}}}},
	}
	keys, candidates := make([]integrations.SourceKey, len(descriptions)), make([]integrations.Candidate, len(descriptions))
	for i, capabilities := range descriptions {
		keys[i], _ = integrations.NewSourceKey("value-"+string(rune('a'+i)), "value")
		candidates[i], err = integrations.NewCandidate(keys[i], capabilities)
		if err != nil {
			t.Fatal(err)
		}
	}
	bindings, err := session.Discover(ctx, candidates)
	if err != nil {
		t.Fatal(err)
	}
	identities, err := fixture.Resolve(ctx, instance, keys)
	if err != nil {
		t.Fatal(err)
	}
	records := make([]integrations.RegistryRecord, len(identities))
	access := make([]view.EntityAccess, len(identities))
	for i, identity := range identities {
		alias, _ := domain.ParseHAAlias("sensor.nha147_" + string(rune('a'+i)))
		records[i], err = integrations.NewRegistryRecord(identity.Device, identity.Entity, instance, keys[i], alias, alias, []domain.HAAlias{alias}, false)
		if err != nil {
			t.Fatal(err)
		}
		all := make([]authorization.CapabilitySlot, len(descriptions[i]))
		for n, description := range descriptions[i] {
			all[n], err = authorization.NewCapabilitySlot(description.SchemaId, description.Endpoint)
			if err != nil {
				t.Fatal(err)
			}
		}
		readable := all
		if i == len(identities)-1 {
			readable = all[:1]
		}
		access[i], err = view.NewEntityAccess(identity.Entity, all, readable)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.PublishRegistry(ctx, records); err != nil {
		t.Fatal(err)
	}
	absent, _ := domain.SolarOccurrenceMissing(domain.SolarNoEventWithinSearchHorizon)
	present, _ := domain.SolarOccurrenceAt(time.Now().UTC().Add(time.Hour))
	input := domain.SolarValueInput{Horizon: domain.SolarAboveHorizon, Rising: true}
	for i := range input.Next {
		input.Next[i] = absent
	}
	noon, _ := domain.SolarNoon.Index()
	midnight, _ := domain.SolarMidnight.Index()
	input.Next[noon] = present
	input.Next[midnight], _ = domain.SolarOccurrenceAt(time.Now().UTC().Add(12 * time.Hour))
	sun, err := domain.NewSolarValue(input)
	if err != nil {
		t.Fatal(err)
	}
	dawn, _ := domain.NewSolarEventValue(domain.SolarDawn, present)
	missing, _ := domain.NewSolarEventValue(domain.SolarDawn, absent)
	values := make([]domain.StateValue, 6)
	values[0], _ = domain.NewHelperStateValue(domain.NewHelperBoolean(false), 1)
	values[1], _ = domain.NewHelperStateValue(domain.NewHelperCounter(0), 2)
	values[2], _ = domain.NewSolarStateValue(sun)
	values[3], _ = domain.NewSolarEventStateValue(dawn)
	values[4], _ = domain.NewSolarEventStateValue(missing)
	values[5] = domain.NewSwitchValue(true)
	publish := func(index int, value domain.StateValue) {
		t.Helper()
		quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		at := time.Now().UTC()
		observation, err := integrations.NewStateObservation("value", quality, &value, &at, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Publish(ctx, bindings[index], observation); err != nil {
			t.Fatal(err)
		}
	}
	for i, value := range values {
		publish(i, value)
	}
	actor, _ := domain.ParseActorID("nha139-test-actor")
	grant, err := view.NewScopedGrant(actor, 1, sha256.Sum256([]byte("nha147-synthetic-credential")), access)
	if err != nil {
		t.Fatal(err)
	}
	stamp, _ := view.NewConfigStamp(1, sha256.Sum256([]byte("nha147-values-config")))
	authority := nha147ValueGrant{grant: grant, nha050ViewConfig: nha050ViewConfig{stamp: stamp}}
	service, err := view.NewService(fixture.Sequencer(), authority, authority, sequencer.RealClock{})
	if err != nil {
		t.Fatal(err)
	}
	handler := &coreRPCHandler{views: service, grants: authority, commands: &integrations.DurableCommandService{}, epoch: fixture.Sequencer()}
	plan, err := NewPublicPlan(&nha139Authenticator{}, &nha139Authorizer{}, &nha139Auditor{})
	if err != nil {
		t.Fatal(err)
	}
	if err := registerCoreRPCRoutes(plan, handler); err != nil {
		t.Fatal(err)
	}
	settings := config.DefaultPublicSettings()
	compiled, err := compilePublicPlan(plan, settings)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(compiled.handler(settings, newPublicAdmissions()))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	client := statev1connect.NewStateServiceClient(server.Client(), server.URL)
	response, err := client.OpenView(ctx, nha147FixtureRequest(&statev1.OpenViewRequest{PageSize: 16, Filter: &statev1.ViewFilter{}}))
	if err != nil {
		t.Fatal(err)
	}
	page := response.Msg
	if len(page.Entities) != 6 || len(page.States) != 5 {
		t.Fatal("partial slot leaked or values vanished", page)
	}
	encoded, err := proto.Marshal(page)
	if err != nil || bytes.Contains(encoded, []byte("NHA147-FORBIDDEN-SLOT-CANARY")) {
		t.Fatal("forbidden slot escaped the public conversion", err)
	}
	byID := make(map[string]*statev1.ObservedState)
	for _, state := range page.States {
		byID[state.Entity.GetEntityId()] = state
	}
	id := func(index int) string { text, _ := identities[index].Entity.Value(); return text }
	for index := 0; index < 5; index++ {
		if byID[id(index)] == nil {
			t.Fatal("typed observation missing", index)
		}
	}
	if _, ok := byID[id(0)].GetHelper().Value.(*helperv1.HelperValue_Boolean); !ok || byID[id(0)].GetHelper().GetBoolean() || byID[id(0)].GetHelper().HelperRevision != 1 {
		t.Fatal("present false became absence")
	}
	if _, ok := byID[id(1)].GetHelper().Value.(*helperv1.HelperValue_Counter); !ok || byID[id(1)].GetHelper().GetCounter() != 0 {
		t.Fatal("present zero became absence")
	}
	if byID[id(2)].GetSolar() == nil || byID[id(2)].GetSolar().ElevationDegrees == nil || *byID[id(2)].GetSolar().ElevationDegrees != 0 {
		t.Fatal("present solar zero became absence")
	}
	if byID[id(3)].GetSolarEvent().Occurrence.GetAt() == nil || byID[id(4)].GetSolarEvent().Occurrence.GetAbsent() != solarv1.SolarAbsenceReason_SOLAR_ABSENCE_REASON_NO_EVENT_WITHIN_SEARCH_HORIZON {
		t.Fatal("solar presence and absence conflated")
	}
	// A newer absent event passes through replay; the old page remains present.
	publish(3, values[4])
	watch, err := client.WatchView(ctx, nha147FixtureRequest(&statev1.WatchViewRequest{Cursor: page.Cursor, Filter: &statev1.ViewFilter{}}))
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close()
	if !watch.Receive() || watch.Msg().GetDelta() == nil || len(watch.Msg().GetDelta().States) != 1 || watch.Msg().GetDelta().States[0].GetSolarEvent().Occurrence.GetAbsent() != solarv1.SolarAbsenceReason_SOLAR_ABSENCE_REASON_NO_EVENT_WITHIN_SEARCH_HORIZON || byID[id(3)].GetSolarEvent().Occurrence.GetAt() == nil {
		t.Fatal("solar replay mutated its capture", watch.Err())
	}
}

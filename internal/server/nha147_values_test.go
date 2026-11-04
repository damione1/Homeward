package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	helperv1 "github.com/damione1/Homeward/gen/home/helpers/v1"
	registryv1 "github.com/damione1/Homeward/gen/home/registry/v1"
	solarv1 "github.com/damione1/Homeward/gen/home/solar/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/registry"
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

type nha147ProjectionOwner struct {
	home    apitypes.HomeMetadata
	records []integrations.RegistryRecord
}

func (o nha147ProjectionOwner) HomeName(context.Context) (apitypes.HomeName, error) {
	return o.home.Name(), nil
}
func (o nha147ProjectionOwner) HomeMetadata(context.Context) (apitypes.HomeMetadata, error) {
	return o.home, nil
}
func (o nha147ProjectionOwner) ResolveEntityName(_ context.Context, name apitypes.EntityName) (domain.EntityID, error) {
	for _, record := range o.records {
		_, id, _ := record.IDs()
		expected, _ := apitypes.EntityNameForIdentity(o.home.Name(), id)
		if name == expected {
			return id, nil
		}
	}
	return domain.EntityID{}, registry.ErrUnconfigured
}
func (o nha147ProjectionOwner) UpdateEntityMetadata(context.Context, authorization.MetadataEvidence, domain.EntityID, apitypes.EntityUpdate, func(apitypes.EntityMetadata) ([]byte, error)) (apitypes.EntityMetadata, error) {
	return apitypes.EntityMetadata{}, errors.New("read-only synthetic fixture")
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
	for i, identity := range identities {
		alias, _ := domain.ParseHAAlias("sensor.nha147_" + string(rune('a'+i)))
		records[i], err = integrations.NewRegistryRecord(identity.Device, identity.Entity, instance, keys[i], alias, alias, []domain.HAAlias{alias}, false)
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
	homeName, _ := apitypes.ParseHomeName("homes/synthetic-values")
	homeMeta, err := apitypes.NewHomeMetadata(homeName, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	owner := nha147ProjectionOwner{home: homeMeta, records: records}
	account, _ := contracts.ParseAccountID("nha139-test-actor")
	var policyYAML strings.Builder
	fmt.Fprintf(&policyYAML, "schema: homeward.authorization/1\naccount_id: %q\ngrants:\n  - scope: {family: state_registry, operation: read}\n    objects:\n      entities:\n        explicit:\n", "nha139-test-actor")
	for i, identity := range identities {
		id, _ := identity.Entity.Value()
		if i == len(identities)-1 {
			fmt.Fprintf(&policyYAML, "          - entity_id: %q\n            capabilities: {explicit: [{schema_id: %s, endpoint: value}]}\n", id, contracts.SwitchSchema)
		} else {
			fmt.Fprintf(&policyYAML, "          - entity_id: %q\n            capabilities: {all: {}}\n", id)
		}
	}
	policySource, err := authorization.AdmitPolicySource(account, []byte(policyYAML.String()))
	if err != nil {
		t.Fatal(err)
	}
	authAuthority := authorization.NewAuthority()
	if err := authAuthority.Hydrate([]authorization.AccountState{nha164AuthorityState(t, policySource, auth.AccountRole_ACCOUNT_ROLE_USER, 1)}); err != nil {
		t.Fatal(err)
	}
	sessionID, _ := contracts.ParseSessionID("nha147-values-session")
	principal, err := authorization.NativePrincipal(account, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	stamp, _ := view.NewConfigStamp(1, sha256.Sum256([]byte("nha147-values-config")))
	viewAuth := &policyViewAuthorizer{authority: authAuthority, source: fixture.Sequencer()}
	core := &coreRPCHandler{epoch: fixture.Sequencer()}
	service, err := view.NewService(fixture.Sequencer(), viewAuth, nha050ViewConfig{stamp: stamp}, sequencer.RealClock{}, core.capabilityProjector(owner, authAuthority))
	if err != nil {
		t.Fatal(err)
	}
	homeHandler, err := newHomeResourceHandler(owner, authAuthority, service)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewPublicPlan(&nha139Authenticator{}, &nha139Authorizer{}, &nha139Auditor{})
	if err != nil {
		t.Fatal(err)
	}
	if err := registerEntityViewRoutes(plan, homeHandler); err != nil {
		t.Fatal(err)
	}
	settings := config.DefaultPublicSettings()
	compiled, err := compilePublicPlan(plan, settings)
	if err != nil {
		t.Fatal(err)
	}
	baseHandler := compiled.handler(settings, newPublicAdmissions())
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		withPrincipal, err := authorization.WithPrincipal(r.Context(), principal)
		if err != nil {
			http.Error(w, "fixture principal", http.StatusInternalServerError)
			return
		}
		baseHandler.ServeHTTP(w, r.WithContext(withPrincipal))
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	client := apiv2connect.NewEntityViewServiceClient(server.Client(), server.URL)
	created, err := client.CreateEntityView(ctx, nha147FixtureRequest(&api.CreateEntityViewRequest{Parent: homeName.String(), EntityView: &api.EntityView{}}))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.QueryEntityView(ctx, nha147FixtureRequest(&api.QueryEntityViewRequest{Name: created.Msg.Name, PageSize: 16}))
	if err != nil {
		t.Fatal(err)
	}
	page := response.Msg
	observedCount := 0
	for _, entry := range page.Entries {
		for _, cap := range entry.Capabilities {
			if cap.GetHelper().GetObservation() != nil || cap.GetSolar().GetObservation() != nil || cap.GetSolarEvent().GetObservation() != nil || cap.GetPower().GetObservation() != nil {
				observedCount++
			}
		}
	}
	if len(page.Entries) != 6 || observedCount != 5 {
		t.Fatal("partial slot leaked or values vanished", page)
	}
	encoded, err := proto.Marshal(page)
	if err != nil || bytes.Contains(encoded, []byte("NHA147-FORBIDDEN-SLOT-CANARY")) {
		t.Fatal("forbidden slot escaped the public conversion", err)
	}
	byName := make(map[string]*api.EntityViewEntry)
	for _, entry := range page.Entries {
		byName[entry.Entity.GetName()] = entry
	}
	entityName := func(index int) string {
		n, _ := apitypes.EntityNameForIdentity(homeName, identities[index].Entity)
		return n.String()
	}
	for index := 0; index < 5; index++ {
		if byName[entityName(index)] == nil || len(byName[entityName(index)].Capabilities) == 0 {
			t.Fatal("typed observation missing", index)
		}
	}
	helper0 := byName[entityName(0)].Capabilities[0].GetHelper().GetObservation().GetValue()
	if _, ok := helper0.GetValue().(*api.HelperValue_BooleanValue); !ok || helper0.GetBooleanValue() || helper0.GetHelperRevision() != 1 {
		t.Fatal("present false became absence")
	}
	helper1 := byName[entityName(1)].Capabilities[0].GetHelper().GetObservation().GetValue()
	if _, ok := helper1.GetValue().(*api.HelperValue_Counter); !ok || helper1.GetCounter() != 0 {
		t.Fatal("present zero became absence")
	}
	solar2 := byName[entityName(2)].Capabilities[0].GetSolar().GetObservation().GetValue()
	if solar2 == nil || solar2.ElevationDegrees == nil || *solar2.ElevationDegrees != 0 {
		t.Fatal("present solar zero became absence")
	}
	event3 := byName[entityName(3)].Capabilities[0].GetSolarEvent().GetObservation().GetValue()
	event4 := byName[entityName(4)].Capabilities[0].GetSolarEvent().GetObservation().GetValue()
	if event3.GetOccurrence().GetEventTime() == nil || event4.GetOccurrence().GetAbsent() != api.SolarAbsenceReason_SOLAR_ABSENCE_REASON_NO_EVENT_WITHIN_SEARCH_HORIZON {
		t.Fatal("solar presence and absence conflated")
	}
	// A newer absent event passes through replay; the old page remains present.
	publish(3, values[4])
	watch, err := client.WatchEntityView(ctx, nha147FixtureRequest(&api.WatchEntityViewRequest{Name: created.Msg.Name, ResumeToken: page.ResumeToken}))
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close()
	if !watch.Receive() || watch.Msg().GetUpsert() == nil || len(watch.Msg().GetUpsert().Capabilities) != 1 || watch.Msg().GetUpsert().Capabilities[0].GetSolarEvent().GetObservation().GetValue().GetOccurrence().GetAbsent() != api.SolarAbsenceReason_SOLAR_ABSENCE_REASON_NO_EVENT_WITHIN_SEARCH_HORIZON || byName[entityName(3)].Capabilities[0].GetSolarEvent().GetObservation().GetValue().GetOccurrence().GetEventTime() == nil {
		t.Fatal("solar replay mutated its capture", watch.Err())
	}
}

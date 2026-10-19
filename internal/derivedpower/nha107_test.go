package derivedpower

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/contracts"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/reactivetemplate"
	"github.com/damione1/Homeward/internal/sequencer"
)

func nha107NextPower(t *testing.T, updates <-chan reactivetemplate.Update, kind Kind) Projection {
	t.Helper()
	select {
	case update := <-updates:
		projection, err := Project(kind, update)
		if err != nil {
			t.Fatal(err)
		}
		return projection
	case <-time.After(2 * time.Second):
		t.Fatal("derived power update timeout")
		return Projection{}
	}
}

func nha107ObservedUPS(t *testing.T, session *integrations.Session, binding integrations.Binding, unit domain.Unit, load float64, freshness domain.Freshness, provenance domain.Provenance) {
	t.Helper()
	measurement, err := domain.NewMeasurement(load, unit, 1, domain.SemanticMeasurement, domain.AggregationCurrent)
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.NewMeasurementValue(measurement)
	if err != nil {
		t.Fatal(err)
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, freshness, provenance)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	observation, err := integrations.NewStateObservation("load", quality, &value, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Publish(context.Background(), binding, observation); err != nil {
		t.Fatal(err)
	}
}

func nha107UnavailableUPS(t *testing.T, session *integrations.Session, binding integrations.Binding) {
	t.Helper()
	quality, err := domain.NewQuality(domain.AvailabilityUnavailable, domain.FreshnessStale, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	observation, err := integrations.NewStateObservation("load", quality, nil, nil, time.Now().UTC(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Publish(context.Background(), binding, observation); err != nil {
		t.Fatal(err)
	}
}

type nha107UPSRig struct {
	session      *integrations.Session
	binding      integrations.Binding
	updates      <-chan reactivetemplate.Update
	host         *integrations.Manager
	fixture      *fake.Fixture
	sourceRecord integrations.RegistryRecord
}

func nha107UPSFixture(t *testing.T, unitText string) nha107UPSRig {
	t.Helper()

	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha107-ups-source")
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := integrations.NewSourceKey("synthetic-ups", "load")
	description := &wireregistry.CapabilityDescription{SchemaId: contracts.MeasurementSchema, Major: 1, DisplayName: "Synthetic UPS load", Endpoint: "load", Constraints: &wireregistry.CapabilityDescription_Measurement{Measurement: &wireregistry.MeasurementConstraints{Unit: unitText, Precision: 1, Semantic: wireregistry.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT, Aggregation: wireregistry.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT}}}
	candidate, err := integrations.NewCandidate(key, []*wireregistry.CapabilityDescription{description})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := session.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil {
		t.Fatal(err)
	}
	device, entity, err := resolved[0].IDs()
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := domain.ParseHAAlias("sensor.ups_load")
	record, err := integrations.NewRegistryRecord(device, entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	output, _ := domain.ParseEntityID("ent_nha107_ups_output")
	definition, err := Definition(KindUPS, output)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := reactivetemplate.NewPlan([]reactivetemplate.Definition{definition}, nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	service, err := reactivetemplate.Start(context.Background(), fixture.Sequencer(), plan, reactivetemplate.RealClock{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return nha107UPSRig{session: session, binding: resolved[0], updates: service.Updates(), host: host, fixture: fixture, sourceRecord: record}
}

type nha107NilContext struct{ context.Context }

func TestNHA107PublisherAdmitsContextAndPublishesEstimatedWattThenAbsence(t *testing.T) {
	rig := nha107UPSFixture(t, "%")
	ctx := context.Background()
	instance, err := Instance()
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]integrations.SourceKey, 0, 3)
	for _, kind := range Kinds() {
		key, err := kind.SourceKey()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	identities, err := rig.fixture.Resolve(ctx, instance, keys)
	if err != nil || len(identities) != 3 {
		t.Fatalf("fixture identities: %v", err)
	}
	bindings := make([]Binding, 0, 3)
	records := []integrations.RegistryRecord{rig.sourceRecord}
	for index, kind := range Kinds() {
		binding, err := NewBinding(kind, identities[index].Entity)
		if err != nil {
			t.Fatal(err)
		}
		bindings = append(bindings, binding)
		aliasName, err := kind.OutputAlias()
		if err != nil {
			t.Fatal(err)
		}
		alias, err := domain.ParseHAAlias(aliasName)
		if err != nil {
			t.Fatal(err)
		}
		record, err := integrations.NewRegistryRecord(identities[index].Device, identities[index].Entity, instance, keys[index], alias, alias, []domain.HAAlias{alias}, false)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if err := rig.fixture.PublishRegistry(ctx, records); err != nil {
		t.Fatal(err)
	}
	publisher, err := NewPublisher(bindings)
	if err != nil {
		t.Fatal(err)
	}
	var nilContext *nha107NilContext
	if err := publisher.Prepare(nilContext, rig.host); err != ErrInvalidPower || publisher.session != nil {
		t.Fatalf("typed-nil Prepare opened a Host session: %v", err)
	}
	if err := publisher.Prepare(ctx, rig.host); err != nil {
		t.Fatalf("Prepare after rejected context: %v", err)
	}
	next := func() reactivetemplate.Update {
		t.Helper()
		select {
		case update := <-rig.updates:
			return update
		case <-time.After(2 * time.Second):
			t.Fatal("UPS output update timeout")
			return reactivetemplate.Update{}
		}
	}
	initial := next()
	if err := publisher.Publish(nilContext, initial); err != ErrInvalidPower {
		t.Fatalf("typed-nil Publish: %v", err)
	}
	if _, exists := rig.fixture.State(bindings[0].Target()); exists {
		t.Fatal("typed-nil Publish changed Host state")
	}
	if err := publisher.Publish(ctx, initial); err != nil {
		t.Fatal(err)
	}
	unit, err := domain.NewUnitCatalog().Parse("%")
	if err != nil {
		t.Fatal(err)
	}
	nha107ObservedUPS(t, rig.session, rig.binding, unit, 100, domain.FreshnessFresh, domain.ProvenanceObserved)
	positive := next()
	for attempt := 0; attempt < 4; attempt++ {
		projection, err := Project(KindUPS, positive)
		if err != nil {
			t.Fatal(err)
		}
		if watts, ok := projection.Watts(); ok && watts == 900 {
			break
		}
		positive = next()
	}
	positiveProjection, err := Project(KindUPS, positive)
	if err != nil {
		t.Fatal(err)
	}
	if watts, ok := positiveProjection.Watts(); !ok || watts != 900 {
		t.Fatal("source report did not yield the 100% UPS projection")
	}
	if err := publisher.Publish(ctx, positive); err != nil {
		t.Fatal(err)
	}
	state, exists := rig.fixture.State(bindings[0].Target())
	if !exists {
		t.Fatal("positive power was not published")
	}
	_, quality, times, _, _, err := state.Components()
	if err != nil {
		t.Fatal(err)
	}
	availability, freshness, provenance, err := quality.Components()
	if err != nil || availability != domain.AvailabilityAvailable || freshness != domain.FreshnessFresh || provenance != domain.ProvenanceEstimated {
		t.Fatalf("positive power quality: %v/%v/%v, %v", availability, freshness, provenance, err)
	}
	if times.ObservedAt != nil || times.ReceivedAt.IsZero() {
		t.Fatal("derived power invented a capture timestamp or lost receipt time")
	}
	value, present := state.CurrentValue()
	if !present {
		t.Fatal("positive power has no current value")
	}
	measurement, ok := value.Measurement()
	if !ok {
		t.Fatal("power output is not a measurement")
	}
	parts, err := measurement.Components()
	unitName, unitErr := parts.Unit.Symbol()
	if err != nil || unitErr != nil || parts.Value != 900 || unitName != "W" ||
		parts.Precision != 3 || parts.Dimension != domain.DimensionPower ||
		parts.Semantic != domain.SemanticMeasurement || parts.Aggregation != domain.AggregationCurrent {
		t.Fatalf("positive power metadata: %+v, %v/%v", parts, err, unitErr)
	}
	nha107UnavailableUPS(t, rig.session, rig.binding)
	missing := next()
	for attempt := 0; attempt < 4; attempt++ {
		projection, err := Project(KindUPS, missing)
		if err != nil {
			t.Fatal(err)
		}
		if projection.Status() == StatusMissingSource {
			break
		}
		missing = next()
	}
	if projection, err := Project(KindUPS, missing); err != nil || projection.Status() != StatusMissingSource {
		t.Fatal("source outage did not yield explicit missing source")
	}
	if err := publisher.Publish(ctx, missing); err != nil {
		t.Fatal(err)
	}
	state, exists = rig.fixture.State(bindings[0].Target())
	if !exists {
		t.Fatal("absence power was not published")
	}
	_, quality, _, _, _, err = state.Components()
	if err != nil {
		t.Fatal(err)
	}
	availability, freshness, provenance, err = quality.Components()
	if err != nil || availability != domain.AvailabilityUnavailable || freshness != domain.FreshnessStale || provenance != domain.ProvenanceEstimated {
		t.Fatalf("absent power quality: %v/%v/%v, %v", availability, freshness, provenance, err)
	}
	if _, present := state.CurrentValue(); present {
		t.Fatal("absent power kept a numeric current value")
	}
	if old, present := state.DisplayValue(); !present {
		t.Fatal("absent power lost the last display value")
	} else if oldMeasurement, ok := old.Measurement(); !ok {
		t.Fatal("display value lost measurement type")
	} else if oldParts, err := oldMeasurement.Components(); err != nil || oldParts.Value != 900 {
		t.Fatalf("display value changed: %+v, %v", oldParts, err)
	}
}

func TestNHA107UPSObservedPercentageZeroAndRestoredProvenance(t *testing.T) {
	rig := nha107UPSFixture(t, "%")
	if got := nha107NextPower(t, rig.updates, KindUPS); got.Status() != StatusMissingSource {
		t.Fatal("unknown UPS load fabricated power")
	}
	unit, err := domain.NewUnitCatalog().Parse("%")
	if err != nil {
		t.Fatal(err)
	}
	nha107ObservedUPS(t, rig.session, rig.binding, unit, 50, domain.FreshnessFresh, domain.ProvenanceObserved)
	if watts, ok := nha107NextPower(t, rig.updates, KindUPS).Watts(); !ok || watts != 450 {
		t.Fatalf("50 percent UPS load: %v %v", watts, ok)
	}
	nha107ObservedUPS(t, rig.session, rig.binding, unit, 50, domain.FreshnessStale, domain.ProvenanceObserved)
	if got := nha107NextPower(t, rig.updates, KindUPS); got.Status() != StatusMissingSource {
		t.Fatal("stale source retained current power")
	}
	nha107ObservedUPS(t, rig.session, rig.binding, unit, 0, domain.FreshnessFresh, domain.ProvenanceObserved)
	if watts, ok := nha107NextPower(t, rig.updates, KindUPS).Watts(); !ok || watts != 0 {
		t.Fatalf("observed zero UPS load: %v %v", watts, ok)
	}
	nha107UnavailableUPS(t, rig.session, rig.binding)
	if got := nha107NextPower(t, rig.updates, KindUPS); got.Status() != StatusMissingSource {
		t.Fatal("unavailable source retained current power")
	}
	nha107ObservedUPS(t, rig.session, rig.binding, unit, 0, domain.FreshnessFresh, domain.ProvenanceObserved)
	if watts, ok := nha107NextPower(t, rig.updates, KindUPS).Watts(); !ok || watts != 0 {
		t.Fatal("valid zero did not recover after source outage")
	}
	// 044 still exposes a Fresh estimated aggregate result. Its retained
	// individual Read must prevent 107 from admitting a restored input.
	nha107ObservedUPS(t, rig.session, rig.binding, unit, 0, domain.FreshnessFresh, domain.ProvenanceRestored)
	if got := nha107NextPower(t, rig.updates, KindUPS); got.Status() != StatusMissingSource {
		t.Fatal("restored fresh UPS value was treated as observed power")
	}
}

func TestNHA107UPSIncompatibleUnitAndNonFiniteHostBoundary(t *testing.T) {
	rig := nha107UPSFixture(t, "kW")
	if got := nha107NextPower(t, rig.updates, KindUPS); got.Status() != StatusMissingSource {
		t.Fatal("source without report fabricated power")
	}
	unit, err := domain.NewUnitCatalog().Parse("kW")
	if err != nil {
		t.Fatal(err)
	}
	nha107ObservedUPS(t, rig.session, rig.binding, unit, 0.5, domain.FreshnessFresh, domain.ProvenanceObserved)
	if got := nha107NextPower(t, rig.updates, KindUPS); got.Status() != StatusMissingSource {
		t.Fatal("kW was treated as percentage")
	}
	// NHA-017 refuses non-finite measurements before a Host state or 044
	// Read can exist. This is the reachable boundary for device reports.
	if _, err := domain.NewMeasurement(math.NaN(), unit, 1, domain.SemanticMeasurement, domain.AggregationCurrent); err == nil {
		t.Fatal("NaN passed the source measurement admission boundary")
	}
	if _, err := domain.NewMeasurement(math.Inf(1), unit, 1, domain.SemanticMeasurement, domain.AggregationCurrent); err == nil {
		t.Fatal("infinity passed the source measurement admission boundary")
	}
}

// Helpers et groupes.md retains the source YAML fallback behavior. The
// separate pinned HA capture executes these exact decoded expressions with
// no source states. 043 deliberately excludes multiplication and blocks.
func TestNHA107LegacyFallbacksRemainSeparate(t *testing.T) {
	legacy := []struct {
		kind   Kind
		id     string
		source string
	}{
		{KindUPS, "ups-legacy", "{{ (states('sensor.ups_load') | float(0)) / 100 * 900 }}"},
		{KindFloorKitchen, "floor-legacy", "{% if state_attr('climate.plancher_cuisine', 'hvac_action') == 'heating' %}1294{% else %}0{% endif %}"},
		{KindHVAC, "hvac-legacy", "{% set action = state_attr('climate.home', 'hvac_action') %}\n  {% if action == 'heating' %}\n    15400\n  {% elif action == 'cooling' %}\n    3200\n  {% elif action == 'fan' %}\n    400\n  {% else %}\n    0\n  {% endif %}"},
	}
	dir := filepath.Join("..", "..", "compat", "ha", "nha107")
	input, err := os.ReadFile(filepath.Join(dir, "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	worker, err := os.ReadFile(filepath.Join(dir, "ha_worker.py"))
	if err != nil {
		t.Fatal(err)
	}
	capture, err := os.ReadFile(filepath.Join(dir, "observed.json"))
	if err != nil {
		t.Fatalf("real pinned HA capture must be recorded before Go checks: %v", err)
	}
	var fixture struct {
		States []json.RawMessage `json:"states"`
		Cases  []struct {
			ID       string `json:"id"`
			Template string `json:"template"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(input, &fixture); err != nil || len(fixture.States) != 0 || len(fixture.Cases) != len(legacy) {
		t.Fatalf("invalid closed legacy corpus: %v", err)
	}
	var observed struct {
		Inputs struct {
			CasesSHA256  string `json:"cases_sha256"`
			WorkerSHA256 string `json:"ha_worker_sha256"`
		} `json:"inputs"`
		Reference struct {
			ImageIndex    string `json:"image_index"`
			ImageManifest string `json:"image_manifest"`
			Platform      string `json:"platform"`
			Runtime       struct {
				Python        string `json:"python"`
				HomeAssistant string `json:"homeassistant"`
				Jinja2        string `json:"jinja2"`
			} `json:"runtime"`
		} `json:"reference"`
		HA []struct {
			ID  string `json:"id"`
			Raw struct {
				Text   string `json:"text"`
				Native struct {
					Type  string      `json:"type"`
					Value json.Number `json:"value"`
				} `json:"native"`
			} `json:"raw"`
		} `json:"ha"`
	}
	if err := json.Unmarshal(capture, &observed); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]string{
		"linux/arm64": "sha256:17100609af8bb79bf5390f5d514b9aaeb00d184ce758f37769cd80c450851db7",
		"linux/amd64": "sha256:a7ecc966ffed643296af32d576ee081910a0362c01e695db24fbee6816175fbe",
	}
	if observed.Inputs.CasesSHA256 != fmt.Sprintf("%x", sha256.Sum256(input)) ||
		observed.Inputs.WorkerSHA256 != fmt.Sprintf("%x", sha256.Sum256(worker)) ||
		observed.Reference.ImageIndex != "sha256:130241f28d01fa80dfa3b44f7d89781c188d8642ecab9024f079f19f9f82baa0" ||
		observed.Reference.ImageManifest == "" || observed.Reference.ImageManifest != manifest[observed.Reference.Platform] ||
		observed.Reference.Runtime.Python != "3.14.6" || observed.Reference.Runtime.HomeAssistant != "2026.10.1" ||
		observed.Reference.Runtime.Jinja2 != "3.1.6" || len(observed.HA) != len(legacy) {
		t.Fatal("legacy HA capture, source hashes, or pinned runtime mismatch")
	}
	for i, item := range legacy {
		name, _ := item.kind.ProgramName()
		t.Run(name, func(t *testing.T) {
			if fixture.Cases[i].ID != item.id || fixture.Cases[i].Template != item.source || observed.HA[i].ID != item.id {
				t.Fatal("legacy source, case order, or captured result drifted")
			}
			if _, err := template.Compile(item.source); err == nil {
				t.Fatal("bounded 043 unexpectedly admitted historical expression")
			}
			raw := observed.HA[i].Raw
			if text := strings.TrimSpace(raw.Text); text != "0" && text != "0.0" {
				t.Fatalf("real HA fallback text: %q", raw.Text)
			}
			if raw.Native.Type != "int" && raw.Native.Type != "float" || raw.Native.Value != "0" && raw.Native.Value != "0.0" {
				t.Fatalf("real HA fallback native value: %+v", raw.Native)
			}
			sequence, err := sequencer.New(sequencer.RealClock{}, 128)
			if err != nil {
				t.Fatal(err)
			}
			if err := sequence.PublishRegistry(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			output, _ := domain.ParseEntityID("ent_nha107_legacy_contrast")
			definition, err := Definition(item.kind, output)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := reactivetemplate.NewPlan([]reactivetemplate.Definition{definition}, nil, time.UTC)
			if err != nil {
				t.Fatal(err)
			}
			service, err := reactivetemplate.Start(context.Background(), sequence, plan, reactivetemplate.RealClock{})
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close(context.Background())
			if projection := nha107NextPower(t, service.Updates(), item.kind); projection.Status() != StatusMissingSource {
				t.Fatal("new projection reproduced legacy zero fallback")
			}
		})
	}
}

func TestNHA107ClimateActivityMatrixAndMissingAttribute(t *testing.T) {
	for _, row := range []struct {
		kind  Kind
		cases []struct {
			activity domain.ClimateActivity
			watts    float64
			present  bool
		}
	}{
		{KindFloorKitchen, []struct {
			activity domain.ClimateActivity
			watts    float64
			present  bool
		}{{domain.ClimateActivityHeating, 1294, true}, {domain.ClimateActivityIdle, 0, true}, {domain.ClimateActivityCooling, 0, false}}},
		{KindHVAC, []struct {
			activity domain.ClimateActivity
			watts    float64
			present  bool
		}{{domain.ClimateActivityHeating, 15400, true}, {domain.ClimateActivityCooling, 3200, true}, {domain.ClimateActivityFanOnly, 400, true}, {domain.ClimateActivityIdle, 0, true}, {domain.ClimateActivityDrying, 0, false}}},
	} {
		name, _ := row.kind.ProgramName()
		t.Run(name, func(t *testing.T) {
			host, fixture, err := fake.NewCommandFixture()
			if err != nil {
				t.Fatal(err)
			}
			instance, _ := domain.ParseIntegrationInstanceID("nha107-climate-source")
			session, err := host.OpenSession(instance)
			if err != nil {
				t.Fatal(err)
			}
			key, _ := integrations.NewSourceKey("synthetic-climate", "activity")
			description := &wireregistry.CapabilityDescription{SchemaId: contracts.ClimateSchema, Major: 1, DisplayName: "Synthetic activity", Endpoint: "climate", Constraints: &wireregistry.CapabilityDescription_Climate{Climate: &wireregistry.ClimateConstraints{
				ModeOptions:         []wireregistry.ClimateMode{wireregistry.ClimateMode_CLIMATE_MODE_HEAT},
				ObservedModeOptions: []wireregistry.ClimateMode{wireregistry.ClimateMode_CLIMATE_MODE_HEAT},
				ActivityOptions:     []wireregistry.ClimateActivity{wireregistry.ClimateActivity_CLIMATE_ACTIVITY_IDLE, wireregistry.ClimateActivity_CLIMATE_ACTIVITY_HEATING, wireregistry.ClimateActivity_CLIMATE_ACTIVITY_COOLING, wireregistry.ClimateActivity_CLIMATE_ACTIVITY_FAN_ONLY, wireregistry.ClimateActivity_CLIMATE_ACTIVITY_DRYING},
			}}}
			candidate, err := integrations.NewCandidate(key, []*wireregistry.CapabilityDescription{description})
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := session.Discover(context.Background(), []integrations.Candidate{candidate})
			if err != nil {
				t.Fatal(err)
			}
			device, entity, err := resolved[0].IDs()
			if err != nil {
				t.Fatal(err)
			}
			aliasName, _ := row.kind.SourceAlias()
			alias, _ := domain.ParseHAAlias(aliasName)
			record, err := integrations.NewRegistryRecord(device, entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{record}); err != nil {
				t.Fatal(err)
			}
			output, _ := domain.ParseEntityID("ent_nha107_climate_output")
			definition, err := Definition(row.kind, output)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := reactivetemplate.NewPlan([]reactivetemplate.Definition{definition}, nil, time.UTC)
			if err != nil {
				t.Fatal(err)
			}
			service, err := reactivetemplate.Start(context.Background(), fixture.Sequencer(), plan, reactivetemplate.RealClock{})
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close(context.Background())
			if got := nha107NextPower(t, service.Updates(), row.kind); got.Status() != StatusMissingSource {
				t.Fatal("absent activity fabricated power")
			}
			publish := func(activity *domain.ClimateActivity) {
				t.Helper()
				mode := domain.ClimateModeHeat
				climate, err := domain.NewClimateValue(domain.ClimateValueInput{Mode: &mode, Activity: activity})
				if err != nil {
					t.Fatal(err)
				}
				value, err := domain.NewClimateStateValue(climate)
				if err != nil {
					t.Fatal(err)
				}
				quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
				at := time.Now().UTC()
				observation, err := integrations.NewStateObservation("climate", quality, &value, &at, at, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := session.Publish(context.Background(), resolved[0], observation); err != nil {
					t.Fatal(err)
				}
			}
			publish(nil)
			if got := nha107NextPower(t, service.Updates(), row.kind); got.Status() != StatusMissingSource {
				t.Fatal("requested heat mode substituted for absent activity")
			}
			for _, item := range row.cases {
				activity := item.activity
				publish(&activity)
				got := nha107NextPower(t, service.Updates(), row.kind)
				watts, present := got.Watts()
				if present != item.present || present && watts != item.watts {
					t.Fatalf("activity %v: %v W present=%v", activity, watts, present)
				}
			}
		})
	}
}

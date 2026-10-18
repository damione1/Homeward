package neviweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/storage"
)

func nha091Settings(t *testing.T) Settings {
	t.Helper()
	input := Input{Instance: "nha091-fixture", BaseURL: "http://127.0.0.1:9000", AccountID: "123", NetworkID: "456", UsernameSecret: "username", PasswordSecret: "password", SessionSecret: "session", Bindings: make([]BindingInput, 0, 11)}
	for i, alias := range retainedAliases {
		device := fmt.Sprintf("device-%02d", i)
		if i == 9 {
			device = ""
		}
		input.Bindings = append(input.Bindings, BindingInput{Alias: alias, DeviceID: device})
	}
	settings, err := ParseSettings(input)
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

func TestNHA091SettingsExplicitElevenAndBudget(t *testing.T) {
	s := nha091Settings(t)
	if s.PollInterval() != 540*time.Second {
		t.Fatal("wrong upstream default cadence")
	}
	limit, reserve, zone := s.Budget()
	if limit != 5000 || reserve != 25000 || zone != "America/Toronto" {
		t.Fatalf("default local budget=%d/%d/%s", limit, reserve, zone)
	}
	bindings := s.Bindings()
	if len(bindings) != 11 {
		t.Fatal("missing retained binding")
	}
	for _, binding := range bindings {
		device, entity, e := binding.SourceKey().Components()
		if e != nil || strings.Contains(device, binding.Alias()) || entity == "" {
			t.Fatal("source identity derived from HA alias")
		}
	}
	zero := 0
	input := Input{Instance: "nha091-fixture", BaseURL: "http://127.0.0.1:9000", AccountID: "123", NetworkID: "456", UsernameSecret: "username", PasswordSecret: "password", SessionSecret: "session", DailyLimit: 30000, ExternalReserve: &zero, PollSeconds: 360, DayZone: "UTC"}
	for i, alias := range retainedAliases {
		device := fmt.Sprintf("device-%02d", i)
		if i == 9 {
			device = ""
		}
		input.Bindings = append(input.Bindings, BindingInput{Alias: alias, DeviceID: device})
	}
	if _, err := ParseSettings(input); err != nil {
		t.Fatalf("explicit maximum and import cadence: %v", err)
	}
	input.DayZone = "Local"
	if _, err := ParseSettings(input); err == nil {
		t.Fatal("host-dependent zone admitted")
	}
	input.DayZone = "UTC"
	input.Bindings[0].DeviceID = input.Bindings[1].DeviceID
	if _, err := ParseSettings(input); err == nil {
		t.Fatal("duplicate physical identity admitted")
	}
	input.Bindings[0].DeviceID = "device-00"
	input.Bindings = input.Bindings[:10]
	if _, err := ParseSettings(input); err == nil {
		t.Fatal("missing retained alias admitted")
	}
}

func TestNHA091SourceModelConversionAndAbsentValues(t *testing.T) {
	climate, err := frameFor(BindingClimate, []byte(`{"signature":{"model":1123,"protocol":"zigbee"},"roomTemperature":{"value":0},"roomSetpoint":19.5,"roomSetpointMin":5,"roomSetpointMax":30}`))
	if err != nil || climate.model.family != familyZigbee || !climate.model.bounded || climate.temperature == nil || *climate.temperature != 0 || climate.target == nil || *climate.target != 19.5 {
		t.Fatalf("zigbee frame: %+v %v", climate, err)
	}
	missing, err := frameFor(BindingClimate, []byte(`{"signature":{"model":738,"protocol":"wifi"},"roomTemperature":{"value":null},"roomSetpoint":0}`))
	if err != nil || missing.temperature != nil || missing.target == nil || *missing.target != 0 || missing.model.family != familyWiFi {
		t.Fatalf("absence confused with zero: %+v %v", missing, err)
	}
	if _, err := frameFor(BindingClimate, []byte(`{"signature":{"model":999999}}`)); !errors.Is(err, ErrProtocol) {
		t.Fatal("unknown model admitted")
	}
	for _, tc := range []struct {
		value string
		want  *bool
	}{{`"on"`, boolPointer(true)}, {`"off"`, boolPointer(false)}, {`"indeterminate"`, nil}} {
		frame, err := frameFor(BindingSwitch, []byte(`{"signature":{"model":2151,"protocol":"zigbee"},"onOff":`+tc.value+`}`))
		if err != nil {
			t.Fatal(err)
		}
		if (frame.on == nil) != (tc.want == nil) || frame.on != nil && *frame.on != *tc.want {
			t.Fatal("unknown switch state became on/off")
		}
	}
	if _, err := modeAttribute(familyHeatCool, wire.ClimateMode_CLIMATE_MODE_COOL); err != nil {
		t.Fatal(err)
	}
	if _, err := modeAttribute(familyZigbee, wire.ClimateMode_CLIMATE_MODE_COOL); !errors.Is(err, ErrProtocol) {
		t.Fatal("unsupported mode admitted")
	}
}

func TestNHA091AttributeQueriesAreQualifiedPerFamily(t *testing.T) {
	for _, tc := range []struct {
		kind  BindingKind
		model int
		want  string
	}{
		{BindingClimate, 1123, "roomTemperature,roomSetpoint,roomSetpointMin,roomSetpointMax,systemMode"},
		{BindingClimate, 738, "roomTemperature,roomSetpoint,roomSetpointMin,roomSetpointMax,setpointMode"},
		{BindingClimate, 1512, "roomTemperatureDisplay,roomSetpoint,roomSetpointMin,roomSetpointMax,coolSetpoint,coolSetpointMin,coolSetpointMax,systemMode"},
		{BindingClimate, 6727, "roomTemperature,roomSetpoint,roomSetpointMin,roomSetpointMax,coolSetpoint,coolSetpointMin,coolSetpointMax,heatCoolMode"},
		{BindingSwitch, 2151, "onOff"},
	} {
		model, err := parseModel(json.RawMessage(fmt.Sprintf(`{"model":%d}`, tc.model)), tc.kind)
		if err != nil || requestedAttributes(tc.kind, model) != tc.want {
			t.Fatalf("model %d request=%q, err=%v", tc.model, requestedAttributes(tc.kind, model), err)
		}
	}
	if requestedAttributes(BindingClimate, modelInfo{}) != "" {
		t.Fatal("unknown model acquired an attribute query")
	}
}
func boolPointer(value bool) *bool { return &value }

func TestNHA091ClimateModeFamiliesAndHeatCoolGap(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		family     modelFamily
		mode       *domain.ClimateMode
		kind       domain.ClimateSetpointKind
		diagnostic bool
	}{
		{"zigbee heat", `{"signature":{"model":1123},"roomTemperature":{"value":0},"roomSetpoint":21,"systemMode":"heat"}`, familyZigbee, nha091Mode(domain.ClimateModeHeat), domain.ClimateSetpointSingle, false},
		{"wifi schedule", `{"signature":{"model":738},"roomTemperature":{"value":20},"roomSetpoint":21,"setpointMode":"auto"}`, familyWiFi, nha091Mode(domain.ClimateModeAuto), domain.ClimateSetpointSingle, false},
		{"wifi manual", `{"signature":{"model":738},"roomTemperature":{"value":20},"roomSetpoint":21,"setpointMode":"manual"}`, familyWiFi, nha091Mode(domain.ClimateModeHeat), domain.ClimateSetpointSingle, false},
		{"heat cool range", `{"signature":{"model":6727},"roomTemperature":{"value":19},"roomSetpoint":20,"coolSetpoint":25,"heatCoolMode":"auto"}`, familyHeatCool, nil, domain.ClimateSetpointRange, true},
		{"heat cool cooling", `{"signature":{"model":6727},"roomTemperature":{"value":26},"roomSetpoint":20,"coolSetpoint":25,"heatCoolMode":"cool"}`, familyHeatCool, nha091Mode(domain.ClimateModeCool), domain.ClimateSetpointSingle, false},
		{"zigbee HC display", `{"signature":{"model":1512},"roomTemperatureDisplay":0,"roomSetpoint":20,"coolSetpoint":25,"systemMode":"cool"}`, familyZigbeeHeatCool, nha091Mode(domain.ClimateModeCool), domain.ClimateSetpointSingle, false},
		{"unknown physical mode", `{"signature":{"model":1123},"roomTemperature":{"value":20},"roomSetpoint":21,"systemMode":"future"}`, familyZigbee, nil, domain.ClimateSetpointSingle, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame, err := frameFor(BindingClimate, []byte(tc.body))
			if err != nil || frame.model.family != tc.family || frame.diagnostic != tc.diagnostic {
				t.Fatalf("frame classification: %+v %v", frame, err)
			}
			value, err := climateStateValue(frame)
			if err != nil || value == nil {
				t.Fatalf("climate value: %v", err)
			}
			climate, ok := value.Climate()
			if !ok {
				t.Fatal("climate was not a typed state")
			}
			parts, err := climate.Components()
			if err != nil || parts.Activity != nil || parts.FanActive != nil || (parts.Mode == nil) != (tc.mode == nil) || parts.Mode != nil && *parts.Mode != *tc.mode {
				t.Fatalf("mode/activity presence: %+v %v", parts, err)
			}
			if parts.Setpoint == nil {
				t.Fatal("setpoint missing")
			}
			kind, err := parts.Setpoint.Kind()
			if err != nil || kind != tc.kind {
				t.Fatalf("setpoint kind: %v %v", kind, err)
			}
			if tc.name == "zigbee heat" || tc.name == "zigbee HC display" {
				if parts.CurrentTemperature == nil {
					t.Fatal("present zero temperature lost")
				}
				current, err := parts.CurrentTemperature.Value()
				if err != nil || current != 0 {
					t.Fatalf("zero temperature: %v %v", current, err)
				}
			}
		})
	}
}

func nha091Mode(mode domain.ClimateMode) *domain.ClimateMode { return &mode }

func TestNHA091ClimateObservationUsesOneHostStateAndReceiptClock(t *testing.T) {
	frame, err := frameFor(BindingClimate, []byte(`{"signature":{"model":1123},"roomTemperature":{"value":0},"roomSetpoint":21,"roomSetpointMin":5,"roomSetpointMax":30,"systemMode":"heat"}`))
	if err != nil {
		t.Fatal(err)
	}
	parser := &Pilot{}
	declarations := parser.descriptions(Binding{alias: "synthetic-neviweb", kind: BindingClimate}, frame.model)
	if len(declarations) != 1 || declarations[0].SchemaId != contracts.ClimateSchema || declarations[0].GetClimate().ObservedTemperatureCelsius == nil || declarations[0].GetClimate().ObservedSetpointCelsius == nil {
		t.Fatal("climate observation was not declared independently of action")
	}
	host, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha091-climate-observation")
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("neviweb-fixture-device", "climate")
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := integrations.NewCandidate(key, declarations)
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := session.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		t.Fatalf("Host discovery: %v", err)
	}
	value, err := climateStateValue(frame)
	if err != nil || value == nil {
		t.Fatalf("climate construction: %v", err)
	}
	if err := publishValue(context.Background(), session, bindings[0], "climate", value, time.Now()); err != nil {
		t.Fatalf("Host publication: %v", err)
	}
	_, entity, err := bindings[0].IDs()
	if err != nil {
		t.Fatal(err)
	}
	state, present := fixture.State(entity)
	if !present {
		t.Fatal("one coherent climate state was not stored")
	}
	_, _, times, _, _, err := state.Components()
	if err != nil || times.ObservedAt != nil || times.ReceivedAt.IsZero() {
		t.Fatalf("source timestamp invented or receipt absent: %+v %v", times, err)
	}
	got, present := state.CurrentValue()
	if !present || got != *value {
		t.Fatal("climate fields were split or replaced")
	}
	if err := publishValue(context.Background(), session, bindings[0], "climate", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	state, _ = fixture.State(entity)
	if _, present := state.CurrentValue(); present {
		t.Fatal("unavailable reading became a current value")
	}
	if previous, present := state.DisplayValue(); !present || previous != *value {
		t.Fatal("last admitted climate display was lost")
	}
}

func TestNHA091SessionEnvelopeAndProviderErrors(t *testing.T) {
	initial, err := InitialSessionValue()
	if err != nil {
		t.Fatal(err)
	}
	s, err := decodeSession(initial)
	if err != nil || s.phase != sessionAbsent {
		t.Fatal("initial session is not explicit absence")
	}
	active, err := activeSessionValue("session-fixture", "cookie=value", "123")
	if err != nil {
		t.Fatal(err)
	}
	s, err = decodeSession(active)
	if err != nil || s.phase != sessionActive {
		t.Fatal("active session rejected")
	}
	trailing, _ := storage.NewSecretValue([]byte(`{"version":1,"state":"absent"} garbage`))
	if _, err := decodeSession(trailing); !errors.Is(err, ErrSession) {
		t.Fatal("malformed trailing JSON accepted")
	}
	if diagnostic, err := json.Marshal(s); err != nil || strings.Contains(string(diagnostic), "private-cookie") || strings.Contains(fmt.Sprintf("%#v", s), "cookie=value") {
		t.Fatal("admitted session leaked into diagnostics")
	}
	for _, tc := range []struct {
		code string
		want error
	}{{"USRBADLOGIN", ErrCredentials}, {"ACCSESSEXC", ErrTooManySessions}, {"USRSESSEXP", ErrSessionExpired}, {"ACCDAYREQMAX", ErrProviderQuota}, {"ACCRATELIMIT", ErrProviderQuota}} {
		if got := providerFailure([]byte(`{"error":{"code":"` + tc.code + `"}}`)); !errors.Is(got, tc.want) {
			t.Fatalf("%s -> %v", tc.code, got)
		}
	}
	if err := providerFailure([]byte(`[{"id":456}]`)); err != nil {
		t.Fatalf("array response rejected: %v", err)
	}
}

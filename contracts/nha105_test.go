package contracts

import (
	"bytes"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	solarwire "github.com/damione1/Homeward/gen/home/solar/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func nha105ContractSolar(t *testing.T) domain.SolarValue {
	t.Helper()
	input := domain.SolarValueInput{Horizon: domain.SolarAboveHorizon, Rising: true}
	for i := range input.Next {
		input.Next[i], _ = domain.SolarOccurrenceAt(time.Unix(int64(i+1)*100, 0))
	}
	input.Next[2], _ = domain.SolarOccurrenceAt(time.Unix(1000, 0))
	value, err := domain.NewSolarValue(input)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestNHA105SolarWireAdmissionAndRoundtrip(t *testing.T) {
	value := nha105ContractSolar(t)
	wire, err := EncodeSolarValue(value)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := AdmitWireSolarValue(wire)
	if err != nil || admitted != value {
		t.Fatal("solar roundtrip changed value")
	}
	*wire.ElevationDegrees = 27
	copy, _ := admitted.Components()
	if copy.ElevationDegrees != 0 {
		t.Fatal("wire alias escaped admission")
	}
	base, _ := EncodeSolarValue(value)
	for name, mutate := range map[string]func(*solarwire.SolarValue){
		"missing-false":      func(v *solarwire.SolarValue) { v.Rising = nil },
		"missing-zero":       func(v *solarwire.SolarValue) { v.AzimuthDegrees = nil },
		"nan":                func(v *solarwire.SolarValue) { *v.ElevationDegrees = math.NaN() },
		"future-horizon":     func(v *solarwire.SolarValue) { v.Horizon = 99 },
		"missing-occurrence": func(v *solarwire.SolarValue) { v.NextDawn = nil },
		"typed-nil":          func(v *solarwire.SolarValue) { v.NextDawn.Occurrence = (*solarwire.SolarOccurrence_At)(nil) },
		"daily-absence": func(v *solarwire.SolarValue) {
			v.NextDawn.Occurrence = &solarwire.SolarOccurrence_Absent{Absent: solarwire.SolarAbsenceReason_SOLAR_ABSENCE_REASON_NO_TRANSIT_ON_DATE}
		},
		"unknown": func(v *solarwire.SolarValue) { v.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 1}) },
	} {
		t.Run(name, func(t *testing.T) {
			malformed := proto.Clone(base).(*solarwire.SolarValue)
			mutate(malformed)
			if _, err := AdmitWireSolarValue(malformed); err == nil {
				t.Fatal("malformed solar admitted")
			}
		})
	}
	missing, _ := domain.SolarOccurrenceMissing(domain.SolarNoEventWithinSearchHorizon)
	for _, kind := range domain.SolarEventKinds() {
		for _, occurrence := range []domain.SolarOccurrence{copy.Next[0], missing} {
			event, _ := domain.NewSolarEventValue(kind, occurrence)
			encoded, err := EncodeSolarEventValue(event)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := AdmitWireSolarEventValue(encoded)
			if err != nil || decoded != event {
				t.Fatal("event roundtrip changed presence")
			}
		}
	}
	future := &solarwire.SolarEventValue{Event: 99, Occurrence: base.NextDawn}
	encoded, err := proto.Marshal(future)
	if err != nil {
		t.Fatal(err)
	}
	var decoded solarwire.SolarEventValue
	if err := proto.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, err := AdmitWireSolarEventValue(&decoded); err == nil {
		t.Fatal("future enum became operational")
	}
	if again, err := proto.Marshal(&decoded); err != nil || !bytes.Equal(encoded, again) {
		t.Fatal("future enum lost decode compatibility")
	}
}

func TestNHA105SolarCatalogueRequiresMatchingObservation(t *testing.T) {
	catalog, _ := NewCapabilityCatalog(domain.NewUnitCatalog())
	entity, _ := domain.ParseEntityID("nha105-contract")
	description := &registrywire.CapabilityDescription{SchemaId: SolarEventSchema, Major: 1, DisplayName: "Synthetic dawn", Endpoint: "value", Constraints: &registrywire.CapabilityDescription_SolarEvent{SolarEvent: &solarwire.SolarEventConstraints{Event: solarwire.SolarEventKind_SOLAR_EVENT_KIND_DAWN}}}
	if err := catalog.AdmitReadOnlyDriverCapability(description); err != nil {
		t.Fatal(err)
	}
	if err := catalog.RegisterDriverCapability(entity, description); err != nil {
		t.Fatal(err)
	}
	missing, _ := domain.SolarOccurrenceMissing(domain.SolarNoEventWithinSearchHorizon)
	wrong, _ := domain.NewSolarEventValue(domain.SolarDusk, missing)
	if catalog.AdmitDomainSolarEvent(entity, "value", wrong) == nil {
		t.Fatal("crossed event kind admitted")
	}
	description.Constraints = (*registrywire.CapabilityDescription_Solar)(nil)
	if !NativeSolarDescription(description) || catalog.AdmitDriverCapability(description) == nil {
		t.Fatal("typed-nil solar constraint escaped boundary")
	}
}

func TestNHA105SolarGoldenContracts(t *testing.T) {
	index, err := os.ReadFile("solar_golden/index.tsv")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(index)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			t.Fatal("invalid solar fixture index")
		}
		name, message, expected := fields[0], fields[1], fields[2] == "true"
		text, err := os.ReadFile(filepath.Join("solar_golden", name+".textproto"))
		if err != nil {
			t.Fatal(err)
		}
		golden, err := os.ReadFile(filepath.Join("solar_golden", name+".bin"))
		if err != nil {
			t.Fatal(err)
		}
		var value proto.Message
		switch message {
		case "SolarValue":
			value = new(solarwire.SolarValue)
		case "SolarEventValue":
			value = new(solarwire.SolarEventValue)
		default:
			t.Fatal("unknown fixture message")
		}
		if err := prototext.Unmarshal(text, value); err != nil {
			t.Fatal(err)
		}
		encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(value)
		if err != nil || !bytes.Equal(encoded, golden) {
			t.Fatal("Go differs from protoc solar fixture")
		}
		var admissionErr error
		switch v := value.(type) {
		case *solarwire.SolarValue:
			_, admissionErr = AdmitWireSolarValue(v)
		case *solarwire.SolarEventValue:
			_, admissionErr = AdmitWireSolarEventValue(v)
		}
		if (admissionErr == nil) != expected {
			t.Fatalf("fixture %s admission differs", name)
		}
	}
}

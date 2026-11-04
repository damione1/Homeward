package apiv2

import (
	"encoding/hex"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func sample() *wire.ExecuteCapabilityRequest {
	return &wire.ExecuteCapabilityRequest{
		Name:       "homes/demo/entities/lamp-a/capabilities/power",
		Command:    &wire.ExecuteCapabilityRequest_Power{Power: &wire.SetPowerCommand{Value: wire.PowerValue_POWER_VALUE_OFF}},
		ExpireTime: timestamppb.New(time.Unix(1800000000, 0)),
	}
}

// Shared actual protobuf bytes exercise both generated decoders and domain
// conversions. This is not a test that only calls a standalone predicate.
func TestSharedCommandCorpus(t *testing.T) {
	data, err := os.ReadFile("../api_v2_commands.tsv")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 4 {
			t.Fatalf("bad fixture %q", line)
		}
		if parts[3] == "-" {
			parts[3] = ""
		}
		t.Run(parts[0], func(t *testing.T) {
			data, err := hex.DecodeString(parts[1])
			if err != nil {
				t.Fatal(err)
			}
			var input wire.ExecuteCapabilityRequest
			if err := proto.Unmarshal(data, &input); err != nil {
				t.Fatal(err)
			}
			command, err := ParseCommand(&input)
			if parts[2] == "ok" {
				if err != nil || !command.Valid() {
					t.Fatalf("unexpected rejection %v", err)
				}
				switch intent := command.Intent().(type) {
				case apitypes.PowerCommand:
					if parts[0] == "off" && intent.IsOn() {
						t.Fatal("OFF lost")
					}
				case apitypes.LevelCommand:
					if parts[0] == "zero" && intent.Percentage().Value() != 0 {
						t.Fatal("zero lost")
					}
				case apitypes.LightCommand, apitypes.FanCommand, apitypes.ThermalCommand, apitypes.CoverCommand, apitypes.LockCommand, apitypes.AlarmCommand, apitypes.HelperCommand, apitypes.MediaCommand:
				default:
					t.Fatal("unhandled domain intent")
				}
			} else {
				violation, ok := err.(*Violation)
				if !ok || violation.Field() != parts[3] {
					t.Fatalf("want %q violation, got %v", parts[3], err)
				}
				if (parts[0] == "light-off-brightness-transition" || parts[0] == "helper-toggle-value") && violation.Kind() != wire.ValidationReason_VALIDATION_REASON_INCONSISTENT_FIELDS {
					t.Fatal("present conflicting fields classified as missing", violation.Kind())
				}
			}
		})
	}
}

// Every generated enum variant must be consciously accepted or be UNSPECIFIED.
// A wire-compatible added variant fails this test until its mapping is updated.
func TestPowerDescriptorCoverage(t *testing.T) {
	values := wire.PowerValue(0).Descriptor().Values()
	for i := 0; i < values.Len(); i++ {
		value := values.Get(i)
		power, err := ParsePower(wire.PowerValue(value.Number()))
		if value.Number() == 0 {
			if err == nil {
				t.Fatal("unspecified admitted")
			}
		} else if err != nil || !power.Valid() {
			t.Fatalf("unhandled declared variant %s", value.Name())
		}
	}
	if _, err := ParsePower(wire.PowerValue(991)); err == nil {
		t.Fatal("unknown enum admitted")
	}
}

func TestCommandDescriptorCoverage(t *testing.T) {
	// Positive boundary fixtures must cover every executable command branch.
	request := sample()
	fields := request.ProtoReflect().Descriptor().Oneofs().ByName("command").Fields()
	fixtures := map[protoreflect.Name]*wire.ExecuteCapabilityRequest{
		"power": sample(),
		"level": {Name: request.Name, ExpireTime: request.ExpireTime,
			Command: &wire.ExecuteCapabilityRequest_Level{Level: &wire.SetLevelCommand{Target: &wire.SetLevelCommand_Percentage{Percentage: 0}}}},
	}
	for name, command := range traitFixtures() {
		fixtures[name] = &wire.ExecuteCapabilityRequest{Name: request.Name, ExpireTime: request.ExpireTime, Command: command.Command}
	}
	if len(fixtures) != fields.Len() {
		t.Fatal("new command variant requires an explicit domain conversion and fixture")
	}
	for i := 0; i < fields.Len(); i++ {
		fixture, exists := fixtures[fields.Get(i).Name()]
		if !exists {
			t.Fatalf("missing branch %s", fields.Get(i).Name())
		}
		if _, err := ParseCommand(fixture); err != nil {
			t.Fatal(err)
		}
	}
	level := (&wire.SetLevelCommand{}).ProtoReflect().Descriptor().Oneofs().ByName("target").Fields()
	if level.Len() != 1 || level.Get(0).Name() != "percentage" {
		t.Fatal("new level target requires explicit admission")
	}
}

func TestTypedNilAndUnknownExecutableFields(t *testing.T) {
	var power *wire.ExecuteCapabilityRequest_Power
	var level *wire.SetLevelCommand_Percentage
	for _, input := range []*wire.ExecuteCapabilityRequest{
		nil, {Command: power},
		{Name: sample().Name, Command: power},
		{Name: sample().Name, Command: &wire.ExecuteCapabilityRequest_Level{Level: &wire.SetLevelCommand{Target: level}}},
	} {
		if _, err := ParseCommand(input); err == nil {
			t.Fatal("typed nil admitted")
		}
	}
	input := sample()
	input.ExpireTime.ProtoReflect().SetUnknown([]byte{0x18, 0x01})
	if _, err := ParseCommand(input); err == nil {
		t.Fatal("nested unknown field admitted")
	}
}

func TestDomainConstructionCannotSkipAdmission(t *testing.T) {
	var empty apitypes.Command
	if empty.Valid() {
		t.Fatal("zero command is executable")
	}
	target, _ := apitypes.ParseCapabilityName(sample().Name)
	var typedNil *apitypes.PowerCommand
	for _, intent := range []apitypes.Intent{nil, typedNil, apitypes.PowerCommand{}} {
		if _, err := apitypes.NewCommand(target, intent, nil, time.Now()); err == nil {
			t.Fatal("invalid intent admitted")
		}
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, 101} {
		if _, err := apitypes.NewPercentage(value); err == nil {
			t.Fatal("invalid percentage admitted")
		}
	}
	for _, value := range []float64{0, 100, 0.5} {
		percentage, err := apitypes.NewPercentage(value)
		if err != nil || percentage.Value() != value {
			t.Fatal("valid percentage lost")
		}
	}
	if target.Entity().Home().String() != "homes/demo" {
		t.Fatal("wrong name parent")
	}
	if _, err := apitypes.NewCommand(apitypes.CapabilityName{}, apitypes.PowerOff(), nil, time.Now()); err == nil {
		t.Fatal("zero name admitted")
	}
	if _, err := apitypes.NewCommand(target, apitypes.PowerOff(), &apitypes.RequestID{}, time.Now()); err == nil {
		t.Fatal("zero request ID admitted")
	}
}

func TestResourceNameCorpus(t *testing.T) {
	data, err := os.ReadFile("../api_v2_names.tsv")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if parts[1] == "-" {
			parts[1] = ""
		}
		name, err := apitypes.ParseCapabilityName(parts[1])
		if (err == nil) != (parts[0] == "ok") {
			t.Fatalf("unexpected name admission %q: %v", parts[1], err)
		}
		if err == nil && name.String() != parts[1] {
			t.Fatal("name normalized")
		}
	}
}

package contracts

import (
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"math"
	"strings"
	"testing"
)

func TestNHA212PublicHistoryAdmitsIntrinsicValuesWithoutLiveCatalog(t *testing.T) {
	home, _ := apitypes.ParseHomeName("homes/synthetic")
	period := &api.HistoryPeriod{Start: &timestamppb.Timestamp{Seconds: 1}, End: &timestamppb.Timestamp{Seconds: 2}}
	state := &api.HistoricalState{Quality: &api.ObservationQuality{Availability: api.Availability_AVAILABILITY_AVAILABLE, Freshness: api.Freshness_FRESHNESS_STALE, Provenance: api.Provenance_PROVENANCE_OBSERVED}, ReceiveTime: period.Start, LastChangeTime: period.Start, LastUpdateTime: period.Start, LastReportTime: period.Start}
	response := &api.QueryObservationsResponse{
		Observations: []*api.HistoryObservation{{ObservationId: strings.Repeat("a", 64), State: state}},
		Scope:        &api.HistoryReadScope{Requested: period, Retained: period, RetentionCutoff: period.Start, AsOf: period.End, Resolution: api.HistoryResolution_HISTORY_RESOLUTION_RAW, Limits: &api.HistoryLimits{MaxPoints: 100, MaxResponseBytes: 256 << 10, MaxTokenBytes: 512, QueryTimeoutMillis: 2000, MaxReaders: 2, MaxPeriodHours: 168}},
		Coverage:     &api.HistoryCoverage{Verdict: api.HistoryCoverageVerdict_HISTORY_COVERAGE_VERDICT_NOT_PROVEN},
	}
	check := func(name string, value *api.HistoricalState, want bool) {
		t.Helper()
		candidate := proto.CloneOf(response)
		candidate.Observations[0].State = value
		if got := AdmitHistoryV2Response(candidate, home) == nil; got != want {
			t.Fatalf("%s admitted=%v want=%v", name, got, want)
		}
	}
	light := &api.HistoricalLight{On: proto.Bool(false), BrightnessPercent: proto.Float64(0), Rgb: &api.RgbColor{Red: proto.Uint32(0), Green: proto.Uint32(255), Blue: proto.Uint32(7)}, Preset: proto.String("retired:exact-key")}
	state.Value = &api.HistoricalState_Light{Light: light}
	check("retired preset and zero channels", state, true)
	for name, mutate := range map[string]func(*api.HistoricalLight){
		"missing channel":     func(v *api.HistoricalLight) { v.Rgb.Green = nil },
		"channel overflow":    func(v *api.HistoricalLight) { v.Rgb.Red = proto.Uint32(256) },
		"exclusive color":     func(v *api.HistoricalLight) { v.ColorTempMireds = proto.Int64(300) },
		"missing power":       func(v *api.HistoricalLight) { v.On = nil },
		"empty preset":        func(v *api.HistoricalLight) { v.Preset = proto.String("") },
		"unknown color field": func(v *api.HistoricalLight) { v.Rgb.ProtoReflect().SetUnknown([]byte{0x20, 1}) },
	} {
		candidate := proto.CloneOf(state)
		mutate(candidate.GetLight())
		check(name, candidate, false)
	}
	for name, fan := range map[string]*api.HistoricalFan{
		"explicit off":                     {On: proto.Bool(false)},
		"unknown power with retired key":   {Speed: proto.String("discontinued:quiet")},
		"native level outside any percent": {Level: proto.Float64(-125.5)},
		"zero native level":                {Level: proto.Float64(0)},
	} {
		candidate := proto.CloneOf(state)
		candidate.Value = &api.HistoricalState_Fan{Fan: fan}
		check(name, candidate, true)
	}
	for name, fan := range map[string]*api.HistoricalFan{
		"absent fan value": {},
		"empty key":        {Speed: proto.String("")},
		"ambiguous level":  {Speed: proto.String("key"), Level: proto.Float64(1)},
		"nonfinite level":  {Level: proto.Float64(math.Inf(1))},
	} {
		candidate := proto.CloneOf(state)
		candidate.Value = &api.HistoricalState_Fan{Fan: fan}
		check(name, candidate, false)
	}
	absent := proto.CloneOf(state)
	absent.Value = nil
	absent.Quality.Availability = api.Availability_AVAILABILITY_UNAVAILABLE
	check("unavailable without invented value", absent, true)
}

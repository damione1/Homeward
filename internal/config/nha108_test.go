package config

import (
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/energy"
)

func TestNHA108MVPBindingsRequireExplicitHistoricalUnknowns(t *testing.T) {
	precision := 3
	row := energyInput{Source: "sensor.hvac_power_consumption", Output: "sensor.hvac_energy_consumption", Precision: &precision, MaxGap: "1h", TimeBasis: "capture_required"}
	admitted, err := parseEnergyBindings([]energyInput{row})
	if err != nil || len(admitted) != 1 || admitted[0].Precision() != 3 || admitted[0].MaxGap() != time.Hour || admitted[0].TimeBasis() != energy.TimeBasisCaptureRequired {
		t.Fatalf("explicit energy binding: %+v %v", admitted, err)
	}
	missingPrecision := row
	missingPrecision.Precision = nil
	missingGap := row
	missingGap.MaxGap = ""
	missingBasis := row
	missingBasis.TimeBasis = ""
	wrongPair := row
	wrongPair.Output = "sensor.ups_energy_consumption"
	for _, invalid := range []energyInput{missingPrecision, missingGap, missingBasis, wrongPair} {
		if _, err := parseEnergyBindings([]energyInput{invalid}); err == nil {
			t.Fatalf("unqualified historical option admitted: %+v", invalid)
		}
	}
	if _, err := parseEnergyBindings([]energyInput{row, row}); err == nil {
		t.Fatal("duplicate counter admitted")
	}
}

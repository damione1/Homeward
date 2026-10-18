package config

import (
	"errors"
	"time"

	"github.com/damione1/Homeward/internal/energy"
	"github.com/damione1/Homeward/internal/registry"
)

var ErrInvalidEnergy = errors.New("configuration: invalid energy binding")

// EnergyBinding is an admitted MVP composition row, not an engine entity enum.
// NHA-090 can supply other registry-resolved IDs to the same energy package.
type EnergyBinding struct {
	sourceAlias string
	outputAlias string
	precision   int
	maxGap      time.Duration
	basis       energy.TimeBasis
}

func (b EnergyBinding) SourceAlias() string         { return b.sourceAlias }
func (b EnergyBinding) OutputAlias() string         { return b.outputAlias }
func (b EnergyBinding) Precision() int              { return b.precision }
func (b EnergyBinding) MaxGap() time.Duration       { return b.maxGap }
func (b EnergyBinding) TimeBasis() energy.TimeBasis { return b.basis }

type energyInput struct {
	Source    string `yaml:"source"`
	Output    string `yaml:"output"`
	Precision *int   `yaml:"precision"`
	MaxGap    string `yaml:"max_gap"`
	TimeBasis string `yaml:"time_basis"`
}

var mvpEnergyPairs = map[string]string{
	"sensor.hvac_power_consumption":   "sensor.hvac_energy_consumption",
	"sensor.plancher_cuisine_power_2": "sensor.plancher_cuisine_energy",
	"sensor.ups_power_consumption":    "sensor.ups_energy_consumption",
}

func parseEnergyBindings(rows []energyInput) ([]EnergyBinding, error) {
	if len(rows) > 3 {
		return nil, ErrInvalidEnergy
	}
	result := make([]EnergyBinding, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		output, known := mvpEnergyPairs[row.Source]
		if !known || output != row.Output || seen[row.Source] || row.Precision == nil || *row.Precision < 0 || *row.Precision > 9 {
			return nil, ErrInvalidEnergy
		}
		if _, err := registry.ParseHAAlias(row.Source); err != nil {
			return nil, ErrInvalidEnergy
		}
		if _, err := registry.ParseHAAlias(row.Output); err != nil {
			return nil, ErrInvalidEnergy
		}
		gap, err := time.ParseDuration(row.MaxGap)
		if err != nil || gap <= 0 || gap > 24*time.Hour {
			return nil, ErrInvalidEnergy
		}
		var basis energy.TimeBasis
		switch row.TimeBasis {
		case "capture_required":
			basis = energy.TimeBasisCaptureRequired
		case "receipt":
			basis = energy.TimeBasisReceipt
		default:
			return nil, ErrInvalidEnergy
		}
		result = append(result, EnergyBinding{row.Source, row.Output, *row.Precision, gap, basis})
		seen[row.Source] = true
	}
	return result, nil
}

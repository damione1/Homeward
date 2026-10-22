package config_test

import (
	"testing"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/derivedpower"
	"github.com/damione1/Homeward/internal/ecobee"
	"github.com/damione1/Homeward/internal/energy"
	"github.com/damione1/Homeward/scope"
)

// The independently versioned NHA-137 justification already admits these
// consumers. Applying it must not need another alias or a rewritten manifest.
func TestNHA135ConfigurationUsesQualifiedAmendmentAndExistingEnergyPair(t *testing.T) {
	raw := "listen: '127.0.0.1:0'\n" + testPostgres(t) + `
scope_revision: ecobee_mvp_v1
derived_power: mvp-v1
ecobee:
  instance: nha135-synthetic
  address: 192.0.2.1:12345
  accessory_id: nha135-thermostat
  credential_secret: unused-synthetic
energy:
  - source: sensor.hvac_power_consumption
    output: sensor.hvac_energy_consumption
    precision: 3
    max_gap: 1h
    time_basis: receipt
`
	selected, err := config.ParseWithEnv([]byte(raw), testLookup)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := selected.ScopeRevision()
	settings, enabled := selected.EcobeeSettings()
	if err != nil || revision != scope.EcobeeMVPV1() || !selected.DerivedPowerProfile().Enabled() || !enabled || !settings.Valid() {
		t.Fatal("qualified HVAC composition was not selected")
	}
	rows := selected.EnergyBindings()
	power, err := derivedpower.KindHVAC.OutputAlias()
	if err != nil || len(rows) != 1 || rows[0].SourceAlias() != power || rows[0].TimeBasis() != energy.TimeBasisReceipt {
		t.Fatal("derived power was not bound to receipt-time integration")
	}
	amendment, err := scope.LoadEcobeeMVPV1()
	if err != nil {
		t.Fatal(err)
	}
	qualification := amendment.Qualification()
	if qualification.Ticket != "NHA-009" || qualification.Commit == "" || qualification.ProfileSHA256 == "" {
		t.Fatal("source qualification lineage is missing")
	}
	added := amendment.AddedIDs()
	if len(added) != 1 || added[0] != ecobee.Alias || len(amendment.AdditionalAliases()) != 0 {
		t.Fatal("HVAC composition expanded the admitted source IDs")
	}
	consumers := amendment.TechnicalConsumers()
	if len(consumers) != 2 || consumers[0].EntityID != power || len(consumers[0].Via) != 1 || consumers[0].Via[0] != ecobee.Alias ||
		consumers[1].EntityID != rows[0].OutputAlias() || len(consumers[1].Via) != 2 || consumers[1].Via[0] != power || consumers[1].Via[1] != ecobee.Alias {
		t.Fatal("configuration bypassed the amendment's two approved consumers")
	}
}

package config_test

import (
	"errors"
	"testing"

	"github.com/damione1/Homeward/internal/config"
)

func TestNHA179PerBindingCasetaHAOptInIsClosedAndPreserved(t *testing.T) {
	base := "listen: '127.0.0.1:0'\n" + testPostgres(t) +
		"external_workers:\n- instance: synthetic-caseta\n  executable: /bin/sh\n  bindings:\n" +
		"  - device: bridge\n    entity: level\n    alias: light.synthetic\n"
	parsed, err := config.ParseWithEnv([]byte(base+"    ha_light_profile: caseta_ha_100_v1\n"), testLookup)
	if err != nil {
		t.Fatal(err)
	}
	workers := parsed.ExternalWorkers()
	if len(workers) != 1 || len(workers[0].Bindings()) != 1 || !workers[0].Bindings()[0].CasetaHA100V1() {
		t.Fatal("Run worker config lost explicit per-binding HA profile")
	}
	legacy, err := config.ParseWithEnv([]byte(base), testLookup)
	if err != nil || legacy.ExternalWorkers()[0].Bindings()[0].CasetaHA100V1() {
		t.Fatalf("default Host worker acquired an HA light profile: %v", err)
	}
	for _, value := range []string{"", "null", "unknown", "caseta_ha_100_v2", "[]"} {
		var suffix string
		if value == "" {
			suffix = "''"
		} else {
			suffix = value
		}
		if _, err := config.ParseWithEnv([]byte(base+"    ha_light_profile: "+suffix+"\n"), testLookup); !errors.Is(err, config.ErrInvalidYAML) {
			t.Fatalf("open or absent-like profile %q accepted: %v", value, err)
		}
	}
	nonLight := "listen: '127.0.0.1:0'\n" + testPostgres(t) +
		"external_workers:\n- instance: synthetic-caseta\n  executable: /bin/sh\n  bindings:\n" +
		"  - {device: bridge, entity: level, alias: switch.synthetic, ha_light_profile: caseta_ha_100_v1}\n"
	if _, err := config.ParseWithEnv([]byte(nonLight), testLookup); err == nil {
		t.Fatal("Caseta HA light profile accepted a switch alias")
	}
}

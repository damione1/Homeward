package config_test

import (
	"errors"
	"testing"

	"github.com/damione1/Homeward/internal/config"
)

func TestNHA046FanProfileRequiresExplicitQualifiedBinding(t *testing.T) {
	base := "listen: '127.0.0.1:0'\n" + testPostgres(t) +
		"external_workers:\n- instance: synthetic-caseta\n  executable: /bin/sh\n  bindings:\n" +
		"  - device: bridge\n    entity: fan-speed\n    alias: fan.synthetic\n"
	parsed, err := config.ParseWithEnv([]byte(base+"    ha_fan_profile: caseta_ha_fan_4_v1\n"), testLookup)
	if err != nil {
		t.Fatal(err)
	}
	workers := parsed.ExternalWorkers()
	if len(workers) != 1 || len(workers[0].Bindings()) != 1 || !workers[0].Bindings()[0].CasetaHAFan4V1() {
		t.Fatal("YAML → worker binding lost the explicit fan conversion policy")
	}
	defaultConfig, err := config.ParseWithEnv([]byte(base), testLookup)
	if err != nil || defaultConfig.ExternalWorkers()[0].Bindings()[0].CasetaHAFan4V1() {
		t.Fatalf("default worker gained a fan profile: %v", err)
	}
	for _, value := range []string{"''", "null", "caseta_ha_fan_4_v2", "[]", "false"} {
		if _, err := config.ParseWithEnv([]byte(base+"    ha_fan_profile: "+value+"\n"), testLookup); !errors.Is(err, config.ErrInvalidYAML) {
			t.Fatalf("unqualified fan profile %q accepted: %v", value, err)
		}
	}
	wrong := "listen: '127.0.0.1:0'\n" + testPostgres(t) +
		"external_workers:\n- instance: synthetic-caseta\n  executable: /bin/sh\n  bindings:\n" +
		"  - {device: bridge, entity: fan-speed, alias: light.synthetic, ha_fan_profile: caseta_ha_fan_4_v1}\n"
	if _, err := config.ParseWithEnv([]byte(wrong), testLookup); err == nil {
		t.Fatal("fan policy accepted light alias")
	}
	both := base + "    ha_fan_profile: caseta_ha_fan_4_v1\n    ha_light_profile: caseta_ha_100_v1\n"
	if _, err := config.ParseWithEnv([]byte(both), testLookup); err == nil {
		t.Fatal("binding accepted contradictory profiles")
	}
}

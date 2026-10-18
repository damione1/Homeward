package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/damione1/Homeward/internal/hilo"
)

func nha090YAML(port int, dataDir, upstream string) string {
	var body strings.Builder
	fmt.Fprintf(&body, "listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\nhilo:\n  instance: nha090-configured\n  account_id: synthetic-account\n  location_id: 10000\n  location_hilo_id: urn:hilo:synthetic:location\n  credential_secret: hilo-credential\n  daily_limit: 500\n  lease_window_seconds: 180\n  energy_max_gap_seconds: 300\n  api_base_url: %q\n  platform_base_url: %q\n  bindings:\n", dataDir, port, upstream, upstream)
	for i, alias := range hilo.RequiredDeviceAliases() {
		fmt.Fprintf(&body, "    - {alias: %q, device_id: %d}\n", alias, i+1)
	}
	return body.String()
}

func TestNHA090HiloConfigurationAdmission(t *testing.T) {
	if _, enabled := (Config{}).HiloSettings(); enabled {
		t.Fatal("zero Config enabled Hilo")
	}
	if len(hilo.RequiredDeviceAliases()) != 37 || len(hilo.RetainedAliases()) != 58 {
		t.Fatal("closed Hilo profile changed without updating composition")
	}
	input := nha090YAML(1, "/tmp/nha090-configuration", "https://api.hiloenergie.com")
	// The two Hilo endpoints have different allowed production hosts.
	input = strings.Replace(input, "platform_base_url: \"https://api.hiloenergie.com\"", "platform_base_url: \"https://platform.hiloenergie.com\"", 1)
	lookup := func(name string) (string, bool) { return "synthetic-password", name == "HOMEWARD_TEST_PASSWORD" }
	configured, err := ParseWithEnv([]byte(input), lookup)
	if err != nil {
		t.Fatal(err)
	}
	settings, enabled := configured.HiloSettings()
	if !enabled || !settings.Valid() || len(settings.Bindings()) != 48 || len(settings.EnergyPairs()) != 10 {
		t.Fatal("admitted YAML did not keep 39 direct, 9 deferred, and 10 calculated aliases")
	}
	for _, changed := range []struct{ name, text string }{
		{"unknown option", strings.Replace(input, "lease_window_seconds:", "lease_window_secondz:", 1)},
		{"energy gap below lease", strings.Replace(input, "energy_max_gap_seconds: 300", "energy_max_gap_seconds: 120", 1)},
		{"production host substitution", strings.Replace(input, "api_base_url: \"https://api.hiloenergie.com\"", "api_base_url: \"https://attacker.example\"", 1)},
		{"missing device binding", input[:strings.LastIndex(input, "    - {alias:")]},
	} {
		t.Run(changed.name, func(t *testing.T) {
			if _, err := ParseWithEnv([]byte(changed.text), lookup); err == nil {
				t.Fatal("invalid Hilo config admitted")
			}
		})
	}
}

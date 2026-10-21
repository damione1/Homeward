package config_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/damione1/Homeward/internal/backups"
	"github.com/damione1/Homeward/internal/config"
)

func nha154LaunchYAML(dataDir, section string) []byte {
	return []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: 5432\n  database: test\n  user: test\n  password_env: TEST_DB_PASSWORD\n  sslmode: disable\n%s", dataDir, section))
}
func TestNHA154LaunchConfiguration(t *testing.T) {
	dataDir := t.TempDir()
	parsed, err := config.ParseWithEnv(nha154LaunchYAML(dataDir, ""), testLookup)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := parsed.BackupProviders()
	if err != nil || !configuration.Valid() || len(configuration.Instances()) != 1 {
		t.Fatalf("default configuration: %v", err)
	}
	instance := configuration.Instances()[0]
	local, err := instance.Local()
	if err != nil || instance.ID().String() != backups.Local.String() || !instance.Enabled() || local.Root() != filepath.Join(dataDir, "backups") {
		t.Fatalf("default instance: %v", err)
	}
	section := "backups:\n  instances:\n    - id: local\n      provider: local\n      enabled: false\n    - id: secondary\n      provider: local\n      local:\n        root: " + fmt.Sprintf("%q", filepath.Join(dataDir, "alternate")) + "\n"
	raw := nha154LaunchYAML(dataDir, section)
	parsed, err = config.ParseWithEnv(raw, testLookup)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err = parsed.BackupProviders()
	if err != nil {
		t.Fatal(err)
	}
	instances := configuration.Instances()
	if len(instances) != 2 || instances[0].Enabled() || !instances[1].Enabled() {
		t.Fatal("false/omission were conflated")
	}
	local, err = instances[1].Local()
	if err != nil || local.Root() != filepath.Join(dataDir, "alternate") {
		t.Fatal("custom local root lost")
	}
	startup, err := config.NewDetachedStartup(raw, "configuration.yaml", nil, testLookup)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := startup.BootstrapSource()
	if err != nil {
		t.Fatal(err)
	}
	sidecar, err := config.ParseBootstrapLaunchWithEnv(bootstrap.Bytes(), testLookup)
	if err != nil {
		t.Fatal(err)
	}
	sidecarProviders, err := sidecar.BackupProviders()
	if err != nil {
		t.Fatal(err)
	}
	retained := sidecarProviders.Instances()
	if len(retained) != 2 || retained[0].Enabled() || !retained[1].Enabled() {
		t.Fatal("sidecar changed instance activation")
	}
	retainedLocal, err := retained[1].Local()
	if err != nil || retainedLocal.Root() != local.Root() {
		t.Fatal("sidecar lost machine root")
	}
	operational, err := config.ParseOperationalWithEnv(raw, testLookup)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := operational.BackupProviders(); !errors.Is(err, config.ErrUninitialized) {
		t.Fatal("historical operational source acquired machine storage authority")
	}
	empty, err := config.ParseWithEnv(nha154LaunchYAML(dataDir, "backups:\n  instances: []\n"), testLookup)
	if err != nil {
		t.Fatal(err)
	}
	none, err := empty.BackupProviders()
	if err != nil || !none.Valid() || len(none.Instances()) != 0 {
		t.Fatal("explicit empty instances must disable all")
	}
	if _, err := (config.Config{}).BackupProviders(); !errors.Is(err, config.ErrUninitialized) {
		t.Fatal("zero config admitted")
	}
}
func TestNHA154LaunchRejectsInvalidProvidersBeforeIO(t *testing.T) {
	dataDir := t.TempDir()
	for name, section := range map[string]string{
		"null":             "backups: null\n",
		"empty-map":        "backups: {}\n",
		"wrong-shape":      "backups: []\n",
		"null-list":        "backups:\n  instances: null\n",
		"unknown-field":    "backups:\n  instances: []\n  retention: 30\n",
		"wrong-enabled":    "backups:\n  instances:\n    - id: local\n      provider: local\n      enabled: 'false'\n",
		"unknown-provider": "backups:\n  instances:\n    - id: local\n      provider: s3\n",
		"missing-id":       "backups:\n  instances:\n    - provider: local\n",
		"missing-provider": "backups:\n  instances:\n    - id: local\n",
		"duplicate-id":     "backups:\n  instances:\n    - id: local\n      provider: local\n    - id: local\n      provider: local\n",
		"duplicate-field":  "backups:\n  instances:\n    - id: local\n      provider: local\n      enabled: true\n      enabled: false\n",
		"union":            "backups:\n  instances:\n    - id: local\n      provider: local\n      s3: {}\n",
		"path-escape":      "backups:\n  instances:\n    - id: '../local'\n      provider: local\n",
		"relative-root":    "backups:\n  instances:\n    - id: local\n      provider: local\n      local:\n        root: '../NHA154_PRIVATE_PATH'\n",
		"disabled-invalid": "backups:\n  instances:\n    - id: local\n      provider: local\n      enabled: false\n      local:\n        root: '/'\n",
		"null-local":       "backups:\n  instances:\n    - id: local\n      provider: local\n      local: null\n",
		"unknown-option":   "backups:\n  instances:\n    - id: local\n      provider: local\n      local:\n        root: '/private/root'\n        password: NHA154_PRIVATE_CANARY\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := config.ParseWithEnv(nha154LaunchYAML(dataDir, section), testLookup)
			if err == nil {
				t.Fatal("invalid launch admitted")
			}
			if strings.Contains(err.Error(), "NHA154_PRIVATE") || strings.Contains(err.Error(), dataDir) {
				t.Fatal("config admission leaked private input")
			}
		})
	}
}

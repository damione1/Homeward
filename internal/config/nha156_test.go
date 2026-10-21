package config_test

import (
	"github.com/damione1/Homeward/internal/backups"
	"github.com/damione1/Homeward/internal/config"
	"strings"
	"testing"
)

const nha156Section = "backups:\n  instances:\n    - id: offsite\n      provider: s3\n      s3:\n        endpoint: 'http://127.0.0.1:1'\n        bucket: nha156-bucket\n        prefix: homeward/house\n        region: us-east-1\n        credential_ref:\n          instance: backup-credentials\n          name: credential-bundle\n"

func TestNHA156StrictConfigAndDisabledDefault(t *testing.T) {
	for _, activation := range []string{"", "      enabled: false\n", "      enabled: true\n"} {
		section := strings.Replace(nha156Section, "      s3:\n", activation+"      s3:\n", 1)
		parsed, err := config.ParseWithEnv(nha154LaunchYAML(t.TempDir(), section), testLookup)
		if err != nil {
			t.Fatal("admit S3 config")
		}
		plan, err := parsed.BackupProviders()
		if err != nil || len(plan.Instances()) != 1 {
			t.Fatal("S3 plan")
		}
		s := plan.Instances()[0]
		if s.Kind() != backups.S3 || s.Enabled() != (activation == "      enabled: true\n") {
			t.Fatal("S3 omission activated network")
		}
	}
	for _, section := range []string{strings.Replace(nha156Section, "      s3:", "      local: {}\n      s3:", 1), strings.Replace(nha156Section, "      s3:", "      s3: null\n      ignored:", 1), strings.Replace(nha156Section, "      s3:", "      enabled: 'true'\n      s3:", 1), strings.Replace(nha156Section, "        region: us-east-1", "        region: us-east-1\n        secret_access_key: NHA156SECRET", 1), strings.Replace(nha156Section, "          name: credential-bundle", "          name: credential-bundle\n          name: other", 1), strings.Replace(nha156Section, "          name: credential-bundle", "          name: null", 1), strings.Replace(nha156Section, "        prefix: homeward/house", "        prefix: ../outside", 1)} {
		if _, err := config.ParseWithEnv(nha154LaunchYAML(t.TempDir(), section), testLookup); err == nil {
			t.Fatal("unsafe S3 schema admitted")
		}
	}
	// An existing bootstrap sidecar is restart-only; it must retain the exact
	// inactive S3 selection rather than silently enabling a provider on replay.
	raw := nha154LaunchYAML(t.TempDir(), nha156Section)
	startup, err := config.NewDetachedStartup(raw, "configuration.yaml", nil, testLookup)
	if err != nil {
		t.Fatal("S3 launch snapshot")
	}
	if !startup.Valid() {
		t.Fatal("invalid restart inputs")
	}
}

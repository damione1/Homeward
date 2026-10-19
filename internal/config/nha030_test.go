package config

import (
	"errors"
	"fmt"
	"testing"

	"github.com/damione1/Homeward/internal/recorder"
)

func TestNHA030RecorderInputAbsenceAndExplicitZero(t *testing.T) {
	defaults, err := ParseRecorderInput(nil)
	if err != nil || !defaults.Valid() || defaults.TotalBytes() != recorder.DefaultBudgetBytes {
		t.Fatalf("missing recorder defaults: %+v %v", defaults, err)
	}
	zero := 0
	if _, err := ParseRecorderInput(&RecorderInput{BudgetMiB: &zero}); err == nil {
		t.Fatal("explicit zero budget admitted")
	}
	max := 32
	settings, err := ParseRecorderInput(&RecorderInput{BudgetMiB: &max})
	if err != nil || settings.TotalBytes() != recorder.MaxBudgetBytes {
		t.Fatalf("maximum admitted budget: %+v %v", settings, err)
	}
	above := 33
	if _, err := ParseRecorderInput(&RecorderInput{BudgetMiB: &above}); err == nil {
		t.Fatal("budget above maximum admitted")
	}
}

func TestNHA030RecorderYAMLBoundary(t *testing.T) {
	base := fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: 5432\n  database: test\n  user: test\n  password_env: TEST_DB_PASSWORD\n  sslmode: disable\n", t.TempDir())
	lookup := func(string) (string, bool) { return "fake", true }
	parsed, err := ParseWithEnv([]byte(base), lookup)
	if err != nil {
		t.Fatal(err)
	}
	limits, err := parsed.RecorderLimits()
	if err != nil || limits.TotalBytes() != recorder.DefaultBudgetBytes {
		t.Fatalf("default Recorder limits: %+v %v", limits, err)
	}
	for _, input := range []string{"recorder:\n  budget_mib: 0\n", "recorder:\n  budget_mib: 33\n", "recorder:\n  unknown: true\n"} {
		if _, err := ParseWithEnv([]byte(base+input), lookup); err == nil {
			t.Fatalf("invalid Recorder YAML admitted: %q", input)
		}
	}
	configured, err := ParseWithEnv([]byte(base+"recorder:\n  budget_mib: 32\n"), lookup)
	if err != nil {
		t.Fatal(err)
	}
	limits, err = configured.RecorderLimits()
	if err != nil || limits.TotalBytes() != recorder.MaxBudgetBytes {
		t.Fatalf("configured Recorder limits: %+v %v", limits, err)
	}
	if _, err := (Config{}).RecorderLimits(); !errors.Is(err, ErrUninitialized) {
		t.Fatalf("zero config supplied Recorder limits: %v", err)
	}
}

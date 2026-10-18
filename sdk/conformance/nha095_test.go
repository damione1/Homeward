package conformance_test

import (
	"context"
	"testing"

	"github.com/damione1/Homeward/sdk/conformance"
)

func TestNHA095PublicSuiteRejectsInvalidOptions(t *testing.T) {
	failures := conformance.Check(context.Background(), nil, conformance.Options{WorkDir: t.TempDir()})
	if len(failures) != 1 || failures[0].Code() != conformance.FailureSetup {
		t.Fatalf("invalid public factory admitted: %v", failures)
	}
}

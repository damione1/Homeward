package integrations_test

import (
	"github.com/damione1/Homeward/internal/domain"
	"testing"
)

// Explicit LEAP fixture, not generic fan semantics.
func nha212FixtureFan(t *testing.T, key string) domain.FanValue {
	t.Helper()
	on := key != "Off"
	value, err := domain.NewFanValue(domain.FanValueInput{On: &on, Speed: &key})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

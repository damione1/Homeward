package domain_test

import (
	"errors"
	"testing"

	"github.com/damione1/Homeward/internal/domain"
)

func TestEntityIDRepresentation(t *testing.T) {
	for _, value := range []string{"synthetic-entity", "Host:opaque/É-42", " id with spaces "} {
		id, err := domain.ParseEntityID(value)
		if err != nil {
			t.Fatal(err)
		}
		got, err := id.Value()
		if err != nil || got != value {
			t.Fatalf("identity normalized or lost: %q, %v", got, err)
		}
	}
	for _, value := range []string{"", string([]byte{0xff})} {
		id, err := domain.ParseEntityID(value)
		if !errors.Is(err, domain.ErrInvalidEntityID) {
			t.Fatalf("accepted %q", value)
		}
		if _, err := id.Value(); !errors.Is(err, domain.ErrInvalidEntityID) {
			t.Fatal("failed parse produced publishable ID")
		}
	}
	var zero domain.EntityID
	if _, err := zero.Value(); !errors.Is(err, domain.ErrInvalidEntityID) {
		t.Fatal("zero ID is publishable")
	}
}

package integrationadmin

import (
	"slices"
	"testing"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/sonos"
)

func TestNHA195SonosDescriptorDeclaresShippedCommonMediaAndObservedVolume(t *testing.T) {
	descriptor, err := findDescriptor("sonos")
	if err != nil {
		t.Fatal(err)
	}
	if len(descriptor.capabilities) != 2 || !slices.Contains(descriptor.capabilities, sonos.Schema) || !slices.Contains(descriptor.capabilities, contracts.MeasurementSchema) || slices.Contains(descriptor.capabilities, "sonos.media.v1") {
		t.Fatal("administrative descriptor differs from native media and volume capabilities")
	}
}

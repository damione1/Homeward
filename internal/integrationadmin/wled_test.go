package integrationadmin

import (
	"github.com/damione1/Homeward/contracts"
	"testing"
)

func TestNHA083WLEDDescriptorDeclaresOnlyShippedLight(t *testing.T) {
	d, e := findDescriptor("wled")
	if e != nil || len(d.capabilities) != 1 || d.capabilities[0] != contracts.LightSchema {
		t.Fatalf("descriptor %v %v", d, e)
	}
}

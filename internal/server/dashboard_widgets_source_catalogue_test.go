package server

import (
	"testing"

	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"google.golang.org/protobuf/proto"
)

// Native UI domains remain valid registry references when a dashboard source
// is published. Their presence must not manufacture unimplemented HA actions
// or prevent every unrelated configuration from being admitted.
func TestDashboardWidgetsNewDomainReferencesRemainPassiveInHACatalogue(t *testing.T) {
	capabilities := []*registrywire.CapabilityDescription{
		(nhf009Device{kind: nhf009Cover, endpoint: "cover", title: "Cover"}).capability(),
		(nhf009Device{kind: nhf009Lock, endpoint: "lock", title: "Lock"}).capability(),
		(nhf009Device{kind: nhf009Alarm, endpoint: "alarm", title: "Alarm"}).capability(),
		(nhf010Device{kind: nhf010Weather, endpoint: "weather", title: "Weather"}).capability(),
		(nhf010Device{kind: nhf010Presence, endpoint: "presence", title: "Presence"}).capability(),
		(nhf010Device{kind: nhf010Details, endpoint: "details", title: "Presence details"}).capability(),
	}
	for _, capability := range capabilities {
		t.Run(capability.Endpoint, func(t *testing.T) {
			catalog, _ := nha055SemanticCatalog(t)
			supports, err := sourceSemanticSupports(catalog, []*registrywire.CapabilityDescription{capability})
			if err != nil || len(supports) != 0 {
				t.Fatalf("valid native reference rejected or gained HA support: %v %v", supports, err)
			}
			invalid := proto.Clone(capability).(*registrywire.CapabilityDescription)
			invalid.Major = 0
			if _, err = sourceSemanticSupports(catalog, []*registrywire.CapabilityDescription{invalid}); err == nil {
				t.Fatal("passive references bypassed authoritative capability validation")
			}
		})
	}
}

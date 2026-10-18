package integrations

import (
	"errors"
	"strconv"
	"testing"

	"github.com/damione1/Homeward/internal/domain"
)

func TestNHA069RetirementFenceSaturationClosesRemainingOwner(t *testing.T) {
	instance, err := domain.ParseIntegrationInstanceID("nha069-saturation")
	if err != nil {
		t.Fatal(err)
	}
	device, err := domain.ParseDeviceID("Host:synthetic/saturation-device")
	if err != nil {
		t.Fatal(err)
	}
	entity, err := domain.ParseEntityID("Host:synthetic/saturation-relay")
	if err != nil {
		t.Fatal(err)
	}
	key, err := NewSourceKey("controller", "still-active")
	if err != nil {
		t.Fatal(err)
	}
	host := &Manager{active: make(map[domain.IntegrationInstanceID]*Session), owners: make(map[domain.EntityID]Binding), retiring: make(map[domain.EntityID]struct{}), retiringSources: make(map[retiringSource]struct{})}
	session := &Session{host: host, instance: instance, id: "synthetic-session", active: true, bindings: make(map[domain.EntityID]Binding)}
	host.active[instance] = session
	binding := Binding{instance: instance, session: session.id, key: key, device: device, entity: entity}
	session.bindings[entity] = binding
	host.owners[entity] = binding
	for i := 0; i < 16384; i++ {
		fenced, err := NewSourceKey("controller", strconv.Itoa(i))
		if err != nil {
			t.Fatal(err)
		}
		host.retiringSources[retiringSource{instance, fenced}] = struct{}{}
	}
	host.fenceRetirementLocked(instance, key, entity)
	if !host.retirementExhausted || len(host.retiringSources) != 16384 {
		t.Fatal("full fence map was evicted or allowed unbounded growth")
	}
	if err := session.owned(binding); !errors.Is(err, ErrAdmissionsClosed) {
		t.Fatalf("unfenced live owner remained usable after saturation: %v", err)
	}
	if (AdmittedCommand{host: host, entity: entity, binding: binding}).liveOwner() {
		t.Fatal("late command acknowledgement remained usable after saturation")
	}
}

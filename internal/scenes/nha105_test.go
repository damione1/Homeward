package scenes

import (
	"context"
	"github.com/damione1/Homeward/internal/domain"
	"testing"
	"time"
)

func TestNHA105SolarObservationCannotBecomeSceneCommand(t *testing.T) {
	missing, _ := domain.SolarOccurrenceMissing(domain.SolarNoEventWithinSearchHorizon)
	event, _ := domain.NewSolarEventValue(domain.SolarDawn, missing)
	value, _ := domain.NewSolarEventStateValue(event)
	alias, _ := domain.ParseHAAlias("sensor.sun_next_dawn")
	actor, _ := domain.ParseActorID("nha105-test")
	id, _ := domain.ParseCommandID("nha105-test")
	// No command service is installed: attempting any preparation would panic.
	service := &Service{}
	prepared := service.prepareMember(context.Background(), memberSpec{origin: originNative, alias: alias, native: value}, actor, id, nil, time.Now().Add(time.Minute))
	if prepared.kind != preparedRefused || prepared.code != RefusalCapability {
		t.Fatal("solar observation became scene command")
	}
}

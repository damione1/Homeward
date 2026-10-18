package integrations_test

import (
	"context"
	"testing"
	"time"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/scenes"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
)

func TestNHA046DurableSceneRecordedPreparedAfterDeleteAndRestart(t *testing.T) {
	store, settings := nha024Store(t)
	ctx := context.Background()
	now := time.Now()
	clock := &nha179Clock{now: now}
	rig := nha179SetupWithClock(t, false, clock)
	level := 37.5
	rig.publish(t, true, &level, domain.FreshnessFresh, domain.ProvenanceObserved)
	durable := nha179Durable(t, rig, store)
	sceneService, ref := nha046TemporaryService(t, durable, rig, clock)
	invocation, _ := domain.ParseCommandID("nha046-durable-scene")
	slot, err := scenes.NewPreparationSlot(sceneService, ref, nha022Actor(t), invocation, nil, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := slot.PrepareOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, err := sceneService.ApplyPrepared(ctx, prepared)
	if err != nil || first.Aggregate() != scenes.AggregateComplete || rig.driver.Count() != 1 {
		t.Fatalf("first durable scene apply: %v %+v sends=%d", err, first, rig.driver.Count())
	}
	response := first.Members()[0].Response()
	if response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("first durable member result: %v", response)
	}
	if err := sceneService.DeleteTemporary(ctx, ref); err != nil {
		t.Fatal(err)
	}
	replayed, err := sceneService.ApplyPrepared(ctx, prepared)
	if err != nil || replayed.Aggregate() != scenes.AggregateComplete || rig.driver.Count() != 1 ||
		!proto.Equal(replayed.Members()[0].Response(), response) {
		t.Fatalf("deleted scene lost exact durable replay: %v %+v sends=%d", err, replayed, rig.driver.Count())
	}
	// A prepared but never admitted member must not create a durable intent
	// after scene invalidation, even if the caller retains the opaque plan.
	lightAlias, _ := domain.ParseHAAlias("light.nha179")
	newRef, _, err := sceneService.CreateTemporary(ctx, nha046SceneAlias(t, 301),
		nil, []domain.HAAlias{lightAlias}, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	unsentID, _ := domain.ParseCommandID("nha046-durable-never-admitted")
	unsentSlot, err := scenes.NewPreparationSlot(sceneService, newRef, nha022Actor(t), unsentID, nil, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	unsent, err := unsentSlot.PrepareOnce(ctx)
	if err != nil || len(unsent.Members()) != 1 || unsent.Members()[0].Outcome() != scenes.OutcomePrepared {
		t.Fatalf("unadmitted member was not prepared: %v %+v", err, unsent.Members())
	}
	if err := sceneService.DeleteTemporary(ctx, newRef); err != nil {
		t.Fatal(err)
	}
	refused, err := sceneService.ApplyPrepared(ctx, unsent)
	if err != nil || refused.Aggregate() != scenes.AggregateNone || rig.driver.Count() != 1 {
		t.Fatalf("deleted unadmitted scene created a durable send: %v %+v", err, refused)
	}
	id, err := domain.ParseCommandID(response.CommandId)
	if err != nil {
		t.Fatal(err)
	}
	status, found, err := durable.Status(ctx, id)
	if err != nil || !found || status.State != storage.IntentResult || !proto.Equal(status.Response, response) {
		t.Fatalf("durable scene record was not retained: %+v found=%t err=%v", status, found, err)
	}
	sceneService.Close()
	if err := durable.Close(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Close)
	secondRig := nha179Setup(t, false)
	second := nha179Durable(t, secondRig, reopened)
	status, found, err = second.Status(ctx, id)
	if err != nil || !found || status.State != storage.IntentResult || !proto.Equal(status.Response, response) || secondRig.driver.Count() != 0 {
		t.Fatalf("restart reconstructed a scene effect: %+v found=%t err=%v sends=%d", status, found, err, secondRig.driver.Count())
	}
}

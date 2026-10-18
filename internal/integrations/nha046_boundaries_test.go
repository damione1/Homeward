package integrations_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/logicselection"
	"github.com/damione1/Homeward/internal/scenes"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/scope"
)

func nha046SceneService(t *testing.T, rig nha179Rig, clock *nha179Clock, capture scenes.CaptureSource) (*scenes.Service, scenes.Catalogue) {
	t.Helper()
	selection, err := logicselection.LoadV1()
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := scenes.CompileSelected(selection, scope.LogicMVPSourcesV1())
	if err != nil {
		t.Fatal(err)
	}
	if capture == nil {
		capture = rig.fixture.Sequencer()
	}
	service, err := scenes.NewService(catalogue, capture, rig.gen, rig.service, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	return service, catalogue
}

func nha046BoundaryRig(t *testing.T) (nha179Rig, *nha179Clock) {
	t.Helper()
	clock := &nha179Clock{now: time.Now()}
	rig := nha179SetupWithClock(t, false, clock)
	level := 25.0
	rig.publish(t, true, &level, domain.FreshnessFresh, domain.ProvenanceObserved)
	return rig, clock
}

func nha046SceneAlias(t *testing.T, i int) domain.HAAlias {
	t.Helper()
	alias, err := domain.ParseHAAlias(fmt.Sprintf("scene.nha046_boundary_%03d", i))
	if err != nil {
		t.Fatal(err)
	}
	return alias
}

func nha046ExplicitSwitches(t *testing.T, count, padding int) []scenes.ExplicitMember {
	t.Helper()
	members := make([]scenes.ExplicitMember, count)
	for i := range members {
		alias, err := domain.ParseHAAlias(fmt.Sprintf("switch.%s%03d", strings.Repeat("x", padding), i))
		if err != nil {
			t.Fatal(err)
		}
		members[i], err = scenes.NewExplicitSwitch(alias, true)
		if err != nil {
			t.Fatal(err)
		}
	}
	return members
}

func TestNHA046TemporarySceneCountAndMemberBounds(t *testing.T) {
	t.Run("128 scenes without eviction", func(t *testing.T) {
		rig, clock := nha046BoundaryRig(t)
		service, _ := nha046SceneService(t, rig, clock, nil)
		member := nha046ExplicitSwitches(t, 1, 0)
		var first scenes.SceneRef
		for i := 0; i < 128; i++ {
			ref, omitted, err := service.CreateTemporary(context.Background(), nha046SceneAlias(t, i), member, nil, time.Hour, nil)
			if err != nil || len(omitted) != 0 {
				t.Fatalf("scene %d of 128: %v %v", i, err, omitted)
			}
			if i == 0 {
				first = ref
			}
		}
		if _, _, err := service.CreateTemporary(context.Background(), nha046SceneAlias(t, 128), member, nil, time.Hour, nil); !errors.Is(err, scenes.ErrSceneCapacity) {
			t.Fatalf("129th scene was admitted or silently evicted another: %v", err)
		}
		if current, err := service.Temporary(context.Background(), nha046SceneAlias(t, 1)); err != nil || current.Version() == 0 {
			t.Fatalf("existing scene was evicted: %v", err)
		}
		if err := service.DeleteTemporary(context.Background(), first); err != nil {
			t.Fatal(err)
		}
		if _, _, err := service.CreateTemporary(context.Background(), nha046SceneAlias(t, 128), member, nil, time.Hour, nil); err != nil {
			t.Fatalf("delete did not release scene capacity: %v", err)
		}
	})
	t.Run("256 members", func(t *testing.T) {
		rig, clock := nha046BoundaryRig(t)
		service, _ := nha046SceneService(t, rig, clock, nil)
		if _, _, err := service.CreateTemporary(context.Background(), nha046SceneAlias(t, 0), nha046ExplicitSwitches(t, 256, 0), nil, time.Hour, nil); err != nil {
			t.Fatalf("256 members refused: %v", err)
		}
		if _, _, err := service.CreateTemporary(context.Background(), nha046SceneAlias(t, 1), nha046ExplicitSwitches(t, 257, 0), nil, time.Hour, nil); !errors.Is(err, scenes.ErrInvalidScene) {
			t.Fatalf("257 members admitted: %v", err)
		}
	})
}

func TestNHA046TemporarySceneBytesAndTTLBounds(t *testing.T) {
	t.Run("retained bytes and replacement", func(t *testing.T) {
		rig, clock := nha046BoundaryRig(t)
		service, _ := nha046SceneService(t, rig, clock, nil)
		wide := nha046ExplicitSwitches(t, 256, 240)
		var refs []scenes.SceneRef
		for i := 0; i < 128; i++ {
			ref, _, err := service.CreateTemporary(context.Background(), nha046SceneAlias(t, i), wide, nil, time.Hour, nil)
			if errors.Is(err, scenes.ErrSceneCapacity) {
				break
			}
			if err != nil {
				t.Fatalf("wide scene %d: %v", i, err)
			}
			refs = append(refs, ref)
		}
		if len(refs) < 2 || len(refs) >= 128 {
			t.Fatalf("4 MiB retention bound was not reached separately from scene count: %d", len(refs))
		}
		if _, err := service.Temporary(context.Background(), nha046SceneAlias(t, 0)); err != nil {
			t.Fatalf("capacity evicted an existing scene: %v", err)
		}
		// Replacing with a small scene must release the old retained-byte cost.
		version := refs[0].Version()
		small := nha046ExplicitSwitches(t, 1, 0)
		if _, _, err := service.CreateTemporary(context.Background(), nha046SceneAlias(t, 0), small, nil, time.Hour, &version); err != nil {
			t.Fatalf("replacement could not release retained bytes: %v", err)
		}
		if _, _, err := service.CreateTemporary(context.Background(), nha046SceneAlias(t, len(refs)), wide, nil, time.Hour, nil); err != nil {
			t.Fatalf("released bytes could not admit one more wide scene: %v", err)
		}
		if err := service.DeleteTemporary(context.Background(), refs[1]); err != nil {
			t.Fatal(err)
		}
		if _, _, err := service.CreateTemporary(context.Background(), nha046SceneAlias(t, len(refs)+1), wide, nil, time.Hour, nil); err != nil {
			t.Fatalf("delete did not release retained bytes: %v", err)
		}
	})
	t.Run("zero exact 24h and greater", func(t *testing.T) {
		rig, clock := nha046BoundaryRig(t)
		service, _ := nha046SceneService(t, rig, clock, nil)
		member := nha046ExplicitSwitches(t, 1, 0)
		id := nha046SceneAlias(t, 0)
		if _, _, err := service.CreateTemporary(context.Background(), id, member, nil, 0, nil); !errors.Is(err, scenes.ErrInvalidScene) {
			t.Fatalf("TTL zero accepted: %v", err)
		}
		if _, _, err := service.CreateTemporary(context.Background(), id, member, nil, 24*time.Hour+time.Nanosecond, nil); !errors.Is(err, scenes.ErrInvalidScene) {
			t.Fatalf("TTL greater than 24h accepted: %v", err)
		}
		ref, _, err := service.CreateTemporary(context.Background(), id, member, nil, 24*time.Hour, nil)
		if err != nil {
			t.Fatalf("exact 24h refused: %v", err)
		}
		clock.Set(clock.Now().Add(24 * time.Hour))
		if _, err := service.Temporary(context.Background(), id); !errors.Is(err, scenes.ErrSceneUnavailable) {
			t.Fatalf("scene stayed visible at exact TTL: %v", err)
		}
		if err := service.DeleteTemporary(context.Background(), ref); !errors.Is(err, scenes.ErrSceneConflict) {
			t.Fatalf("expired version was deletable: %v", err)
		}
	})
}

type nha046EpochCapture struct {
	base  *sequencer.Sequencer
	epoch string
}

func (c *nha046EpochCapture) Capture() (sequencer.Snapshot, error)   { return c.base.Capture() }
func (c *nha046EpochCapture) CurrentAdmissionEpoch() (string, error) { return c.epoch, nil }

func TestNHA046TemporarySceneEpochConfigAndSourceRefusals(t *testing.T) {
	rig, clock := nha046BoundaryRig(t)
	epoch, err := rig.fixture.Sequencer().CurrentAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	capture := &nha046EpochCapture{base: rig.fixture.Sequencer(), epoch: epoch}
	service, catalogue := nha046SceneService(t, rig, clock, capture)
	good := nha046ExplicitSwitches(t, 1, 0)
	ctx := context.Background()
	id := nha046SceneAlias(t, 0)
	if _, _, err := service.CreateTemporary(ctx, catalogue.IDs()[0], good, nil, time.Hour, nil); !errors.Is(err, scenes.ErrSceneConflict) {
		t.Fatalf("declarative ID was replaced: %v", err)
	}
	ref, _, err := service.CreateTemporary(ctx, id, good, nil, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	rig.gen.mu.Lock()
	rig.gen.revision++
	rig.gen.mu.Unlock()
	if _, err := service.Temporary(ctx, id); !errors.Is(err, scenes.ErrSceneUnavailable) {
		t.Fatalf("config revision did not purge temporary scene: %v", err)
	}
	if _, _, err := service.CreateTemporary(ctx, id, good, nil, time.Hour, nil); err != nil {
		t.Fatalf("config purge retained old ID: %v", err)
	}
	capture.epoch = "synthetic-other-epoch"
	if _, err := service.Temporary(ctx, id); !errors.Is(err, scenes.ErrSceneUnavailable) {
		t.Fatalf("epoch change did not purge temporary scene: %v", err)
	}
	if err := service.DeleteTemporary(ctx, ref); !errors.Is(err, scenes.ErrSceneConflict) {
		t.Fatalf("invalidated old version was deleted: %v", err)
	}
	// A source alias cannot be supplied twice, even once as explicit intent.
	alias, _ := domain.ParseHAAlias("switch.overlap")
	explicit, _ := scenes.NewExplicitSwitch(alias, true)
	if _, _, err := service.CreateTemporary(ctx, nha046SceneAlias(t, 1), []scenes.ExplicitMember{explicit}, []domain.HAAlias{alias}, time.Hour, nil); !errors.Is(err, scenes.ErrInvalidScene) {
		t.Fatalf("explicit/capture overlap accepted: %v", err)
	}
	// Restore the real epoch before evaluating the sequencer's registry copy.
	capture.epoch = epoch
	instance := nha016Instance(t, "nha179-synthetic")
	a, _ := integrations.NewSourceKey("nha046-ambiguous-a", "relay")
	b, _ := integrations.NewSourceKey("nha046-ambiguous-b", "relay")
	ids, err := rig.fixture.Resolve(ctx, instance, []integrations.SourceKey{a, b})
	if err != nil {
		t.Fatal(err)
	}
	ambiguous, _ := domain.ParseHAAlias("switch.nha046_ambiguous")
	records := []integrations.RegistryRecord{
		nha022Record(t, ids[0], instance, a, "switch.nha046_ambiguous", "switch.nha046_ambiguous", false),
		nha022Record(t, ids[1], instance, b, "switch.nha046_ambiguous", "switch.nha046_ambiguous", false),
	}
	if err := rig.fixture.PublishRegistry(ctx, records); err != nil {
		t.Fatal(err)
	}
	_, omitted, err := service.CreateTemporary(ctx, nha046SceneAlias(t, 2), good, []domain.HAAlias{ambiguous}, time.Hour, nil)
	if err != nil || len(omitted) != 1 || omitted[0].Code() != scenes.OmissionAlias {
		t.Fatalf("ambiguous alias was not omitted: %v %+v", err, omitted)
	}
	retired, _ := domain.ParseHAAlias("switch.nha046_retired")
	if err := rig.fixture.PublishRegistry(ctx, []integrations.RegistryRecord{nha022Record(t, ids[0], instance, a, "switch.nha046_retired", "switch.nha046_retired", true)}); err != nil {
		t.Fatal(err)
	}
	_, omitted, err = service.CreateTemporary(ctx, nha046SceneAlias(t, 3), good, []domain.HAAlias{retired}, time.Hour, nil)
	if err != nil || len(omitted) != 1 || omitted[0].Code() != scenes.OmissionAlias {
		t.Fatalf("retired alias was not omitted: %v %+v", err, omitted)
	}
	if _, _, err := service.CreateTemporary(ctx, nha046SceneAlias(t, 4), nil, []domain.HAAlias{retired}, time.Hour, nil); !errors.Is(err, scenes.ErrSceneUnavailable) {
		t.Fatalf("zero admissible members accepted: %v", err)
	}
}

func TestNHA046DeclarativeClimateMembersRefuseWithoutPartialClimateSend(t *testing.T) {
	rig, clock := nha046BoundaryRig(t)
	service, _ := nha046SceneService(t, rig, clock, nil)
	for _, source := range []string{"scene.climate_day_temperature", "scene.climate_night_temperature"} {
		t.Run(source, func(t *testing.T) {
			id, _ := domain.ParseHAAlias(source)
			ref, err := service.Declarative(id)
			if err != nil {
				t.Fatal(err)
			}
			invocation, _ := domain.ParseCommandID("nha046-" + strings.TrimPrefix(source, "scene."))
			slot, err := scenes.NewPreparationSlot(service, ref, nha022Actor(t), invocation, nil, clock.Now().Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := slot.PrepareOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			members := prepared.Members()
			if len(members) != 2 || members[0].Outcome() != scenes.OutcomeRefused || members[1].Outcome() != scenes.OutcomeRefused ||
				members[0].Field() != "humidity,fan_mode" || members[1].Field() != "preset_mode" ||
				members[0].Code() != scenes.RefusalCapability || members[1].Code() != scenes.RefusalCapability ||
				members[0].Source().Line == 0 || members[1].Source().Line == 0 {
				t.Fatalf("operative climate requirements lost: %+v", members)
			}
			result, err := service.ApplyPrepared(context.Background(), prepared)
			if err != nil || result.Aggregate() != scenes.AggregateNone || rig.driver.Count() != 0 {
				t.Fatalf("unsupported climate member reached a driver: %v %+v", err, result)
			}
		})
	}
}

func TestNHA046TemporaryCaptureOmitsNonCriticalObservation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		availability domain.Availability
		freshness    domain.Freshness
		provenance   domain.Provenance
	}{
		{"stale", domain.AvailabilityAvailable, domain.FreshnessStale, domain.ProvenanceObserved},
		{"restored", domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceRestored},
		{"estimated", domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceEstimated},
		{"unavailable", domain.AvailabilityUnavailable, domain.FreshnessFresh, domain.ProvenanceObserved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig, clock := nha046BoundaryRig(t)
			quality, err := domain.NewQuality(tc.availability, tc.freshness, tc.provenance)
			if err != nil {
				t.Fatal(err)
			}
			level := 25.0
			rig.publishQuality(t, true, &level, quality)
			service, _ := nha046SceneService(t, rig, clock, nil)
			alias, _ := domain.ParseHAAlias("light.nha179")
			ref, omissions, err := service.CreateTemporary(context.Background(), nha046SceneAlias(t, 0),
				nha046ExplicitSwitches(t, 1, 0), []domain.HAAlias{alias}, time.Hour, nil)
			if err != nil || len(omissions) != 1 || omissions[0].Code() != scenes.OmissionQuality {
				t.Fatalf("%s was captured as critical: ref=%+v omissions=%+v err=%v", tc.name, ref, omissions, err)
			}
		})
	}
}

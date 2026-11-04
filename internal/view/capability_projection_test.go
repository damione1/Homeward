package view_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/apiresources"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/view"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func projectPreparedItem(t *testing.T, item view.Item) ([]apiresources.Capability, error) {
	t.Helper()
	home, err := apitypes.ParseHomeName("homes/synthetic-capture")
	if err != nil {
		t.Fatal(err)
	}
	_, entity, err := item.Registry().IDs()
	if err != nil {
		t.Fatal(err)
	}
	observed, exists := item.State()
	if exists {
		return apiresources.ProjectCapabilities(home, entity, item.Capabilities(), &observed)
	}
	return apiresources.ProjectCapabilities(home, entity, item.Capabilities(), nil)
}

func TestCapabilityProjectionIsPreparedOnceAndFrozenAcrossPages(t *testing.T) {
	f := newWatchFixture(t, 2)
	f.publish(t, 0, false)
	f.publish(t, 1, false)
	calls := 0
	var returned [][]apiresources.Capability
	var service *view.Service
	projector := func(ctx context.Context, actor view.ActorID, item view.Item, cursor view.Cursor) ([]apiresources.Capability, error) {
		calls++
		if actor != f.actor {
			t.Fatal("projector received another actor")
		}
		if _, err := cursor.CataloguePosition(); err != nil {
			t.Fatal("missing capture catalogue fence", err)
		}
		if _, _, _, _, err := cursor.Components(); err != nil {
			t.Fatal(err)
		}
		// This acquires the view mutex. A projector under that mutex would deadlock.
		if err := service.ReleaseView(ctx, actor, strings.Repeat("a", 64)); err != nil {
			t.Fatal(err)
		}
		values, err := projectPreparedItem(t, item)
		returned = append(returned, values)
		return values, err
	}
	var err error
	service, err = view.NewService(f.seq, f.auth, f.auth, f.clock, projector)
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.OpenView(context.Background(), f.actor, view.All(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || first.NextPageToken == "" {
		t.Fatal("projection did not prepare the complete capture once")
	}
	for _, values := range returned {
		values[0] = apiresources.Capability{}
	}
	f.publish(t, 0, true)
	f.publish(t, 1, true)
	tail, err := service.ReadPageSize(context.Background(), f.actor, first.SnapshotToken, first.NextPageToken, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range []view.Page{first, tail} {
		if _, err := view.EncodePage(page); err != nil {
			t.Fatal(err)
		}
		values, prepared := page.Items[0].ProjectedCapabilities()
		if !prepared || len(values) != 1 {
			t.Fatal("prepared image missing")
		}
		power := values[0].Resource().GetPower()
		if power == nil || power.Observation == nil || power.Observation.Value == nil || *power.Observation.Value != api.PowerValue_POWER_VALUE_OFF {
			t.Fatal("projection refreshed an immutable page")
		}
		values[0] = apiresources.Capability{}
		again, prepared := page.Items[0].ProjectedCapabilities()
		if !prepared || again[0].Resource() == nil {
			t.Fatal("getter exposed the retained slice")
		}
	}
	if calls != 2 {
		t.Fatal("render or pagination reran the projector")
	}
}

func TestCapabilityProjectionIsDiscardedWhenCaptureFencesChange(t *testing.T) {
	for _, kind := range []string{"grant", "configuration", "catalogue"} {
		t.Run(kind, func(t *testing.T) {
			f := newWatchFixture(t, 1)
			f.publish(t, 0, false)
			projector := func(ctx context.Context, _ view.ActorID, item view.Item, _ view.Cursor) ([]apiresources.Capability, error) {
				values, err := projectPreparedItem(t, item)
				if err != nil {
					return nil, err
				}
				switch kind {
				case "grant":
					f.auth.SetGrant(testGrant(t, f.actor, 2, nil))
				case "configuration":
					f.auth.SetConfig(testConfig(t, 2, "changed-during-projection"))
				case "catalogue":
					record := item.Registry()
					device, entity, _ := record.IDs()
					instance, key, _ := record.Source()
					original, _, aliases, _ := record.Aliases()
					alias, err := domain.ParseHAAlias("switch.capture_changed")
					if err != nil {
						t.Fatal(err)
					}
					changed, err := integrations.NewRegistryRecord(device, entity, instance, key, original, alias, append(aliases, alias), false)
					if err != nil {
						t.Fatal(err)
					}
					if err := f.seq.PublishRegistry(ctx, []integrations.RegistryRecord{changed}); err != nil {
						t.Fatal(err)
					}
				}
				return values, nil
			}
			service, err := view.NewService(f.seq, f.auth, f.auth, f.clock, projector)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.OpenView(context.Background(), f.actor, view.All(), 1); !errors.Is(err, view.ErrViewChanged) {
				t.Fatal("changed capture was published", err)
			}
		})
	}
}

func TestCapabilityProjectionCannotAttachForeignEntityOrExceedCaptureBudget(t *testing.T) {
	t.Run("entity", func(t *testing.T) {
		f := newWatchFixture(t, 2)
		foreign, err := apitypes.ParseHomeName("homes/synthetic-capture")
		if err != nil {
			t.Fatal(err)
		}
		projector := func(_ context.Context, _ view.ActorID, item view.Item, _ view.Cursor) ([]apiresources.Capability, error) {
			_, id, _ := item.Registry().IDs()
			other := f.ids[0]
			if id == other {
				other = f.ids[1]
			}
			return apiresources.ProjectCapabilities(foreign, other, item.Capabilities(), nil)
		}
		service, err := view.NewService(f.seq, f.auth, f.auth, f.clock, projector)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.OpenView(context.Background(), f.actor, view.All(), 1); !errors.Is(err, view.ErrInvalidInput) {
			t.Fatal("foreign entity attached", err)
		}
	})
	t.Run("budget", func(t *testing.T) {
		f := newWatchFixture(t, 1)
		// A small, admitted metadata-only native extension. The trusted projector
		// below simulates an oversized owner-cache value; it must not be retained.
		encoded, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{protodesc.ToFileDescriptorProto(timestamppb.File_google_protobuf_timestamp_proto)}})
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(encoded)
		description := &registry.CapabilityDescription{SchemaId: "example.capture.v1", Major: 1, Endpoint: "metadata", DisplayName: "Synthetic extension", DescriptorSet: encoded, DescriptorSha256: digest[:], Constraints: &registry.CapabilityDescription_Extension{Extension: &registry.ExtensionConstraints{}}}
		_, key, err := f.bindings[0].Source()
		if err != nil {
			t.Fatal(err)
		}
		candidate, err := integrations.NewCandidate(key, []*registry.CapabilityDescription{description})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.session.Discover(context.Background(), []integrations.Candidate{candidate}); err != nil {
			t.Fatal(err)
		}
		projector := func(_ context.Context, _ view.ActorID, item view.Item, _ view.Cursor) ([]apiresources.Capability, error) {
			descriptions := item.Capabilities()
			descriptions[0].DisplayName = strings.Repeat("x", view.MaxSnapshotBytes/4+1)
			home, _ := apitypes.ParseHomeName("homes/synthetic-capture")
			_, entity, _ := item.Registry().IDs()
			return apiresources.ProjectCapabilities(home, entity, descriptions, nil)
		}
		service, err := view.NewService(f.seq, f.auth, f.auth, f.clock, projector)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.OpenView(context.Background(), f.actor, view.All(), 1); !errors.Is(err, view.ErrResourceExhausted) {
			t.Fatal("oversized projection escaped the existing capture budget", err)
		}
	})
}

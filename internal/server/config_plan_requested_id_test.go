package server

import (
	"fmt"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"testing"
	"time"
)

func TestNHA204RequestedPlanIDIsHonoredAndCollisionsAreAtomic(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	catalog, err := newConfigurationPlanCatalog(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	home, _ := apitypes.ParseHomeName("homes/synthetic")
	id, err := storage.ParseAPIRequestedResourceID("chosen-plan")
	if err != nil {
		t.Fatal(err)
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	first, err := catalog.prepareAddLocked(home, nha203PlanBinding(t, 1), nha203PlanMetadata(), now.Add(time.Minute), &id)
	if err != nil {
		t.Fatal(err)
	}
	if first.name.String() != "homes/synthetic/configurationPlans/chosen-plan" {
		t.Fatal("requested plan identifier ignored")
	}
	catalog.publishLocked(first)
	_, err = catalog.prepareAddLocked(home, nha203PlanBinding(t, 1), nha203PlanMetadata(), now.Add(time.Minute), &id)
	nha203AssertPlanFault(t, err, api.ApiErrorReason_API_ERROR_REASON_RESOURCE_ALREADY_EXISTS)
	if len(catalog.records) != 1 || len(catalog.pending) != 0 {
		t.Fatal("collision changed catalogue")
	}
}

func TestNHA204RequestedPlanIDCollisionPreservesCredentialBoundary(t *testing.T) {
	for _, pending := range []bool{false, true} {
		for _, foreign := range []bool{false, true} {
			t.Run(fmt.Sprintf("pending=%t/foreign=%t", pending, foreign), func(t *testing.T) {
				now := time.Unix(1700000000, 0).UTC()
				catalog, err := newConfigurationPlanCatalog(func() time.Time { return now })
				if err != nil {
					t.Fatal(err)
				}
				home, _ := apitypes.ParseHomeName("homes/synthetic")
				id, err := storage.ParseAPIRequestedResourceID("chosen-plan")
				if err != nil {
					t.Fatal(err)
				}
				owner := nha203PlanBinding(t, 1)
				catalog.mu.Lock()
				defer catalog.mu.Unlock()
				first, err := catalog.prepareAddLocked(home, owner, nha203PlanMetadata(), now.Add(time.Minute), &id)
				if err != nil {
					t.Fatal(err)
				}
				if pending {
					catalog.pending[first.name.String()] = first
				} else {
					catalog.publishLocked(first)
				}
				caller := owner
				reason := api.ApiErrorReason_API_ERROR_REASON_RESOURCE_ALREADY_EXISTS
				if foreign {
					caller = nha203PlanBinding(t, 2)
					reason = api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED
				}
				revision := catalog.revision
				_, err = catalog.prepareAddLocked(home, caller, nha203PlanMetadata(), now.Add(time.Minute), &id)
				nha203AssertPlanFault(t, err, reason)
				if len(catalog.records)+len(catalog.pending) != 1 || catalog.revision != revision {
					t.Fatal("collision mutated catalogue")
				}
				existing := catalog.records[first.name.String()]
				if pending {
					existing = catalog.pending[first.name.String()]
				}
				if existing.binding != owner || !proto.Equal(existing.metadata, first.metadata) {
					t.Fatal("collision changed owner or result")
				}
			})
		}
	}
}

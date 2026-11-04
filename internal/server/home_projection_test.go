package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/view"
)

// Read-only native Host fixture. Metadata updates deliberately have no owner in
// this fixture; the actual SQL update owner is covered in home_metadata_update.
type nha201ProjectionOwner struct {
	home   apitypes.HomeMetadata
	record integrations.RegistryRecord
}

func (o nha201ProjectionOwner) HomeName(context.Context) (apitypes.HomeName, error) {
	return o.home.Name(), nil
}
func (o nha201ProjectionOwner) HomeMetadata(context.Context) (apitypes.HomeMetadata, error) {
	return o.home, nil
}
func (o nha201ProjectionOwner) ResolveEntityName(_ context.Context, name apitypes.EntityName) (domain.EntityID, error) {
	_, id, _ := o.record.IDs()
	expected, _ := apitypes.EntityNameForIdentity(o.home.Name(), id)
	if name != expected {
		return domain.EntityID{}, registry.ErrUnconfigured
	}
	return id, nil
}
func (o nha201ProjectionOwner) UpdateEntityMetadata(context.Context, authorization.MetadataEvidence, domain.EntityID, apitypes.EntityUpdate, func(apitypes.EntityMetadata) ([]byte, error)) (apitypes.EntityMetadata, error) {
	return apitypes.EntityMetadata{}, errors.New("read-only synthetic fixture")
}

func TestNHA201HomeProjectionKeepsHardwareSupportSeparateFromCurrentGrant(t *testing.T) {
	f := nha165GrantSetup(t)
	// Reuse the native read fixture identity, advertising an actionable source.
	// Start only publishes synthetic discovery/state; it dispatches no action.
	driver, err := fake.NewActionDriver(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(f.ctx, f.session); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(f.ctx, actorContextKey{}, f.actor)
	name, _ := apitypes.ParseHomeName("homes/synthetic-projection")
	metadata, err := apitypes.NewHomeMetadata(name, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	owner := nha201ProjectionOwner{home: metadata, record: f.record}
	core := &coreRPCHandler{epoch: f.a.source}
	stamp, _ := view.NewConfigStamp(1, [32]byte{1})
	views, err := view.NewService(f.a.source, f.a, nha050ViewConfig{stamp}, sequencer.RealClock{}, core.capabilityProjector(owner, f.a.authority))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newHomeResourceHandler(owner, f.a.authority, views)
	if err != nil {
		t.Fatal(err)
	}
	_, entity, _ := f.record.IDs()
	entityName, _ := apitypes.EntityNameForIdentity(name, entity)
	response, err := handler.ListCapabilities(ctx, connect.NewRequest(&api.ListCapabilitiesRequest{Parent: entityName.String(), PageSize: 1}))
	if err != nil || len(response.Msg.Capabilities) != 1 {
		t.Fatal("native capability projection missing", err)
	}
	capability := response.Msg.Capabilities[0]
	if capability.GetPower() == nil || !capability.GetPower().SupportsSet || capability.ActionAccess != api.ActionAccess_ACTION_ACCESS_DENIED || capability.Etag == "" {
		t.Fatal("hardware support became an execution grant", capability)
	}
	read, err := handler.GetCapability(ctx, connect.NewRequest(&api.GetCapabilityRequest{Name: capability.Name}))
	if err != nil || read.Msg.Etag != capability.Etag {
		t.Fatal("same definition changed across Get and List", err)
	}
	// Single-page lists must release captures rather than exhaust all sixteen
	// slots after a few ordinary reads. Traits are selected before pagination.
	for i := 0; i < view.MaxSnapshots+2; i++ {
		page, err := handler.ListEntities(ctx, connect.NewRequest(&api.ListEntitiesRequest{Parent: name.String(), PageSize: 1, Filter: "trait(power)"}))
		if err != nil || len(page.Msg.Entities) != 1 || page.Msg.NextPageToken != "" {
			t.Fatal("single-page capture leaked or filter missed native trait", i, err)
		}
	}
	grant, err := authorization.AdmitPolicySource(f.account, []byte("schema: homeward.authorization/1\naccount_id: nha165-cache-account\ngrants:\n  - scope: {family: state_registry, operation: read}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n  - scope: {family: actions, operation: write}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.a.authority.Publish(nha164AuthorityState(t, grant, auth.AccountRole_ACCOUNT_ROLE_USER, 2)); err != nil {
		t.Fatal(err)
	}
	allowed, err := handler.GetCapability(ctx, connect.NewRequest(&api.GetCapabilityRequest{Name: capability.Name}))
	if err != nil || allowed.Msg.ActionAccess != api.ActionAccess_ACTION_ACCESS_ALLOWED || allowed.Msg.Etag == capability.Etag {
		t.Fatal("new grant did not change its definition binding", err)
	}
	if err := f.a.authority.Publish(nha164AuthorityState(t, f.source, auth.AccountRole_ACCOUNT_ROLE_USER, 3)); err != nil {
		t.Fatal(err)
	}
	denied, err := handler.GetCapability(ctx, connect.NewRequest(&api.GetCapabilityRequest{Name: capability.Name}))
	if err != nil || denied.Msg.ActionAccess != api.ActionAccess_ACTION_ACCESS_DENIED {
		t.Fatal("revoked write grant retained permission", err)
	}
}

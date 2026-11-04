package server

import (
	"context"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/storage"
	"net/http"
	"testing"
)

func nha203ObservationHome(t *testing.T, ctx context.Context, client *http.Client, base, token string) string {
	t.Helper()
	result, err := apiv2connect.NewHomeServiceClient(client, base).ListHomes(ctx, nha056SessionRequest(&api.ListHomesRequest{PageSize: 2}, token))
	if err != nil || len(result.Msg.Homes) != 1 {
		t.Fatal("fixture must discover its actual native home", err)
	}
	return result.Msg.Homes[0].Name
}
func nha203ObservationInstance(t *testing.T, ctx context.Context, store *storage.Store, home, key string) string {
	t.Helper()
	id, err := store.LookupAPIResourceIdentity(ctx, storage.APIIntegrationInstances, key)
	if err != nil {
		t.Fatal(err)
	}
	return home + "/integrationInstances/" + id.ResourceID()
}
func nha203ObservationRun(t *testing.T, controller string, run uint64) string {
	t.Helper()
	name, err := apitypes.ParseRuntimeControllerName(controller)
	if err != nil {
		t.Fatal(err)
	}
	value, err := apitypes.RuntimeRunNameForIdentity(name, run)
	if err != nil {
		t.Fatal(err)
	}
	return value.String()
}

func nha203ObservationAccount(t *testing.T, ctx context.Context, store *storage.Store, home, key string) string {
	t.Helper()
	id, err := store.LookupAPIResourceIdentity(ctx, storage.APIAccounts, key)
	if err != nil {
		t.Fatal(err)
	}
	return home + "/accounts/" + id.ResourceID()
}

func nha203ObservationInstanceFromAPI(t *testing.T, ctx context.Context, client *http.Client, base, token, home, key string) string {
	t.Helper()
	result, err := apiv2connect.NewIntegrationServiceClient(client, base).ListIntegrationInstances(ctx, nha056SessionRequest(&api.ListIntegrationInstancesRequest{Parent: home, PageSize: 100}, token))
	if err != nil {
		t.Fatal(err)
	}
	for _, instance := range result.Msg.IntegrationInstances {
		if instance.InstanceKey == key {
			return instance.Name
		}
	}
	t.Fatal("configured synthetic instance absent from native resource projection")
	return ""
}

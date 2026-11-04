package server

import (
	"connectrpc.com/connect"
	"context"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/storage"
	"strings"
	"testing"
)

func nha203DocumentName(t *testing.T, ctx context.Context, run *nha162Running, nativeKey string) string {
	t.Helper()
	home, ok := any(run.observed.lists.registry).(administrationHomeIdentity)
	if !ok {
		t.Fatal("registry home provider unavailable")
	}
	names := administrationNames{home: home, store: run.observed.sessions.store}
	value, err := names.name(ctx, storage.APIConfigurationDocuments, nativeKey)
	if err != nil {
		t.Fatal("durable document name", err)
	}
	return value
}
func nha203HomeName(t *testing.T, ctx context.Context, run *nha162Running) string {
	t.Helper()
	home, ok := any(run.observed.lists.registry).(administrationHomeIdentity)
	if !ok {
		t.Fatal("registry home provider unavailable")
	}
	value, err := home.HomeName(ctx)
	if err != nil {
		t.Fatal("durable home name", err)
	}
	return value.String()
}

// Native fixture setup discovers the current home and document catalogue through
// the same protected public handlers; the subsequent tested caller gets no grant.
func nha203PublicDocumentName(t *testing.T, ctx context.Context, httpClient connect.HTTPClient, origin, token, nativeKey string) string {
	t.Helper()
	parent := nha203PublicHomeName(t, ctx, httpClient, origin, token)
	service := apiv2connect.NewConfigurationServiceClient(httpClient, origin)
	cursor := ""
	for page := 0; page < 32; page++ {
		response, err := service.ListConfigurationDocuments(ctx, nha056SessionRequest(&api.ListConfigurationDocumentsRequest{Parent: parent, PageSize: 100, PageToken: cursor}, token))
		if err != nil {
			t.Fatal("public document discovery", err)
		}
		for _, document := range response.Msg.Documents {
			if document.Id.Value == nativeKey {
				return document.Name
			}
		}
		cursor = response.Msg.NextPageToken
		if cursor == "" {
			break
		}
	}
	t.Fatal("fixture document not found")
	return ""
}

func nha203DocumentParent(name string) string {
	parent, _, _ := strings.Cut(name, "/configurationDocuments/")
	return parent
}

func nha203PublicHomeName(t *testing.T, ctx context.Context, httpClient connect.HTTPClient, origin, token string) string {
	t.Helper()
	if httpClient == nil || origin == "" {
		t.Fatal("fixture omitted current public connection")
	}
	response, err := apiv2connect.NewHomeServiceClient(httpClient, origin).ListHomes(ctx, nha056SessionRequest(&api.ListHomesRequest{PageSize: 1}, token))
	if err != nil || response == nil || response.Msg == nil || len(response.Msg.Homes) != 1 || response.Msg.NextPageToken != "" {
		t.Fatal("protected public home discovery", err)
	}
	return response.Msg.Homes[0].Name
}

package server

import (
	"context"
	"testing"

	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/storage"
)

// Only isolated direct-handler fixtures use this explicit synthetic home.
// Run/TLS fixtures obtain the home/account names from their admitted session.
type nha203AuthFixtureHome struct{}

func (nha203AuthFixtureHome) HomeName(context.Context) (apitypes.HomeName, error) {
	return apitypes.ParseHomeName("homes/auth-fixture")
}
func nha203AuthFixtureNames(store *storage.Store) administrationNames {
	return administrationNames{home: nha203AuthFixtureHome{}, store: store}
}
func nha203SessionAccount(t *testing.T, session *api.Session) apitypes.AccountName {
	t.Helper()
	if session == nil {
		t.Fatal("missing session")
	}
	name, err := apitypes.ParseSessionName(session.Name)
	if err != nil {
		t.Fatal(err)
	}
	return name.Account()
}

// Assertions about native persistence use the existing private storage port.
// No reverse encoding, public lookup RPC, or native ID enters a public request.
func nha203NativeAccount(t *testing.T, ctx context.Context, store *storage.Store, name string) contracts.AccountID {
	t.Helper()
	parsed, err := apitypes.ParseAccountName(name)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := store.ResolveAPIResourceIdentity(ctx, storage.APIAccounts, parsed.ResourceID())
	if err != nil {
		t.Fatal(err)
	}
	id, err := contracts.ParseAccountID(identity.NativeKey())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func nha203AuthHandler(store *storage.Store, coordinator *sessionCoordinator, provider factorProvider) (*authResourceHandler, error) {
	native, err := newAuthHandler(store, coordinator, provider)
	if err != nil {
		return nil, err
	}
	pages, err := newAPIPageCipher()
	if err != nil {
		return nil, err
	}
	return &authResourceHandler{authHandler: native, names: authResourceNames{nha203AuthFixtureNames(store)}, pages: pages}, nil
}
func nha203AccountHandler(store *storage.Store, coordinator *sessionCoordinator) *accountResourceHandler {
	return &accountResourceHandler{accountHandler: &accountHandler{store: store, coordinator: coordinator}, names: authResourceNames{nha203AuthFixtureNames(store)}}
}

func nha203NativeSession(t *testing.T, ctx context.Context, store *storage.Store, name string) contracts.SessionID {
	t.Helper()
	parsed, err := apitypes.ParseSessionName(name)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := store.ResolveAPIResourceIdentity(ctx, storage.APISessions, parsed.ResourceID())
	if err != nil {
		t.Fatal(err)
	}
	id, err := contracts.ParseSessionID(identity.NativeKey())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func nha203AccountText(t *testing.T, account contracts.AccountID) string {
	t.Helper()
	value, err := account.Value()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func nha203SessionText(t *testing.T, session contracts.SessionID) string {
	t.Helper()
	value, err := session.Value()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

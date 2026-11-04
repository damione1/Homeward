package storage

import (
	"errors"
	"strings"
	"testing"
	"time"

	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
)

func TestNHA204AccountPublicIDAdmission(t *testing.T) {
	for _, value := range []string{"a", "chosen-account", "a" + strings.Repeat("0", 62)} {
		if _, err := ParseAPIRequestedResourceID(value); err != nil {
			t.Fatalf("valid public ID rejected: %q: %v", value, err)
		}
	}
	for _, value := range []string{"", "1account", "Account", "-account", "account-", "a/b", "a_b", "é", strings.Repeat("a", 64)} {
		if _, err := ParseAPIRequestedResourceID(value); !errors.Is(err, ErrAPIResourceIdentity) {
			t.Fatalf("invalid public ID admitted: %q: %v", value, err)
		}
	}
}

// The public choice is committed with the native account, and force never
// bypasses native CAS, authorization or the last-administrator safeguard.
func TestNHA204AccountPublicIDAtomicAndForceChildren(t *testing.T) {
	s, ctx := nha151Store(t)
	admin := nha049AccountInput(t, "AIP Administrator", auth.AccountRole_ACCOUNT_ROLE_ADMIN)
	if _, err := s.BootstrapAdmin(ctx, admin); err != nil {
		t.Fatal(err)
	}
	chosen, err := ParseAPIRequestedResourceID("chosen-account")
	if err != nil {
		t.Fatal(err)
	}
	user := nha049AccountInput(t, "AIP User", auth.AccountRole_ACCOUNT_ROLE_USER)
	user.PublicID = &chosen
	created, err := s.CreateAccount(ctx, admin.ID, user)
	if err != nil {
		t.Fatal(err)
	}
	nativeID, _ := user.ID.Value()
	identity, err := s.LookupAPIResourceIdentity(ctx, APIAccounts, nativeID)
	if err != nil || identity.ResourceID() != "chosen-account" || identity.NativeKey() != nativeID {
		t.Fatal("public choice replaced or ignored native identity", err)
	}
	other := nha049AccountInput(t, "AIP Other User", auth.AccountRole_ACCOUNT_ROLE_USER)
	other.PublicID = &chosen
	if _, err := s.CreateAccount(ctx, user.ID, other); !errors.Is(err, ErrAccountPermission) {
		t.Fatal("public-ID existence diagnosed before permission", err)
	}
	if _, err := s.CreateAccount(ctx, admin.ID, other); !errors.Is(err, ErrAccountConflict) {
		t.Fatal("duplicate public ID did not conflict", err)
	}
	if _, err := s.GetAccount(ctx, admin.ID, other.ID); !errors.Is(err, ErrAccountNotFound) {
		t.Fatal("public-ID collision retained native account", err)
	}
	// The collision must roll back the login claim as well as the native row.
	other.PublicID = nil
	if _, err := s.CreateAccount(ctx, admin.ID, other); err != nil {
		t.Fatal("collision leaked a login claim", err)
	}
	version, _ := created.Version()
	revision, _ := created.Revision()
	id, _ := accounts.NewSessionID()
	access, refresh := nha141Digests(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := s.CreateSession(ctx, SessionCreation{ID: id, Account: user.ID, Revision: revision, AccessDigest: access, RefreshDigest: refresh, Now: now}); err != nil {
		t.Fatal(err)
	}
	owner := SessionOwner{Account: user.ID, Session: id}
	if _, err := s.GetSession(ctx, user.ID, owner, now); err != nil {
		t.Fatal("direct session read rejected its owner", err)
	}
	if _, err := s.GetSession(ctx, other.ID, owner, now); !errors.Is(err, ErrSessionPermission) {
		t.Fatal("direct session read escaped owner admission", err)
	}
	if err := s.DeleteAccountIfCurrent(ctx, admin.ID, user.ID, version, false); !errors.Is(err, ErrAccountHasChildren) {
		t.Fatal("default delete cascaded child session", err)
	}
	if _, err := s.GetSession(ctx, user.ID, owner, now); err != nil {
		t.Fatal("rejected delete changed child session", err)
	}
	if err := s.DeleteAccountIfCurrent(ctx, admin.ID, user.ID, version, true); err != nil {
		t.Fatal("explicit cascade failed", err)
	}
	if _, err := s.GetSession(ctx, admin.ID, owner, now); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal("explicit cascade retained session", err)
	}
	adminRecord, err := s.GetAccount(ctx, admin.ID, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	adminVersion, _ := adminRecord.Version()
	if err := s.DeleteAccountIfCurrent(ctx, admin.ID, admin.ID, adminVersion, true); !errors.Is(err, ErrLastActiveAdmin) {
		t.Fatal("force bypassed last administrator", err)
	}
}

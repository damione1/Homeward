package storage

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
)

func TestNHA203FactorIdentityPreservesOpaqueScopedNativeIDs(t *testing.T) {
	seen := map[string]bool{}
	for _, pair := range [][2]string{{"a", "bc"}, {"ab", "c"}, {"é:/x", "clé:非-hex"}, {strings.Repeat("x", 128), strings.Repeat("y", 128)}} {
		a, _ := contracts.ParseAccountID(pair[0])
		f, _ := contracts.ParseFactorCredentialID(pair[1])
		key, err := FactorAPIIdentityKey(a, f)
		if err != nil || seen[key] {
			t.Fatal("identity collision/rejection", err)
		}
		seen[key] = true
		actualAccount, actualFactor, err := ParseFactorAPIIdentityKey(key)
		if err != nil || actualAccount != a || actualFactor != f {
			t.Fatal("opaque identity changed", err)
		}
	}
	for _, key := range []string{"", "0:af", "01:af", "-1:af", "2:af", "1:éf", "129:" + strings.Repeat("a", 130), "1:a\x00"} {
		if _, _, err := ParseFactorAPIIdentityKey(key); !errors.Is(err, ErrAPIResourceIntegrity) {
			t.Errorf("malformed stored pair admitted: %q, %v", key, err)
		}
	}
}

func TestNHA203AccountVersionCASNoOpAndNamesStayNative(t *testing.T) {
	s, ctx := nha151Store(t)
	admin := nha049AccountInput(t, "Version Admin", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	initial, err := s.BootstrapAdmin(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	version, err := initial.Version()
	if err != nil {
		t.Fatal(err)
	}
	etag, err := version.Etag()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseAccountVersion(etag)
	if err != nil || parsed != version {
		t.Fatal("etag did not retain native CAS", err)
	}
	id, _ := admin.ID.Value()
	identity, err := s.LookupAPIResourceIdentity(ctx, APIAccounts, id)
	if err != nil {
		t.Fatal("bootstrap did not allocate API name atomically", err)
	}
	noOp, err := s.UpdateAccountWithOutcome(ctx, admin.ID, admin.ID, AccountChanges{Expected: &version})
	if err != nil {
		t.Fatal(err)
	}
	unchanged, rightsChanged, err := noOp.Result()
	unchangedVersion, versionErr := unchanged.Version()
	if err != nil || versionErr != nil || rightsChanged || unchangedVersion != version {
		t.Fatal("no-op edited metadata or authority", err, versionErr)
	}
	display, _ := accounts.ParseDisplayName("Changed display")
	changed, err := s.UpdateAccountWithOutcome(ctx, admin.ID, admin.ID, AccountChanges{Display: &display, Expected: &version})
	if err != nil {
		t.Fatal(err)
	}
	current, rightsChanged, err := changed.Result()
	currentVersion, versionErr := current.Version()
	if err != nil || versionErr != nil || rightsChanged || currentVersion == version {
		t.Fatal("display edit failed CAS separation", err, versionErr)
	}
	if _, err := s.UpdateAccountWithOutcome(ctx, admin.ID, admin.ID, AccountChanges{Expected: &version}); !errors.Is(err, ErrAccountVersionConflict) {
		t.Fatal("stale no-op bypassed CAS", err)
	}
	if err := s.DeleteAccountIfCurrent(ctx, admin.ID, admin.ID, version); !errors.Is(err, ErrAccountVersionConflict) {
		t.Fatal("stale delete bypassed CAS", err)
	}
	if err := s.DeleteAccountIfCurrent(ctx, admin.ID, admin.ID, currentVersion); !errors.Is(err, ErrLastActiveAdmin) {
		t.Fatal("CAS bypassed last-admin invariant", err)
	}
	after, err := s.LookupAPIResourceIdentity(ctx, APIAccounts, id)
	if err != nil || after != identity {
		t.Fatal("native metadata edit changed API identity", err)
	}
	user := nha049AccountInput(t, "Version User", authv1.AccountRole_ACCOUNT_ROLE_USER)
	if _, err := s.CreateAccount(ctx, admin.ID, user); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateAccountWithOutcome(ctx, user.ID, admin.ID, AccountChanges{Expected: &version}); !errors.Is(err, ErrAccountPermission) {
		t.Fatal("CAS diagnosis preceded permission", err)
	}
}

// A lost response may replay only the same native refresh+renewal key. API names
// stay attached to the original native session, including unrecorded MFA data.
func TestNHA203SavedRenewalKeepsNamesAndUnrecordedAuthentication(t *testing.T) {
	s, ctx := nha151Store(t)
	input := nha049AccountInput(t, "Renewal Name Administrator", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	account, err := s.BootstrapAdmin(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	revision, _ := account.Revision()
	id, _ := accounts.NewSessionID()
	access, _ := nha141Digests(t)
	secret, err := accounts.NewRefreshSecret()
	if err != nil {
		t.Fatal(err)
	}
	oldDigest, _ := secret.Digest()
	key, err := accounts.ParseRenewalKey([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	successor, err := secret.Successor(key)
	if err != nil {
		t.Fatal(err)
	}
	nextDigest, _ := successor.Digest()
	now := time.Now().UTC().Truncate(time.Microsecond)
	created, err := s.CreateSession(ctx, SessionCreation{ID: id, Account: input.ID, Revision: revision,
		AccessDigest: access, RefreshDigest: oldDigest, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	nativeID, _ := id.Value()
	identity, err := s.LookupAPIResourceIdentity(ctx, APISessions, nativeID)
	if err != nil {
		t.Fatal("session identity missing from creation transaction", err)
	}
	owner := SessionOwner{Account: input.ID, Session: id}
	check := func(record SessionRecord) {
		t.Helper()
		actualID, err := record.ID()
		satisfaction, authenticated, evidenceErr := record.Authentication()
		if err != nil || evidenceErr != nil || actualID != id || satisfaction != authv1.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_UNSPECIFIED || !authenticated.IsZero() {
			t.Fatal("refresh invented identity/authentication evidence", err, evidenceErr)
		}
		current, err := s.LookupAPIResourceIdentity(ctx, APISessions, nativeID)
		if err != nil || current != identity {
			t.Fatal("refresh replaced resource identity", err)
		}
	}
	check(created.Session)
	nextAccess, _ := nha141Digests(t)
	rotation := RefreshRotation{Owner: owner, OldDigest: oldDigest, NewDigest: nextDigest, NewAccessDigest: nextAccess, Now: now.Add(6 * 24 * time.Hour), Recoverable: true}
	first, err := s.RotateRefresh(ctx, rotation)
	record, _, ok := first.Rotated()
	if err != nil || !ok {
		t.Fatal("initial saved renewal failed", err)
	}
	check(record)
	// Recover the same operation with the exact durable 32-byte key after the
	// previous local deadline. No second identity or evidence may be allocated.
	repeated, _ := secret.Successor(key)
	repeatedDigest, _ := repeated.Digest()
	if repeatedDigest != nextDigest {
		t.Fatal("same saved key changed successor")
	}
	rotation.NewDigest = repeatedDigest
	rotation.NewAccessDigest, _ = nha141Digests(t)
	rotation.Now = now.Add(12 * 24 * time.Hour)
	recovered, err := s.RotateRefresh(ctx, rotation)
	record, _, ok = recovered.Rotated()
	if err != nil || !ok {
		t.Fatal("saved renewal not recoverable", err)
	}
	check(record)
	differentKey, _ := accounts.ParseRenewalKey([]byte("abcdef0123456789abcdef0123456789"))
	different, _ := secret.Successor(differentKey)
	rotation.NewDigest, _ = different.Digest()
	rotation.NewAccessDigest, _ = nha141Digests(t)
	rotation.Now = rotation.Now.Add(time.Minute)
	replay, err := s.RotateRefresh(ctx, rotation)
	if err != nil || !replay.ReplayRevoked() {
		t.Fatal("different renewal key did not revoke replay", err)
	}
}

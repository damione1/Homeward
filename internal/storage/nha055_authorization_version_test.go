package storage

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
)

func TestNHA055ExactPolicyVersionReadsIndependent050HistoryAndRejectsForeignHashOrPurge(t *testing.T) {
	s, ctx := nha151Store(t)
	admin := nha049AccountInput(t, "NHA055 documentary admin", auth.AccountRole_ACCOUNT_ROLE_ADMIN)
	if _, err := s.BootstrapAdmin(ctx, admin); err != nil {
		t.Fatal(err)
	}
	user := nha049AccountInput(t, "NHA055 documentary user", auth.AccountRole_ACCOUNT_ROLE_USER)
	if _, err := s.CreateAccount(ctx, admin.ID, user); err != nil {
		t.Fatal(err)
	}
	current, err := s.AuthorizationPolicy(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	p1 := nha050Source(t, user.ID, true, "P1 direct050 without029")
	current, _ = nha050Replace(t, s, ctx, admin.ID, user.ID, p1, nha050Expected(t, current))
	p1Version, err := current.Version()
	if err != nil {
		t.Fatal(err)
	}
	current, _ = nha050Replace(t, s, ctx, admin.ID, user.ID, nha050Source(t, user.ID, false, "P2 direct050"), nha050Expected(t, current))
	var deltas int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM public.config_activation_policy_changes`).Scan(&deltas); err != nil || deltas != 0 {
		t.Fatal("fixture accidentally created an029 policy mapping", err)
	}
	read, err := s.ReadAuthorizationPolicyVersion(ctx, user.ID, p1Version)
	if err != nil {
		t.Fatal("retained policy outside every029 activation was unavailable", err)
	}
	exact, _ := read.Bytes()
	want, _ := p1.Bytes()
	if !bytes.Equal(exact, want) {
		t.Fatal("exact documentary selector substituted latest policy bytes")
	}
	if _, err := s.ReadAuthorizationPolicyVersion(ctx, admin.ID, p1Version); err == nil {
		t.Fatal("document version from another account crossed canonical binding")
	}
	revision, hash, _ := p1Version.Values()
	hash[0] ^= 1
	wrongHash, _ := authorization.NewVersion(revision, hash)
	if _, err := s.ReadAuthorizationPolicyVersion(ctx, user.ID, wrongHash); err == nil {
		t.Fatal("same revision with mismatched requested SHA was admitted")
	} else {
		var failure *Error
		if !errors.As(err, &failure) || failure.Kind() != ConfigConflict {
			t.Fatalf("hash mismatch lacks structured conflict: %v", err)
		}
	}
	for i := 0; i < 21; i++ {
		current, _ = nha050Replace(t, s, ctx, admin.ID, user.ID, nha050Source(t, user.ID, true, fmt.Sprintf("retention-%d", i)), nha050Expected(t, current))
	}
	if _, err := s.ReadAuthorizationPolicyVersion(ctx, user.ID, p1Version); err == nil {
		t.Fatal("purged policy was replaced by an arbitrary retained version")
	} else {
		var failure *Error
		if !errors.As(err, &failure) || failure.Kind() != ConfigNotFound {
			t.Fatalf("purged version lacks structured not-found refusal: %v", err)
		}
	}
}

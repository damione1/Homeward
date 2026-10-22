package configdocuments

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/storage"
)

func TestNHA055PolicyWriteExpiryAfterMutationWaitNeverStages(t *testing.T) {
	service, ctx, path, raw := nha055Documents(t)
	deadline := time.Now().Add(75 * time.Millisecond)
	before, expectedOperational, basis := nha055DocumentBasis(t, service, ctx, deadline)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	account, err := contracts.ParseAccountID("nha055-expiry-target")
	if err != nil {
		t.Fatal(err)
	}
	source, err := authorization.AdmitPolicySource(account, []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants: []\n", "nha055-expiry-target")))
	if err != nil {
		t.Fatal(err)
	}
	binding, err := storage.AuthorizationPolicyBinding(account)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := NewExpectedSource(binding, storage.ConfigVersionRevision{}, [32]byte{}, true, ExpectAbsentMember())
	if err != nil {
		t.Fatal(err)
	}
	author, _ := storage.NewConfigAuthor("nha055-policy-expiry")
	release, err := service.acquireMutation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := service.WritePolicy(ctx, source, authorization.ExpectAbsent(), expectedOperational, expected, author, basis)
		done <- err
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		release()
		t.Fatal(ctx.Err())
	}
	release()
	select {
	case err := <-done:
		nha149Failure(t, err, Conflict)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	nha055DocumentsUnchanged(t, service, ctx, before, path, raw, info)
}

func TestNHA055ExpectedPolicyVersionDoesNotInferAbsentOrFreshBase(t *testing.T) {
	account, _ := contracts.ParseAccountID("nha055-version-target")
	binding, err := storage.AuthorizationPolicyBinding(account)
	if err != nil {
		t.Fatal(err)
	}
	absent, err := NewExpectedSource(binding, storage.ConfigVersionRevision{}, [32]byte{}, true, ExpectAbsentMember())
	if err != nil {
		t.Fatal(err)
	}
	var hash [32]byte
	hash[0] = 1
	revision, _ := storage.NewConfigVersionRevision(4)
	existing, err := NewExpectedSource(binding, revision, hash, false, ExpectAbsentMember())
	if err != nil {
		t.Fatal(err)
	}
	version, _ := authorization.NewVersion(4, hash)
	expected, _ := authorization.ExpectVersion(version)
	if !absent.MatchesPolicyVersion(authorization.ExpectAbsent()) || absent.MatchesPolicyVersion(expected) || existing.MatchesPolicyVersion(authorization.ExpectAbsent()) || !existing.MatchesPolicyVersion(expected) || existing.MatchesPolicyVersion(authorization.ExpectedVersion{}) {
		t.Fatal("policy active/version/absence CAS was conflated")
	}
	hash[0] = 2
	version, _ = authorization.NewVersion(4, hash)
	expected, _ = authorization.ExpectVersion(version)
	if existing.MatchesPolicyVersion(expected) {
		t.Fatal("same revision with changed source hash matched")
	}
}

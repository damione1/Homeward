package configdocuments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"testing"
	"time"

	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/storage"
)

func nha055MaterializedPolicyObservationFixture(t *testing.T) (*Service, context.Context, storage.ConfigDocumentBinding, string, []byte) {
	t.Helper()
	service, ctx, operational, _ := nha055Documents(t)
	id, err := accounts.NewAccountID()
	if err != nil {
		t.Fatal(err)
	}
	login, err := accounts.ParseLoginName("nha055 observation policy")
	if err != nil {
		t.Fatal(err)
	}
	display, err := accounts.ParseDisplayName("NHA055 observation policy")
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := accounts.HashNewPassword(ctx, "NHA055 observation fixture credential")
	if err != nil {
		t.Fatal(err)
	}
	record, err := service.store.BootstrapAdmin(ctx, storage.NewAccount{ID: id, Login: login, Display: display, Role: auth.AccountRole_ACCOUNT_ROLE_ADMIN, Verifier: verifier})
	if err != nil {
		t.Fatal(err)
	}
	account, err := record.ID()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := service.store.AuthorizationPolicy(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	source, err := policy.Source()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := source.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	binding, err := storage.AuthorizationPolicyBinding(account)
	if err != nil {
		t.Fatal(err)
	}
	physical := nha055ObservedFile(t, operational, binding.ID(), binding, raw)
	return service, ctx, binding, physical, raw
}

func TestNHA055GlobalSourceDigestDetectsReadCThenReturnBBetweenPolls(t *testing.T) {
	service, ctx, binding, physical, b := nha055MaterializedPolicyObservationFixture(t)
	active, present, err := service.store.ReadActiveConfig(ctx)
	if err != nil || !present {
		t.Fatal(err)
	}
	capture := func() SourceObservationSet {
		t.Helper()
		value, err := service.CaptureSourceObservations(ctx, active)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	first := capture()
	digestB, present := first.GlobalSHA256()
	if !present {
		t.Fatal("materialized policy omitted from global digest")
	}
	info, err := os.Stat(physical)
	if err != nil {
		t.Fatal(err)
	}
	c := append(bytes.Clone(b), []byte("# C seen by an API Read between joined watcher polls\n")...)
	if err := os.WriteFile(physical, c, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(physical, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	middle := capture()
	digestC, present := middle.GlobalSHA256()
	if !present || digestC == digestB {
		t.Fatal("date-preserved C was not included in global observation digest")
	}
	metadata, err := service.store.ReadConfigDocument(ctx, binding.ID())
	if err != nil {
		t.Fatal(err)
	}
	projected, err := service.ReadSourceObservation(metadata, middle)
	if err != nil || projected.Dirty() != DirtyUnvalidated || !projected.Located() {
		t.Fatal("selected policy did not use the same globally captured cut", err)
	}
	position, err := service.journal.Position()
	if err != nil {
		t.Fatal(err)
	}
	boundC, err := position.Bind(digestC)
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.journal.Encode(boundC)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(physical, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(physical, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	last := capture()
	returnedB, present := last.GlobalSHA256()
	if !present || returnedB != digestB {
		t.Fatal("return to exact B did not restore global cut")
	}
	decoded, err := service.journal.Decode(token)
	if err != nil {
		t.Fatal(err)
	}
	now, err := service.journal.Position()
	if err != nil || !position.Equal(now) {
		t.Fatal("read-only capture invented watcher journal history", err)
	}
	if decoded.Authority() == returnedB {
		t.Fatal("C read token could resume on B with no intervening watcher journal event")
	}
}

// This is the integration oracle for ROOT's Service.Read signer adapter. It
// intentionally exercises the public149 subscription and must be selected only
// after that adapter is adopted; no substitute observer/publisher is used.
func TestNHA055PublicReadCThenReturnBRejectsResumeWithoutAWatcherPoll(t *testing.T) {
	service, ctx, _, physical, b := nha055MaterializedPolicyObservationFixture(t)
	initial, err := service.Read(ctx, operationalID())
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(physical)
	if err != nil {
		t.Fatal(err)
	}
	c := append(bytes.Clone(b), []byte("# C API read without watcher poll\n")...)
	if err := os.WriteFile(physical, c, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(physical, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	readC, err := service.Read(ctx, operationalID())
	if err != nil {
		t.Fatal(err)
	}
	if readC.ResumeToken() == initial.ResumeToken() {
		t.Fatal("operational Read did not bind all materialized source observations")
	}
	if err := os.WriteFile(physical, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(physical, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	watch, err := service.Subscribe(ctx, readC.ResumeToken())
	if watch != nil {
		watch.Close()
	}
	var resync *Resync
	if !errors.As(err, &resync) || resync.Reason() != ResyncAuthorityChanged {
		t.Fatal("C token resumed on B between watcher polls", err)
	}
}

func TestNHA055SourceReadDiagnosticIsExactAndCleanWins(t *testing.T) {
	service, ctx, binding, physical, raw := nha055MaterializedPolicyObservationFixture(t)
	active, present, err := service.store.ReadActiveConfig(ctx)
	if err != nil || !present {
		t.Fatal(err)
	}
	metadata, err := service.store.ReadConfigDocument(ctx, binding.ID())
	if err != nil {
		t.Fatal(err)
	}
	captured, err := service.CaptureSourceObservations(ctx, active)
	if err != nil {
		t.Fatal(err)
	}
	clean, err := service.ReadSourceObservation(metadata, captured)
	if err != nil || !clean.Valid() || !clean.Located() || clean.Dirty() != Clean {
		t.Fatal("known clean policy was not projected", err)
	}
	dirty := append(bytes.Clone(raw), []byte("# exact rejected candidate\n")...)
	if err := os.WriteFile(physical, dirty, 0600); err != nil {
		t.Fatal(err)
	}
	captured, err = service.CaptureSourceObservations(ctx, active)
	if err != nil {
		t.Fatal(err)
	}
	if len(captured.targets) != 1 {
		t.Fatal("unexpected policy target count")
	}
	target := captured.targets[0]
	service.recordSourceTarget(target, DirtyRejected, CandidateRejected)
	rejected, err := service.ReadSourceObservation(metadata, captured)
	if err != nil || rejected.Dirty() != DirtyRejected {
		t.Fatal("exact rejection diagnostic lost", err)
	}
	if diagnostic, present := rejected.Diagnostic(); !present || diagnostic.Document() != binding.ID() {
		t.Fatal("rejection attached to another document")
	}
	member := target.members[0]
	member.observed.observation.updated = member.observed.observation.updated.Add(time.Second)
	target.members[0] = member
	if later := service.projectSourceObservation(member, target); later.Dirty() != DirtyUnvalidated {
		t.Fatal("rejection of another observation date leaked onto current candidate")
	}
	member.observed.observation.hash = member.sql
	if later := service.projectSourceObservation(member, target); later.Dirty() != Clean {
		t.Fatal("SQL clean source inherited an old rejection")
	}
	if err := os.Remove(physical); err != nil {
		t.Fatal(err)
	}
	captured, err = service.CaptureSourceObservations(ctx, active)
	if err != nil {
		t.Fatal(err)
	}
	absent, err := service.ReadSourceObservation(metadata, captured)
	if err != nil || absent.Located() || absent.Dirty() != DirtyUnknown {
		t.Fatal("never/materialized-absent policy invented a current physical locator", err)
	}
	if _, present := captured.GlobalSHA256(); present {
		t.Fatal("absent policy without native/declarative locators invented a global digest")
	}
	if empty, present := sourceObservationGlobalDigest(nil); present || empty != ([32]byte{}) {
		t.Fatal("legacy absence was not explicit")
	}
}

func TestNHA055GlobalSourceDigestIncludesKnownMissingMembersAndIgnoresUnmaterializedPolicy(t *testing.T) {
	binding := nha055ObservedBinding(t, "nha055.global.native", "dashboard.yaml")
	group, err := storage.NewConfigDocumentID("nha055.global.group")
	if err != nil {
		t.Fatal(err)
	}
	revision, err := storage.NewConfigVersionRevision(1)
	if err != nil {
		t.Fatal(err)
	}
	target := sourceWatchTarget{variant: sourceWatchNative, group: group, members: []sourceWatchMember{{binding: binding, revision: revision, sql: sha256.Sum256([]byte("views: []\n")), observed: SourceObservation{binding: binding, absent: true}}}}
	target.observation = sourceTargetObservation(target)
	missing, present := sourceObservationGlobalDigest([]sourceWatchTarget{target})
	if !present || missing == ([32]byte{}) {
		t.Fatal("known native absence was hidden")
	}
	policy := target
	policy.variant = sourceWatchPolicy
	if _, present := sourceObservationGlobalDigest([]sourceWatchTarget{policy}); present {
		t.Fatal("absent seeded policy invented a physical source")
	}
	policy.unavailable = true
	policy.observation = sourceTargetObservation(policy)
	if _, present := sourceObservationGlobalDigest([]sourceWatchTarget{policy}); !present {
		t.Fatal("unsafe policy was confused with admitted absence")
	}
	target.members[0].observed.absent = false
	target.members[0].observed.observation = Observation{present: true, hash: sha256.Sum256(nil), updated: time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)}
	target.observation = sourceTargetObservation(target)
	empty, present := sourceObservationGlobalDigest([]sourceWatchTarget{target})
	if !present || empty == missing {
		t.Fatal("present empty candidate reused the absent global cut")
	}
}

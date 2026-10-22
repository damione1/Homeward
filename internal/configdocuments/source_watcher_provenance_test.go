package configdocuments

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/authorization"
)

func nha055PolicyProvenanceTarget(t *testing.T, revision uint64, sql, physical string) sourceWatchTarget {
	t.Helper()
	binding := nha055ObservedBinding(t, "nha055.policy.provenance", "policy.yaml")
	version, err := authorization.NewVersion(revision, sha256.Sum256([]byte(sql)))
	if err != nil {
		t.Fatal(err)
	}
	observation := SourceObservation{binding: binding, observation: Observation{present: true, hash: sha256.Sum256([]byte(physical)), updated: time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)}}
	target := sourceWatchTarget{variant: sourceWatchPolicy, group: binding.ID(), policy: version, complete: true, materialized: true, members: []sourceWatchMember{{binding: binding, sql: sha256.Sum256([]byte(sql)), observed: observation}}}
	target.observation = sourceTargetObservation(target)
	return target
}

func TestNHA055PolicyWatcherFirstDirtyCaptureAndRestartLosePhysicalIntent(t *testing.T) {
	target := nha055PolicyProvenanceTarget(t, 2, "P2 revoked", "P1 old grant")
	var provenance policyObservationProvenance
	for i := 0; i < 20; i++ {
		if provenance.observe(target) {
			t.Fatal("old physical grant armed on first/repeated dirty capture")
		}
	}
	target.members[0].observed.observation.updated = target.members[0].observed.observation.updated.Add(time.Second)
	if provenance.observe(target) {
		t.Fatal("mtime touch armed an old grant")
	}
	edited := target
	edited.members = append([]sourceWatchMember(nil), target.members...)
	edited.members[0].observed.observation.hash = sha256.Sum256([]byte("P3 explicit local edit"))
	if !provenance.observe(edited) || !provenance.observe(edited) {
		t.Fatal("later actual bytes change did not retain its intention")
	}
	var restarted policyObservationProvenance
	if restarted.observe(edited) {
		t.Fatal("restart recovered an unproven physical intention")
	}
}

func TestNHA055PolicyWatcherSQLChangeDisarmsEvenWhenPhysicalChangesInSameInterval(t *testing.T) {
	clean := nha055PolicyProvenanceTarget(t, 1, "P1 grant", "P1 grant")
	var provenance policyObservationProvenance
	if provenance.observe(clean) {
		t.Fatal("clean baseline requested activation")
	}
	changedSQL := nha055PolicyProvenanceTarget(t, 2, "P2 revoked", "P1 grant")
	for i := 0; i < 20; i++ {
		if provenance.observe(changedSQL) {
			t.Fatal("new SQL base rebased unchanged old grant")
		}
	}
	simultaneous := nha055PolicyProvenanceTarget(t, 3, "P3 revoked", "P4 locally changed in unknown order")
	if provenance.observe(simultaneous) || provenance.observe(simultaneous) {
		t.Fatal("unknown SQL/FS ordering was treated as an authorized edit")
	}
	after := nha055PolicyProvenanceTarget(t, 3, "P3 revoked", "P5 subsequent physical edit")
	if !provenance.observe(after) {
		t.Fatal("later explicit edit under unchanged SQL base did not arm")
	}
	// A catalogue/generation cut is outside policy provenance. Keeping the same
	// window must not discard the proven later edit or rebase an old conflict.
	window := sourceWatchWindow{policy: provenance, cut: sha256.Sum256([]byte("old catalogue"))}
	window.cut = sha256.Sum256([]byte("unrelated catalogue"))
	window.stable = stableObservation{}
	if !window.policy.observe(after) {
		t.Fatal("unrelated catalogue cut discarded physical provenance")
	}
}

func TestNHA055PolicyWatcherPresenceChangesCountButUnavailableNeverArms(t *testing.T) {
	target := nha055PolicyProvenanceTarget(t, 1, "current", "current")
	var provenance policyObservationProvenance
	if provenance.observe(target) {
		t.Fatal("clean initial observation armed")
	}
	target.members[0].observed.absent = true
	target.members[0].observed.observation = Observation{}
	if !provenance.observe(target) {
		t.Fatal("observed deletion was not a presence change")
	}
	target.unavailable = true
	if provenance.observe(target) || provenance.known {
		t.Fatal("unsafe read preserved old intention")
	}
	target.unavailable = false
	target.members[0].observed.absent = false
	target.members[0].observed.observation = Observation{present: true, hash: sha256.Sum256([]byte("new bytes")), updated: time.Now()}
	if provenance.observe(target) {
		t.Fatal("first observation after unavailable gap armed")
	}
	target.observation.pending = true
	if provenance.observe(target) || provenance.known {
		t.Fatal("pending reconciliation preserved old intention")
	}
	target.observation.pending = false
	if provenance.observe(target) {
		t.Fatal("post-reconciliation dirty candidate armed without a later edit")
	}
	target.policy = authorization.Version{}
	if provenance.observe(target) || provenance.known {
		t.Fatal("zero SQL version was accepted as a base")
	}
}

func TestNHA055PolicyWatcherKnownAbsentBaseAllowsOnlyALaterRealCreation(t *testing.T) {
	target := nha055PolicyProvenanceTarget(t, 2, "P2 SQL current", "new physical edit")
	target.materialized = false
	target.complete = false
	target.members[0].observed.absent = true
	target.members[0].observed.observation = Observation{}
	var provenance policyObservationProvenance
	if provenance.observe(target) || !provenance.known {
		t.Fatal("observed absence did not establish a disarmed SQL base")
	}
	target.materialized = true
	target.complete = true
	target.members[0].observed.absent = false
	target.members[0].observed.observation = Observation{present: true, hash: sha256.Sum256([]byte("new physical edit")), updated: time.Now()}
	if !provenance.observe(target) {
		t.Fatal("later creation under the observed unchanged SQL base was not recognized")
	}
	var restarted policyObservationProvenance
	if restarted.observe(target) {
		t.Fatal("first dirty capture after restart bypassed missing absence provenance")
	}
}

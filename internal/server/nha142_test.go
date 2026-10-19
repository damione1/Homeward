package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/damione1/Homeward/internal/config"
)

func nha142PolicyVersion(t *testing.T, revision uint64, label byte) authmfa.ExpectedPolicyVersion {
	t.Helper()
	var hash [32]byte
	hash[0] = label
	version, err := authmfa.NewExpectedPolicyVersion(revision, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func nha142Fence(t *testing.T) (*mfaPolicyFence, authmfa.ExpectedPolicyVersion) {
	t.Helper()
	settings, err := nha139Config(t, 5432, "").PublicTransport()
	if err != nil {
		t.Fatal(err)
	}
	version := nha142PolicyVersion(t, 1, 1)
	fence, err := newMFAPolicyFence(version, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fence.closeAdmissions)
	return fence, version
}

func nha142FenceChanging(t *testing.T, wake <-chan struct{}) {
	t.Helper()
	select {
	case <-wake:
	case <-time.After(2 * time.Second):
		t.Fatal("policy closure barrier did not publish")
	}
}

type nha142FenceChangeResult struct {
	change *mfaPolicyChange
	err    error
}
type nha142ServerNilContext struct{ context.Context }

func nha142StartFenceChange(t *testing.T, fence *mfaPolicyFence, version authmfa.ExpectedPolicyVersion, ctx context.Context) <-chan nha142FenceChangeResult {
	t.Helper()
	fence.mu.Lock()
	wake := fence.changed
	fence.mu.Unlock()
	owned, cancel := context.WithCancel(ctx)
	result := make(chan nha142FenceChangeResult, 1)
	joined := make(chan struct{})
	var outcome nha142FenceChangeResult
	go func() {
		defer close(joined)
		change, err := fence.beginReplace(owned, version)
		outcome = nha142FenceChangeResult{change, err}
		result <- outcome
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-joined:
			if outcome.change != nil {
				_ = outcome.change.abandon()
			}
		case <-time.After(2 * time.Second):
			t.Error("policy owner outlived test cleanup")
		}
	})
	nha142FenceChanging(t, wake)
	return result
}

func nha142ReceiveFenceChange(t *testing.T, result <-chan nha142FenceChangeResult) nha142FenceChangeResult {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("policy owner did not join")
	}
	return nha142FenceChangeResult{}
}

func TestNHA142PolicyFenceAdmissionAndTicketCopies(t *testing.T) {
	fence, version := nha142Fence(t)
	if _, err := newMFAPolicyFence(version, config.PublicSettings{}); authmfa.KindOf(err) != authmfa.ErrorInvalidInput {
		t.Fatal("zero transport budget admitted")
	}
	settings, _ := nha139Config(t, 5432, "").PublicTransport()
	if _, err := newMFAPolicyFence(authmfa.ExpectedPolicyVersion{}, settings); authmfa.KindOf(err) != authmfa.ErrorInvalidInput {
		t.Fatal("zero version admitted")
	}
	var absent *mfaPolicyFence
	for _, candidate := range []*mfaPolicyFence{absent, {}} {
		if _, err := candidate.tryEnter(version); authmfa.KindOf(err) != authmfa.ErrorUnavailable {
			t.Fatal("uninitialized policy admitted")
		}
		candidate.closeAdmissions()
	}
	for _, stale := range []authmfa.ExpectedPolicyVersion{nha142PolicyVersion(t, 1, 2), nha142PolicyVersion(t, 2, 1)} {
		if _, err := fence.tryEnter(stale); authmfa.KindOf(err) != authmfa.ErrorConflict {
			t.Fatal("revision/hash mismatch admitted")
		}
		if _, err := fence.beginReplace(context.Background(), stale); authmfa.KindOf(err) != authmfa.ErrorConflict {
			t.Fatal("stale writer closed fence")
		}
	}
	ticket, err := fence.tryEnter(version)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fence.tryEnter(version); authmfa.KindOf(err) != authmfa.ErrorBusy {
		t.Fatal("configured N+1 ticket admitted")
	}
	copyTicket := *ticket
	var joined sync.WaitGroup
	joined.Add(3)
	go func() { defer joined.Done(); ticket.release() }()
	go func() { defer joined.Done(); copyTicket.release() }()
	go func() { defer joined.Done(); ticket.release() }()
	joined.Wait()
	fence.mu.Lock()
	active := fence.active
	fence.mu.Unlock()
	if active != 0 {
		t.Fatalf("copies released one admission %d times", 1-active)
	}
	next, err := fence.tryEnter(version)
	if err != nil {
		t.Fatal("capacity not returned once", err)
	}
	next.release()
}

func TestNHA142PolicyFenceClosesBeforeDrainAndPublishesOnce(t *testing.T) {
	fence, version := nha142Fence(t)
	ticket, err := fence.tryEnter(version)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(ticket.release)
	result := nha142StartFenceChange(t, fence, version, ctx)
	if _, err := fence.tryEnter(version); authmfa.KindOf(err) != authmfa.ErrorBusy {
		t.Fatal("new sensitive operation crossed closure")
	}
	if _, err := fence.beginReplace(context.Background(), version); authmfa.KindOf(err) != authmfa.ErrorBusy {
		t.Fatal("second writer admitted")
	}
	select {
	case <-result:
		t.Fatal("change ignored admitted operation")
	default:
	}
	ticket.release()
	owner := nha142ReceiveFenceChange(t, result)
	if owner.err != nil || owner.change == nil {
		t.Fatal(owner.err)
	}
	t.Cleanup(func() { _ = owner.change.abandon() })
	deadline, present := owner.change.ctx.Deadline()
	if !present || time.Until(deadline) > authmfa.PolicyMutationTimeout {
		t.Fatal("change did not retain its shared bounded context")
	}
	if _, err := fence.tryEnter(version); authmfa.KindOf(err) != authmfa.ErrorBusy {
		t.Fatal("drain reopened before publication")
	}
	published := nha142PolicyVersion(t, 2, 2)
	if err := owner.change.publish(published); err != nil {
		t.Fatal(err)
	}
	if owner.change.ctx.Err() == nil {
		t.Fatal("resolved change leaked its timeout owner")
	}
	if err := owner.change.rollback(); authmfa.KindOf(err) != authmfa.ErrorConflict {
		t.Fatal("stale owner reopened policy")
	}
	if _, err := fence.tryEnter(version); authmfa.KindOf(err) != authmfa.ErrorConflict {
		t.Fatal("old policy admitted after publication")
	}
	next, err := fence.tryEnter(published)
	if err != nil {
		t.Fatal(err)
	}
	next.release()
}

func TestNHA142PolicyFenceCancelledDrainIsKnownRollback(t *testing.T) {
	fence, version := nha142Fence(t)
	ticket, err := fence.tryEnter(version)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(ticket.release)
	result := nha142StartFenceChange(t, fence, version, ctx)
	cancel()
	owner := nha142ReceiveFenceChange(t, result)
	if owner.change != nil || !errors.Is(owner.err, context.Canceled) {
		t.Fatal("pre-SQL cancellation was not known rollback", owner.err)
	}
	ticket.release()
	next, err := fence.tryEnter(version)
	if err != nil {
		t.Fatal("cancelled drain left a fabricated unknown commit", err)
	}
	next.release()
	var typedNil *nha142ServerNilContext
	if _, err := fence.beginReplace(typedNil, version); authmfa.KindOf(err) != authmfa.ErrorInvalidInput {
		t.Fatal("typed nil reached context methods")
	}
}

func TestNHA142PolicyFenceUnknownRequiresPrimaryOwner(t *testing.T) {
	fence, version := nha142Fence(t)
	owner, err := fence.beginReplace(context.Background(), version)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := contracts.ParseAccountID("account-a")
	second, _ := contracts.ParseAccountID("account-b")
	targets := []contracts.AccountID{second, first}
	if err := owner.setUnknown(targets); err != nil {
		t.Fatal(err)
	}
	targets[0] = contracts.AccountID{}
	if _, err := fence.tryEnter(version); authmfa.KindOf(err) != authmfa.ErrorUnavailable {
		t.Fatal("unknown mutation admitted admin")
	}
	if _, err := fence.beginReplace(context.Background(), version); authmfa.KindOf(err) != authmfa.ErrorUnavailable {
		t.Fatal("unknown mutation retried Set")
	}
	reconcile, known, err := fence.beginReconcile(context.Background())
	if err != nil || len(known) != 2 || known[0] != first || known[1] != second {
		t.Fatal("uncertain cohort was not copied/canonical", err)
	}
	if _, _, err := fence.beginReconcile(context.Background()); authmfa.KindOf(err) != authmfa.ErrorBusy {
		t.Fatal("two primary reconciliation owners admitted")
	}
	if err := reconcile.rollback(); authmfa.KindOf(err) != authmfa.ErrorInvalidInput {
		t.Fatal("reconciliation invented a known rollback")
	}
	known[0] = contracts.AccountID{}
	if err := reconcile.abandon(); err != nil {
		t.Fatal(err)
	}
	again, retained, err := fence.beginReconcile(context.Background())
	if err != nil || len(retained) != 2 || retained[0] != first {
		t.Fatal("failed reconciliation lost its owned cohort", err)
	}
	t.Cleanup(func() { _ = again.abandon() })
	if err := again.publish(nha142PolicyVersion(t, 1, 99)); authmfa.KindOf(err) != authmfa.ErrorIntegrity {
		t.Fatal("same revision with different hash published")
	}
	if _, err := fence.tryEnter(version); authmfa.KindOf(err) != authmfa.ErrorBusy {
		t.Fatal("bad primary result reopened fence")
	}
	updated := nha142PolicyVersion(t, 2, 2)
	if err := again.publish(updated); err != nil {
		t.Fatal(err)
	}
	next, err := fence.tryEnter(updated)
	if err != nil {
		t.Fatal(err)
	}
	next.release()
}

func TestNHA142PolicyFenceCohortBoundsAndRealDrain(t *testing.T) {
	targets := make([]contracts.AccountID, authmfa.PolicyAffectedAccountLimit)
	for i := range targets {
		var err error
		targets[i], err = contracts.ParseAccountID(fmt.Sprintf("account-%05d", i))
		if err != nil {
			t.Fatal(err)
		}
	}
	if admitted, err := admittedMFAPolicyTargets(targets); err != nil || len(admitted) != len(targets) {
		t.Fatal("maximum cohort refused", err)
	}
	if _, err := admittedMFAPolicyTargets(append(targets, targets[0])); authmfa.KindOf(err) != authmfa.ErrorBusy {
		t.Fatal("N+1 cohort admitted")
	}
	if _, err := admittedMFAPolicyTargets([]contracts.AccountID{targets[0], targets[0]}); authmfa.KindOf(err) != authmfa.ErrorIntegrity {
		t.Fatal("duplicate cohort admitted")
	}
	if _, err := admittedMFAPolicyTargets([]contracts.AccountID{{}}); authmfa.KindOf(err) != authmfa.ErrorIntegrity {
		t.Fatal("zero cohort admitted")
	}
	fence, version := nha142Fence(t)
	ticket, err := fence.tryEnter(version)
	if err != nil {
		t.Fatal(err)
	}
	fence.closeAdmissions()
	select {
	case <-fence.Done():
		t.Fatal("Done preceded a real admitted operation")
	default:
	}
	ticket.release()
	select {
	case <-fence.Done():
	case <-time.After(time.Second):
		t.Fatal("ticket was not joined")
	}
	if _, err := fence.tryEnter(version); authmfa.KindOf(err) != authmfa.ErrorUnavailable {
		t.Fatal("shutdown reopened admission")
	}
	fence2, version2 := nha142Fence(t)
	owner, err := fence2.beginReplace(context.Background(), version2)
	if err != nil {
		t.Fatal(err)
	}
	fence2.closeAdmissions()
	select {
	case <-fence2.Done():
		t.Fatal("Done preceded the actual SQL/publication owner")
	default:
	}
	if err := owner.abandon(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fence2.Done():
	case <-time.After(time.Second):
		t.Fatal("owner was not joined")
	}
	if err := owner.publish(nha142PolicyVersion(t, 2, 2)); authmfa.KindOf(err) != authmfa.ErrorConflict {
		t.Fatal("retired owner rejection lost its conflict classification", err)
	}
	fence3, version3 := nha142Fence(t)
	cancelled, err := fence3.beginReplace(context.Background(), version3)
	if err != nil {
		t.Fatal(err)
	}
	cancelled.cancel()
	if err := cancelled.publish(nha142PolicyVersion(t, 2, 2)); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled current owner published without its shared deadline", err)
	}
	if err := cancelled.abandon(); err != nil {
		t.Fatal(err)
	}
	if _, err := fence3.tryEnter(version3); authmfa.KindOf(err) != authmfa.ErrorUnavailable {
		t.Fatal("unknown post-write cancellation reopened sensitive admission", err)
	}
}

package configdocuments

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"
)

func TestNHA150StabilityRequiresExactDigestAndDateWithoutLosingRenames(t *testing.T) {
	start := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	a := Observation{hash: sha256.Sum256([]byte("a")), updated: start, present: true}
	b := Observation{hash: sha256.Sum256([]byte("b")), updated: start, present: true}
	var window stableObservation
	if window.admit(a, start) || window.admit(a, start.Add(499*time.Millisecond)) {
		t.Fatal("unstable source was admitted")
	}
	// The editor deliberately preserves mtime while replacing the inode/bytes.
	if window.admit(b, start.Add(500*time.Millisecond)) || window.admit(b, start.Add(999*time.Millisecond)) {
		t.Fatal("digest change did not restart stability")
	}
	if !window.admit(b, start.Add(time.Second)) {
		t.Fatal("stable exact replacement was lost")
	}
	b.updated = b.updated.Add(time.Second)
	if window.admit(b, start.Add(1250*time.Millisecond)) {
		t.Fatal("date change escaped stability")
	}
	b.pending = true
	if window.admit(b, start.Add(2*time.Second)) || window.admit(Observation{}, start.Add(3*time.Second)) {
		t.Fatal("pending or unavailable source was admitted")
	}
	b.pending = false
	if window.admit(b, start.Add(4*time.Second)) || window.admit(b, start.Add(3*time.Second)) {
		t.Fatal("availability gap or reversed clock reused an old window")
	}
}

func TestNHA150DiagnosticIsBoundToSourceAndDateAndClosedKinds(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	a := Observation{hash: sha256.Sum256([]byte("private synthetic A")), updated: now, present: true}
	diagnostic, err := newWorkspaceDiagnostic(a, CandidateRejected)
	if err != nil || !diagnostic.matches(a) || diagnostic.Document().String() != "operational.main" {
		t.Fatal("lost source binding", err)
	}
	b := a
	b.hash = sha256.Sum256([]byte("private synthetic B"))
	if diagnostic.matches(b) {
		t.Fatal("late rejection of A attached to B")
	}
	b = a
	b.updated = now.Add(time.Second)
	if diagnostic.matches(b) {
		t.Fatal("old diagnostic attached to rewritten observation")
	}
	for _, kind := range []DiagnosticKind{DiagnosticUnknown, DiagnosticKind(255)} {
		if _, err := newWorkspaceDiagnostic(a, kind); err == nil {
			t.Fatal("open diagnostic enum admitted")
		}
	}
	for _, observation := range []Observation{{}, {hash: a.hash, present: true}, {updated: now, present: true}} {
		if _, err := newWorkspaceDiagnostic(observation, CandidateRejected); err == nil {
			t.Fatal("partial source binding admitted")
		}
	}
}

func TestNHA150WatcherRejectsZeroOwnersAndClosesBeforeStart(t *testing.T) {
	for _, service := range []*Service{nil, {}, {admitted: true}} {
		if _, err := NewWatcher(service); err == nil {
			t.Fatal("partial owner admitted")
		}
	}
	var zero Watcher
	if err := zero.Start(context.Background()); err == nil {
		t.Fatal("zero watcher started")
	}
	if err := zero.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := &Watcher{done: make(chan struct{})}
	w.BeginClose()
	w.BeginClose()
	select {
	case <-w.Done():
	default:
		t.Fatal("unstarted watcher did not join")
	}
	if err := w.Start(context.Background()); err == nil {
		t.Fatal("closed watcher restarted")
	}
	if err := w.Close(nil); err == nil {
		t.Fatal("nil close context admitted")
	}
	type nilContext struct{ context.Context }
	var absent *nilContext
	if err := w.Close(absent); err == nil {
		t.Fatal("typed nil context admitted")
	}
}

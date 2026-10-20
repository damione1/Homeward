package nha045

import (
	"context"
	"testing"
	"time"
)

func TestNHA045ManualClockSeparatesCivilElapsedAndCancellation(t *testing.T) {
	c := &manualClock{origin: time.Date(2024, 3, 11, 4, 30, 0, 0, time.UTC)}
	first := c.NewTimer(5 * time.Second)
	cancelled := c.NewTimer(2 * time.Second)
	if !cancelled.Stop() {
		t.Fatal("pending timer was not stopped")
	}
	c.mu.Lock()
	c.civilOffset = time.Hour
	c.mu.Unlock()
	if c.MonoNow() != 0 || !c.Now().Equal(c.origin.Add(time.Hour)) {
		t.Fatal("civil correction moved elapsed time")
	}
	if e := c.advance(5000); e != nil {
		t.Fatal(e)
	}
	select {
	case <-first.C():
	default:
		t.Fatal("due timer not delivered")
	}
	select {
	case <-cancelled.C():
		t.Fatal("cancelled timer delivered")
	default:
	}
	if e := c.advance(6000); e != nil {
		t.Fatal(e)
	}
	select {
	case <-first.C():
		t.Fatal("one-shot timer delivered twice")
	default:
	}
	if e := c.advance(1); !issueIs(e, InvalidFixture) {
		t.Fatal("backward elapsed time admitted")
	}
	if c.NewTimer(-1) != nil {
		t.Fatal("negative timer admitted")
	}
}

type nilContext struct{}

func (*nilContext) Deadline() (time.Time, bool) { panic("typed nil context reached") }
func (*nilContext) Done() <-chan struct{}       { panic("typed nil context reached") }
func (*nilContext) Err() error                  { panic("typed nil context reached") }
func (*nilContext) Value(any) any               { panic("typed nil context reached") }
func TestNHA045TypedNilContextAndPayloadAreRefused(t *testing.T) {
	var pointer *nilContext
	var ctx context.Context = pointer
	if e := await(ctx, func() bool { return true }); !issueIs(e, InvalidFixture) {
		t.Fatalf("typed nil barrier admitted: %v", e)
	}
	var payload *statePayload
	if _, e := payloadWire(payload); e == nil {
		t.Fatal("typed nil payload admitted")
	}
	if _, e := payloadWire(actionPayload{}); e == nil {
		t.Fatal("zero action payload admitted")
	}
	if _, e := payloadWire(statePayload{}); e == nil {
		t.Fatal("zero state payload admitted")
	}
}

func TestNHA045NativeControlInversionSurvivesProjection(t *testing.T) {
	all, e := Corpus()
	if e != nil {
		t.Fatal(e)
	}
	f := all[0]
	facts := []factWire{{Key: "a/terminal"}, {Key: "b/start"}}
	w := captureWire{Facts: facts, Orders: []streamWire{{Scope: "control", Keys: []string{"a/terminal", "b/start"}}}}
	normal := causalEdges(w, f)
	if len(normal) != 1 || normal[0] != (edgeWire{"a/terminal", "b/start"}) {
		t.Fatalf("missing native join edge: %+v", normal)
	}
	w.Orders[0].Keys = []string{"b/start", "a/terminal"}
	inverted := causalEdges(w, f)
	if len(inverted) != 1 || inverted[0] != (edgeWire{"b/start", "a/terminal"}) {
		t.Fatalf("native inversion hidden: %+v", inverted)
	}
}

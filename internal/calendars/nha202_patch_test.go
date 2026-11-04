package calendars

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"
)

func nha202DaySpan(t *testing.T, start, end string) Span {
	t.Helper()
	a, err := ParseDate(start)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseDate(end)
	if err != nil {
		t.Fatal(err)
	}
	span, err := AllDaySpan(a, b)
	if err != nil {
		t.Fatal(err)
	}
	return span
}
func TestNHA202EventPatchRetainsUnselectedContentAndReanchorsRule(t *testing.T) {
	ctx := context.Background()
	span := nha202DaySpan(t, "2026-11-03", "2026-11-04")
	rule, err := ParseRule("FREQ=DAILY;COUNT=3", span)
	if err != nil {
		t.Fatal(err)
	}
	event, err := NewEvent(ctx, EventInput{Summary: "Original", Description: "Remove explicitly", Location: "Retain", Span: span, Rule: rule})
	if err != nil {
		t.Fatal(err)
	}
	next := nha202DaySpan(t, "2026-11-04", "2026-11-05")
	patch, err := NewEventPatch(EventPatchInput{Span: next}, []EventField{EventSpan, EventDescription})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := patch.apply(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Summary() != "Original" || updated.Description() != "" || updated.Location() != "Retain" || !updated.Rule().validFor(next) || event.Span() != span {
		t.Fatal("patch lost unselected content, failed explicit empty, mutated original or retained stale recurrence anchor")
	}
	clear, err := NewEventPatch(EventPatchInput{}, []EventField{EventRule})
	if err != nil {
		t.Fatal(err)
	}
	updated, err = clear.apply(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := updated.Rule().Text(); present {
		t.Fatal("selected absent recurrence did not clear the rule")
	}
	if _, err := NewEventPatch(EventPatchInput{}, []EventField{EventField(255)}); err == nil {
		t.Fatal("unknown editable field entered domain")
	}
	if _, err := NewEventPatch(EventPatchInput{}, []EventField{EventSummary}); err == nil {
		t.Fatal("selected empty required summary entered domain")
	}
}
func TestNHA202ResolvedRecurrencePointRetainsOriginalWallAndFoldInstant(t *testing.T) {
	point, err := ParsePoint(Timed, "2026-11-01T01:30:00@2026-11-01T06:30:00Z")
	if err != nil {
		t.Fatal(err)
	}
	local, instant, resolved := point.ResolvedPoint()
	if !resolved || local.String() != "2026-11-01T01:30:00" || instant.String() != "2026-11-01T06:30:00Z" {
		t.Fatal("explicit recurrence fold lost")
	}
	civil, err := ParsePoint(Timed, "2026-11-01T01:30:00")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, resolved := civil.ResolvedPoint(); resolved {
		t.Fatal("civil recurrence point silently acquired an instant")
	}
}
func TestNHA202CalendarCursorEncryptsAndBindsCallerAndRejectsAlterations(t *testing.T) {
	codec, err := NewCursorCodec()
	if err != nil {
		t.Fatal(err)
	}
	binding := [32]byte{1}
	payload := []byte(`{"private":"SYNTHETIC_CONTENT_CANARY"}`)
	token, err := codec.Seal(payload, binding)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := codec.Open(token, binding)
	if err != nil || !bytes.Equal(decoded, payload) {
		t.Fatal("current opaque cursor roundtrip failed")
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, payload) {
		t.Fatal("cursor disclosed plaintext capture contents")
	}
	again, err := codec.Seal(payload, binding)
	if err != nil || again == token {
		t.Fatal("cursor nonce was reused")
	}
	if _, err := codec.Open(token, [32]byte{2}); err == nil {
		t.Fatal("cross-caller cursor admitted")
	}
	raw[len(raw)-1] ^= 1
	if _, err := codec.Open(base64.RawURLEncoding.EncodeToString(raw), binding); err == nil {
		t.Fatal("modified encrypted cursor admitted")
	}
	if _, err := codec.Seal(make([]byte, 4097), binding); err == nil {
		t.Fatal("cursor payload exceeded bound")
	}
}

package sequencer

import (
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/damione1/Homeward/internal/domain"
)

func TestNHA097HelperRetentionChargesPrivatePayloadAndStrings(t *testing.T) {
	base := 96
	if got := retainedStateValueBytes(base, domain.NewSwitchValue(false)); got != base {
		t.Fatalf("fixed switch payload gained a variable charge: %d", got)
	}
	empty, err := domain.NewHelperText("")
	if err != nil {
		t.Fatal(err)
	}
	emptyState, err := domain.NewHelperStateValue(empty, 1)
	if err != nil {
		t.Fatal(err)
	}
	emptyCharge := retainedStateValueBytes(base, emptyState)
	privateBytes := int(unsafe.Sizeof(domain.HelperValue{}))
	if emptyCharge-base < privateBytes {
		t.Fatalf("private helper payload omitted from retained budget: %d < %d", emptyCharge-base, privateBytes)
	}
	if got := retainedStateValueBytes(MaxJournalBytes-privateBytes+1, emptyState); got <= MaxJournalBytes {
		t.Fatalf("private helper payload crossed journal limit: %d", got)
	}
	longText := strings.Repeat("t", 1024)
	text, err := domain.NewHelperText(longText)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := domain.NewHelperSelect(longText)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []domain.HelperValue{text, selected} {
		state, err := domain.NewHelperStateValue(value, 1)
		if err != nil {
			t.Fatal(err)
		}
		if got := retainedStateValueBytes(base, state); got-emptyCharge < len(longText) {
			t.Fatalf("helper string backing bytes omitted: delta=%d", got-emptyCharge)
		}
	}
	entity, err := domain.ParseEntityID("nha097:helper-retention")
	if err != nil {
		t.Fatal(err)
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	source, err := domain.NewSourceStamp("nha097-session", nil)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	state, err := domain.NewHelperStateValue(text, 1)
	if err != nil {
		t.Fatal(err)
	}
	report, err := domain.NewStateReport(quality, &state, nil, at, source, 1)
	if err != nil {
		t.Fatal(err)
	}
	captured, _, err := domain.ApplyStateReport(entity, nil, report)
	if err != nil {
		t.Fatal(err)
	}
	state = domain.NewSwitchValue(true)
	current, present := captured.CurrentValue()
	retained, revision, helperPresent := current.Helper()
	if !present || !helperPresent || revision != 1 {
		t.Fatal("captured helper changed when caller reused its StateValue")
	}
	if got, ok := retained.Text(); !ok || got != longText {
		t.Fatal("captured helper text changed after admission")
	}
}

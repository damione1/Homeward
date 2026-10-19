package sequencer

import (
	"github.com/damione1/Homeward/internal/domain"
	"testing"
	"time"
	"unsafe"
)

func TestNHA105StructuralPayloadChargesAndBoundedCapture(t *testing.T) {
	missing, _ := domain.SolarOccurrenceMissing(domain.SolarNoEventWithinSearchHorizon)
	input := domain.SolarValueInput{Horizon: domain.SolarAboveHorizon, Rising: true}
	for i := range input.Next {
		input.Next[i] = missing
	}
	input.Next[3], _ = domain.SolarOccurrenceAt(time.Unix(100, 0))
	input.Next[2], _ = domain.SolarOccurrenceAt(time.Unix(200, 0))
	solar, _ := domain.NewSolarValue(input)
	event, _ := domain.NewSolarEventValue(domain.SolarDawn, missing)
	sun, _ := domain.NewSolarStateValue(solar)
	next, _ := domain.NewSolarEventStateValue(event)
	helper, _ := domain.NewHelperStateValue(domain.NewHelperBoolean(false), 1)
	for _, item := range []struct {
		value domain.StateValue
		bytes int
	}{{sun, int(unsafe.Sizeof(domain.SolarValue{}))}, {next, int(unsafe.Sizeof(domain.SolarEventValue{}))}, {helper, int(unsafe.Sizeof(struct {
		value    domain.HelperValue
		revision uint64
	}{}))}} {
		if got := retainedStateValueBytes(0, item.value); got != item.bytes {
			t.Fatal("retained native wrapper charge omitted or confused with allocator class")
		}
		if retainedStateValueBytes(MaxJournalBytes-item.bytes+1, item.value) <= MaxJournalBytes {
			t.Fatal("native payload exceeded a logical budget without saturation")
		}
	}
	sequence, err := New(RealClock{}, 8)
	if err != nil {
		t.Fatal(err)
	}
	if err := sequence.CheckCaptureAvailable(); err != nil {
		t.Fatal("detached registry changed constructor availability")
	}
	before, err := sequence.CaptureBounded(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sequence.CaptureBounded(1); err != ErrSnapshotBudget {
		t.Fatal("global capture copied an oversized image")
	}
	after, err := sequence.CaptureBounded(1 << 20)
	if err != nil || before.Cursor != after.Cursor {
		t.Fatal("refused capture mutated the predecessor")
	}
	sequence.InvalidateRegistry()
	if sequence.CheckCaptureAvailable() != ErrRegistryUnavailable {
		t.Fatal("unavailable registry passed no-copy availability check")
	}
	if unsafe.Sizeof(uintptr(0)) == 8 && (unsafe.Sizeof(Entry{}) != 1096 || unsafe.Sizeof(RecorderState{}) != 616) {
		t.Fatal("production retention layout changed")
	}
}

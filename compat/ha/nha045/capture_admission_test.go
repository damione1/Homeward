package nha045

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Reject JSON null before it becomes a valid zero-valued domain scalar.
func TestNHA045CaptureRejectsNullZeroValuedPrimitives(t *testing.T) {
	base := testCapture(t, "ha", "ha-null-boundary")
	plain, e := base.JSON()
	if e != nil {
		t.Fatal(e)
	}
	withTrigger := changed(t, base, func(w *captureWire) {
		w.Facts[1].Kind = factName(Wait)
		w.Facts[1].Data = dataWire{Outcome: ptr(outcomeName(Completed)), Wait: &waitWire{Completed: ptr(true), RemainingMS: ptr(int64(1000)), Trigger: &triggerWire{ID: "release", Index: 0, Platform: "event"}}}
	})
	triggered, e := withTrigger.JSON()
	if e != nil {
		t.Fatal(e)
	}
	var plainCompact, triggeredCompact bytes.Buffer
	if e = json.Compact(&plainCompact, plain); e != nil {
		t.Fatal(e)
	}
	if e = json.Compact(&triggeredCompact, triggered); e != nil {
		t.Fatal(e)
	}
	plain = plainCompact.Bytes()
	triggered = triggeredCompact.Bytes()
	for _, tc := range []struct {
		name  string
		data  []byte
		field string
	}{
		{"fact_at_ms", plain, `"at_ms":0`},
		{"capture_evicted", plain, `"evicted":0`},
		{"trigger_index", triggered, `"index":0`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Count(string(tc.data), tc.field) != 1 {
				t.Fatal("counterexample must replace exactly one valid primitive")
			}
			field := strings.TrimSuffix(tc.field, "0") + "null"
			changed := strings.Replace(string(tc.data), tc.field, field, 1)
			if _, e := DecodeCapture([]byte(changed)); !issueIs(e, InvalidObservation) {
				t.Fatalf("JSON null silently converted into valid primitive zero: %v", e)
			}
		})
	}
}

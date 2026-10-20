package nha045

import (
	"encoding/json"
	"testing"
)

func TestNHA045ExpectedChecksRejectMissingNullAndContradictoryValues(t *testing.T) {
	all, e := Corpus()
	if e != nil {
		t.Fatal(e)
	}
	original, e := all[3].JSON()
	if e != nil {
		t.Fatal(e)
	}
	// Case04's first check has a real, present at_ms:0. Keep that positive control.
	var admitted fixtureWire
	if e = json.Unmarshal(original, &admitted); e != nil {
		t.Fatal(e)
	}
	if admitted.Checks[0].AtMS != 0 {
		t.Fatal("zero-valued check reference changed")
	}
	if _, e = DecodeFixture(original); e != nil {
		t.Fatalf("explicit zero check refused: %v", e)
	}
	for _, tc := range []struct {
		name   string
		index  int
		field  string
		value  json.RawMessage
		remove bool
	}{
		{"at_ms_null", 0, "at_ms", json.RawMessage("null"), false},
		{"at_ms_absent", 0, "at_ms", nil, true},
		{"admission_finished", 0, "outcome", json.RawMessage(`"finished"`), false},
		{"action_cancelled", 1, "outcome", json.RawMessage(`"cancelled"`), false},
		{"terminal_accepted", 3, "outcome", json.RawMessage(`"accepted"`), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fixture map[string]json.RawMessage
			if e := json.Unmarshal(original, &fixture); e != nil {
				t.Fatal(e)
			}
			var checks []map[string]json.RawMessage
			if e := json.Unmarshal(fixture["checks"], &checks); e != nil {
				t.Fatal(e)
			}
			if tc.remove {
				delete(checks[tc.index], tc.field)
			} else {
				checks[tc.index][tc.field] = tc.value
			}
			fixture["checks"], e = json.Marshal(checks)
			if e != nil {
				t.Fatal(e)
			}
			data, e := json.Marshal(fixture)
			if e != nil {
				t.Fatal(e)
			}
			if _, e := DecodeFixture(data); !issueIs(e, InvalidFixture) {
				t.Fatalf("contradictory expected observation admitted: %v", e)
			}
		})
	}
}

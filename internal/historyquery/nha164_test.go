package historyquery

import (
	"errors"
	"testing"
	"time"
)

func TestNHA164QueryAdmission(t *testing.T) {
	start := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	if (Query{}).Valid() {
		t.Fatal("zero query admitted")
	}
	for _, kind := range []Kind{Invalid, Observations, Aggregates, ExecutionTraces, Kind(255)} {
		entity := "hw:fixture:temperature"
		if kind == ExecutionTraces {
			entity = ""
		}
		_, err := NewQuery(kind, entity, start, start.Add(501*time.Hour), 1, "")
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("oversize kind %d admitted: %v", kind, err)
		}
	}
	if _, err := NewQuery(ExecutionTraces, "", start, start.Add(time.Hour), 32, ""); !errors.Is(err, ErrInvalid) {
		t.Fatal("trace page over 31 admitted")
	}
	if _, err := NewQuery(ExecutionTraces, "", start, start.Add(time.Hour), 1, "00000000000000000000000000000000"); !errors.Is(err, ErrInvalid) {
		t.Fatal("zero epoch admitted")
	}
}
func TestNHA164CursorBindingsAndLifetime(t *testing.T) {
	start := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	q, err := NewQuery(ExecutionTraces, "", start, start.Add(time.Hour), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	binding, _ := NewBinding([32]byte{1}, [32]byte{2})
	codec, err := NewCursorCodec()
	if err != nil {
		t.Fatal(err)
	}
	var now time.Duration
	codec.elapsed = func() time.Duration { return now }
	token, err := codec.Seal("traces", q, binding, []byte{1, 2, 3})
	if err != nil || len(token) > MaxTokenBytes {
		t.Fatal(err)
	}
	if state, err := codec.Open("traces", q, binding, token); err != nil || string(state) != string([]byte{1, 2, 3}) {
		t.Fatal(err)
	}
	other, _ := NewBinding([32]byte{1}, [32]byte{3})
	changed, _ := NewQuery(ExecutionTraces, "", start, start.Add(time.Hour), 2, "")
	for _, tc := range []struct {
		procedure string
		q         Query
		b         Binding
		token     string
	}{{"raw", q, binding, token}, {"traces", changed, binding, token}, {"traces", q, other, token}, {"traces", q, binding, token + "!"}} {
		if _, err := codec.Open(tc.procedure, tc.q, tc.b, tc.token); !errors.Is(err, ErrStale) {
			t.Fatal("cursor binding accepted")
		}
	}
	restarted, _ := NewCursorCodec()
	if _, err := restarted.Open("traces", q, binding, token); !errors.Is(err, ErrStale) {
		t.Fatal("restart accepted old cursor")
	}
	now = CursorTTL
	if _, err := codec.Open("traces", q, binding, token); !errors.Is(err, ErrStale) {
		t.Fatal("expired cursor admitted")
	}
}

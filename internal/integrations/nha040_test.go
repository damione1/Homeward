package integrations

import (
	"errors"
	"testing"
)

func TestNHA040NativeMQTTEdgeAdmissionAndCopy(t *testing.T) {
	for _, test := range []struct {
		name     string
		topic    string
		payload  []byte
		retained bool
		seq      uint64
	}{
		{"retained", "buttons/a", nil, true, 1},
		{"zero sequence", "buttons/a", nil, false, 0},
		{"wildcard", "buttons/#", nil, false, 1},
		{"long topic", string(make([]byte, 257)), nil, false, 1},
		{"long payload", "buttons/a", make([]byte, (64<<10)+1), false, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewNativeMQTTEdge(test.topic, test.payload, test.retained, test.seq); !errors.Is(err, ErrInvalidNativeMQTTEdge) {
				t.Fatalf("unsafe edge admitted: %v", err)
			}
		})
	}
	payload := []byte("on")
	edge, err := NewNativeMQTTEdge("buttons/a", payload, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	payload[0] = 'x'
	name, received, seq, err := edge.Components()
	if err != nil || name != "buttons/a" || string(received) != "on" || seq != 1 {
		t.Fatalf("edge was mutated: %q %q %d %v", name, received, seq, err)
	}
	received[0] = 'x'
	_, again, _, _ := edge.Components()
	if string(again) != "on" {
		t.Fatal("components exposed mutable payload")
	}
	maximum, err := NewNativeMQTTEdge("buttons/a", make([]byte, 64<<10), false, 2)
	if err != nil {
		t.Fatal(err)
	}
	_, bytes, seq, err := maximum.Components()
	if err != nil || len(bytes) != 64<<10 || seq != 2 {
		t.Fatalf("admission boundary failed: %d %d %v", len(bytes), seq, err)
	}
}

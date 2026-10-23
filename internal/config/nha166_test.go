package config

import "testing"

func TestNHA166OperatorSelectionIsImmutable(t *testing.T) {
	original, _ := nha150CaptureInputs(t)
	selected := original.WithMQTTReadOnlyObservation()
	if original.MQTTReadOnlyObservation() || !selected.MQTTReadOnlyObservation() || !selected.Valid() {
		t.Fatal("operator selection mutated the original or lost source admission")
	}
	if !selected.WithMQTTReadOnlyObservation().MQTTReadOnlyObservation() {
		t.Fatal("observation selection was not monotone")
	}
	source, err := selected.CaptureWorkspace()
	if err != nil || !source.Valid() || !selected.MQTTReadOnlyObservation() {
		t.Fatal("source capture cleared operator selection", err)
	}
}

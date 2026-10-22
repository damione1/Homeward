package storage

import (
	"bytes"
	"crypto/sha256"
	"testing"
)

func TestNHA055Prior25WindowAdmitsOnlyExactReleasedContract(t *testing.T) {
	plan := schemaPlanSHA256(migrations[:25])
	good := prior25SchemaWindow{singleton: true, format: 1, schema: 25, baseline: 25,
		release: prior25SchemaRelease, reader: prior25SchemaRelease, writer: prior25SchemaRelease, maximum: prior25SchemaRelease,
		baselinePlan: bytes.Clone(plan[:]), plan: bytes.Clone(plan[:])}
	if !good.validFor(plan) || good.validFor([sha256.Size]byte{}) || (prior25SchemaWindow{}).validFor(plan) {
		t.Fatal("exact prior25 declaration was not admitted exclusively")
	}
	for _, change := range []func(*prior25SchemaWindow){
		func(w *prior25SchemaWindow) { w.singleton = false },
		func(w *prior25SchemaWindow) { w.format++ },
		func(w *prior25SchemaWindow) { w.schema++ },
		func(w *prior25SchemaWindow) { w.baseline++ },
		func(w *prior25SchemaWindow) { w.release = schemaWindowBaselineRelease },
		func(w *prior25SchemaWindow) { w.reader = "homeward-window-25-26-successor" },
		func(w *prior25SchemaWindow) { w.writer = "" },
		func(w *prior25SchemaWindow) { w.maximum = "unknown" },
		func(w *prior25SchemaWindow) { w.baselinePlan = nil },
		func(w *prior25SchemaWindow) { w.plan = plan[:31] },
		func(w *prior25SchemaWindow) {
			other := sha256.Sum256([]byte("another baseline"))
			w.baselinePlan = other[:]
		},
		func(w *prior25SchemaWindow) { other := sha256.Sum256([]byte("another plan")); w.plan = other[:] },
	} {
		bad := good
		change(&bad)
		if bad.validFor(plan) {
			t.Fatal("changed predecessor declaration admitted")
		}
	}
	if parseSchemaRelease(prior25SchemaRelease) != schemaReleaseInvalid {
		t.Fatal("historical bridge extended the current release parser")
	}
}

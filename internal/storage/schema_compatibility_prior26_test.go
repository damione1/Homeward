package storage

import (
	"bytes"
	"crypto/sha256"
	"testing"
)

func TestNHF009Prior26WindowAuthenticatesOnlyReleasedBytes(t *testing.T) {
	plan := schemaPlanSHA256(migrations[:26])
	good := prior26SchemaWindow{singleton: true, format: 1, schema: 26, baseline: 26,
		release: prior26SchemaRelease, reader: prior26SchemaRelease, writer: prior26SchemaRelease, maximum: prior26SchemaRelease,
		baselinePlan: bytes.Clone(plan[:]), plan: bytes.Clone(plan[:])}
	if !good.validFor(plan) || good.validFor([sha256.Size]byte{}) || (prior26SchemaWindow{}).validFor(plan) {
		t.Fatal("exact released26 row not exclusively admitted")
	}
	for _, mutate := range []func(*prior26SchemaWindow){
		func(w *prior26SchemaWindow) { w.singleton = false },
		func(w *prior26SchemaWindow) { w.format++ },
		func(w *prior26SchemaWindow) { w.schema++ },
		func(w *prior26SchemaWindow) { w.baseline++ },
		func(w *prior26SchemaWindow) { w.release = schemaWindowBaselineRelease },
		func(w *prior26SchemaWindow) { w.reader = prior25SchemaRelease },
		func(w *prior26SchemaWindow) { w.writer = "" },
		func(w *prior26SchemaWindow) { w.maximum = "unknown" },
		func(w *prior26SchemaWindow) { w.baselinePlan = nil },
		func(w *prior26SchemaWindow) { w.plan = plan[:31] },
		func(w *prior26SchemaWindow) { wrong := sha256.Sum256([]byte("wrong")); w.baselinePlan = wrong[:] },
		func(w *prior26SchemaWindow) { wrong := sha256.Sum256([]byte("wrong")); w.plan = wrong[:] },
	} {
		bad := good
		mutate(&bad)
		if bad.validFor(plan) {
			t.Fatal("modified predecessor admitted")
		}
	}
	if parseSchemaRelease(prior26SchemaRelease) != schemaReleaseInvalid {
		t.Fatal("old release became a current runtime profile")
	}
}

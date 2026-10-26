package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestNHF010Prior27WindowAuthenticatesOnlyReleasedBytes(t *testing.T) {
	plan := schemaPlanSHA256(migrations[:27])
	// Independently recorded from NHF009's delivered SQL27 executable manifest.
	if hex.EncodeToString(plan[:]) != "4059f122e08a86cc978804b61edb356a3c32116dc0921f0dbf8c153a2e08a009" {
		t.Fatal("SQL27 prefix differs from the delivered predecessor")
	}
	good := prior27SchemaWindow{singleton: true, format: 1, schema: 27, baseline: 27,
		release: prior27SchemaRelease, reader: prior27SchemaRelease, writer: prior27SchemaRelease, maximum: prior27SchemaRelease,
		baselinePlan: bytes.Clone(plan[:]), plan: bytes.Clone(plan[:])}
	if !good.validFor(plan) || good.validFor([sha256.Size]byte{}) || (prior27SchemaWindow{}).validFor(plan) {
		t.Fatal("exact released27 row not exclusively admitted")
	}
	for _, mutate := range []func(*prior27SchemaWindow){
		func(w *prior27SchemaWindow) { w.singleton = false },
		func(w *prior27SchemaWindow) { w.format++ },
		func(w *prior27SchemaWindow) { w.schema++ },
		func(w *prior27SchemaWindow) { w.baseline++ },
		func(w *prior27SchemaWindow) { w.release = schemaWindowBaselineRelease },
		func(w *prior27SchemaWindow) { w.reader = prior25SchemaRelease },
		func(w *prior27SchemaWindow) { w.writer = "" },
		func(w *prior27SchemaWindow) { w.maximum = "unknown" },
		func(w *prior27SchemaWindow) { w.baselinePlan = nil },
		func(w *prior27SchemaWindow) { w.plan = plan[:31] },
		func(w *prior27SchemaWindow) { wrong := sha256.Sum256([]byte("wrong")); w.baselinePlan = wrong[:] },
		func(w *prior27SchemaWindow) { wrong := sha256.Sum256([]byte("wrong")); w.plan = wrong[:] },
	} {
		bad := good
		mutate(&bad)
		if bad.validFor(plan) {
			t.Fatal("modified predecessor admitted")
		}
	}
	if parseSchemaRelease(prior27SchemaRelease) != schemaReleaseInvalid {
		t.Fatal("old release became a current runtime profile")
	}
}

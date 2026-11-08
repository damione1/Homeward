package storage

import (
	"bytes"
	"encoding/hex"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestNHA212FrozenFanPayloadMigrationPreservesUnknownAndFalse(t *testing.T) {
	// Canonical old fan field14 contains enum field1. Unknown top-level field30
	// and fan field20 are deliberately retained byte-for-byte.
	raw, _ := hex.DecodeString("f0010772050801a001095000")
	next, changed, err := migrateLegacyFanPayload(raw)
	if err != nil || !changed {
		t.Fatal(err)
	}
	want, _ := hex.DecodeString("f00107720a10001a034f6666a001095000")
	if !bytes.Equal(next, want) {
		t.Fatalf("migration bytes %x want %x", next, want)
	}
	var value state.ObservedState
	if err = proto.Unmarshal(next, &value); err != nil {
		t.Fatal(err)
	}
	if value.GetFan().On == nil || *value.GetFan().On || value.GetFan().GetSpeed() != "Off" {
		t.Fatal("OFF became absent or inferred from key")
	}
	nonFan, _ := hex.DecodeString("5800f00107")
	unchanged, changed, err := migrateLegacyFanPayload(nonFan)
	if err != nil || changed || !bytes.Equal(nonFan, unchanged) {
		t.Fatal("non-fan false/unknown bytes changed", err)
	}
	for _, invalid := range []string{"7200", "72020800", "72020806", "720408010805", "720410001801"} {
		b, _ := hex.DecodeString(invalid)
		if _, _, err := migrateLegacyFanPayload(b); err == nil {
			t.Fatalf("invalid legacy %s admitted", invalid)
		}
	}
}

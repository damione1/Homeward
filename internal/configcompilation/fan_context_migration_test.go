package configcompilation

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"testing"
)

func historicalFanContext(t *testing.T) []byte {
	t.Helper()
	identity := nha055ContextIdentity(t, "migration_fan")
	desc := &registry.CapabilityDescription{SchemaId: "core.fan.v1", Major: 1, DisplayName: "Synthetic ventilation", Endpoint: "fan", Constraints: &registry.CapabilityDescription_Fan{Fan: &registry.FanConstraints{SetOn: true}}}
	ctx, err := NewContext("v1", sha256.Sum256([]byte("synthetic manifest")), []Identity{identity}, map[domain.EntityID][]*registry.CapabilityDescription{identity.Entity: {desc}})
	if err != nil {
		t.Fatal(err)
	}
	var wire wireContext
	if err = json.Unmarshal(ctx.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	desc.Constraints = nil
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(desc)
	if err != nil {
		t.Fatal(err)
	}
	old := protowire.AppendTag(nil, 1, protowire.VarintType)
	old = protowire.AppendVarint(old, 1)
	for _, key := range []string{"quiet/7", "boost:91", "night/13"} {
		old = protowire.AppendTag(old, 2, protowire.BytesType)
		old = protowire.AppendString(old, key)
	}
	encoded = protowire.AppendTag(encoded, 9, protowire.BytesType)
	encoded = protowire.AppendBytes(encoded, old)
	wire.Capabilities[0].Descriptions[0] = encoded
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestSQL33MigratesHistoricalFanCompilerContext(t *testing.T) {
	raw := historicalFanContext(t)
	if _, err := ParseContext(raw); err == nil {
		t.Fatal("reproduction must be rejected by the current runtime reader")
	}
	migrated, changed, err := MigrateFanContextSQL33(raw)
	if err != nil || !changed || !migrated.Valid() {
		t.Fatalf("historical context must migrate and readmit: changed=%v err=%v", changed, err)
	}
	var before, after wireContext
	_ = json.Unmarshal(raw, &before)
	_ = json.Unmarshal(migrated.Bytes(), &after)
	before.Capabilities = after.Capabilities
	same, _ := json.Marshal(before)
	if !bytes.Equal(same, migrated.Bytes()) {
		t.Fatal("identity/manifest context changed")
	}
	desc := new(registry.CapabilityDescription)
	if err = proto.Unmarshal(after.Capabilities[0].Descriptions[0], desc); err != nil {
		t.Fatal(err)
	}
	fan := desc.GetFan()
	if !fan.SetOn || !fan.ObservesPower {
		t.Fatal("historical mandatory power semantics lost")
	}
	for i, key := range []string{"quiet/7", "boost:91", "night/13"} {
		if fan.Speed.Choices[i].Key != key || fan.Speed.Choices[i].DisplayName != "" {
			t.Fatal("source key/order changed or label fabricated")
		}
	}
	next, changed, err := MigrateFanContextSQL33(migrated.Bytes())
	if err != nil || changed || !bytes.Equal(next.Bytes(), migrated.Bytes()) {
		t.Fatal("migration not idempotent")
	}
}

func TestSQL33RejectsCorruptHistoricalFanContext(t *testing.T) {
	raw := historicalFanContext(t)
	for _, mutate := range []func(*wireContext){
		func(w *wireContext) {
			w.Capabilities[0].Descriptions[0] = append(w.Capabilities[0].Descriptions[0], 0xff)
		},
		func(w *wireContext) { w.Identities[0].Entity = "invalid" },
		func(w *wireContext) { w.ManifestSHA256 = "invalid" },
	} {
		var w wireContext
		_ = json.Unmarshal(raw, &w)
		mutate(&w)
		bad, _ := json.Marshal(w)
		if _, _, err := MigrateFanContextSQL33(bad); err == nil {
			t.Fatal("corrupt historical input admitted")
		}
	}
	// A canonical context that already uses the new codec must remain exact.
	current, _, _ := nha055ContextFixture(t)
	got, changed, err := MigrateFanContextSQL33(current.Bytes())
	if err != nil || changed || !bytes.Equal(got.Bytes(), current.Bytes()) {
		t.Fatal("unrelated context rewritten")
	}
	for _, bad := range [][]byte{nil, []byte(`{}`), append(raw, ' ')} {
		if _, _, err := MigrateFanContextSQL33(bad); err == nil {
			t.Fatal("noncanonical context admitted")
		}
	}
}

func TestSQL33RejectsMalformedLegacyFanAndPreservesCurrentFan(t *testing.T) {
	for _, raw := range [][]byte{
		{8, 0},                   // noncanonical false bool
		{8, 1, 8, 1},             // duplicate power
		{18, 1, 'a', 18, 1, 'a'}, // duplicate source choice
		{18, 0},                  // empty source key
		{18, 1, 0xff},            // invalid UTF-8
		{24, 1},                  // no historical field3
		{18, 3, ' ', 'a', ' '},   // old admission refused padded keys
	} {
		if _, e := migrateHistoricalFanConstraints(raw); e == nil {
			t.Fatal("invalid old fan constraints admitted")
		}
	}
	current := &registry.CapabilityDescription{SchemaId: "core.fan.v1", Major: 1, Endpoint: "fan", DisplayName: "Synthetic fan", Constraints: &registry.CapabilityDescription_Fan{Fan: &registry.FanConstraints{Level: &registry.NumberBounds{Maximum: 100}, ObservesPower: true}}}
	raw, e := proto.MarshalOptions{Deterministic: true}.Marshal(current)
	if e != nil {
		t.Fatal(e)
	}
	got, changed, e := migrateHistoricalFanDescription(raw)
	if e != nil || changed || !bytes.Equal(got, raw) {
		t.Fatal("already current fan changed")
	}
	context, _, _ := nha055ContextFixture(t)
	if _, _, e = MigrateFanContextSQL33(append(context.Bytes(), ' ')); e == nil {
		t.Fatal("current context with noncanonical hash input admitted")
	}
}

func TestSQL33RejectsOverlongHistoricalFanEnvelope(t *testing.T) {
	var wire wireContext
	_ = json.Unmarshal(historicalFanContext(t), &wire)
	raw := wire.Capabilities[0].Descriptions[0]
	for rest := raw; len(rest) > 0; {
		n, k, l := protowire.ConsumeTag(rest)
		v := protowire.ConsumeFieldValue(n, k, rest[l:])
		if n == 9 {
			start := len(raw) - len(rest)
			for _, badField := range [][]byte{
				append([]byte{0xca, 0x00}, rest[l:]...),
				append([]byte{0x4a, rest[l] | 0x80, 0x00}, rest[l+1:]...),
			} {
				bad := append(append([]byte(nil), raw[:start]...), badField...)
				if _, _, e := migrateHistoricalFanDescription(bad); e == nil {
					t.Fatal("overlong historical fan field normalized")
				}
			}
			return
		}
		rest = rest[l+v:]
	}
	t.Fatal("fan fixture absent")
}

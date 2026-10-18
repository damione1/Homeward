package contracts

import (
	"bytes"
	actions "github.com/damione1/new-home-assistant/gen/home/actions/v1"
	registry "github.com/damione1/new-home-assistant/gen/home/registry/v1"
	state "github.com/damione1/new-home-assistant/gen/home/state/v1"
	"google.golang.org/protobuf/proto"
	"os"
	"path/filepath"
	"testing"
)

func golden(t *testing.T, name string) []byte {
	t.Helper()
	b, e := os.ReadFile(filepath.Join("golden", name+".bin"))
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func roundtrip(t *testing.T, name string, m proto.Message) {
	t.Helper()
	data := golden(t, name)
	if e := proto.Unmarshal(data, m); e != nil {
		t.Fatal(e)
	}
	out, e := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if e != nil || !bytes.Equal(out, data) {
		t.Fatalf("%s roundtrip %v", name, e)
	}
}
func TestNHA004Command(t *testing.T) {
	c := new(actions.CommandRequest)
	roundtrip(t, "command", c)
	if c.ConfigRevision != 9007199254740993 || c.ExpectedRevision == nil || *c.ExpectedRevision != 0 || c.ExpiresAt.Nanos != 123456789 || ValidateCommand(c, "epoch-a", 9007199254740993) != nil {
		t.Fatal("lost 64bits/optional/timestamp/admission")
	}
	if ValidateCommand(c, "epoch-b", c.ConfigRevision).Code != actions.ErrorCode_ERROR_CODE_STALE_EPOCH {
		t.Fatal("stale epoch admitted")
	}
	if ValidateCommand(c, "epoch-a", c.ConfigRevision+1).Code != actions.ErrorCode_ERROR_CODE_STALE_CONFIGURATION {
		t.Fatal("stale config admitted")
	}
	c.Target.Reference = nil
	if ValidateCommand(c, "epoch-a", c.ConfigRevision).FieldPath != "target" {
		t.Fatal("missing oneof admitted")
	}
}
func TestNHA004UnknownValuesAndFields(t *testing.T) {
	s := new(state.ObservedState)
	roundtrip(t, "unknown-quality", s)
	if int32(s.Quality.Availability) != 99 || s.Revision != ^uint64(0) || s.SourceSeq == nil || s.GetMeasurement().GetDecimalValue() != "0.123456789012345678" {
		t.Fatal("unknown enum/precision/presence lost")
	}
	d := new(registry.CapabilityDescription)
	roundtrip(t, "unknown-description", d)
	if d.DisplayName != "Synthetic readable capability" {
		t.Fatal("unknown description unreadable")
	}
	c := new(actions.CommandRequest)
	roundtrip(t, "unknown-extension", c)
	e := ValidateCommand(c, "epoch-a", c.ConfigRevision)
	if e == nil || e.Code != actions.ErrorCode_ERROR_CODE_UNSUPPORTED_CAPABILITY {
		t.Fatal("unknown extension admitted")
	}
	data := append(golden(t, "command"), 0x98, 0x06, 0x07)
	if err := proto.Unmarshal(data, c); err != nil {
		t.Fatal(err)
	}
	out, _ := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	if !bytes.Equal(data, out) {
		t.Fatal("unknown field lost")
	}
}
func TestNHA004SnapshotGeneration(t *testing.T) {
	p1 := new(state.OpenViewResponse)
	p2 := new(state.ReadPageResponse)
	stream := new(state.WatchViewResponse)
	terminal := new(state.WatchViewResponse)
	roundtrip(t, "snapshot-page-1", p1)
	roundtrip(t, "snapshot-page-2", p2)
	roundtrip(t, "filtered-stream", stream)
	roundtrip(t, "resync-required", terminal)
	if !proto.Equal(p1.Cursor, p2.Page.Cursor) || p1.SnapshotToken != p2.Page.SnapshotToken || p1.NextPageToken == "" || p2.Page.NextPageToken != "" {
		t.Fatal("mixed snapshot pages")
	}
	if stream.StreamSequence != 1 || stream.Cursor.GlobalSequence != 108 || stream.Cursor.ConfigRevision != p1.Cursor.ConfigRevision {
		t.Fatal("filtered stream cursor")
	}
	if terminal.StreamSequence != 2 || terminal.GetResyncRequired().Reason != state.ResyncReason_RESYNC_REASON_CONFIGURATION_CHANGED {
		t.Fatal("missing generation resync")
	}
	roundtrip(t, "structured-error", new(actions.ContractError))
}

func TestNHA004EmptyReferences(t *testing.T) {
	for _, name := range []string{"empty-entity-id", "empty-alias", "absent-target"} {
		c := new(actions.CommandRequest)
		roundtrip(t, name, c)
		err := ValidateCommand(c, "epoch-a", c.ConfigRevision)
		if err == nil || err.Code != actions.ErrorCode_ERROR_CODE_INVALID_ARGUMENT || err.FieldPath != "target" {
			t.Fatalf("%s admitted", name)
		}
	}
}

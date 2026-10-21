package contracts

import (
	"bytes"
	"testing"
	"time"

	wire "github.com/damione1/Homeward/gen/home/configuration/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func nha150DiagnosticRead() *wire.ReadDocumentResponse {
	at := timestamppb.New(time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC))
	return &wire.ReadDocumentResponse{Document: &wire.Document{Id: &wire.DocumentId{Value: "operational.main"}, Kind: wire.DocumentKind_DOCUMENT_KIND_OPERATIONAL_YAML, SchemaVersion: "homeward.operational/1", SyntaxProfile: "homeward.operational-yaml/1", Version: &wire.DocumentVersion{Revision: 1, SourceSha256: bytes.Repeat([]byte{1}, 32)}, UpdatedAt: at, Active: true, ProtectedSourceReference: bytes.Repeat([]byte{2}, 32), WorkspaceState: &wire.Document_Workspace{Workspace: &wire.WorkspaceObservation{SourceSha256: bytes.Repeat([]byte{3}, 32), UpdatedAt: at, Dirty: wire.DirtyState_DIRTY_STATE_REJECTED, Diagnostic: &wire.WorkspaceDiagnostic{Document: &wire.DocumentId{Value: "operational.main"}, ObservedSourceSha256: bytes.Repeat([]byte{3}, 32), ObservedAt: at, Code: wire.DiagnosticCode_DIAGNOSTIC_CODE_CANDIDATE_REJECTED}}}}, ActiveGeneration: &wire.ActiveGeneration{Revision: 1, ManifestSha256: bytes.Repeat([]byte{4}, 32)}, ResumeToken: string(bytes.Repeat([]byte{'x'}, 118))}
}
func TestNHA150ConfigurationDiagnosticRefusesForeignStaleUnknownAndCleanBindings(t *testing.T) {
	valid := nha150DiagnosticRead()
	if err := AdmitConfigurationResponse(valid); err != nil {
		t.Fatal("bound diagnostic refused", err)
	}
	for _, test := range []struct {
		name   string
		change func(*wire.WorkspaceObservation)
	}{
		{"foreign_document", func(w *wire.WorkspaceObservation) { w.Diagnostic.Document.Value = "bootstrap.launch" }},
		{"stale_hash", func(w *wire.WorkspaceObservation) { w.Diagnostic.ObservedSourceSha256[0] = 9 }},
		{"stale_date", func(w *wire.WorkspaceObservation) { w.Diagnostic.ObservedAt.Seconds++ }},
		{"unknown_code", func(w *wire.WorkspaceObservation) { w.Diagnostic.Code = wire.DiagnosticCode(999) }},
		{"zero_code", func(w *wire.WorkspaceObservation) {
			w.Diagnostic.Code = wire.DiagnosticCode_DIAGNOSTIC_CODE_UNSPECIFIED
		}},
		{"clean_with_error", func(w *wire.WorkspaceObservation) {
			w.Dirty = wire.DirtyState_DIRTY_STATE_CLEAN
			w.SourceSha256 = bytes.Repeat([]byte{1}, 32)
			w.Diagnostic.ObservedSourceSha256 = bytes.Repeat([]byte{1}, 32)
		}},
		{"unknown_nested", func(w *wire.WorkspaceObservation) { w.Diagnostic.ProtoReflect().SetUnknown([]byte{0x28, 0x01}) }},
		{"missing_timestamp", func(w *wire.WorkspaceObservation) { w.Diagnostic.ObservedAt = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			read := proto.Clone(valid).(*wire.ReadDocumentResponse)
			test.change(read.Document.GetWorkspace())
			if err := AdmitConfigurationResponse(read); err == nil {
				t.Fatal("incoherent diagnostic admitted")
			}
		})
	}
}
func TestNHA150UnknownDiagnosticWireStillDecodesWithoutOperationalAdmission(t *testing.T) {
	read := nha150DiagnosticRead()
	read.Document.GetWorkspace().Diagnostic.ProtoReflect().SetUnknown([]byte{0x28, 0x01})
	raw, err := proto.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	decoded := new(wire.ReadDocumentResponse)
	if err := proto.Unmarshal(raw, decoded); err != nil {
		t.Fatal("forward-compatible decoding rejected an unknown field")
	}
	if !bytes.Equal(decoded.Document.GetWorkspace().Diagnostic.ProtoReflect().GetUnknown(), []byte{0x28, 0x01}) || AdmitConfigurationResponse(decoded) == nil {
		t.Fatal("wire compatibility became operational admission")
	}
}

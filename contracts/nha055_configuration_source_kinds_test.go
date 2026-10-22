package contracts

import (
	"bytes"
	"strings"
	"testing"
	"time"

	wire "github.com/damione1/Homeward/gen/home/configuration/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type nha055ConfigurationSourceCase struct {
	name, schema, profile string
	kind                  wire.DocumentKind
}

func nha055ConfigurationSourceCases() []nha055ConfigurationSourceCase {
	return []nha055ConfigurationSourceCase{
		{"dashboard", "homeward-dashboard-1", "ha-2026.10/lovelace-1", wire.DocumentKind_DOCUMENT_KIND_DASHBOARD_SOURCE},
		{"automation", "ha-logic-1", "ha-2026.10/logic-1", wire.DocumentKind_DOCUMENT_KIND_AUTOMATION_SOURCE},
		{"script", "ha-logic-1", "ha-2026.10/logic-1", wire.DocumentKind_DOCUMENT_KIND_SCRIPT_SOURCE},
		{"scene", "ha-scene-1", "ha-2026.10/scene-1", wire.DocumentKind_DOCUMENT_KIND_SCENE_SOURCE},
		{"ownership", "homeward.heating-ownership/1", "homeward.heating-ownership-json/1", wire.DocumentKind_DOCUMENT_KIND_OWNERSHIP_SOURCE},
	}
}

func nha055ConfigurationSourceDocument(c nha055ConfigurationSourceCase) *wire.Document {
	return &wire.Document{
		Id: &wire.DocumentId{Value: "retained." + c.name}, Kind: c.kind,
		SchemaVersion: c.schema, SyntaxProfile: c.profile,
		Version:                  &wire.DocumentVersion{Revision: 2, SourceSha256: bytes.Repeat([]byte{1}, 32)},
		UpdatedAt:                timestamppb.New(time.Unix(1792314000, 0)),
		WorkspaceState:           &wire.Document_WorkspaceUnobserved{WorkspaceUnobserved: &wire.WorkspaceUnobserved{Reason: wire.WorkspaceUnobservedReason_WORKSPACE_UNOBSERVED_REASON_NO_PHYSICAL_LOCATOR}},
		ProtectedSourceReference: bytes.Repeat([]byte{2}, 32),
	}
}

func nha055ConfigurationComplete() *wire.PageContinuation {
	return &wire.PageContinuation{Result: &wire.PageContinuation_Complete{Complete: &emptypb.Empty{}}}
}

func nha055ConfigurationBinaryRoundTrip(t *testing.T, input proto.Message) proto.Message {
	t.Helper()
	raw, err := proto.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	output := input.ProtoReflect().Type().New().Interface()
	if err := proto.Unmarshal(raw, output); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(input, output) {
		t.Fatal("binary transport changed source metadata")
	}
	return output
}

// These are binary response-contract tests. They need generated bindings, but
// no SQL26 catalogue, physical source locator, live driver or executable action.
func TestNHA055ConfigurationSourceKindsBinaryReadListSchemas(t *testing.T) {
	list := &wire.ListDocumentsResponse{Continuation: nha055ConfigurationComplete()}
	schemas := &wire.ListSchemasResponse{Continuation: nha055ConfigurationComplete()}
	for i, c := range nha055ConfigurationSourceCases() {
		t.Run(c.name, func(t *testing.T) {
			if int32(c.kind) != int32(6+i) {
				t.Fatal("new wire kind renumbered")
			}
			document := nha055ConfigurationSourceDocument(c)
			read := &wire.ReadDocumentResponse{
				Document:         document,
				ActiveGeneration: &wire.ActiveGeneration{Revision: 3, ManifestSha256: bytes.Repeat([]byte{3}, 32)},
				ResumeToken:      strings.Repeat("r", 118),
			}
			if err := AdmitConfigurationResponse(nha055ConfigurationBinaryRoundTrip(t, read)); err != nil {
				t.Fatal("retained source metadata rejected after binary transport", err)
			}
			list.Documents = append(list.Documents, document)
			schemas.Schemas = append(schemas.Schemas, &wire.Schema{Kind: c.kind, SchemaVersion: c.schema, SyntaxProfile: c.profile})
		})
	}
	for _, response := range []proto.Message{list, schemas} {
		if err := AdmitConfigurationResponse(nha055ConfigurationBinaryRoundTrip(t, response)); err != nil {
			t.Fatal("five source kinds lost by list admission", err)
		}
	}
	if len(list.Documents) != 5 || len(schemas.Schemas) != 5 {
		t.Fatal("source metadata table lost an entry")
	}
}

func TestNHA055ConfigurationSourceKindsRemainClosedWithOptionalObservations(t *testing.T) {
	for _, c := range nha055ConfigurationSourceCases() {
		t.Run(c.name, func(t *testing.T) {
			mutations := []struct {
				name   string
				change func(*wire.Document)
			}{
				{"wrong-profile", func(d *wire.Document) { d.SyntaxProfile = "homeward.operational-yaml/1" }},
				{"wrong-schema", func(d *wire.Document) { d.SchemaVersion = "homeward.authorization/1" }},
				{"unknown-kind", func(d *wire.Document) { d.Kind = wire.DocumentKind(99) }},
				{"unspecified-kind", func(d *wire.Document) { d.Kind = wire.DocumentKind_DOCUMENT_KIND_UNSPECIFIED }},
				{"unknown-field", func(d *wire.Document) { d.Version.ProtoReflect().SetUnknown([]byte{0x18, 0x01}) }},
				{"incoherent-clean-observation", func(d *wire.Document) {
					d.WorkspaceState = &wire.Document_Workspace{Workspace: &wire.WorkspaceObservation{SourceSha256: bytes.Repeat([]byte{9}, 32), UpdatedAt: d.UpdatedAt, Dirty: wire.DirtyState_DIRTY_STATE_CLEAN}}
				}},
				{"missing-observation-hash", func(d *wire.Document) {
					d.WorkspaceState = &wire.Document_Workspace{Workspace: &wire.WorkspaceObservation{UpdatedAt: d.UpdatedAt, Dirty: wire.DirtyState_DIRTY_STATE_UNVALIDATED}}
				}},
			}
			for _, mutation := range mutations {
				t.Run(mutation.name, func(t *testing.T) {
					document := nha055ConfigurationSourceDocument(c)
					mutation.change(document)
					response := &wire.ListDocumentsResponse{Documents: []*wire.Document{document}, Continuation: nha055ConfigurationComplete()}
					if AdmitConfigurationResponse(nha055ConfigurationBinaryRoundTrip(t, response)) == nil {
						t.Fatal("invalid source metadata admitted")
					}
				})
			}
			for _, change := range []func(*wire.Schema){
				func(s *wire.Schema) { s.SyntaxProfile = "homeward.operational-yaml/1" },
				func(s *wire.Schema) { s.SchemaVersion = "homeward.authorization/1" },
				func(s *wire.Schema) { s.Kind = wire.DocumentKind(99) },
				func(s *wire.Schema) { s.EditableOperationalWorkspace = true },
			} {
				schema := &wire.Schema{Kind: c.kind, SchemaVersion: c.schema, SyntaxProfile: c.profile}
				change(schema)
				if AdmitConfigurationResponse(&wire.ListSchemasResponse{Schemas: []*wire.Schema{schema}, Continuation: nha055ConfigurationComplete()}) == nil {
					t.Fatal("invalid or operationally editable domain schema admitted")
				}
			}
			for _, missing := range []bool{false, true} {
				document := nha055ConfigurationSourceDocument(c)
				if missing {
					document.WorkspaceState = &wire.Document_WorkspaceUnobserved{WorkspaceUnobserved: &wire.WorkspaceUnobserved{Reason: wire.WorkspaceUnobservedReason_WORKSPACE_UNOBSERVED_REASON_SOURCE_UNAVAILABLE}}
				} else {
					document.WorkspaceState = &wire.Document_Workspace{Workspace: &wire.WorkspaceObservation{SourceSha256: bytes.Clone(document.Version.SourceSha256), UpdatedAt: document.UpdatedAt, Dirty: wire.DirtyState_DIRTY_STATE_CLEAN}}
				}
				if err := AdmitConfigurationResponse(nha055ConfigurationBinaryRoundTrip(t, &wire.ListDocumentsResponse{Documents: []*wire.Document{document}, Continuation: nha055ConfigurationComplete()})); err != nil {
					t.Fatal("valid optional source observation refused", err)
				}
			}
		})
	}
}

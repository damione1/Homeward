package contracts

import (
	"bytes"
	"strings"
	"testing"
	"time"

	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"google.golang.org/protobuf/proto"
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

func nha055ConfigurationSourceDocument(c nha055ConfigurationSourceCase) *wire.ConfigurationDocument {
	return &wire.ConfigurationDocument{Name: "homes/synthetic/configurationDocuments/r-" + c.name, Etag: "synthetic-etag",
		Id: &wire.DocumentId{Value: "retained." + c.name}, Kind: c.kind,
		SchemaVersion: c.schema, SyntaxProfile: c.profile,
		Version:                  &wire.DocumentVersion{Revision: 2, SourceSha256: bytes.Repeat([]byte{1}, 32)},
		UpdatedAt:                timestamppb.New(time.Unix(1792314000, 0)),
		WorkspaceState:           &wire.ConfigurationDocument_WorkspaceUnobserved{WorkspaceUnobserved: &wire.WorkspaceUnobserved{Reason: wire.WorkspaceUnobservedReason_WORKSPACE_UNOBSERVED_REASON_NO_PHYSICAL_LOCATOR}},
		ProtectedSourceReference: bytes.Repeat([]byte{2}, 32),
	}
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
	list := &wire.ListConfigurationDocumentsResponse{NextPageToken: ""}
	schemas := &wire.ListConfigurationSchemasResponse{NextPageToken: ""}
	for i, c := range nha055ConfigurationSourceCases() {
		t.Run(c.name, func(t *testing.T) {
			if int32(c.kind) != int32(6+i) {
				t.Fatal("new wire kind renumbered")
			}
			document := nha055ConfigurationSourceDocument(c)
			read := proto.Clone(document).(*wire.ConfigurationDocument)
			read.ActiveGeneration = &wire.ActiveGeneration{Revision: 3, ManifestSha256: bytes.Repeat([]byte{3}, 32)}
			read.ResumeToken = strings.Repeat("r", 118)
			if err := AdmitConfigurationResponse(nha055ConfigurationBinaryRoundTrip(t, read)); err != nil {
				t.Fatal("retained source metadata rejected after binary transport", err)
			}
			list.Documents = append(list.Documents, document)
			schemas.Schemas = append(schemas.Schemas, &wire.ConfigurationSchema{Name: "homes/synthetic/configurationSchemas/s-" + c.name, Kind: c.kind, SchemaVersion: c.schema, SyntaxProfile: c.profile})
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
				change func(*wire.ConfigurationDocument)
			}{
				{"wrong-profile", func(d *wire.ConfigurationDocument) { d.SyntaxProfile = "homeward.operational-yaml/1" }},
				{"wrong-schema", func(d *wire.ConfigurationDocument) { d.SchemaVersion = "homeward.authorization/1" }},
				{"unknown-kind", func(d *wire.ConfigurationDocument) { d.Kind = wire.DocumentKind(99) }},
				{"unspecified-kind", func(d *wire.ConfigurationDocument) { d.Kind = wire.DocumentKind_DOCUMENT_KIND_UNSPECIFIED }},
				{"unknown-field", func(d *wire.ConfigurationDocument) { d.Version.ProtoReflect().SetUnknown([]byte{0x18, 0x01}) }},
				{"incoherent-clean-observation", func(d *wire.ConfigurationDocument) {
					d.WorkspaceState = &wire.ConfigurationDocument_Workspace{Workspace: &wire.WorkspaceObservation{SourceSha256: bytes.Repeat([]byte{9}, 32), UpdatedAt: d.UpdatedAt, Dirty: wire.DirtyState_DIRTY_STATE_CLEAN}}
				}},
				{"missing-observation-hash", func(d *wire.ConfigurationDocument) {
					d.WorkspaceState = &wire.ConfigurationDocument_Workspace{Workspace: &wire.WorkspaceObservation{UpdatedAt: d.UpdatedAt, Dirty: wire.DirtyState_DIRTY_STATE_UNVALIDATED}}
				}},
			}
			for _, mutation := range mutations {
				t.Run(mutation.name, func(t *testing.T) {
					document := nha055ConfigurationSourceDocument(c)
					mutation.change(document)
					response := &wire.ListConfigurationDocumentsResponse{Documents: []*wire.ConfigurationDocument{document}, NextPageToken: ""}
					if AdmitConfigurationResponse(nha055ConfigurationBinaryRoundTrip(t, response)) == nil {
						t.Fatal("invalid source metadata admitted")
					}
				})
			}
			for _, change := range []func(*wire.ConfigurationSchema){
				func(s *wire.ConfigurationSchema) { s.SyntaxProfile = "homeward.operational-yaml/1" },
				func(s *wire.ConfigurationSchema) { s.SchemaVersion = "homeward.authorization/1" },
				func(s *wire.ConfigurationSchema) { s.Kind = wire.DocumentKind(99) },
				func(s *wire.ConfigurationSchema) { s.EditableOperationalWorkspace = true },
			} {
				schema := &wire.ConfigurationSchema{Name: "homes/synthetic/configurationSchemas/s-" + c.name, Kind: c.kind, SchemaVersion: c.schema, SyntaxProfile: c.profile}
				change(schema)
				if AdmitConfigurationResponse(&wire.ListConfigurationSchemasResponse{Schemas: []*wire.ConfigurationSchema{schema}, NextPageToken: ""}) == nil {
					t.Fatal("invalid or operationally editable domain schema admitted")
				}
			}
			for _, missing := range []bool{false, true} {
				document := nha055ConfigurationSourceDocument(c)
				if missing {
					document.WorkspaceState = &wire.ConfigurationDocument_WorkspaceUnobserved{WorkspaceUnobserved: &wire.WorkspaceUnobserved{Reason: wire.WorkspaceUnobservedReason_WORKSPACE_UNOBSERVED_REASON_SOURCE_UNAVAILABLE}}
				} else {
					document.WorkspaceState = &wire.ConfigurationDocument_Workspace{Workspace: &wire.WorkspaceObservation{SourceSha256: bytes.Clone(document.Version.SourceSha256), UpdatedAt: document.UpdatedAt, Dirty: wire.DirtyState_DIRTY_STATE_CLEAN}}
				}
				if err := AdmitConfigurationResponse(nha055ConfigurationBinaryRoundTrip(t, &wire.ListConfigurationDocumentsResponse{Documents: []*wire.ConfigurationDocument{document}, NextPageToken: ""})); err != nil {
					t.Fatal("valid optional source observation refused", err)
				}
			}
		})
	}
}

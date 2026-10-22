package server

import (
	"bytes"
	"testing"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/configuration/v1"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/scope"
)

// Requires the real migration chain through SQL26, after 036/25. It neither
// installs future DDL inline nor skips an unavailable schema. The actual Run
// fixture has no physical driver and this test only stores retained metadata;
// it never publishes an executable program or dispatches a command.
func TestNHA055SQL26ActualRunTLSReadListSchemasPreserveSourceKinds(t *testing.T) {
	f := nha149StartTLS(t)
	store := f.run.observed.sessions.store
	before := f.read(t)
	beforeAttempts, beforeCommands := nha150Counts(t, store)
	if beforeCommands != 0 {
		t.Fatal("isolated source metadata fixture already dispatched a command")
	}
	cases := []struct {
		name, path, schema, profile string
		kind                        wire.DocumentKind
		raw                         []byte
		describe                    func(string, string) (storage.SourceDescriptor, error)
	}{
		{"dashboard", "source-kinds/dashboard.yaml", "homeward-dashboard-1", "ha-2026.10/lovelace-1", wire.DocumentKind_DOCUMENT_KIND_DASHBOARD_SOURCE, []byte("views: []\n"), storage.DashboardSourceDescriptor},
		{"automation", "source-kinds/automation.yaml", "ha-logic-1", "ha-2026.10/logic-1", wire.DocumentKind_DOCUMENT_KIND_AUTOMATION_SOURCE, []byte("[]\n"), storage.AutomationSourceDescriptor},
		{"script", "source-kinds/script.yaml", "ha-logic-1", "ha-2026.10/logic-1", wire.DocumentKind_DOCUMENT_KIND_SCRIPT_SOURCE, []byte("{}\n"), storage.ScriptSourceDescriptor},
		{"scene", "source-kinds/scene.yaml", "ha-scene-1", "ha-2026.10/scene-1", wire.DocumentKind_DOCUMENT_KIND_SCENE_SOURCE, []byte("[]\n"), storage.SceneSourceDescriptor},
		{"ownership", "source-kinds/ownership.json", "homeward.heating-ownership/1", "homeward.heating-ownership-json/1", wire.DocumentKind_DOCUMENT_KIND_OWNERSHIP_SOURCE, scope.HeatingOwnershipV1(), storage.OwnershipSourceDescriptor},
	}
	author, err := storage.NewConfigAuthor("NHA055 source metadata fixture")
	if err != nil {
		t.Fatal(err)
	}
	source, err := storage.NewConfigSource("isolated source-kind transport fixture")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := storage.NewConfigVersionMetadata(author, source)
	if err != nil {
		t.Fatal(err)
	}
	refs := make(map[string]storage.ConfigVersionRef, len(cases))
	hashes := make(map[string][32]byte, len(cases))
	outcome, err := store.InConfigTransactionOutcome(f.ctx, func(tx *storage.ConfigTx) error {
		for _, c := range cases {
			id, err := storage.NewConfigDocumentID("retained.source-kind." + c.name)
			if err != nil {
				return err
			}
			descriptor, err := c.describe("source-kind."+c.name, c.path)
			if err != nil {
				return err
			}
			var binding storage.ConfigDocumentBinding
			var file parser.SourceFile
			if c.kind == wire.DocumentKind_DOCUMENT_KIND_OWNERSHIP_SOURCE {
				binding, err = storage.NewJSONConfigDocumentBinding(id, c.path)
				if err == nil {
					file, err = parser.NewJSONSourceFile(c.path, c.raw)
				}
			} else {
				binding, err = storage.NewConfigDocumentBinding(id, c.path)
				if err == nil {
					file, err = parser.NewSourceFile(c.path, c.raw)
				}
			}
			if err != nil {
				return err
			}
			ref, err := tx.SaveEditableSource(descriptor, binding, file, metadata)
			if err != nil {
				return err
			}
			refs[id.String()], hashes[id.String()] = ref, file.SHA256()
		}
		return nil
	})
	if err != nil || outcome != storage.TransactionCommitted {
		t.Fatal("real source persistence requires admitted SQL26", outcome, err)
	}
	assertDocument := func(t *testing.T, document *wire.Document, expectedKind wire.DocumentKind, schema, profile string) {
		t.Helper()
		if document == nil || document.Id == nil || document.Version == nil {
			t.Fatal("incomplete retained document response")
		}
		ref, known := refs[document.Id.Value]
		hash := hashes[document.Id.Value]
		if !known || document.Kind != expectedKind || document.SchemaVersion != schema || document.SyntaxProfile != profile || document.Version.Revision != ref.Revision().Number() || !bytes.Equal(document.Version.SourceSha256, hash[:]) || document.Active || len(document.ProtectedSourceReference) != 32 {
			t.Fatal("TLS document lost exact SQL identity, kind, schema or retained revision")
		}
		if document.GetWorkspace() != nil || document.GetWorkspaceUnobserved() == nil || document.GetWorkspaceUnobserved().Reason != wire.WorkspaceUnobservedReason_WORKSPACE_UNOBSERVED_REASON_NO_PHYSICAL_LOCATOR {
			t.Fatal("retained source was given an unsupported physical workspace observation")
		}
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			response, err := f.api.ReadDocument(f.ctx, nha056SessionRequest(&wire.ReadDocumentRequest{Id: &wire.DocumentId{Value: "retained.source-kind." + c.name}}, f.token))
			if err != nil {
				t.Fatal("actual Run/TLS retained source read", err)
			}
			if err := contracts.AdmitConfigurationResponse(response.Msg); err != nil {
				t.Fatal(err)
			}
			assertDocument(t, response.Msg.Document, c.kind, c.schema, c.profile)
		})
	}
	listed := make(map[string]*wire.Document)
	page := &wire.PageRequest{PageSize: 2}
	for n := 0; ; n++ {
		if n >= 32 {
			t.Fatal("document pagination did not terminate")
		}
		response, err := f.api.ListDocuments(f.ctx, nha056SessionRequest(&wire.ListDocumentsRequest{Page: page}, f.token))
		if err != nil {
			t.Fatal("actual TLS source-kind list", err)
		}
		if err := contracts.AdmitConfigurationResponse(response.Msg); err != nil {
			t.Fatal(err)
		}
		for _, document := range response.Msg.Documents {
			if _, duplicate := listed[document.Id.Value]; duplicate {
				t.Fatal("document repeated across pages")
			}
			listed[document.Id.Value] = document
		}
		if response.Msg.Continuation.GetComplete() != nil {
			break
		}
		token := response.Msg.Continuation.GetNextPageToken()
		if token == "" {
			t.Fatal("missing document continuation")
		}
		page = &wire.PageRequest{PageSize: 2, PageToken: &token}
	}
	for _, c := range cases {
		assertDocument(t, listed["retained.source-kind."+c.name], c.kind, c.schema, c.profile)
	}
	schemas := make(map[wire.DocumentKind]*wire.Schema)
	page = &wire.PageRequest{PageSize: 1}
	for n := 0; ; n++ {
		if n >= 32 {
			t.Fatal("schema pagination did not terminate")
		}
		response, err := f.api.ListSchemas(f.ctx, nha056SessionRequest(&wire.ListSchemasRequest{Page: page}, f.token))
		if err != nil {
			t.Fatal("actual TLS admitted SQL schemas", err)
		}
		if err := contracts.AdmitConfigurationResponse(response.Msg); err != nil {
			t.Fatal(err)
		}
		for _, schema := range response.Msg.Schemas {
			// Legacy declarative-1/2 legitimately have the same document kind.
			if schema.Kind >= wire.DocumentKind_DOCUMENT_KIND_DASHBOARD_SOURCE {
				if _, duplicate := schemas[schema.Kind]; duplicate {
					t.Fatal("source schema repeated across pages")
				}
				schemas[schema.Kind] = schema
			}
		}
		if response.Msg.Continuation.GetComplete() != nil {
			break
		}
		token := response.Msg.Continuation.GetNextPageToken()
		if token == "" {
			t.Fatal("missing schema continuation")
		}
		page = &wire.PageRequest{PageSize: 1, PageToken: &token}
	}
	for _, c := range cases {
		schema := schemas[c.kind]
		if schema == nil || schema.SchemaVersion != c.schema || schema.SyntaxProfile != c.profile || schema.EditableOperationalWorkspace {
			t.Fatal("schema list lost a declared source kind or widened workspace authority", c.name)
		}
	}
	after := f.read(t)
	if after.ActiveGeneration.Revision != before.ActiveGeneration.Revision || after.Document.Version.Revision != before.Document.Version.Revision || !bytes.Equal(after.Document.Version.SourceSha256, before.Document.Version.SourceSha256) {
		t.Fatal("metadata transport changed operational activation")
	}
	afterAttempts, afterCommands := nha150Counts(t, store)
	if afterAttempts != beforeAttempts || afterCommands != 0 {
		t.Fatal("source metadata storage/read produced an activation or command")
	}
}

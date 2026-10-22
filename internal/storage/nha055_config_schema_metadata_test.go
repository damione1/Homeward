package storage

import "testing"

func TestNHA055ConfigSchemaEntriesExpandSharedLogicLabel(t *testing.T) {
	cases := []struct {
		schema, profile string
		kinds           []ConfigDocumentKind
	}{
		{"homeward-dashboard-1", "ha-2026.10/lovelace-1", []ConfigDocumentKind{ConfigDocumentKindDashboardSource}},
		{"ha-logic-1", "ha-2026.10/logic-1", []ConfigDocumentKind{ConfigDocumentKindAutomationSource, ConfigDocumentKindScriptSource}},
		{"ha-scene-1", "ha-2026.10/scene-1", []ConfigDocumentKind{ConfigDocumentKindSceneSource}},
		{"homeward.heating-ownership/1", "homeward.heating-ownership-json/1", []ConfigDocumentKind{ConfigDocumentKindOwnershipSource}},
	}
	for _, c := range cases {
		t.Run(c.schema, func(t *testing.T) {
			entries, ok := configPublicSchemaEntries(c.schema)
			if !ok || len(entries) != len(c.kinds) {
				t.Fatal("installed source schema omitted a declared document kind")
			}
			for i, entry := range entries {
				if entry.Kind() != c.kinds[i] || entry.SchemaVersion() != c.schema || entry.SyntaxProfile() != c.profile {
					t.Fatal("public schema differs from the admitted source format")
				}
				if !configPublicSchema(entry.Kind(), entry.SchemaVersion(), entry.SyntaxProfile()) || configPublicSchema(entry.Kind(), entry.SchemaVersion(), "homeward.operational-yaml/1") {
					t.Fatal("kind/profile admission widened or rejected the source pair")
				}
			}
			entries[0] = ConfigSchemaMetadata{}
			again, ok := configPublicSchemaEntries(c.schema)
			if !ok || again[0].Kind() != c.kinds[0] {
				t.Fatal("caller mutation altered schema admission")
			}
		})
	}
	if entries, ok := configPublicSchemaEntries("ha-future-99"); ok || len(entries) != 0 {
		t.Fatal("unknown SQL schema was projected as a known source format")
	}
}

func TestNHA055ConfigSchemaEntriesPreserveExistingSchemas(t *testing.T) {
	for _, schema := range []string{"ha-declarative-1", "ha-declarative-2", "homeward.authorization/1", "homeward.mfa-policy/1", "homeward.operational/1", "homeward.bootstrap-launch/1"} {
		before, ok := configPublicSchemaMetadata(schema)
		after, expanded := configPublicSchemaEntries(schema)
		if !ok || !expanded || len(after) != 1 || after[0] != before {
			t.Fatal("existing schema metadata changed", schema)
		}
	}
}

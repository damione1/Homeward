package storage

import (
	"errors"
	"testing"
)

func TestNHA149BoundedMetadataPagesDistinguishProtectedAndPurgedHistory(t *testing.T) {
	f := nha029HistoryFixture(t, 256, nil)
	page, err := f.store.ReadConfigDocumentPage(f.ctx, "", 1, [32]byte{})
	if err != nil || len(page.Documents()) != 1 || !page.Cut().Valid() {
		t.Fatal("bounded metadata cut failed", err)
	}
	metadata, err := f.store.ReadConfigDocument(f.ctx, f.binding.ID())
	if err != nil || metadata.Kind() != ConfigDocumentKindOperationalYAML || metadata.SchemaVersion() != configOperationalSchema || metadata.SyntaxProfile() != configOperationalProfile || metadata.SourceSHA256() != f.active.hash || metadata.Ref() != f.active.ref || !metadata.Active() {
		t.Fatal("exact public document metadata is incoherent", err)
	}
	if updated, present := metadata.UpdatedAt(); !present || updated.IsZero() {
		t.Fatal("missing accepted timestamp")
	}
	oldCut := page.Cut().SHA256()
	// Registering history may reuse exact retained versions without changing
	// document revisions or the active pointer. It must still stale page cuts.
	outcome, err := f.store.InConfigTransactionOutcome(f.ctx, func(tx *ConfigTx) error {
		_, err := tx.RegisterCompiledGeneration(f.binding.Path(), f.active.hash, []ConfigVersionRef{f.active.ref})
		return err
	})
	if err != nil || outcome != TransactionCommitted {
		t.Fatal("retained-ref history registration failed", err)
	}
	_, err = f.store.ReadConfigGenerationPage(f.ctx, 0, 1, oldCut)
	var changed *Error
	if !errors.As(err, &changed) || changed.Kind() != ConfigConflict {
		t.Fatal("history page blended registration reusing unchanged document refs", err)
	}
	for n := 0; n < 22; n++ {
		f.active = f.write(t, byte('C'+n))
	}
	_, err = f.store.ReadConfigDocumentPage(f.ctx, "", 1, oldCut)
	var failure *Error
	if !errors.As(err, &failure) || failure.Kind() != ConfigConflict {
		t.Fatal("continuation blended concurrent metadata/retention cuts", err)
	}
	versions, err := f.store.ReadConfigVersionPage(f.ctx, f.binding.ID(), 0, 100, [32]byte{})
	if err != nil || versions.More() {
		t.Fatal("retained/pruned page failed", err)
	}
	retained, purged := 0, 0
	for _, version := range versions.Documents() {
		if version.Retained() {
			retained++
			if _, present := version.UpdatedAt(); !present || version.SchemaVersion() != configOperationalSchema {
				t.Fatal("retained metadata missing")
			}
		} else {
			purged++
			if _, present := version.UpdatedAt(); present || version.SchemaVersion() != "" {
				t.Fatal("purged metadata invented timestamp/schema")
			}
		}
	}
	if retained != configVersionLimit || purged < 1 {
		t.Fatalf("retention classification: retained=%d purged=%d", retained, purged)
	}
	generations, err := f.store.ReadConfigGenerationPage(f.ctx, 0, 100, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	partial, complete := false, false
	for _, generation := range generations.Generations() {
		if generation.Revision() == f.first.generation {
			partial = generation.Availability() == ConfigGenerationIncomplete
		}
		if generation.Revision() == f.active.generation {
			complete = generation.Availability() == ConfigGenerationComplete
		}
	}
	if !partial || !complete {
		t.Fatal("generation pages hid partial pruning")
	}
	schemas, err := f.store.ReadConfigSchemas(f.ctx)
	if err != nil || len(schemas) < 6 {
		t.Fatal("authoritative schema enum catalogue missing", err)
	}
	for _, schema := range schemas {
		if !configPublicSchema(schema.Kind(), schema.SchemaVersion(), schema.SyntaxProfile()) {
			t.Fatal("schema/profile catalogue drift")
		}
	}
}
func TestNHA149MetadataPageBoundsAndZeroValuesFailClosed(t *testing.T) {
	s, ctx := nha151Store(t)
	for _, size := range []int{-1, 0, 101} {
		if _, err := s.ReadConfigDocumentPage(ctx, "", size, [32]byte{}); err == nil {
			t.Fatal("unbounded page admitted")
		}
	}
	if _, err := s.ReadConfigDocument(ctx, ConfigDocumentID{}); err == nil {
		t.Fatal("zero document identity admitted")
	}
	if _, err := s.ReadConfigVersionPage(ctx, ConfigDocumentID{}, 0, 1, [32]byte{}); err == nil {
		t.Fatal("zero version-page identity admitted")
	}
	if _, err := s.ReadConfigGenerationPage(ctx, -1, 1, [32]byte{}); err == nil {
		t.Fatal("negative generation cursor admitted")
	}
}

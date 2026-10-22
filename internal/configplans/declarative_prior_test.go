package configplans

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/configcompilation"
	"github.com/damione1/Homeward/internal/storage"
)

// Future SQL26 storage/read-side fixture: it registers real source refs and
// locators, but does not claim runtime publication from synthetic compiled IDs.
func TestNHA055DeclarativePriorUsesCurrentGroupEntrySchemaAndDocumentRevision(t *testing.T) {
	service, ctx := nha055RollbackDocuments(t)
	store := service.documents.Store()
	author, _ := storage.NewConfigAuthor("nha055:declarative-locator")
	source, _ := storage.NewConfigSource("nha055:read-side-fixture")
	metadata, err := storage.NewConfigVersionMetadata(author, source)
	if err != nil {
		t.Fatal(err)
	}
	write := func(family Family, groupName, document, path, raw string) (storage.ConfigGenerationSnapshot, storage.ConfigVersionRef, Candidate) {
		t.Helper()
		file, err := parser.NewSourceFile(path, []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		candidate, err := NewCandidate(family, groupName, path, []parser.SourceFile{file})
		if err != nil {
			t.Fatal(err)
		}
		var compiled bundle.Candidate
		if family == Declarative1 {
			compiled, err = bundle.CompileSources(path, []parser.SourceFile{file})
		} else {
			compiled, err = bundle.CompileSourcesV2(path, []parser.SourceFile{file})
		}
		if err != nil {
			t.Fatal(err)
		}
		group, _ := storage.NewConfigDocumentID(groupName)
		locator, err := storage.NewDeclarativeWorkspaceLocator(group)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := storage.NewConfigDocumentID(document)
		binding, err := storage.NewConfigDocumentBinding(id, path)
		if err != nil {
			t.Fatal(err)
		}
		active, present, err := store.ReadActiveConfig(ctx)
		if err != nil || !present {
			t.Fatal(err)
		}
		var operational storage.ConfigVersionRef
		for _, version := range active.Versions() {
			kind, present := version.Kind()
			if present && kind == storage.ConfigDocumentKindOperationalYAML {
				operational = version.Ref()
			}
		}
		if !operational.Valid() {
			t.Fatal("missing protected operational ref")
		}
		expected, err := storage.ExpectConfigActive(active.Revision(), active.ManifestSHA256())
		if err != nil {
			t.Fatal(err)
		}
		attempt, err := storage.RandomActivationAttemptID()
		if err != nil {
			t.Fatal(err)
		}
		var generation storage.ConfigGenerationRevision
		var entryRef storage.ConfigVersionRef
		outcome, err := store.InConfigTransactionOutcome(ctx, func(tx *storage.ConfigTx) error {
			refs, err := tx.SaveCandidate(compiled, []storage.ConfigDocumentBinding{binding}, metadata)
			if err != nil {
				return err
			}
			entryRef = refs[0]
			refs = append(refs, operational)
			generation, err = tx.RegisterCompiledGenerationWithWorkspace(path, sha256.Sum256([]byte(groupName+raw)), refs, locator, configcompilation.Context{})
			if err != nil {
				return err
			}
			if err := tx.ActivateGeneration(expected, generation); err != nil {
				return err
			}
			return tx.RecordActivationAttempt(attempt, metadata)
		})
		if err != nil || outcome != storage.TransactionCommitted {
			t.Fatal("declarative fixture registration failed", outcome, err)
		}
		snapshot, err := store.ReadConfigGeneration(ctx, generation)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot, entryRef, candidate
	}
	old, _, targetA := write(Declarative1, "workspace.group-a", "document.entry-a", "entry-a.yaml", "# exact A\ninput_boolean: {a: {initial: false}}\n")
	current, currentRef, currentB := write(Declarative2, "workspace.group-b", "document.entry-b", "entry-b.yaml", "# exact B\ninput_boolean: {b: {initial: true}}\n")
	prior, revision, err := capturedDeclarativeCandidate(targetA, current)
	if err != nil || !prior.Valid() || prior.family != Declarative2 || prior.identity != "workspace.group-b" || prior.entry != "entry-b.yaml" || revision != currentRef.Revision().Number() || prior.hash != currentB.hash || prior.hash == targetA.hash {
		t.Fatal("target A relabelled the current group/entry/schema/ref", err)
	}
	if prior.identity == currentRef.Document().String() || !bytes.Equal(prior.files[0].Bytes(), currentB.files[0].Bytes()) {
		t.Fatal("group namespace was guessed from the document ID or historical bytes")
	}
	restoredA, _, err := capturedDeclarativeCandidate(currentB, old)
	if err != nil || restoredA.hash != targetA.hash || restoredA.entry != targetA.entry || restoredA.identity != targetA.identity {
		t.Fatal("retained target A lost its exact descriptor", err)
	}
	if targetA.entry != "entry-a.yaml" || targetA.identity != "workspace.group-a" {
		t.Fatal("capture mutated rollback target A")
	}
}

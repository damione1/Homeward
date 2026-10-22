package configplans

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/configcompilation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/storage"
)

// Future SQL26 storage/read-side fixture. Actual immutable versions, complete
// generation manifests and current SQL CAS are exercised. Synthetic compiled
// digests do not claim a published Owner runtime; the authenticated composed
// 149/029 rollback publication and zero-action oracle remain separate gates.
func TestNHA055RollbackDefinitionsSQLHistoryCurrentCASAndEntryBToA(t *testing.T) {
	for _, targetFamily := range []Family{Declarative1, Declarative2} {
		t.Run(targetFamily.Schema(), func(t *testing.T) {
			service, ctx := nha055RollbackDocuments(t)
			store := service.documents.Store()
			author, _ := storage.NewConfigAuthor("nha055:definition-rollback")
			source, _ := storage.NewConfigSource("nha055:definition-history-fixture")
			metadata, err := storage.NewConfigVersionMetadata(author, source)
			if err != nil {
				t.Fatal(err)
			}
			type storedBundle struct {
				snapshot  storage.ConfigGenerationSnapshot
				refs      []storage.ConfigVersionRef
				candidate bundle.Candidate
				bindings  []storage.ConfigDocumentBinding
			}
			write := func(family Family, groupName, tag, comment string, activate bool) storedBundle {
				t.Helper()
				entry, err := parser.NewSourceFile("entry-"+tag+".yaml", []byte(comment+"input_boolean: !include parts/"+tag+".yaml\n"))
				if err != nil {
					t.Fatal(err)
				}
				part, err := parser.NewSourceFile("parts/"+tag+".yaml", []byte(comment+"flag: {initial: false}\n"))
				if err != nil {
					t.Fatal(err)
				}
				var candidate bundle.Candidate
				if family == Declarative1 {
					candidate, err = bundle.CompileSources(entry.Path(), []parser.SourceFile{entry, part})
				} else {
					candidate, err = bundle.CompileSourcesV2(entry.Path(), []parser.SourceFile{entry, part})
				}
				if err != nil {
					t.Fatal(err)
				}
				group, err := storage.NewConfigDocumentID(groupName)
				if err != nil {
					t.Fatal(err)
				}
				locator, err := storage.NewDeclarativeWorkspaceLocator(group)
				if err != nil {
					t.Fatal(err)
				}
				bindings := make([]storage.ConfigDocumentBinding, 0, 2)
				for index, file := range candidate.Sources() {
					id, err := storage.NewConfigDocumentID(fmt.Sprintf("document.%s.%d", tag, index))
					if err != nil {
						t.Fatal(err)
					}
					binding, err := storage.NewConfigDocumentBinding(id, file.Path())
					if err != nil {
						t.Fatal(err)
					}
					bindings = append(bindings, binding)
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
					t.Fatal("fixture operational source missing")
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
				var refs []storage.ConfigVersionRef
				outcome, err := store.InConfigTransactionOutcome(ctx, func(tx *storage.ConfigTx) error {
					var err error
					refs, err = tx.SaveCandidate(candidate, bindings, metadata)
					if err != nil {
						return err
					}
					all := append(append([]storage.ConfigVersionRef(nil), refs...), operational)
					generation, err = tx.RegisterCompiledGenerationWithWorkspace(candidate.Entry(), sha256.Sum256([]byte(groupName+comment)), all, locator, configcompilation.Context{})
					if err != nil {
						return err
					}
					if !activate {
						return nil
					}
					if err := tx.ActivateGeneration(expected, generation); err != nil {
						return err
					}
					return tx.RecordActivationAttempt(attempt, metadata)
				})
				if err != nil || outcome != storage.TransactionCommitted {
					t.Fatal("real history registration failed", outcome, err)
				}
				snapshot, err := store.ReadConfigGeneration(ctx, generation)
				if err != nil {
					t.Fatal(err)
				}
				return storedBundle{snapshot: snapshot, refs: refs, candidate: candidate, bindings: bindings}
			}
			old := write(targetFamily, "workspace.a", "a", "# exact historical A\n", true)
			currentFamily := Declarative2
			if targetFamily == Declarative2 {
				currentFamily = Declarative1
			}
			current := write(currentFamily, "workspace.b", "b", "# current active B\n", true)
			latestA := write(targetFamily, "workspace.a", "a", "# newer immutable A never activated\n", false)
			expected := make([]configdocuments.ExpectedSource, 0, len(latestA.refs))
			for index, ref := range latestA.refs {
				file := latestA.candidate.Sources()[index]
				value, err := configdocuments.NewExpectedSource(latestA.bindings[index], ref.Revision(), file.SHA256(), false, configdocuments.ExpectAbsentMember())
				if err != nil {
					t.Fatal(err)
				}
				expected = append(expected, value)
				if err := store.CheckSourceBinding(ctx, latestA.bindings[index], storage.ConfigDocumentKindYAMLSource, old.refs[index].Revision(), old.candidate.Sources()[index].SHA256(), false); err == nil {
					t.Fatal("historical ref was accepted as current immutable SQL CAS")
				} else {
					var failure *storage.Error
					if !errors.As(err, &failure) || failure.Kind() != storage.ConfigConflict {
						t.Fatal("wrong CAS failure", err)
					}
				}
				if err := store.CheckSourceBinding(ctx, latestA.bindings[index], storage.ConfigDocumentKindYAMLSource, ref.Revision(), file.SHA256(), false); err != nil {
					t.Fatal("current SQL version was confused with active-generation membership", err)
				}
			}
			// Historical legacy manifests legitimately predate any physical
			// workspace locator. Only an explicit request namespace may bind one.
			var legacyGeneration storage.ConfigGenerationRevision
			outcome, err := store.InConfigTransactionOutcome(ctx, func(tx *storage.ConfigTx) error {
				var err error
				legacyGeneration, err = tx.RegisterGeneration(old.candidate, old.refs)
				return err
			})
			if err != nil || outcome != storage.TransactionCommitted {
				t.Fatal("legacy exact-history fixture failed", outcome, err)
			}
			legacy, err := store.ReadConfigGeneration(ctx, legacyGeneration)
			if err != nil {
				t.Fatal(err)
			}
			if _, present := legacy.DeclarativeWorkspace().Group(); present {
				t.Fatal("legacy manifest acquired an invented locator")
			}
			explicit, err := rollbackDefinitionsFromSnapshot(targetFamily, "workspace.explicit.new", legacy, expected)
			if err != nil || explicit.identity != "workspace.explicit.new" || explicit.entry != old.candidate.Entry() {
				t.Fatal("legacy history did not retain the explicit request namespace", err)
			}
			target, err := rollbackDefinitionsFromSnapshot(targetFamily, "workspace.a", old.snapshot, expected)
			if err != nil || target.entry != old.candidate.Entry() || target.identity != "workspace.a" || target.family != targetFamily || len(target.files) != len(old.refs) {
				t.Fatal("exact A target lost descriptor/members", err)
			}
			for _, file := range target.files {
				found := false
				for _, original := range old.candidate.Sources() {
					if file.Path() == original.Path() {
						found = bytes.Equal(file.Bytes(), original.Bytes()) && file.SHA256() == original.SHA256()
					}
				}
				if !found {
					t.Fatal("target borrowed newer unactivated SQL bytes")
				}
			}
			prior, _, err := capturedDeclarativeCandidate(target, current.snapshot)
			if err != nil || prior.entry != current.candidate.Entry() || prior.identity != "workspace.b" || prior.family != currentFamily {
				t.Fatal("rollback A target relabelled current B", err)
			}
			if _, err := rollbackDefinitionsFromSnapshot(targetFamily, "workspace.b", old.snapshot, expected); !errors.Is(err, ErrInvalid) {
				t.Fatal("rollback silently changed a recorded physical group", err)
			}
			before, present, err := store.ReadActiveConfig(ctx)
			if err != nil || !present {
				t.Fatal(err)
			}
			wrongManifest := old.snapshot.ManifestSHA256()
			wrongManifest[0] ^= 1
			result, err := service.RollbackDefinitions(ctx, targetFamily, "workspace.a", configdocuments.ExpectedDocument{}, expected, old.snapshot.Revision(), wrongManifest, author)
			if !errors.Is(err, ErrStale) || result.Attempt().Valid() {
				t.Fatal("manifest mismatch entered current apply", err)
			}
			// Purge A's exact retained versions while active B remains protected.
			for n := 0; n < 22; n++ {
				write(targetFamily, "workspace.a", "a", fmt.Sprintf("# later A %d\n", n), false)
			}
			purged, err := store.ReadConfigGeneration(ctx, old.snapshot.Revision())
			if err != nil || purged.Availability() != storage.ConfigGenerationIncomplete {
				t.Fatal("fixture did not collect exact historical versions", err)
			}
			if candidate, err := rollbackDefinitionsFromSnapshot(targetFamily, "workspace.a", purged, expected); !errors.Is(err, ErrUnavailable) || candidate.Valid() {
				t.Fatal("purged A target used latest bytes", err)
			}
			result, err = service.RollbackDefinitions(ctx, targetFamily, "workspace.a", configdocuments.ExpectedDocument{}, expected, old.snapshot.Revision(), old.snapshot.ManifestSHA256(), author)
			if !errors.Is(err, ErrUnavailable) || result.Attempt().Valid() {
				t.Fatal("purged history entered current writer", err)
			}
			after, present, err := store.ReadActiveConfig(ctx)
			if err != nil || !present || after.Revision() != before.Revision() || after.ManifestSHA256() != before.ManifestSHA256() {
				t.Fatal("history refusal moved active authority", err)
			}
		})
	}
}

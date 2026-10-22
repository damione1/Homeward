package configdocuments

import (
	"bytes"
	"fmt"
	"os"
	"testing"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/configcompilation"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
)

// FUTURE SQL26 ONLY: normal, unskipped test requiring the real25 parent and
// actually registered26. It qualifies immutable locator persistence, exact
// observation and real bundle compilers, not a runtime publication gate.
func TestNHA055FutureSQL26DeclarativeWatcherUsesOneAdmittedGroupAcrossEntryChanges(t *testing.T) {
	service, ctx, operational, operationalRaw := nha055Documents(t)
	active, present, err := service.store.ReadActiveConfig(ctx)
	if err != nil || !present {
		t.Fatal(err)
	}
	if _, present, err := declarativeSourceWatchTarget(active); err != nil || present {
		t.Fatal("legacy absence invented a declarative root", err)
	}
	author, err := storage.NewConfigAuthor("nha055:declarative-watcher-fixture")
	if err != nil {
		t.Fatal(err)
	}
	origin, err := storage.NewConfigSource("nha055:declarative-watcher-fixture")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := storage.NewConfigVersionMetadata(author, origin)
	if err != nil {
		t.Fatal(err)
	}
	// SQL document identity belongs to its kind/path, across both entry changes
	// and schema profiles. Only the immutable version and group locator change.
	bindingsByPath := make(map[string]storage.ConfigDocumentBinding, 3)
	for index, path := range []string{"entry-a.yaml", "entry-b.yaml", "parts/boolean.yaml"} {
		bindingsByPath[path] = nha055ObservedBinding(t, fmt.Sprintf("nha055.declarative.document.%d", index), path)
	}
	for _, variant := range []sourceWatchVariant{sourceWatchDeclarative1, sourceWatchDeclarative2} {
		group, err := storage.NewConfigDocumentID(fmt.Sprintf("nha055.declarative.group.%d", variant))
		if err != nil {
			t.Fatal(err)
		}
		locator, err := storage.NewDeclarativeWorkspaceLocator(group)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range []string{"entry-a.yaml", "entry-b.yaml"} {
			entryFile, err := parser.NewSourceFile(entry, []byte("input_boolean: !include parts/boolean.yaml\n"))
			if err != nil {
				t.Fatal(err)
			}
			includeFile, err := parser.NewSourceFile("parts/boolean.yaml", []byte("ready: {initial: false}\n"))
			if err != nil {
				t.Fatal(err)
			}
			files := []parser.SourceFile{entryFile, includeFile}
			var candidate bundle.Candidate
			if variant == sourceWatchDeclarative1 {
				candidate, err = bundle.CompileSources(entry, files)
			} else {
				candidate, err = bundle.CompileSourcesV2(entry, files)
			}
			if err != nil {
				t.Fatal("real declarative compiler", err)
			}
			bindings := make([]storage.ConfigDocumentBinding, 0, len(files))
			for _, file := range files {
				binding, known := bindingsByPath[file.Path()]
				if !known {
					t.Fatal("fixture source has no stable document identity")
				}
				if binding.ID() == group {
					t.Fatal("fixture confused group and document identity")
				}
				bindings = append(bindings, binding)
			}
			var generation storage.ConfigGenerationRevision
			outcome, err := service.store.InConfigTransactionOutcome(ctx, func(tx *storage.ConfigTx) error {
				refs, err := tx.SaveCandidate(candidate, bindings, metadata)
				if err != nil {
					return err
				}
				// Every unrelated admitted publication remains exact; this fixture's
				// existing operational version is retained while only ONE bundle changes.
				for _, version := range active.Versions() {
					kind, present := version.Kind()
					if !present {
						return fmt.Errorf("missing retained kind")
					}
					if kind != storage.ConfigDocumentKindYAMLSource {
						refs = append(refs, version.Ref())
					}
				}
				generation, err = tx.RegisterCompiledGenerationWithWorkspace(entry, candidate.SHA256(), refs, locator, configcompilation.Context{})
				return err
			})
			if err != nil || outcome != storage.TransactionCommitted {
				t.Fatal("real migration26 required; no synthetic schema permitted", err)
			}
			sealed, err := service.store.ReadConfigGeneration(ctx, generation)
			if err != nil {
				t.Fatal(err)
			}
			target, present, err := declarativeSourceWatchTarget(sealed)
			if err != nil || !present || target.variant != variant || target.group != group || target.entry != entry || len(target.members) != 2 {
				t.Fatal("exact admitted declarative locator lost", err)
			}
			for _, version := range active.Versions() {
				kind, present := version.Kind()
				if !present {
					t.Fatal("invalid old snapshot")
				}
				if kind == storage.ConfigDocumentKindYAMLSource {
					continue
				}
				found := false
				for _, retained := range sealed.Versions() {
					found = found || retained.Ref() == version.Ref() && retained.SHA256() == version.SHA256()
				}
				if !found {
					t.Fatal("declarative grouping dropped an unrelated retained publication")
				}
			}
			for _, file := range files {
				for _, binding := range bindings {
					if binding.Path() == file.Path() {
						nha055ObservedFile(t, operational, group, binding, file.Bytes())
					}
				}
			}
			observed, err := service.workspace.ReadSources(group, bindings)
			if err != nil {
				t.Fatal(err)
			}
			exact := make([]parser.SourceFile, 0, len(observed))
			for _, member := range observed {
				file, err := parser.NewSourceFile(member.Binding().Path(), member.Bytes())
				if err != nil {
					t.Fatal(err)
				}
				exact = append(exact, file)
			}
			recompiled, err := compileObservedDeclarative(target, exact)
			if err != nil || recompiled.SHA256() != candidate.SHA256() {
				t.Fatal("watcher recompile changed admitted exact sources", err)
			}
		}
	}
	after, present, err := service.store.ReadActiveConfig(ctx)
	if err != nil || !present || after.Revision() != active.Revision() || after.ManifestSHA256() != active.ManifestSHA256() {
		t.Fatal("locator-only test activated a generation", err)
	}
	if raw, err := os.ReadFile(operational); err != nil || !bytes.Equal(raw, operationalRaw) {
		t.Fatal("source observation changed operational bytes", err)
	}
	var commands int
	outcome, err := service.store.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM public.command_intents").Scan(&commands)
	})
	if err != nil || outcome != storage.TransactionCommitted || commands != 0 {
		t.Fatal("documentary locator entered command ledger", err)
	}
}

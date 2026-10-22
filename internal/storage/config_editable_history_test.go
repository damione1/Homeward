package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/configcompilation"
	"github.com/damione1/Homeward/scope"
)

func nha055HistoryMember(t *testing.T, identity, entry, id, path, raw string) EditableSourceSnapshot {
	t.Helper()
	descriptor, err := DashboardSourceDescriptor(identity, entry)
	if err != nil {
		t.Fatal(err)
	}
	binding := nha151Binding(t, id, path)
	revision, err := NewConfigVersionRevision(3)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := NewConfigVersionRef(binding.ID(), revision)
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.NewSourceFile(path, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return EditableSourceSnapshot{descriptor: descriptor, binding: binding, ref: ref, file: file}
}

func TestNHA055EditableHistorySelectionSeparatesIdentityFromNativeEntry(t *testing.T) {
	first := nha055HistoryMember(t, "dashboard.first", "dashboard.yaml", "source.dashboard.first", "dashboard.yaml", "# first exact\nviews: []\n")
	second := nha055HistoryMember(t, "dashboard.second", "dashboard.yaml", "source.dashboard.second", "dashboard.yaml", "# second exact\nviews: []\n")
	for _, target := range []EditableSourceSnapshot{first, second} {
		selected, err := selectEditableSourceGroup([]EditableSourceSnapshot{second, first}, target.descriptor.Kind(), target.descriptor.Identity())
		if err != nil || len(selected) != 1 || selected[0].ref != target.ref || selected[0].binding != target.binding || selected[0].descriptor != target.descriptor || !bytes.Equal(selected[0].file.Bytes(), target.file.Bytes()) {
			t.Fatal("entry-only history selection mixed group identity or exact ref", err)
		}
	}
	selected, err := selectEditableSourceGroup([]EditableSourceSnapshot{first, second}, first.descriptor.Kind(), "dashboard.absent")
	if err != nil || len(selected) != 0 {
		t.Fatal("missing group was replaced by an entry-path match", err)
	}
}

func TestNHA055EditableHistoryMixedEntriesAndCrossReferencesAreIntegrityFailures(t *testing.T) {
	entry := nha055HistoryMember(t, "dashboard.first", "entry-a.yaml", "source.dashboard.first.a", "entry-a.yaml", "views: []\n")
	otherEntry := nha055HistoryMember(t, "dashboard.first", "entry-b.yaml", "source.dashboard.first.b", "entry-b.yaml", "views: []\n")
	crossed := entry
	crossed.ref = otherEntry.ref
	missingEntry := nha055HistoryMember(t, "dashboard.first", "entry-a.yaml", "source.dashboard.first.include", "parts/include.yaml", "[]\n")
	for _, values := range [][]EditableSourceSnapshot{{entry, otherEntry}, {crossed}, {entry, entry}, {missingEntry}} {
		selected, err := selectEditableSourceGroup(values, entry.descriptor.Kind(), entry.descriptor.Identity())
		if len(selected) != 0 {
			t.Fatal("integrity failure exposed a partial group")
		}
		nha151Kind(t, err, ConfigIntegrity)
	}
}

// PostgreSQL fixture: source rows and sealed generations are storage admission
// data, not a fake runtime activation. Run/Owner publication is qualified by
// the composed 055 suite. This test requires the future SQL26 migration chain.
func TestNHA055EditableHistoryReadsExactRetainedGenerationAndRejectsStaleCurrentCAS(t *testing.T) {
	f := nha029HistoryFixture(t, 256, nil)
	// This storage fixture has no configured identities, Host or pilot.
	// Its canonical baseline context therefore contains no declarations;
	// the same admitted input belongs to every generation in this test.
	manifestRevision, err := scope.BaselineV1().Value()
	if err != nil {
		t.Fatal(err)
	}
	idsDigest, err := hex.DecodeString(scope.BaselineIDsSHA256)
	if err != nil || len(idsDigest) != sha256.Size {
		t.Fatal("canonical baseline ID digest is invalid", err)
	}
	var manifestDigest [sha256.Size]byte
	copy(manifestDigest[:], idsDigest)
	compilation, err := configcompilation.NewContext(manifestRevision, manifestDigest, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var identities int64
	if err := f.store.pool.QueryRow(f.ctx, `SELECT count(*) FROM public.registry_entities`).Scan(&identities); err != nil || identities != 0 {
		t.Fatal("empty-declaration fixture unexpectedly contains configured identities", err)
	}
	first := nha055HistoryMember(t, "dashboard.first", "dashboard.yaml", "source.dashboard.first", "dashboard.yaml", "# historical first\nviews: []\n")
	second := nha055HistoryMember(t, "dashboard.second", "dashboard.yaml", "source.dashboard.second", "dashboard.yaml", "# historical second\nviews: []\n")
	include := nha055HistoryMember(t, "dashboard.first", "dashboard.yaml", "source.dashboard.first.include", "parts/include.yaml", "# exact old include\n[]\n")
	write := func(label string, values []EditableSourceSnapshot) (ConfigGenerationRevision, [32]byte, []ConfigVersionRef) {
		t.Helper()
		var generation ConfigGenerationRevision
		var refs []ConfigVersionRef
		outcome, err := f.store.InConfigTransactionOutcome(f.ctx, func(tx *ConfigTx) error {
			refs = []ConfigVersionRef{f.active.ref}
			for _, value := range values {
				ref, err := tx.SaveEditableSource(value.descriptor, value.binding, value.file, nha151Metadata(t))
				if err != nil {
					return err
				}
				refs = append(refs, ref)
			}
			var err error
			generation, err = tx.RegisterCompiledGenerationWithContext(f.binding.Path(), sha256.Sum256([]byte(label)), refs, compilation)
			return err
		})
		if err != nil || outcome != TransactionCommitted {
			t.Fatal("history fixture registration failed", outcome, err)
		}
		snapshot, err := f.store.ReadConfigGeneration(f.ctx, generation)
		if err != nil {
			t.Fatal(err)
		}
		storedContext, err := f.store.ReadConfigCompilationContext(f.ctx, generation, snapshot.ManifestSHA256(), sha256.Sum256([]byte(label)))
		if err != nil || storedContext.SHA256() != compilation.SHA256() {
			t.Fatal("generation lost its exact canonical compilation input", err)
		}
		return generation, snapshot.ManifestSHA256(), refs[1:]
	}
	oldGeneration, oldManifest, oldRefs := write("historical exact group set", []EditableSourceSnapshot{first, second, include})
	changed := first
	changed.file, err = parser.NewSourceFile(first.file.Path(), []byte("# current immutable SQL version\nviews: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	currentGeneration, currentManifest, newRefs := write("current contracted group set", []EditableSourceSnapshot{changed, second})
	currentMembers, err := f.store.ReadEditableSourceGroup(f.ctx, currentGeneration, currentManifest, first.descriptor.Kind(), first.descriptor.Identity())
	if err != nil || len(currentMembers) != 1 || currentMembers[0].binding != first.binding {
		t.Fatal("contracted generation retained an old include member", err)
	}
	untouched, err := f.store.ReadEditableSourceGroup(f.ctx, currentGeneration, currentManifest, second.descriptor.Kind(), second.descriptor.Identity())
	if err != nil || len(untouched) != 1 || !bytes.Equal(untouched[0].file.Bytes(), second.file.Bytes()) {
		t.Fatal("contracting one group changed another group with the same entry", err)
	}
	before := f.fingerprint(t)
	readEffects := func() [3]int64 {
		t.Helper()
		var counts [3]int64
		if err := f.store.pool.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM public.command_intents),(SELECT count(*) FROM public.execution_history_records),(SELECT count(*) FROM public.scheduled_jobs)`).Scan(&counts[0], &counts[1], &counts[2]); err != nil {
			t.Fatal(err)
		}
		return counts
	}
	beforeEffects := readEffects()
	selected, err := f.store.ReadEditableSourceGroup(f.ctx, oldGeneration, oldManifest, first.descriptor.Kind(), first.descriptor.Identity())
	if err != nil || len(selected) != 2 {
		t.Fatal("retained exact group membership was not restored", err)
	}
	refs := map[ConfigDocumentID]ConfigVersionRef{first.binding.ID(): oldRefs[0], include.binding.ID(): oldRefs[2]}
	for _, value := range selected {
		if value.ref != refs[value.binding.ID()] || value.descriptor != first.descriptor {
			t.Fatal("history guessed a current revision or descriptor")
		}
		if value.binding.ID() == first.binding.ID() && !bytes.Equal(value.file.Bytes(), first.file.Bytes()) {
			t.Fatal("history read latest document bytes instead of retained generation")
		}
	}
	wrongManifest := oldManifest
	wrongManifest[0] ^= 1
	if _, err := f.store.ReadEditableSourceGroup(f.ctx, oldGeneration, wrongManifest, first.descriptor.Kind(), first.descriptor.Identity()); err == nil {
		t.Fatal("generation-only rollback bypassed manifest binding")
	} else {
		nha151Kind(t, err, ConfigConflict)
	}
	err = f.store.CheckEditableSourceBinding(f.ctx, first.descriptor, first.binding, oldRefs[0].Revision(), first.file.SHA256(), false)
	nha151Kind(t, err, ConfigConflict)
	if err := f.store.CheckEditableSourceBinding(f.ctx, first.descriptor, first.binding, newRefs[0].Revision(), changed.file.SHA256(), false); err != nil {
		t.Fatal("current immutable SQL CAS was not accepted independently of historical revision", err)
	}
	err = f.store.CheckEditableSourceBinding(f.ctx, first.descriptor, first.binding, ConfigVersionRevision{}, [32]byte{}, true)
	nha151Kind(t, err, ConfigConflict)
	outcome, err := f.store.InConfigTransactionOutcome(f.ctx, func(tx *ConfigTx) error {
		return tx.ConfirmEditableSourceVersion(first.descriptor, first.binding, oldRefs[0].Revision(), first.file.SHA256(), false)
	})
	if outcome != TransactionNotCommitted {
		t.Fatal("stale SQL CAS crossed the commit boundary")
	}
	nha151Kind(t, err, ConfigConflict)
	if f.fingerprint(t) != before || readEffects() != beforeEffects || beforeEffects != ([3]int64{}) {
		t.Fatal("history selection/stale CAS created an activation, run, schedule or command")
	}
	// Once the exact old revision is collected, a same-path recent version
	// must never repair the selected historical generation implicitly.
	for n := 0; n < configVersionLimit+1; n++ {
		changed.file, err = parser.NewSourceFile(first.file.Path(), []byte(fmt.Sprintf("# later %d\nviews: []\n", n)))
		if err != nil {
			t.Fatal(err)
		}
		write(fmt.Sprintf("later group %d", n), []EditableSourceSnapshot{changed, second})
	}
	if _, err := f.store.ReadEditableSourceGroup(f.ctx, oldGeneration, oldManifest, first.descriptor.Kind(), first.descriptor.Identity()); err == nil {
		t.Fatal("collected historical group silently used latest versions")
	} else {
		nha151Kind(t, err, ConfigIncomplete)
	}
}

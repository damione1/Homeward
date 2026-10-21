package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/storage"
)

// A historical source is identified here only by SQL membership and hashes.
// No snapshot/candidate/plaintext A is retained as an alternative restore input.
type nha029C5HistorySource struct {
	revision storage.ConfigGenerationRevision
	manifest [sha256.Size]byte
	ref      storage.ConfigVersionRef
	hash     [sha256.Size]byte
}

func nha029C5HistoryActive(t *testing.T, ctx context.Context, r *nha029OwnerRig) nha029C5HistorySource {
	t.Helper()
	active, present, err := r.store.ReadActiveConfig(ctx)
	if err != nil || !present || active.Availability() != storage.ConfigGenerationComplete || active.ManifestFormat() != storage.ConfigManifestCompiledV2 {
		t.Fatalf("history refusal requires complete SQL authority: present=%t err=%v", present, err)
	}
	versions := active.Versions()
	if len(versions) != 1 {
		t.Fatal("history refusal fixture must have operational source only")
	}
	kind, present := versions[0].Kind()
	if !present || kind != storage.ConfigDocumentKindOperationalYAML {
		t.Fatal("history refusal source is not protected operational YAML")
	}
	if _, plain := versions[0].Bytes(); plain {
		t.Fatal("generic SQL snapshot disclosed protected source")
	}
	return nha029C5HistorySource{revision: active.Revision(), manifest: active.ManifestSHA256(), ref: versions[0].Ref(), hash: versions[0].SHA256()}
}

func nha029C5HistoryActivate(t *testing.T, ctx context.Context, r *nha029OwnerRig, comment string) nha029C5HistorySource {
	t.Helper()
	r.edit(t, "# "+comment+"\n")
	result, err := r.owner.Activate(ctx, r.request(t, nil, nil, nil))
	if err != nil || result.Outcome() != configactivation.Published || result.AttemptOutcome() != storage.TransactionCommitted {
		t.Fatalf("history fixture activation: outcome=%v transaction=%v err=%v", result.Outcome(), result.AttemptOutcome(), err)
	}
	active := nha029C5HistoryActive(t, ctx, r)
	if active.revision != result.Revision() {
		t.Fatal("history fixture publication differs from SQL")
	}
	return active
}

func nha029C5HistoryReplay(t *testing.T, ctx context.Context, r *nha029OwnerRig) *storage.ConfigReplayRepository {
	t.Helper()
	dataDir, err := r.environment.config.DataDir()
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := r.store.BootstrapSecrets(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := storage.NewConfigReplayRepository(r.store, secrets)
	if err != nil {
		t.Fatal(err)
	}
	return replay
}

func nha029C5HistoryCounts(t *testing.T, ctx context.Context, r *nha029OwnerRig) [4]int64 {
	t.Helper()
	var counts [4]int64
	if err := r.environment.observer.QueryRow(ctx, `SELECT (SELECT count(*) FROM public.config_generations),(SELECT count(*) FROM public.config_versions),(SELECT count(*) FROM public.config_activation_attempts),(SELECT count(*) FROM public.command_intents)`).Scan(&counts[0], &counts[1], &counts[2], &counts[3]); err != nil {
		t.Fatal(err)
	}
	return counts
}

func nha029C5HistoryWaits(t *testing.T, original *automationruns.Interpreter) [2]automationruns.RunID {
	t.Helper()
	// The live A waits retain their causal interpreter across publications.
	// This pointer adds no image ref or uncharged observation hold.
	traces, evicted := original.NodeTraces()
	if evicted != 0 {
		t.Fatal("history refusal lost native wait observations to eviction")
	}
	var waits [2]automationruns.RunID
	var stages [2]automationruns.NodeStage
	for _, trace := range traces {
		if trace.Source().Object() == "script.nha042_parent" && trace.Kind() == ir.ActionWaitTemplate {
			waits[0] = trace.RunID()
			stages[0] = trace.Stage()
		}
		if trace.Source().Object() == "script.nha042_child" && trace.Kind() == ir.ActionWait {
			waits[1] = trace.RunID()
			stages[1] = trace.Stage()
		}
	}
	if waits[0] == 0 || waits[1] == 0 || waits[0] == waits[1] {
		t.Fatal("history refusal lost its two actual live waits")
	}
	for _, trace := range traces {
		if trace.RunID() != waits[0] && trace.RunID() != waits[1] {
			continue
		}
		if trace.Stage() == automationruns.NodeCancelled || trace.Stage() == automationruns.NodeFailed {
			t.Fatal("history refusal canceled or failed an intact run")
		}
	}
	if stages[0] != automationruns.NodeWaiting || stages[1] != automationruns.NodeWaiting {
		t.Fatal("history refusal changed actual wait state")
	}
	return waits
}

func TestNHA029C5HistoricalRestoreRefusesConcurrentSQLWinner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r := nha029StartOwner(t, nha029OwnerParent, nha029OwnerChild, nil)
	// A is the protected SQL generation admitted by the actual Run startup.
	// Start its waits before any hot publication replaces the initial image.
	a := nha029C5HistoryActive(t, ctx, r)
	r.waitBoth(t)
	original := r.composition.scripts.interpreter
	waits := nha029C5HistoryWaits(t, original)
	b := nha029C5HistoryActivate(t, ctx, r, "history CAS source B")
	if b.revision.Number() <= a.revision.Number() || b.hash == a.hash {
		t.Fatal("history CAS fixture did not replace A with B")
	}
	if nha029C5HistoryWaits(t, original) != waits {
		t.Fatal("B replaced an intact A wait")
	}
	replay := nha029C5HistoryReplay(t, ctx, r)
	// This exact plaintext comes from the authenticated historical SQL reader,
	// after B replaced the file; it is never reconstructed from r.raw.
	historical, err := replay.ReadHistoricalProtectedSource(ctx, a.revision, a.manifest, a.ref)
	if err != nil || historical.SHA256() != a.hash {
		t.Fatalf("historical SQL A was not admitted: %v", err)
	}
	raw := historical.Bytes()
	defer clear(raw)
	if sha256.Sum256(raw) != a.hash {
		t.Fatal("historical SQL A digest changed")
	}
	r.edit(t, "# independent SQL producer D wins after the historical read\n")
	winner := r.commitCurrentWorkspace(t)
	d := nha029C5HistoryActive(t, ctx, r)
	if d.revision != winner || d.revision.Number() <= b.revision.Number() || d.hash == a.hash || d.hash == b.hash {
		t.Fatal("independent SQL producer did not commit distinct D")
	}
	if err := os.WriteFile(r.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	clear(r.raw)
	r.raw = nil
	before := nha029C5HistoryCounts(t, ctx, r)
	visible, err := r.owner.CurrentConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	revision, manifest, err := visible.Components()
	if err != nil || revision != uint64(b.revision.Number()) || manifest != b.manifest {
		t.Fatal("SQL-only D unexpectedly changed the Owner's B basis")
	}
	controller := r.controller()
	result, err := r.owner.Activate(ctx, r.request(t, nil, nil, nil))
	if !errors.Is(err, configactivation.ErrStartupConflict) || result.Outcome() != configactivation.RejectedBeforeCommit {
		t.Fatalf("stale B restore did not refuse SQL D: outcome=%v err=%v", result.Outcome(), err)
	}
	if after := nha029C5HistoryActive(t, ctx, r); after != d {
		t.Fatal("refused historical restore overwrote SQL D")
	}
	afterVisible, err := r.owner.CurrentConfig(ctx)
	if err != nil || afterVisible != visible {
		t.Fatalf("refused restore changed B visibility: %v", err)
	}
	if _, err := r.identity.Sequence().Capture(); err != nil {
		t.Fatalf("refused restore left capture unavailable: %v", err)
	}
	if nha029C5HistoryCounts(t, ctx, r) != before || len(r.driver.snapshot()) != 0 {
		t.Fatal("refused restore wrote SQL or emitted a command")
	}
	if nha029C5HistoryWaits(t, original) != waits || r.controller() != controller {
		t.Fatal("refused restore replaced intact waits or Controller")
	}
	r.close(t)
	if traces, _ := original.NodeTraces(); len(traces) != 0 {
		t.Fatal("closed history refusal retained A's native ring")
	}
	if len(r.driver.snapshot()) != 0 {
		t.Fatal("refused restore or Close replayed an action")
	}
}

func TestNHA029C5HistoricalRestoreRefusesNormallyCollectedSource(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r := nha029StartOwner(t, nha029OwnerParent, nha029OwnerChild, nil)
	// A is the protected SQL generation admitted by the actual Run startup.
	// Start its waits before any hot publication replaces the initial image.
	a := nha029C5HistoryActive(t, ctx, r)
	r.waitBoth(t)
	original := r.composition.scripts.interpreter
	waits := nha029C5HistoryWaits(t, original)
	current := a
	// All successors are admitted/written/published by the actual Owner. No SQL
	// DELETE or altered GC setting manufactures the historical absence.
	for index := 1; index <= 21; index++ {
		next := nha029C5HistoryActivate(t, ctx, r, fmt.Sprintf("history retention successor %02d", index))
		if next.revision.Number() <= current.revision.Number() || next.ref.Revision().Number() <= current.ref.Revision().Number() || next.hash == current.hash {
			t.Fatal("retention fixture did not create a fresh version")
		}
		current = next
	}
	var retained int
	var oldPresent bool
	if err := r.environment.observer.QueryRow(ctx, `SELECT count(*),bool_or(revision=$2) FROM public.config_versions WHERE document_id=$1`, a.ref.Document().String(), a.ref.Revision().Number()).Scan(&retained, &oldPresent); err != nil || retained != 20 || oldPresent {
		t.Fatalf("normal retention did not collect A: retained=%d old_present=%t err=%v", retained, oldPresent, err)
	}
	if nha029C5HistoryWaits(t, original) != waits {
		t.Fatal("normal version retention replaced intact A waits")
	}
	replay := nha029C5HistoryReplay(t, ctx, r)
	clear(r.raw)
	r.raw = nil
	before := nha029C5HistoryCounts(t, ctx, r)
	visible, err := r.owner.CurrentConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	controller := r.controller()
	refused, err := replay.ReadHistoricalProtectedSource(ctx, a.revision, a.manifest, a.ref)
	var failure *storage.Error
	if !errors.As(err, &failure) || failure.Kind() != storage.ConfigIncomplete {
		t.Fatalf("collected history was not explicitly incomplete: %v", err)
	}
	if refused.Ref().Valid() || len(refused.Bytes()) != 0 || refused.SHA256() != ([sha256.Size]byte{}) {
		t.Fatal("collected history returned a source or fallback bytes")
	}
	if after := nha029C5HistoryActive(t, ctx, r); after != current {
		t.Fatal("collected history refusal changed SQL authority")
	}
	afterVisible, err := r.owner.CurrentConfig(ctx)
	if err != nil || afterVisible != visible {
		t.Fatalf("collected history refusal changed visibility: %v", err)
	}
	if _, err := r.identity.Sequence().Capture(); err != nil {
		t.Fatalf("collected history refusal left capture unavailable: %v", err)
	}
	if nha029C5HistoryCounts(t, ctx, r) != before || len(r.driver.snapshot()) != 0 {
		t.Fatal("collected history refusal mutated SQL or emitted an action")
	}
	if nha029C5HistoryWaits(t, original) != waits || r.controller() != controller {
		t.Fatal("collected history refusal changed intact waits or Controller")
	}
	active, err := replay.ReadActiveProtectedSource(ctx, current.revision, current.manifest, current.ref)
	if err != nil || active.SHA256() != current.hash {
		t.Fatalf("retention damaged current protected source: %v", err)
	}
	r.close(t)
	if traces, _ := original.NodeTraces(); len(traces) != 0 {
		t.Fatal("closed history retention refusal retained A's native ring")
	}
	if len(r.driver.snapshot()) != 0 {
		t.Fatal("collected history refusal or Close replayed an action")
	}
}

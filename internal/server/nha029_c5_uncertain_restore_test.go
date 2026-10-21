package server

import (
	"crypto/sha256"
	"os"
	"testing"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
)

// The first crash is deliberately BEFORE activation B: a real outstanding
// dispatch must not be pretended to drain across the normal activation fence.
func TestNHA029C5RestoreNeverReplaysEffectLeftUncertainByCrash(t *testing.T) {
	f := nha029C5NewFixture(t)
	child := f.start(t, false)
	child.ready(t, nil)
	a := f.active(t)
	var historical storage.ConfigVersionRef
	var historicalHash [sha256.Size]byte
	for _, version := range a.Versions() {
		kind, present := version.Kind()
		if present && kind == storage.ConfigDocumentKindOperationalYAML {
			historical, historicalHash = version.Ref(), version.SHA256()
		}
	}
	if !historical.Valid() {
		t.Fatal("uncertain restore lacks real A source reference")
	}
	child.send(t, nha029C5Message{kind: nha029C5Fire})
	effect := child.next(t)
	if effect.kind != nha029C5Effect {
		t.Fatal("uncertain crash did not reach the actual fake driver")
	}
	child.recordEffect(t, effect, false)
	id, err := domain.ParseCommandID(effect.value)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := nha042ServerIntent(t, f.observer, id)
	if state != "dispatching" || f.effects[effect.value] != 1 {
		t.Fatal("uncertain effect was not pending in the real ledger")
	}
	child.kill(t)
	// This factory supplies the same explicitly transient IR fixture. It does
	// not claim to reconstruct a persisted Logic1 program or a dead run.
	recovered := f.start(t, false)
	recovered.ready(t, &a)
	state, response := nha042ServerIntent(t, f.observer, id)
	if state != "indeterminate" || response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
		t.Fatalf("real DCS recovery did not preserve uncertain outcome: %s %v", state, response.Status)
	}
	intent := nha029C5RestoreDigest(t, f.ctx, f.observer, nha029C5RestoreIntentDigest, effect.value)
	f.edit(t, "B after recovery of uncertain command")
	recovered.send(t, nha029C5Message{kind: nha029C5Activate})
	publishedB := recovered.await(t, nha029C5Activated)
	if publishedB.count>>32 != 0 || configactivation.Outcome(uint32(publishedB.count)) != configactivation.Published {
		t.Fatal("B was not published after real uncertain recovery")
	}
	b := f.active(t)
	if b.Revision().Number() <= a.Revision().Number() {
		t.Fatal("B did not advance A")
	}
	// Drop the original plaintext; C can only obtain A from authenticated SQL.
	clear(f.raw)
	f.raw = nil
	secrets, err := f.store.BootstrapSecrets(f.ctx, f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := storage.NewConfigReplayRepository(f.store, secrets)
	if err != nil {
		t.Fatal(err)
	}
	protected, err := replay.ReadHistoricalProtectedSource(f.ctx, a.Revision(), a.ManifestSHA256(), historical)
	if err != nil || protected.SHA256() != historicalHash {
		t.Fatalf("historical A unavailable: %v", err)
	}
	raw := protected.Bytes()
	if sha256.Sum256(raw) != historicalHash {
		t.Fatal("historical A source changed")
	}
	err = os.WriteFile(f.path, raw, 0600)
	clear(raw)
	if err != nil {
		t.Fatal(err)
	}
	recovered.send(t, nha029C5Message{kind: nha029C5Activate})
	publishedC := recovered.await(t, nha029C5Activated)
	if publishedC.count>>32 != 0 || configactivation.Outcome(uint32(publishedC.count)) != configactivation.Published {
		t.Fatal("historical C was not published")
	}
	c := f.active(t)
	if c.Revision().Number() <= b.Revision().Number() || c.CandidateSHA256() != a.CandidateSHA256() {
		t.Fatal("historical C did not restore A content at a new revision")
	}
	matched := false
	for _, version := range c.Versions() {
		kind, present := version.Kind()
		if present && kind == storage.ConfigDocumentKindOperationalYAML {
			matched = version.SHA256() == historicalHash && version.Ref().Revision().Number() > historical.Revision().Number()
		}
	}
	if !matched || c.ManifestSHA256() == a.ManifestSHA256() {
		t.Fatal("C did not bind the historical operational bytes to a new authenticated version")
	}
	counts := f.counts(t)
	if counts[2] != 1 || len(f.effects) != 1 || nha029C5RestoreDigest(t, f.ctx, f.observer, nha029C5RestoreIntentDigest, effect.value) != intent {
		t.Fatal("B or C rewrote or replayed the uncertain command")
	}
	recovered.stop(t)
	if err := os.WriteFile(f.path, []byte("invalid: [\n"), 0600); err != nil {
		t.Fatal(err)
	}
	configured := f.start(t, true)
	configured.ready(t, &c)
	configured.stop(t)
	if f.counts(t) != counts || len(f.effects) != 1 || f.effects[effect.value] != 1 || nha029C5RestoreDigest(t, f.ctx, f.observer, nha029C5RestoreIntentDigest, effect.value) != intent {
		t.Fatal("configured C restart replayed or changed the uncertain command")
	}
}

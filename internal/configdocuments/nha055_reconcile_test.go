package configdocuments

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/storage"
)

func TestNHA055WorkspaceSetRecoveryRejectsMixedSQLCutAndPreservesCandidates(t *testing.T) {
	w, path, _ := nha149Workspace(t)
	members := []WorkspaceMember{
		nha055Member(t, "helpers.main", "helpers.main", "helpers.yaml", "input_boolean: {}\n", ExpectAbsentMember(), ExpectAbsentMember()),
		nha055Member(t, "helpers.main", "helpers.include", "include.yaml", "away_mode: {initial: false}\n", ExpectAbsentMember(), ExpectAbsentMember()),
	}
	edit, err := w.BeginSetEdit(members)
	if err != nil {
		t.Fatal(err)
	}
	nha149Failure(t, edit.Finish(nil, false), ReconciliationRequired)
	mixed := map[storage.ConfigDocumentID]MemberExpectation{members[0].binding.ID(): ExpectAbsentMember()}
	mixed[members[1].binding.ID()], _ = ExpectExistingMember(members[1].file.SHA256())
	found, err := w.ReconcileSet(mixed)
	if !found {
		t.Fatal("recovery lost the persisted complete source-set intent")
	}
	nha149Failure(t, err, ReconciliationRequired)
	for _, member := range members {
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(path), filepath.FromSlash(member.location)))
		if err != nil || !bytes.Equal(raw, member.file.Bytes()) {
			t.Fatal("mixed SQL proof modified an editable member", err)
		}
	}
	allPrior := map[storage.ConfigDocumentID]MemberExpectation{}
	for _, member := range members {
		allPrior[member.binding.ID()] = ExpectAbsentMember()
	}
	found, err = w.ReconcileSet(allPrior)
	if !found || err != nil {
		t.Fatal("known prior SQL authority could not retire the unchanged candidate set", err)
	}
	observation, err := w.Observe()
	if err != nil || observation.ReconciliationPending() {
		t.Fatal("known resolution retained an uncertain source-set intent", err)
	}
}

func TestNHA055RealDocumentRecoveryUsesActiveAuthorityInsteadOfUnactivatedVersions(t *testing.T) {
	service, ctx, path, operational := nha055Documents(t)
	before, err := service.Read(ctx, operationalID())
	if err != nil {
		t.Fatal(err)
	}
	member := nha055Member(t, "helpers.recovery", "helpers.recovery", "recovery.yaml", "input_boolean:\n  away_mode: {initial: false}\n", ExpectAbsentMember(), ExpectAbsentMember())
	edit, err := service.workspace.BeginSetEdit([]WorkspaceMember{member})
	if err != nil {
		t.Fatal(err)
	}
	nha149Failure(t, edit.Finish(nil, false), ReconciliationRequired)
	// An independent producer stores C without activating any generation.
	// Latest retained C is neither the intent's prior active absence nor B.
	// Recovery must use the actual active pointer, preserve C and leave B dirty.
	latentRaw := []byte("input_boolean:\n  away_mode: {initial: true}\n")
	latentFile, err := parser.NewSourceFile(member.binding.Path(), latentRaw)
	if err != nil {
		t.Fatal(err)
	}
	latent, err := bundle.CompileSources(latentFile.Path(), []parser.SourceFile{latentFile})
	if err != nil {
		t.Fatal(err)
	}
	author, _ := storage.NewConfigAuthor("nha055:unactivated-history")
	source, _ := storage.NewConfigSource("nha055:unactivated-history")
	metadata, _ := storage.NewConfigVersionMetadata(author, source)
	outcome, err := service.store.InConfigTransactionOutcome(ctx, func(tx *storage.ConfigTx) error {
		_, err := tx.SaveCandidate(latent, []storage.ConfigDocumentBinding{member.binding}, metadata)
		return err
	})
	if err != nil || outcome != storage.TransactionCommitted {
		t.Fatal("real unactivated immutable history registration failed", err)
	}
	if found, err := service.ReconcileWorkspace(ctx); !found || err != nil {
		t.Fatal("shared document recovery confused latest history with active authority", err)
	}
	after, err := service.Read(ctx, operationalID())
	if err != nil || after.Generation() != before.Generation() || after.ManifestSHA256() != before.ManifestSHA256() || after.Workspace().ReconciliationPending() {
		t.Fatal("recovery invented an activation or retained a resolved intent", err)
	}
	latest, err := service.store.ReadConfigDocument(ctx, member.binding.ID())
	if err != nil || latest.Active() || latest.SourceSHA256() != sha256.Sum256(latentRaw) {
		t.Fatal("recovery changed or activated the independent immutable history", err)
	}
	physical, err := os.ReadFile(filepath.Join(filepath.Dir(path), filepath.FromSlash(member.location)))
	if err != nil || !bytes.Equal(physical, member.file.Bytes()) {
		t.Fatal("recovery overwrote the editable candidate with SQL history", err)
	}
	exact, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(exact, operational) {
		t.Fatal("source-set recovery changed the selected operational YAML", err)
	}
}

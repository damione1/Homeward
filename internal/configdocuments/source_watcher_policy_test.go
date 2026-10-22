package configdocuments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/config/bundle"

	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/storage"
)

// This real SQL24 fixture exercises the independent active policy pointer and
// the actual149 source writer guard. It does not select or project SQL26.
func TestNHA055PolicyWatcherObservesOnlyMaterializedCanonicalSourceAndRefusesStaleSQL(t *testing.T) {
	service, ctx, operational, _ := nha055Documents(t)
	id, err := accounts.NewAccountID()
	if err != nil {
		t.Fatal(err)
	}
	login, err := accounts.ParseLoginName("nha055 watcher admin")
	if err != nil {
		t.Fatal(err)
	}
	display, err := accounts.ParseDisplayName("NHA055 source watcher")
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := accounts.HashNewPassword(ctx, "NHA055 isolated watcher fixture credential")
	if err != nil {
		t.Fatal(err)
	}
	record, err := service.store.BootstrapAdmin(ctx, storage.NewAccount{ID: id, Login: login, Display: display, Role: auth.AccountRole_ACCOUNT_ROLE_ADMIN, Verifier: verifier})
	if err != nil {
		t.Fatal(err)
	}
	account, err := record.ID()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := service.store.AuthorizationPolicy(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	source, err := policy.Source()
	if err != nil {
		t.Fatal(err)
	}
	version, err := policy.Version()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := source.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	binding, err := storage.AuthorizationPolicyBinding(account)
	if err != nil {
		t.Fatal(err)
	}
	before, err := service.sourceWatchCapture(ctx)
	if err != nil || len(before.targets) != 1 || before.targets[0].materialized {
		t.Fatal("seeded absent policy was materialized by observation", err)
	}
	dirty := append([]byte("# direct local edit\n"), raw...)
	physical := nha055ObservedFile(t, operational, binding.ID(), binding, dirty)
	captured, err := service.sourceWatchCapture(ctx)
	if err != nil || len(captured.targets) != 1 {
		t.Fatal("canonical materialized policy was not observed on SQL24", err)
	}
	target := captured.targets[0]
	if target.descriptor.Valid() || target.account != account || !target.complete || !sourceTargetDirty(target) || !bytes.Equal(target.members[0].observed.Bytes(), dirty) {
		t.Fatal("policy observation inferred another owner or source")
	}
	// A separate real policy transaction moves the active pointer without an
	// operational generation. The old watcher cut must fail before any staging.
	expected, err := authorization.ExpectVersion(version)
	if err != nil {
		t.Fatal(err)
	}
	replacementRaw := append([]byte("# independent SQL policy edit\n"), raw...)
	replacement, err := authorization.AdmitPolicySource(account, replacementRaw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.store.ReplaceAuthorizationPolicy(ctx, account, account, replacement, expected); err != nil {
		t.Fatal(err)
	}
	_, err = service.activateSourceTarget(ctx, captured.operational, target)
	nha149Failure(t, err, Conflict)
	after, err := os.ReadFile(physical)
	if err != nil || !bytes.Equal(after, dirty) {
		t.Fatal("stale watcher replaced the external candidate", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(operational), service.workspace.intent)); !os.IsNotExist(err) {
		t.Fatal("stale watcher created a workspace intent")
	}
	refreshed, err := service.sourceWatchCapture(ctx)
	if err != nil || len(refreshed.targets) != 1 {
		t.Fatal(err)
	}
	if refreshed.targets[0].policy == version {
		t.Fatal("watcher reused a stale independently active policy version")
	}
	active, present, err := service.store.ReadActiveConfig(context.Background())
	if err != nil || !present || active.Revision() != before.operational.generation {
		t.Fatal("policy read/guard activated a generation", err)
	}
}

func TestNHA055LegacyWriterRejectsReservedNativeAndPolicyRootsBeforeStaging(t *testing.T) {
	service, ctx, operational, raw := nha055Documents(t)
	before, expectedOperational, basis := nha055DocumentBasis(t, service, ctx, time.Now().Add(10*time.Second))
	info, err := os.Stat(operational)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := storage.DashboardSourceDescriptor("dashboard.nha055_reserved", "dashboard.yaml")
	if err != nil {
		t.Fatal(err)
	}
	nativeGroup, err := SourceWorkspaceGroup(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	account, err := contracts.ParseAccountID("nha055-reserved-policy-root")
	if err != nil {
		t.Fatal(err)
	}
	policyBinding, err := storage.AuthorizationPolicyBinding(account)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct {
		group   storage.ConfigDocumentID
		logical string
	}{{nativeGroup, "dashboard.yaml"}, {policyBinding.ID(), policyBinding.Path()}} {
		file, err := parser.NewSourceFile(target.logical, []byte("input_boolean:\n  away_mode: {initial: false}\n"))
		if err != nil {
			t.Fatal(err)
		}
		candidate, err := bundle.CompileSources(file.Path(), []parser.SourceFile{file})
		if err != nil {
			t.Fatal(err)
		}
		document, err := storage.NewConfigDocumentID("nha055.legacy-namespace-candidate")
		if err != nil {
			t.Fatal(err)
		}
		binding, err := storage.NewConfigDocumentBinding(document, file.Path())
		if err != nil {
			t.Fatal(err)
		}
		protected := []byte("# native or policy root remains owned by its source kind\n")
		physical := nha055ObservedFile(t, operational, target.group, binding, protected)
		expectedWorkspace, err := ExpectExistingMember(sha256.Sum256(protected))
		if err != nil {
			t.Fatal(err)
		}
		expected, err := NewExpectedSource(binding, storage.ConfigVersionRevision{}, [32]byte{}, true, expectedWorkspace)
		if err != nil {
			t.Fatal(err)
		}
		author, err := storage.NewConfigAuthor("nha055:legacy-namespace-fixture")
		if err != nil {
			t.Fatal(err)
		}
		_, err = service.WriteDefinitions(ctx, target.group, candidate, expectedOperational, []ExpectedSource{expected}, author, basis)
		nha149Failure(t, err, FailureInvalid)
		current, err := os.ReadFile(physical)
		if err != nil || !bytes.Equal(current, protected) {
			t.Fatal("legacy writer changed reserved source bytes before refusal", err)
		}
	}
	nha055DocumentsUnchanged(t, service, ctx, before, operational, raw, info)
}

package server

import (
	"bytes"
	"fmt"
	"os"
	"testing"

	"connectrpc.com/connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	configuration "github.com/damione1/Homeward/gen/home/configuration/v1"
	wire "github.com/damione1/Homeward/gen/home/configurationapply/v1"
	planwire "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

func nha055PolicyRollbackNoCommands(t *testing.T, f nha055TLSFixture) {
	t.Helper()
	var commands int
	outcome, err := f.run.observed.sessions.store.InTransactionOutcome(f.ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(f.ctx, `SELECT count(*) FROM public.command_intents`).Scan(&commands)
	})
	if err != nil || outcome != storage.TransactionCommitted || commands != 0 {
		t.Fatalf("documentary policy rollback entered the command ledger: count=%d err=%v", commands, err)
	}
}

// This actual default Run has no configured physical pilot. Both P1 and P2
// originate in050, so no029 generation-to-policy audit can identify P1.
func TestNHA055ActualRunTLSPolicyDocumentVersionRollbackCurrentCASAndRevokedRights(t *testing.T) {
	f := nha055StartTLS(t)
	created, err := f.accounts.CreateAccount(f.ctx, nha056SessionRequest(&auth.CreateAccountRequest{LoginName: "NHA055 version user", DisplayName: "NHA055 version user", Role: auth.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: "NHA055 version user password"}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	account := created.Msg.Account.Id
	seed, err := f.policies.GetPolicy(f.ctx, nha056SessionRequest(&auth.GetPolicyRequest{AccountId: account}, f.token))
	if err != nil || seed.Msg.GetPresent() == nil {
		t.Fatal("seed policy", err)
	}
	p1Raw := []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n - scope: {family: state_registry, operation: read}\n   objects: {entities: {all: {capabilities: {all: {}}}}}\n# direct050 P1\n", account.Value))
	p1, err := f.policies.ReplacePolicy(f.ctx, nha056SessionRequest(&auth.ReplacePolicyRequest{AccountId: account, YamlSource: p1Raw, Precondition: &auth.ReplacePolicyRequest_Expected{Expected: seed.Msg.GetPresent().Version}}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	p2Raw := []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants: []\n# direct050 P2\n", account.Value))
	p2, err := f.policies.ReplacePolicy(f.ctx, nha056SessionRequest(&auth.ReplacePolicyRequest{AccountId: account, YamlSource: p2Raw, Precondition: &auth.ReplacePolicyRequest_Expected{Expected: p1.Msg.Policy.Version}}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	before := f.read(t)
	nha055PolicyRollbackNoCommands(t, f)
	request := &wire.RollbackRequest{Id: before.Document.Id, Expected: before.Document.Version, ExpectedWorkspaceSha256: before.Document.GetWorkspace().SourceSha256, Family: planwire.SourceFamily_SOURCE_FAMILY_AUTHORIZATION, Identity: account.Value, HistoricalPolicyVersion: &configuration.DocumentVersion{Revision: int64(p1.Msg.Policy.Version.Revision), SourceSha256: bytes.Clone(p1.Msg.Policy.Version.SourceSha256)}, Sources: []*wire.SourcePrecondition{nha055PolicyExpected(t, account, p2.Msg.Policy.Version, nil)}}
	if request.Historical != nil {
		t.Fatal("documentary rollback test accidentally depends on a historical generation")
	}
	// P2-to-P3 occurs outside029 after the caller observed current policy CAS.
	// Generation and operational source stay equal; a stale policy base fails.
	p3Raw := append(bytes.Clone(p2Raw), []byte("# independent050 P3 after rollback preview\n")...)
	p3, err := f.policies.ReplacePolicy(f.ctx, nha056SessionRequest(&auth.ReplacePolicyRequest{AccountId: account, YamlSource: p3Raw, Precondition: &auth.ReplacePolicyRequest_Expected{Expected: p2.Msg.Policy.Version}}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	afterP3 := f.read(t)
	if !proto.Equal(afterP3.ActiveGeneration, before.ActiveGeneration) {
		t.Fatal("independent P2-to-P3 changed generation; wrong CAS oracle")
	}
	_, err = f.apply.Rollback(f.ctx, nha056SessionRequest(request, f.token))
	nha055Conflict(t, err, connect.CodeAborted)
	request.Expected = afterP3.Document.Version
	request.ExpectedWorkspaceSha256 = afterP3.Document.GetWorkspace().SourceSha256
	request.Sources = []*wire.SourcePrecondition{nha055PolicyExpected(t, account, p3.Msg.Policy.Version, nil)}
	restored, err := f.apply.Rollback(f.ctx, nha056SessionRequest(request, f.token))
	if err != nil || restored.Msg.Activation.Outcome != configuration.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED {
		t.Fatal("050 documentary P1 was not restored through149/029", err)
	}
	actual, err := f.policies.GetPolicy(f.ctx, nha056SessionRequest(&auth.GetPolicyRequest{AccountId: account}, f.token))
	if err != nil || actual.Msg.GetPresent() == nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual.Msg.GetPresent().YamlSource, p1Raw) || actual.Msg.GetPresent().Version.Revision != p3.Msg.Policy.Version.Revision+1 || restored.Msg.Current.ActiveGeneration.Revision != before.ActiveGeneration.Revision+1 {
		t.Fatal("rollback substituted latest bytes or rewound policy/generation revisions")
	}
	exact, err := os.ReadFile(f.path)
	if err != nil || !bytes.Equal(exact, f.raw) {
		t.Fatal("documentary policy rollback changed operational YAML", err)
	}
	nha055PolicyRollbackNoCommands(t, f)
	// Withdraw the caller's real admin role. The historical policy source
	// cannot recover that permission even with fresh current-policy CAS.
	if _, err := f.accounts.CreateAccount(f.ctx, nha056SessionRequest(&auth.CreateAccountRequest{LoginName: "NHA055 alternate admin", DisplayName: "NHA055 alternate admin", Role: auth.AccountRole_ACCOUNT_ROLE_ADMIN, InitialPassword: "NHA055 alternate admin password"}, f.token)); err != nil {
		t.Fatal(err)
	}
	other := nha162RunLogin(t, f.ctx, f.authentication, "NHA055 alternate admin", "NHA055 alternate admin password")
	userRole := auth.AccountRole_ACCOUNT_ROLE_USER
	if _, err := f.accounts.UpdateAccount(f.ctx, nha056SessionRequest(&auth.UpdateAccountRequest{AccountId: f.tokens.Session.AccountId, Role: &userRole}, other.AccessToken)); err != nil {
		t.Fatal(err)
	}
	denied := proto.Clone(request).(*wire.RollbackRequest)
	denied.Expected = restored.Msg.Current.Document.Version
	denied.ExpectedWorkspaceSha256 = restored.Msg.Current.Document.GetWorkspace().SourceSha256
	denied.Sources = []*wire.SourcePrecondition{nha055PolicyExpected(t, account, actual.Msg.GetPresent().Version, p1Raw)}
	_, err = f.apply.Rollback(f.ctx, nha056SessionRequest(denied, f.token))
	if code := connect.CodeOf(err); code != connect.CodePermissionDenied && code != connect.CodeUnauthenticated {
		t.Fatal("historical policy bytes restored revoked administrative permission", err)
	}
	still, err := f.policies.GetPolicy(f.ctx, nha056SessionRequest(&auth.GetPolicyRequest{AccountId: account}, other.AccessToken))
	if err != nil || !bytes.Equal(still.Msg.GetPresent().YamlSource, p1Raw) || still.Msg.GetPresent().Version.Revision != actual.Msg.GetPresent().Version.Revision {
		t.Fatal("refused rollback changed current policy or created a revision", err)
	}
	nha055PolicyRollbackNoCommands(t, f)
}

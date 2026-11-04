package server

import (
	"bytes"
	"fmt"
	"github.com/jackc/pgx/v5"
	"os"
	"testing"

	"connectrpc.com/connect"
	configuration "github.com/damione1/Homeward/gen/home/api/v2"
	planwire "github.com/damione1/Homeward/gen/home/api/v2"
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/storage"
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
	created, err := f.accounts.CreateAccount(f.ctx, nha056SessionRequest(&configuration.CreateAccountRequest{Parent: nha203SessionAccount(t, f.tokens.Session).Home().String(), Account: &configuration.Account{LoginName: "NHA055 version user", DisplayName: "NHA055 version user", Role: configuration.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: "NHA055 version user password"}}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	accountName := created.Msg.Name
	account := nha203NativeAccount(t, f.ctx, f.run.observed.sessions.store, accountName)
	seed, err := f.policies.GetAuthorizationPolicy(f.ctx, nha056SessionRequest(&configuration.GetAuthorizationPolicyRequest{Name: accountName + "/authorizationPolicy"}, f.token))
	if err != nil || seed.Msg == nil {
		t.Fatal("seed policy", err)
	}
	p1Raw := []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n - scope: {family: state_registry, operation: read}\n   objects: {entities: {all: {capabilities: {all: {}}}}}\n# direct050 P1\n", nha203AccountText(t, account)))
	p1, err := f.policies.ReplaceAuthorizationPolicy(f.ctx, nha056SessionRequest(&configuration.ReplaceAuthorizationPolicyRequest{Name: accountName + "/authorizationPolicy", YamlSource: p1Raw, Precondition: &configuration.ReplaceAuthorizationPolicyRequest_ExpectedEtag{ExpectedEtag: seed.Msg.Etag}}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	p2Raw := []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants: []\n# direct050 P2\n", nha203AccountText(t, account)))
	p2, err := f.policies.ReplaceAuthorizationPolicy(f.ctx, nha056SessionRequest(&configuration.ReplaceAuthorizationPolicyRequest{Name: accountName + "/authorizationPolicy", YamlSource: p2Raw, Precondition: &configuration.ReplaceAuthorizationPolicyRequest_ExpectedEtag{ExpectedEtag: p1.Msg.Policy.Etag}}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	before := f.read(t)
	nha055PolicyRollbackNoCommands(t, f)
	request := &wire.RollbackConfigurationRequest{Name: before.Name, Expected: before.Version, ExpectedWorkspaceSha256: before.GetWorkspace().SourceSha256, Family: planwire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_AUTHORIZATION, Identity: nha203AccountText(t, account), HistoricalPolicyVersion: &configuration.DocumentVersion{Revision: int64(nha055PolicyVersion(t, p1.Msg.Policy).Revision), SourceSha256: bytes.Clone(nha055PolicyVersion(t, p1.Msg.Policy).SourceSha256)}, Sources: []*wire.SourcePrecondition{nha055PolicyExpected(t, account, nha055PolicyVersion(t, p2.Msg.Policy), nil)}}
	if request.Historical != nil {
		t.Fatal("documentary rollback test accidentally depends on a historical generation")
	}
	// P2-to-P3 occurs outside029 after the caller observed current policy CAS.
	// Generation and operational source stay equal; a stale policy base fails.
	p3Raw := append(bytes.Clone(p2Raw), []byte("# independent050 P3 after rollback preview\n")...)
	p3, err := f.policies.ReplaceAuthorizationPolicy(f.ctx, nha056SessionRequest(&configuration.ReplaceAuthorizationPolicyRequest{Name: accountName + "/authorizationPolicy", YamlSource: p3Raw, Precondition: &configuration.ReplaceAuthorizationPolicyRequest_ExpectedEtag{ExpectedEtag: p2.Msg.Policy.Etag}}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	afterP3 := f.read(t)
	if !proto.Equal(afterP3.ActiveGeneration, before.ActiveGeneration) {
		t.Fatal("independent P2-to-P3 changed generation; wrong CAS oracle")
	}
	_, err = f.apply.RollbackConfiguration(f.ctx, nha056SessionRequest(request, f.token))
	nha055Conflict(t, err, connect.CodeAborted)
	request.Expected = afterP3.Version
	request.ExpectedWorkspaceSha256 = afterP3.GetWorkspace().SourceSha256
	request.Sources = []*wire.SourcePrecondition{nha055PolicyExpected(t, account, nha055PolicyVersion(t, p3.Msg.Policy), nil)}
	restored, err := f.apply.RollbackConfiguration(f.ctx, nha056SessionRequest(request, f.token))
	if err != nil || !restored.Msg.Done {
		t.Fatal("050 documentary P1 was not restored through149/029", err)
	}
	restoredCurrent := f.read(t)
	actual, err := f.policies.GetAuthorizationPolicy(f.ctx, nha056SessionRequest(&configuration.GetAuthorizationPolicyRequest{Name: accountName + "/authorizationPolicy"}, f.token))
	if err != nil || actual.Msg == nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual.Msg.YamlSource, p1Raw) || nha055PolicyVersion(t, actual.Msg).Revision != nha055PolicyVersion(t, p3.Msg.Policy).Revision+1 || restoredCurrent.ActiveGeneration.Revision != before.ActiveGeneration.Revision+1 {
		t.Fatal("rollback substituted latest bytes or rewound policy/generation revisions")
	}
	exact, err := os.ReadFile(f.path)
	if err != nil || !bytes.Equal(exact, f.raw) {
		t.Fatal("documentary policy rollback changed operational YAML", err)
	}
	nha055PolicyRollbackNoCommands(t, f)
	// Withdraw the caller's real admin role. The historical policy source
	// cannot recover that permission even with fresh current-policy CAS.
	if _, err := f.accounts.CreateAccount(f.ctx, nha056SessionRequest(&configuration.CreateAccountRequest{Parent: nha203SessionAccount(t, f.tokens.Session).Home().String(), Account: &configuration.Account{LoginName: "NHA055 alternate admin", DisplayName: "NHA055 alternate admin", Role: configuration.AccountRole_ACCOUNT_ROLE_ADMIN, InitialPassword: "NHA055 alternate admin password"}}, f.token)); err != nil {
		t.Fatal(err)
	}
	other := nha162RunLogin(t, f.ctx, f.authentication, "NHA055 alternate admin", "NHA055 alternate admin password")
	currentAccount, err := f.accounts.GetAccount(f.ctx, nha056SessionRequest(&configuration.GetAccountRequest{Name: nha203SessionAccount(t, f.tokens.Session).String()}, other.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.accounts.UpdateAccount(f.ctx, nha056SessionRequest(&configuration.UpdateAccountRequest{Account: &configuration.Account{Name: currentAccount.Msg.Name, Etag: currentAccount.Msg.Etag, Role: configuration.AccountRole_ACCOUNT_ROLE_USER}}, other.AccessToken)); err != nil {
		t.Fatal(err)
	}

	denied := proto.Clone(request).(*wire.RollbackConfigurationRequest)
	denied.Expected = restoredCurrent.Version
	denied.ExpectedWorkspaceSha256 = restoredCurrent.GetWorkspace().SourceSha256
	denied.Sources = []*wire.SourcePrecondition{nha055PolicyExpected(t, account, nha055PolicyVersion(t, actual.Msg), p1Raw)}
	_, err = f.apply.RollbackConfiguration(f.ctx, nha056SessionRequest(denied, f.token))
	if code := connect.CodeOf(err); code != connect.CodePermissionDenied && code != connect.CodeUnauthenticated {
		t.Fatal("historical policy bytes restored revoked administrative permission", err)
	}
	still, err := f.policies.GetAuthorizationPolicy(f.ctx, nha056SessionRequest(&configuration.GetAuthorizationPolicyRequest{Name: accountName + "/authorizationPolicy"}, other.AccessToken))
	if err != nil || !bytes.Equal(still.Msg.YamlSource, p1Raw) || nha055PolicyVersion(t, still.Msg).Revision != nha055PolicyVersion(t, actual.Msg).Revision {
		t.Fatal("refused rollback changed current policy or created a revision", err)
	}
	nha055PolicyRollbackNoCommands(t, f)
}

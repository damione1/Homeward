package server

import (
	"bytes"
	"fmt"
	"os"
	"testing"

	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	configuration "github.com/damione1/Homeward/gen/home/configuration/v1"
	wire "github.com/damione1/Homeward/gen/home/configurationapply/v1"
	planwire "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	"github.com/damione1/Homeward/internal/authorization"
)

// A self-policy mutation changes the caller's rights binding intentionally.
// Its actual authenticated response must preserve the published transaction.
func TestNHA055ActualRunTLSSelfPolicyApplyRollbackReportsCommittedChanges(t *testing.T) {
	f := nha055StartTLS(t)
	account := f.tokens.Session.AccountId
	initial, err := f.policies.GetPolicy(f.ctx, nha056SessionRequest(&auth.GetPolicyRequest{AccountId: account}, f.token))
	if err != nil || initial.Msg.GetPresent() == nil {
		t.Fatal("initial own policy", err)
	}
	first := initial.Msg.GetPresent()
	accountID, err := contracts.ParseAccountID(account.Value)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := authorization.AdmitPolicySource(accountID, first.YamlSource)
	if err != nil {
		t.Fatal(err)
	}
	priorSemantic, err := prior.SemanticSHA256()
	if err != nil {
		t.Fatal(err)
	}
	choices := [][]byte{
		[]byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n - scope: {family: state_registry, operation: read}\n   objects: {entities: {all: {capabilities: {all: {}}}}}\n", account.Value)),
		[]byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants: []\n", account.Value)),
	}
	var changedRaw []byte
	for _, raw := range choices {
		source, err := authorization.AdmitPolicySource(accountID, raw)
		if err != nil {
			t.Fatal(err)
		}
		semantic, err := source.SemanticSHA256()
		if err != nil {
			t.Fatal(err)
		}
		if semantic != priorSemantic {
			changedRaw = raw
			break
		}
	}
	if len(changedRaw) == 0 {
		t.Fatal("fixture did not change actual policy semantics")
	}
	before := f.read(t)
	request := nha055Request(before, nha055PolicyCandidate(t, account, changedRaw), nil)
	request.Sources = []*wire.SourcePrecondition{nha055PolicyExpected(t, account, first.Version, nil)}
	applied, err := f.apply.Apply(f.ctx, nha056SessionRequest(request, f.token))
	if err != nil {
		t.Fatal("authorized self-policy commit must return its published result", err)
	}
	if applied.Msg.Activation.Outcome != configuration.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED || applied.Msg.Activation.Transaction != configuration.TransactionOutcome_TRANSACTION_OUTCOME_COMMITTED || applied.Msg.Current == nil {
		t.Fatal("self-policy apply lost its committed publication")
	}
	changed, err := f.policies.GetPolicy(f.ctx, nha056SessionRequest(&auth.GetPolicyRequest{AccountId: account}, f.token))
	if err != nil || changed.Msg.GetPresent() == nil {
		t.Fatal(err)
	}
	current := changed.Msg.GetPresent()
	if !bytes.Equal(current.YamlSource, changedRaw) || current.Version.Revision != first.Version.Revision+1 || applied.Msg.Current.ActiveGeneration.Revision != before.ActiveGeneration.Revision+1 {
		t.Fatal("self-policy commit did not create exact new policy and generation revisions")
	}
	restore := &wire.RollbackRequest{Id: before.Document.Id, Expected: applied.Msg.Current.Document.Version, ExpectedWorkspaceSha256: applied.Msg.Current.Document.GetWorkspace().SourceSha256, Family: planwire.SourceFamily_SOURCE_FAMILY_AUTHORIZATION, Identity: account.Value, HistoricalPolicyVersion: &configuration.DocumentVersion{Revision: int64(first.Version.Revision), SourceSha256: bytes.Clone(first.Version.SourceSha256)}, Sources: []*wire.SourcePrecondition{nha055PolicyExpected(t, account, current.Version, changedRaw)}}
	rolledBack, err := f.apply.Rollback(f.ctx, nha056SessionRequest(restore, f.token))
	if err != nil {
		t.Fatal("authorized self-policy rollback must return its published result", err)
	}
	if rolledBack.Msg.Activation.Outcome != configuration.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED || rolledBack.Msg.Activation.Transaction != configuration.TransactionOutcome_TRANSACTION_OUTCOME_COMMITTED || rolledBack.Msg.Current == nil {
		t.Fatal("self-policy rollback lost its committed publication")
	}
	actual, err := f.policies.GetPolicy(f.ctx, nha056SessionRequest(&auth.GetPolicyRequest{AccountId: account}, f.token))
	if err != nil || actual.Msg.GetPresent() == nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual.Msg.GetPresent().YamlSource, first.YamlSource) || actual.Msg.GetPresent().Version.Revision != current.Version.Revision+1 || rolledBack.Msg.Current.ActiveGeneration.Revision != applied.Msg.Current.ActiveGeneration.Revision+1 {
		t.Fatal("self-policy rollback rewound or substituted content")
	}
	operational, err := os.ReadFile(f.path)
	if err != nil || !bytes.Equal(operational, f.raw) {
		t.Fatal("self-policy mutation changed operational YAML", err)
	}
	nha055PolicyRollbackNoCommands(t, f)
}

package server

import (
	"bytes"
	"fmt"
	"os"
	"testing"

	"github.com/damione1/Homeward/contracts"
	configuration "github.com/damione1/Homeward/gen/home/api/v2"
	planwire "github.com/damione1/Homeward/gen/home/api/v2"
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/authorization"
)

// A self-policy mutation changes the caller's rights binding intentionally.
// Its actual authenticated response must preserve the published transaction.
func TestNHA055ActualRunTLSSelfPolicyApplyRollbackReportsCommittedChanges(t *testing.T) {
	f := nha055StartTLS(t)
	accountName := nha203SessionAccount(t, f.tokens.Session).String()
	account := nha203NativeAccount(t, f.ctx, f.run.observed.sessions.store, accountName)
	initial, err := f.policies.GetAuthorizationPolicy(f.ctx, nha056SessionRequest(&configuration.GetAuthorizationPolicyRequest{Name: accountName + "/authorizationPolicy"}, f.token))
	if err != nil || initial.Msg == nil {
		t.Fatal("initial own policy", err)
	}
	first := initial.Msg
	accountID, err := contracts.ParseAccountID(nha203AccountText(t, account))
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
		[]byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n - scope: {family: state_registry, operation: read}\n   objects: {entities: {all: {capabilities: {all: {}}}}}\n", nha203AccountText(t, account))),
		[]byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants: []\n", nha203AccountText(t, account))),
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
	request := nha055Request(before, nha055PolicyCandidate(t, account, changedRaw), f.build(t, nha055PolicyCandidate(t, account, changedRaw)))
	request.Sources = []*wire.SourcePrecondition{nha055PolicyExpected(t, account, nha055PolicyVersion(t, first), nil)}
	applied, err := f.apply.ApplyConfigurationPlan(f.ctx, nha056SessionRequest(request, f.token))
	if err != nil {
		t.Fatal("authorized self-policy commit must return its published result", err)
	}
	if !applied.Msg.Done {
		t.Fatal("self-policy apply lost its committed publication")
	}
	appliedCurrent := f.read(t)
	changed, err := f.policies.GetAuthorizationPolicy(f.ctx, nha056SessionRequest(&configuration.GetAuthorizationPolicyRequest{Name: accountName + "/authorizationPolicy"}, f.token))
	if err != nil || changed.Msg == nil {
		t.Fatal(err)
	}
	current := changed.Msg
	if !bytes.Equal(current.YamlSource, changedRaw) || nha055PolicyVersion(t, current).Revision != nha055PolicyVersion(t, first).Revision+1 || appliedCurrent.ActiveGeneration.Revision != before.ActiveGeneration.Revision+1 {
		t.Fatal("self-policy commit did not create exact new policy and generation revisions")
	}
	restore := &wire.RollbackConfigurationRequest{Name: before.Name, Expected: appliedCurrent.Version, ExpectedWorkspaceSha256: appliedCurrent.GetWorkspace().SourceSha256, Family: planwire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_AUTHORIZATION, Identity: nha203AccountText(t, account), HistoricalPolicyVersion: &configuration.DocumentVersion{Revision: int64(nha055PolicyVersion(t, first).Revision), SourceSha256: bytes.Clone(nha055PolicyVersion(t, first).SourceSha256)}, Sources: []*wire.SourcePrecondition{nha055PolicyExpected(t, account, nha055PolicyVersion(t, current), changedRaw)}}
	rolledBack, err := f.apply.RollbackConfiguration(f.ctx, nha056SessionRequest(restore, f.token))
	if err != nil {
		t.Fatal("authorized self-policy rollback must return its published result", err)
	}
	if !rolledBack.Msg.Done {
		t.Fatal("self-policy rollback lost its committed publication")
	}
	rolledBackCurrent := f.read(t)
	actual, err := f.policies.GetAuthorizationPolicy(f.ctx, nha056SessionRequest(&configuration.GetAuthorizationPolicyRequest{Name: accountName + "/authorizationPolicy"}, f.token))
	if err != nil || actual.Msg == nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual.Msg.YamlSource, first.YamlSource) || nha055PolicyVersion(t, actual.Msg).Revision != nha055PolicyVersion(t, current).Revision+1 || rolledBackCurrent.ActiveGeneration.Revision != appliedCurrent.ActiveGeneration.Revision+1 {
		t.Fatal("self-policy rollback rewound or substituted content")
	}
	operational, err := os.ReadFile(f.path)
	if err != nil || !bytes.Equal(operational, f.raw) {
		t.Fatal("self-policy mutation changed operational YAML", err)
	}
	nha055PolicyRollbackNoCommands(t, f)
}

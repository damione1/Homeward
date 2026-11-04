package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	configuration "github.com/damione1/Homeward/gen/home/api/v2"
	planwire "github.com/damione1/Homeward/gen/home/api/v2"
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

func nha055PolicyCandidate(t *testing.T, account contracts.AccountID, raw []byte) *planwire.ConfigurationPlanCandidate {
	t.Helper()
	id := account
	binding, err := storage.AuthorizationPolicyBinding(id)
	if err != nil {
		t.Fatal(err)
	}
	return &planwire.ConfigurationPlanCandidate{Family: planwire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_AUTHORIZATION, Identity: nha203AccountText(t, account), Entry: binding.Path(), Files: []*planwire.ConfigurationPlanSourceFile{{LogicalPath: binding.Path(), ExactSource: raw}}}
}
func nha055PolicyExpected(t *testing.T, account contracts.AccountID, version *configuration.DocumentVersion, workspace []byte) *wire.SourcePrecondition {
	t.Helper()
	id := account
	binding, err := storage.AuthorizationPolicyBinding(id)
	if err != nil {
		t.Fatal(err)
	}
	expected := &wire.SourcePrecondition{Id: &configuration.DocumentId{Value: binding.ID().String()}, LogicalPath: binding.Path(), SqlState: &wire.SourcePrecondition_Existing{Existing: &configuration.DocumentVersion{Revision: int64(version.Revision), SourceSha256: bytes.Clone(version.SourceSha256)}}}
	if workspace == nil {
		expected.WorkspaceState = &wire.SourcePrecondition_WorkspaceAbsent{WorkspaceAbsent: &emptypb.Empty{}}
	} else {
		hash := sha256.Sum256(workspace)
		expected.WorkspaceState = &wire.SourcePrecondition_WorkspaceSha256{WorkspaceSha256: hash[:]}
	}
	return expected
}

func nha055PolicyVersion(t *testing.T, policy *configuration.AuthorizationPolicy) *configuration.DocumentVersion {
	t.Helper()
	revision, hash, err := parseAuthPolicyEtag(authAuthorizationPolicy, policy.Etag)
	if err != nil {
		t.Fatal(err)
	}
	return &configuration.DocumentVersion{Revision: int64(revision), SourceSha256: hash[:]}
}

// PG24 and the actual TLS Run are used; no physical pilot/action is configured.
func TestNHA055ActualRunTLSAuthorizationApplyRollbackAndIndependentActiveCAS(t *testing.T) {
	f := nha055StartTLS(t)
	created, err := f.accounts.CreateAccount(f.ctx, nha056SessionRequest(&configuration.CreateAccountRequest{Parent: nha203SessionAccount(t, f.tokens.Session).Home().String(), Account: &configuration.Account{LoginName: "NHA055 policy user", DisplayName: "NHA055 policy user", Role: configuration.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: "NHA055 isolated policy password"}}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	accountName := created.Msg.Name
	account := nha203NativeAccount(t, f.ctx, f.run.observed.sessions.store, accountName)
	policy, err := f.policies.GetAuthorizationPolicy(f.ctx, nha056SessionRequest(&configuration.GetAuthorizationPolicyRequest{Name: accountName + "/authorizationPolicy"}, f.token))
	if err != nil || policy.Msg == nil {
		t.Fatal("initial active policy", err)
	}
	before := f.read(t)
	firstRaw := []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n - scope: {family: state_registry, operation: read}\n   objects: {entities: {all: {capabilities: {all: {}}}}}\n", nha203AccountText(t, account)))
	firstCandidate := nha055PolicyCandidate(t, account, firstRaw)
	plan := f.build(t, firstCandidate)
	request := nha055Request(before, firstCandidate, plan)
	request.Sources = []*wire.SourcePrecondition{nha055PolicyExpected(t, account, nha055PolicyVersion(t, policy.Msg), nil)}
	ordinary := nha162RunLogin(t, f.ctx, f.authentication, "NHA055 policy user", "NHA055 isolated policy password")
	if _, err := f.apply.ApplyConfigurationPlan(f.ctx, nha056SessionRequest(request, ordinary.AccessToken)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("optional diagnostic receipt granted ordinary user configuration permission", err)
	}
	first, err := f.apply.ApplyConfigurationPlan(f.ctx, nha056SessionRequest(request, f.token))
	if err != nil || !first.Msg.Done {
		t.Fatal("policy apply through common owner", err)
	}
	firstCurrent := f.read(t)
	firstPolicy, err := f.policies.GetAuthorizationPolicy(f.ctx, nha056SessionRequest(&configuration.GetAuthorizationPolicyRequest{Name: accountName + "/authorizationPolicy"}, f.token))
	if err != nil || !bytes.Equal(firstPolicy.Msg.YamlSource, firstRaw) {
		t.Fatal("active exact policy source was not changed", err)
	}
	if nha055PolicyVersion(t, firstPolicy.Msg).Revision != nha055PolicyVersion(t, policy.Msg).Revision+1 || firstCurrent.ActiveGeneration.Revision != before.ActiveGeneration.Revision+1 {
		t.Fatal("policy or configuration revision was not monotone")
	}
	secondRaw := append(bytes.Clone(firstRaw), []byte("# exact second retained policy version\n")...)
	secondCandidate := nha055PolicyCandidate(t, account, secondRaw)
	secondRequest := nha055Request(firstCurrent, secondCandidate, f.build(t, secondCandidate))
	secondRequest.Sources = []*wire.SourcePrecondition{nha055PolicyExpected(t, account, nha055PolicyVersion(t, firstPolicy.Msg), firstRaw)}
	second, err := f.apply.ApplyConfigurationPlan(f.ctx, nha056SessionRequest(secondRequest, f.token))
	if err != nil || !second.Msg.Done {
		t.Fatal("optional-plan policy apply", err)
	}
	secondCurrent := f.read(t)
	secondPolicy, err := f.policies.GetAuthorizationPolicy(f.ctx, nha056SessionRequest(&configuration.GetAuthorizationPolicyRequest{Name: accountName + "/authorizationPolicy"}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	restore := &wire.RollbackConfigurationRequest{Name: secondCurrent.Name, Expected: secondCurrent.Version, ExpectedWorkspaceSha256: secondCurrent.GetWorkspace().SourceSha256, Historical: firstCurrent.ActiveGeneration, Family: planwire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_AUTHORIZATION, Identity: nha203AccountText(t, account), Sources: []*wire.SourcePrecondition{nha055PolicyExpected(t, account, nha055PolicyVersion(t, secondPolicy.Msg), secondRaw)}}
	restored, err := f.apply.RollbackConfiguration(f.ctx, nha056SessionRequest(restore, f.token))
	if err != nil || !restored.Msg.Done {
		t.Fatal("exact policy rollback through common writer", err)
	}
	restoredCurrent := f.read(t)
	currentPolicy, err := f.policies.GetAuthorizationPolicy(f.ctx, nha056SessionRequest(&configuration.GetAuthorizationPolicyRequest{Name: accountName + "/authorizationPolicy"}, f.token))
	if err != nil || !bytes.Equal(currentPolicy.Msg.YamlSource, firstRaw) || nha055PolicyVersion(t, currentPolicy.Msg).Revision != nha055PolicyVersion(t, secondPolicy.Msg).Revision+1 {
		t.Fatal("policy rollback rewound revision or changed exact bytes", err)
	}
	accountID, _ := contracts.ParseAccountID(nha203AccountText(t, account))
	binding, _ := storage.AuthorizationPolicyBinding(accountID)
	groupHash := sha256.Sum256([]byte(binding.ID().String()))
	exact, err := os.ReadFile(filepath.Join(filepath.Dir(f.path), "configuration.d", hex.EncodeToString(groupHash[:]), filepath.FromSlash(binding.Path())))
	if err != nil || !bytes.Equal(exact, firstRaw) {
		t.Fatal("policy workspace did not follow published active pointer", err)
	}
	operational, err := os.ReadFile(f.path)
	if err != nil || !bytes.Equal(operational, f.raw) {
		t.Fatal("policy edit changed operational source", err)
	}
	// Change only the independent policy authority after preview. The active
	// configuration generation remains identical, but the receipt must be stale.
	staleCandidate := nha055PolicyCandidate(t, account, secondRaw)
	stalePlan := f.build(t, staleCandidate)
	changed := append(bytes.Clone(firstRaw), []byte("# direct050 independent policy change\n")...)
	if _, err := f.policies.ReplaceAuthorizationPolicy(f.ctx, nha056SessionRequest(&configuration.ReplaceAuthorizationPolicyRequest{Name: accountName + "/authorizationPolicy", YamlSource: changed, Precondition: &configuration.ReplaceAuthorizationPolicyRequest_ExpectedEtag{ExpectedEtag: currentPolicy.Msg.Etag}}, f.token)); err != nil {
		t.Fatal(err)
	}
	afterDirect := f.read(t)
	if !proto.Equal(afterDirect.ActiveGeneration, restoredCurrent.ActiveGeneration) {
		t.Fatal("independent policy CAS test changed generation")
	}
	staleRequest := nha055Request(afterDirect, staleCandidate, stalePlan)
	staleRequest.Sources = []*wire.SourcePrecondition{nha055PolicyExpected(t, account, nha055PolicyVersion(t, currentPolicy.Msg), firstRaw)}
	_, err = f.apply.ApplyConfigurationPlan(f.ctx, nha056SessionRequest(staleRequest, f.token))
	nha055Conflict(t, err, connect.CodeAborted)
	freshPlanWithStalePreconditions := nha055Request(afterDirect, staleCandidate, f.build(t, staleCandidate))
	freshPlanWithStalePreconditions.Sources = staleRequest.Sources
	_, err = f.apply.ApplyConfigurationPlan(f.ctx, nha056SessionRequest(freshPlanWithStalePreconditions, f.token))
	nha055Conflict(t, err, connect.CodeAborted)
	still, err := f.policies.GetAuthorizationPolicy(f.ctx, nha056SessionRequest(&configuration.GetAuthorizationPolicyRequest{Name: accountName + "/authorizationPolicy"}, f.token))
	if err != nil || !bytes.Equal(still.Msg.YamlSource, changed) {
		t.Fatal("stale apply overwrote independent active policy", err)
	}
}

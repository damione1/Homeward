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
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	configuration "github.com/damione1/Homeward/gen/home/configuration/v1"
	wire "github.com/damione1/Homeward/gen/home/configurationapply/v1"
	planwire "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

func nha055PolicyCandidate(t *testing.T, account *auth.AccountId, raw []byte) *planwire.Candidate {
	t.Helper()
	id, err := contracts.ParseAccountID(account.Value)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := storage.AuthorizationPolicyBinding(id)
	if err != nil {
		t.Fatal(err)
	}
	return &planwire.Candidate{Family: planwire.SourceFamily_SOURCE_FAMILY_AUTHORIZATION, Identity: account.Value, Entry: binding.Path(), Files: []*planwire.SourceFile{{LogicalPath: binding.Path(), ExactSource: raw}}}
}
func nha055PolicyExpected(t *testing.T, account *auth.AccountId, version *auth.PolicyVersion, workspace []byte) *wire.SourcePrecondition {
	t.Helper()
	id, err := contracts.ParseAccountID(account.Value)
	if err != nil {
		t.Fatal(err)
	}
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

// PG24 and the actual TLS Run are used; no physical pilot/action is configured.
func TestNHA055ActualRunTLSAuthorizationApplyRollbackAndIndependentActiveCAS(t *testing.T) {
	f := nha055StartTLS(t)
	created, err := f.accounts.CreateAccount(f.ctx, nha056SessionRequest(&auth.CreateAccountRequest{LoginName: "NHA055 policy user", DisplayName: "NHA055 policy user", Role: auth.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: "NHA055 isolated policy password"}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	account := created.Msg.Account.Id
	policy, err := f.policies.GetPolicy(f.ctx, nha056SessionRequest(&auth.GetPolicyRequest{AccountId: account}, f.token))
	if err != nil || policy.Msg.GetPresent() == nil {
		t.Fatal("initial active policy", err)
	}
	before := f.read(t)
	firstRaw := []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n - scope: {family: state_registry, operation: read}\n   objects: {entities: {all: {capabilities: {all: {}}}}}\n", account.Value))
	firstCandidate := nha055PolicyCandidate(t, account, firstRaw)
	plan := f.build(t, firstCandidate)
	request := nha055Request(before, firstCandidate, &plan.Receipt)
	request.Sources = []*wire.SourcePrecondition{nha055PolicyExpected(t, account, policy.Msg.GetPresent().Version, nil)}
	ordinary := nha162RunLogin(t, f.ctx, f.authentication, "NHA055 policy user", "NHA055 isolated policy password")
	if _, err := f.apply.Apply(f.ctx, nha056SessionRequest(request, ordinary.AccessToken)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("optional diagnostic receipt granted ordinary user configuration permission", err)
	}
	first, err := f.apply.Apply(f.ctx, nha056SessionRequest(request, f.token))
	if err != nil || first.Msg.Activation.Outcome != configuration.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED {
		t.Fatal("policy apply through common owner", err)
	}
	firstPolicy, err := f.policies.GetPolicy(f.ctx, nha056SessionRequest(&auth.GetPolicyRequest{AccountId: account}, f.token))
	if err != nil || !bytes.Equal(firstPolicy.Msg.GetPresent().YamlSource, firstRaw) {
		t.Fatal("active exact policy source was not changed", err)
	}
	if firstPolicy.Msg.GetPresent().Version.Revision != policy.Msg.GetPresent().Version.Revision+1 || first.Msg.Current.ActiveGeneration.Revision != before.ActiveGeneration.Revision+1 {
		t.Fatal("policy or configuration revision was not monotone")
	}
	secondRaw := append(bytes.Clone(firstRaw), []byte("# exact second retained policy version\n")...)
	secondRequest := nha055Request(first.Msg.Current, nha055PolicyCandidate(t, account, secondRaw), nil)
	secondRequest.Sources = []*wire.SourcePrecondition{nha055PolicyExpected(t, account, firstPolicy.Msg.GetPresent().Version, firstRaw)}
	second, err := f.apply.Apply(f.ctx, nha056SessionRequest(secondRequest, f.token))
	if err != nil || second.Msg.Activation.Outcome != configuration.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED {
		t.Fatal("optional-plan policy apply", err)
	}
	secondPolicy, err := f.policies.GetPolicy(f.ctx, nha056SessionRequest(&auth.GetPolicyRequest{AccountId: account}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	restore := &wire.RollbackRequest{Id: second.Msg.Current.Document.Id, Expected: second.Msg.Current.Document.Version, ExpectedWorkspaceSha256: second.Msg.Current.Document.GetWorkspace().SourceSha256, Historical: first.Msg.Current.ActiveGeneration, Family: planwire.SourceFamily_SOURCE_FAMILY_AUTHORIZATION, Identity: account.Value, Sources: []*wire.SourcePrecondition{nha055PolicyExpected(t, account, secondPolicy.Msg.GetPresent().Version, secondRaw)}}
	restored, err := f.apply.Rollback(f.ctx, nha056SessionRequest(restore, f.token))
	if err != nil || restored.Msg.Activation.Outcome != configuration.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED {
		t.Fatal("exact policy rollback through common writer", err)
	}
	currentPolicy, err := f.policies.GetPolicy(f.ctx, nha056SessionRequest(&auth.GetPolicyRequest{AccountId: account}, f.token))
	if err != nil || !bytes.Equal(currentPolicy.Msg.GetPresent().YamlSource, firstRaw) || currentPolicy.Msg.GetPresent().Version.Revision != secondPolicy.Msg.GetPresent().Version.Revision+1 {
		t.Fatal("policy rollback rewound revision or changed exact bytes", err)
	}
	accountID, _ := contracts.ParseAccountID(account.Value)
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
	if _, err := f.policies.ReplacePolicy(f.ctx, nha056SessionRequest(&auth.ReplacePolicyRequest{AccountId: account, YamlSource: changed, Precondition: &auth.ReplacePolicyRequest_Expected{Expected: currentPolicy.Msg.GetPresent().Version}}, f.token)); err != nil {
		t.Fatal(err)
	}
	afterDirect := f.read(t)
	if !proto.Equal(afterDirect.ActiveGeneration, restored.Msg.Current.ActiveGeneration) {
		t.Fatal("independent policy CAS test changed generation")
	}
	staleRequest := nha055Request(afterDirect, staleCandidate, &stalePlan.Receipt)
	staleRequest.Sources = []*wire.SourcePrecondition{nha055PolicyExpected(t, account, currentPolicy.Msg.GetPresent().Version, firstRaw)}
	_, err = f.apply.Apply(f.ctx, nha056SessionRequest(staleRequest, f.token))
	nha055Conflict(t, err, connect.CodeAborted)
	staleWithoutReceipt := nha055Request(afterDirect, staleCandidate, nil)
	staleWithoutReceipt.Sources = staleRequest.Sources
	_, err = f.apply.Apply(f.ctx, nha056SessionRequest(staleWithoutReceipt, f.token))
	nha055Conflict(t, err, connect.CodeAborted)
	still, err := f.policies.GetPolicy(f.ctx, nha056SessionRequest(&auth.GetPolicyRequest{AccountId: account}, f.token))
	if err != nil || !bytes.Equal(still.Msg.GetPresent().YamlSource, changed) {
		t.Fatal("stale apply overwrote independent active policy", err)
	}
}

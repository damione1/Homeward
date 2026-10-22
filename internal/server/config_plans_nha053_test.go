package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	configwire "github.com/damione1/Homeward/gen/home/configuration/v1"
	"github.com/damione1/Homeward/gen/home/configuration/v1/configurationv1connect"
	wire "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	"github.com/damione1/Homeward/gen/home/configurationplan/v1/configurationplanv1connect"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
	"google.golang.org/protobuf/proto"
)

type nha053TLSFixture struct {
	nha149TLSFixture
	plans    configurationplanv1connect.ConfigurationPlanServiceClient
	policies authv1connect.AuthorizationPolicyServiceClient
}

func nha053StartTLS(t *testing.T) nha053TLSFixture {
	t.Helper()
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	cfg := nha181Config(t, cluster.Port, data, "127.0.0.1:0", nha181TLSYAML())
	source, e := cfg.startup.CaptureWorkspace()
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "configuration.yaml")
	if e := os.WriteFile(path, source.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	startup, e := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029TestEnv)
	if e != nil {
		t.Fatal(e)
	}
	run := nha162StartDefaultRun(t, startup)
	client := nha162RunClient(t, roots, run.address)
	base := "https://" + run.address.String()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	const password = "NHA053 synthetic isolated password"
	nha162RunBootstrap(t, ctx, data, "NHA053 admin", password)
	authentication := authv1connect.NewAuthServiceClient(client, base)
	tokens := nha162RunLogin(t, ctx, authentication, "NHA053 admin", password)
	return nha053TLSFixture{nha149TLSFixture: nha149TLSFixture{api: configurationv1connect.NewConfigurationServiceClient(client, base), authentication: authentication, accounts: authv1connect.NewAccountServiceClient(client, base), tokens: tokens, token: tokens.AccessToken, path: path, raw: source.Bytes(), run: run, ctx: ctx}, plans: configurationplanv1connect.NewConfigurationPlanServiceClient(client, base), policies: authv1connect.NewAuthorizationPolicyServiceClient(client, base)}
}
func nha053WireCandidate(family wire.SourceFamily, identity string, source []byte) *wire.Candidate {
	return &wire.Candidate{Family: family, Identity: identity, Entry: "configuration.yaml", Files: []*wire.SourceFile{{LogicalPath: "configuration.yaml", ExactSource: bytes.Clone(source)}}}
}
func (f nha053TLSFixture) build(t *testing.T, candidate *wire.Candidate) *wire.Plan {
	t.Helper()
	response, e := f.plans.BuildPlan(f.ctx, nha056SessionRequest(&wire.BuildPlanRequest{Candidate: candidate}, f.token))
	if e != nil {
		t.Fatal("actual Run/TLS plan", e)
	}
	if e := contracts.AdmitConfigurationPlanResponse(response.Msg); e != nil {
		t.Fatal(e)
	}
	return response.Msg.Plan
}
func (f nha053TLSFixture) unchanged(t *testing.T, before *configwire.ReadDocumentResponse, raw []byte, info os.FileInfo) {
	t.Helper()
	after := f.read(t)
	current, e := os.ReadFile(f.path)
	if e != nil {
		t.Fatal(e)
	}
	stat, e := os.Stat(f.path)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(current, raw) || stat.Mode() != info.Mode() || !stat.ModTime().Equal(info.ModTime()) || !os.SameFile(info, stat) || !proto.Equal(after.Document.Version, before.Document.Version) || !proto.Equal(after.ActiveGeneration, before.ActiveGeneration) {
		t.Fatal("diagnostic mutated file identity/bytes/mode/time or active SQL generation")
	}
	entries, e := os.ReadDir(filepath.Dir(f.path))
	if e != nil {
		t.Fatal(e)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "staging") || strings.Contains(entry.Name(), "intent") || strings.Contains(entry.Name(), "reconcile") {
			t.Fatal("plan created workspace mutation artifacts")
		}
	}
}
func TestNHA053ActualRunTLSPurePlanAndStaleAfterCAS(t *testing.T) {
	f := nha053StartTLS(t)
	before := f.read(t)
	info, e := os.Stat(f.path)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(f.path)
	if e != nil {
		t.Fatal(e)
	}
	candidate := nha053WireCandidate(wire.SourceFamily_SOURCE_FAMILY_OPERATIONAL, "operational.main", append(bytes.Clone(raw), []byte("# NHA053_PRIVATE_COMMENT_CANARY\n")...))
	plan := f.build(t, candidate)
	digest := sha256.Sum256(candidate.Files[0].ExactSource)
	if plan.State != wire.PlanState_PLAN_STATE_ADMITTED || plan.Author != f.tokens.Session.AccountId.Value || !bytes.Equal(plan.SourceSha256, digest[:]) || plan.Base.Kind != wire.BaseKind_BASE_KIND_ACTIVE_SQL || plan.Base.GetRevision() != before.Document.Version.Revision || !bytes.Equal(plan.Base.SourceSha256, before.Document.Version.SourceSha256) || plan.GenerationRevision != before.ActiveGeneration.Revision || plan.SchemaVersion != "homeward.operational/1" || plan.Effect != wire.Effect_EFFECT_NONE || len(plan.Changes) != 0 || plan.ApplicationAvailable {
		t.Fatal("plan lost provenance or invented an effect/application")
	}
	encoded, e := proto.Marshal(plan)
	if e != nil || bytes.Contains(encoded, []byte("NHA053_PRIVATE_COMMENT_CANARY")) {
		t.Fatal("source canary leaked in public response")
	}
	f.unchanged(t, before, raw, info)
	fresh, e := f.plans.RevalidatePlan(f.ctx, nha056SessionRequest(&wire.RevalidatePlanRequest{Candidate: candidate, Receipt: plan.Receipt}, f.token))
	if e != nil || fresh.Msg.Plan.State != wire.PlanState_PLAN_STATE_ADMITTED {
		t.Fatal("unchanged candidate could not revalidate", e)
	}
	// B changes the exact source/generation through real 149/029, with identical
	// semantics. The old receipt must still become stale; no synthetic stamps.
	changed, e := f.write(before, append(bytes.Clone(raw), []byte("# concurrent external API edit B\n")...))
	if e != nil || changed.Msg.Activation.Outcome != configwire.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED {
		t.Fatal("real base change failed", e)
	}
	stale, e := f.plans.RevalidatePlan(f.ctx, nha056SessionRequest(&wire.RevalidatePlanRequest{Candidate: candidate, Receipt: plan.Receipt}, f.token))
	if e != nil || stale.Msg.Plan.State != wire.PlanState_PLAN_STATE_STALE || stale.Msg.Plan.Receipt != "" {
		t.Fatal("exact base change remained fresh", e)
	}
	recalculated := f.build(t, candidate)
	if recalculated.State != wire.PlanState_PLAN_STATE_ADMITTED || recalculated.Base.GetRevision() <= plan.Base.GetRevision() {
		t.Fatal("explicit recalculation did not use new authority")
	}
	altered := proto.Clone(candidate).(*wire.Candidate)
	altered.Files[0].ExactSource = append(altered.Files[0].ExactSource, '\n')
	if _, e := f.plans.RevalidatePlan(f.ctx, nha056SessionRequest(&wire.RevalidatePlanRequest{Candidate: altered, Receipt: plan.Receipt}, f.token)); connect.CodeOf(e) != connect.CodeInvalidArgument {
		t.Fatal("receipt authenticated different content", e)
	}
}
func TestNHA053ActualRunTLSRejectedCandidatesLeaveGenerationAndWorkspace(t *testing.T) {
	f := nha053StartTLS(t)
	before := f.read(t)
	raw, e := os.ReadFile(f.path)
	if e != nil {
		t.Fatal(e)
	}
	info, e := os.Stat(f.path)
	if e != nil {
		t.Fatal(e)
	}
	candidates := []*wire.Candidate{
		nha053WireCandidate(wire.SourceFamily_SOURCE_FAMILY_OPERATIONAL, "operational.main", []byte("unknown_future_key: NHA053_SECRET_REJECTED_CANARY\n")),
		nha053WireCandidate(wire.SourceFamily_SOURCE_FAMILY_OPERATIONAL, "operational.main", []byte("external_workers: [\n")),
		nha053WireCandidate(wire.SourceFamily_SOURCE_FAMILY_DECLARATIVE_V1, "new.helpers", []byte("input_boolean: {excluded_outside_manifest: {initial: false}}\n")),
		nha053WireCandidate(wire.SourceFamily_SOURCE_FAMILY_DASHBOARD, "dashboard.preview", []byte("views:\n - type: sections\n   path: preview\n   title: Preview\n   sections:\n    - type: grid\n      cards:\n       - type: tile\n         entity: light.excluded_outside_manifest\n")),
	}
	for i, candidate := range candidates {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			plan := f.build(t, candidate)
			if plan.State != wire.PlanState_PLAN_STATE_REJECTED || len(plan.Diagnostics) == 0 || plan.SemanticSha256 != nil || plan.ApplicationAvailable {
				t.Fatal("invalid candidate became applicable")
			}
			encoded, _ := proto.Marshal(plan)
			if bytes.Contains(encoded, []byte("NHA053_SECRET_REJECTED_CANARY")) {
				t.Fatal("rejected source leaked in diagnostic")
			}
			f.unchanged(t, before, raw, info)
		})
	}
	request := &wire.BuildPlanRequest{Candidate: proto.Clone(candidates[0]).(*wire.Candidate)}
	request.Candidate.Family = wire.SourceFamily(1000)
	if _, e := f.plans.BuildPlan(f.ctx, nha056SessionRequest(request, f.token)); connect.CodeOf(e) != connect.CodeInvalidArgument {
		t.Fatal("future schema family admitted", e)
	}
	request.Candidate.Family = wire.SourceFamily_SOURCE_FAMILY_OPERATIONAL
	request.Candidate.Files[0].ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
	if _, e := f.plans.BuildPlan(f.ctx, nha056SessionRequest(request, f.token)); connect.CodeOf(e) != connect.CodeInvalidArgument {
		t.Fatal("nested future field admitted", e)
	}
	f.unchanged(t, before, raw, info)
}
func TestNHA053ActualRunTLSDomainEffectsAndBaseProvenance(t *testing.T) {
	f := nha053StartTLS(t)
	account := f.tokens.Session.AccountId.Value
	policy := []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n - scope: {family: state_registry, operation: read}\n   objects: {entities: {all: {capabilities: {all: {}}}}}\n", account))
	rights := f.build(t, nha053WireCandidate(wire.SourceFamily_SOURCE_FAMILY_AUTHORIZATION, account, policy))
	if rights.State != wire.PlanState_PLAN_STATE_ADMITTED || rights.Effect != wire.Effect_EFFECT_HUMAN_RIGHTS || rights.Base.Kind != wire.BaseKind_BASE_KIND_ACTIVE_SQL {
		t.Fatal("actual policy preview lost sensitive effect/SQL provenance")
	}
	layout := f.build(t, nha053WireCandidate(wire.SourceFamily_SOURCE_FAMILY_DASHBOARD, "dashboard.preview", []byte("views: []\n")))
	if layout.State != wire.PlanState_PLAN_STATE_ADMITTED || layout.Effect != wire.Effect_EFFECT_DASHBOARD || layout.Base.Kind != wire.BaseKind_BASE_KIND_NO_ACTIVE_DOCUMENT || layout.Base.Revision != nil || layout.Base.SourceSha256 != nil {
		t.Fatal("new dashboard invented active SQL/version or sensitive effect")
	}
	var corpus map[string]any
	if e := json.Unmarshal(scope.HeatingOwnershipV1(), &corpus); e != nil {
		t.Fatal(e)
	}
	pair := corpus["candidates"].([]any)[0].(map[string]any)
	pair["relation"] = map[string]any{"kind": "corroborated_hardware_identity", "hilo": map[string]any{"manufacturer": "SYNTHETIC", "model": "fixture", "serial": "NHA053-only", "serial_scope": "manufacturer_global", "source_ref": "synthetic-hilo"}, "neviweb": map[string]any{"manufacturer": "SYNTHETIC", "model": "fixture", "serial": "NHA053-only", "serial_scope": "manufacturer_global", "source_ref": "synthetic-neviweb"}}
	ownerRaw, e := json.Marshal(corpus)
	if e != nil {
		t.Fatal(e)
	}
	ownership := f.build(t, nha053WireCandidate(wire.SourceFamily_SOURCE_FAMILY_OWNERSHIP, "heating.ownership", ownerRaw))
	if ownership.State != wire.PlanState_PLAN_STATE_ADMITTED || ownership.Effect != wire.Effect_EFFECT_TECHNICAL_OWNERSHIP || ownership.Base.Kind != wire.BaseKind_BASE_KIND_IMMUTABLE_REFERENCE || ownership.Base.Revision != nil {
		t.Fatal("technical ownership lost separate immutable provenance")
	}
	script := f.build(t, nha053WireCandidate(wire.SourceFamily_SOURCE_FAMILY_SCRIPT, "script.preview", []byte("sequence:\n - action: light.turn_on\n   target: {entity_id: light.basement_lights}\n   data: {}\n")))
	if script.State != wire.PlanState_PLAN_STATE_ADMITTED || script.Effect != wire.Effect_EFFECT_RUN_TARGETS || len(script.Changes) != 1 || script.Changes[0].ReferencesAdded != 1 {
		t.Fatal("admitted IR targets became layout/physical execution")
	}
	for _, plan := range []*wire.Plan{rights, layout, ownership, script} {
		if plan.ApplicationAvailable {
			t.Fatal("diagnostic claimed missing apply path")
		}
	}
}
func TestNHA053ActualRunTLSHumanUserInvalidAndRevokedAreDenied(t *testing.T) {
	f := nha053StartTLS(t)
	candidate := nha053WireCandidate(wire.SourceFamily_SOURCE_FAMILY_OPERATIONAL, "operational.main", f.raw)
	plan := f.build(t, candidate)
	const password = "NHA053 synthetic ordinary password"
	if _, e := f.accounts.CreateAccount(f.ctx, nha056SessionRequest(&auth.CreateAccountRequest{LoginName: "NHA053 user", DisplayName: "NHA053 user", Role: auth.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: password}, f.token)); e != nil {
		t.Fatal(e)
	}
	user := nha162RunLogin(t, f.ctx, f.authentication, "NHA053 user", password)
	if _, e := f.plans.BuildPlan(f.ctx, nha056SessionRequest(&wire.BuildPlanRequest{Candidate: candidate}, user.AccessToken)); connect.CodeOf(e) != connect.CodePermissionDenied {
		t.Fatal("ordinary user built admin plan", e)
	}
	if _, e := f.plans.RevalidatePlan(f.ctx, nha056SessionRequest(&wire.RevalidatePlanRequest{Candidate: candidate, Receipt: plan.Receipt}, user.AccessToken)); connect.CodeOf(e) != connect.CodePermissionDenied {
		t.Fatal("receipt granted ordinary user authority", e)
	}
	if _, e := f.plans.BuildPlan(f.ctx, nha056SessionRequest(&wire.BuildPlanRequest{Candidate: candidate}, "synthetic-invalid-bearer")); connect.CodeOf(e) != connect.CodeUnauthenticated {
		t.Fatal("invalid bearer reached compiler", e)
	}
	if _, e := f.authentication.RevokeSession(f.ctx, nha056SessionRequest(&auth.RevokeSessionRequest{SessionId: f.tokens.Session.Id}, f.token)); e != nil {
		t.Fatal(e)
	}
	if _, e := f.plans.RevalidatePlan(f.ctx, nha056SessionRequest(&wire.RevalidatePlanRequest{Candidate: candidate, Receipt: plan.Receipt}, f.token)); connect.CodeOf(e) != connect.CodeUnauthenticated {
		t.Fatal("revoked session reused plan receipt", e)
	}
}
func TestNHA053ActualRunTLSPolicyOutsideManifestMakesPlanStale(t *testing.T) {
	f := nha053StartTLS(t)
	account := f.tokens.Session.AccountId
	policySource := []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants: []\n", account.Value))
	candidate := nha053WireCandidate(wire.SourceFamily_SOURCE_FAMILY_AUTHORIZATION, account.Value, policySource)
	plan := f.build(t, candidate)
	before := f.read(t)
	existing, e := f.policies.GetPolicy(f.ctx, nha056SessionRequest(&auth.GetPolicyRequest{AccountId: account}, f.token))
	if e != nil || existing.Msg.GetPresent() == nil {
		t.Fatal("real policy source missing", e)
	}
	changed := append(bytes.Clone(policySource), []byte("# exact policy source revision changed outside manifest\n")...)
	if _, e := f.policies.ReplacePolicy(f.ctx, nha056SessionRequest(&auth.ReplacePolicyRequest{AccountId: account, YamlSource: changed, Precondition: &auth.ReplacePolicyRequest_Expected{Expected: existing.Msg.GetPresent().Version}}, f.token)); e != nil {
		t.Fatal(e)
	}
	after := f.read(t)
	if !proto.Equal(before.ActiveGeneration, after.ActiveGeneration) {
		t.Fatal("policy-only fixture changed generation; wrong staleness oracle")
	}
	response, e := f.plans.RevalidatePlan(f.ctx, nha056SessionRequest(&wire.RevalidatePlanRequest{Candidate: candidate, Receipt: plan.Receipt}, f.token))
	if e != nil || response.Msg.Plan.State != wire.PlanState_PLAN_STATE_STALE {
		t.Fatal("off-manifest exact policy change kept plan fresh", e)
	}
}

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	configuration "github.com/damione1/Homeward/gen/home/api/v2"
	planwire "github.com/damione1/Homeward/gen/home/api/v2"
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	configurationapplyv1connect "github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	configurationplanv1connect "github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	configurationv1connect "github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/testpg"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/protobuf/proto"
)

type nha055TLSFixture struct {
	nha053TLSFixture
	apply configurationapplyv1connect.ConfigurationApplyServiceClient
}

func nha055StartTLS(t *testing.T) nha055TLSFixture {
	t.Helper()
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	cfg := nha181Config(t, cluster.Port, data, "127.0.0.1:0", nha181TLSYAML())
	source, err := cfg.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "configuration.yaml")
	if err := os.WriteFile(path, source.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	run := nha162StartDefaultRun(t, startup)
	client := nha162RunClient(t, roots, run.address)
	base := "https://" + run.address.String()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	const password = "NHA055 isolated synthetic password"
	nha162RunBootstrap(t, ctx, data, "NHA055 admin", password)
	authentication := apiv2connect.NewAuthServiceClient(client, base)
	tokens := nha162RunLogin(t, ctx, authentication, "NHA055 admin", password)
	return nha055TLSFixture{
		nha053TLSFixture: nha053TLSFixture{
			nha149TLSFixture: nha149TLSFixture{http: client, origin: base, home: nha203PublicHomeName(t, ctx, client, base, tokens.AccessToken), api: configurationv1connect.NewConfigurationServiceClient(client, base), authentication: authentication, accounts: apiv2connect.NewAccountServiceClient(client, base), tokens: tokens, token: tokens.AccessToken, path: path, raw: source.Bytes(), run: run, ctx: ctx},
			plans:            configurationplanv1connect.NewConfigurationPlanServiceClient(client, base), policies: apiv2connect.NewAuthorizationPolicyServiceClient(client, base),
		},
		apply: configurationapplyv1connect.NewConfigurationApplyServiceClient(client, base),
	}
}

func nha055Request(before *configuration.ConfigurationDocument, candidate *planwire.ConfigurationPlanCandidate, plan *planwire.ConfigurationPlan) *wire.ApplyConfigurationPlanRequest {
	return &wire.ApplyConfigurationPlanRequest{Name: plan.Name, Etag: plan.Etag, Candidate: candidate, Expected: before.Version, ExpectedWorkspaceSha256: before.GetWorkspace().SourceSha256}
}

func nha055Conflict(t *testing.T, err error, code connect.Code) {
	t.Helper()
	if connect.CodeOf(err) != code {
		t.Fatalf("expected structured %v, got %v", code, err)
	}
	var rpc *connect.Error
	if !errors.As(err, &rpc) || len(rpc.Details()) < 1 {
		t.Fatal("rejection lacks typed configuration detail", err)
	}
	value, detailErr := rpc.Details()[0].Value()
	if detailErr != nil {
		t.Fatal(detailErr)
	}
	if _, ok := value.(*errdetails.ErrorInfo); !ok {
		t.Fatal("rejection did not use canonical error info")
	}
}

func TestNHA055ActualRunTLSApplyPublishesNotifiesAndRollbackCreatesRevision(t *testing.T) {
	f := nha055StartTLS(t)
	before := f.read(t)
	raw := append(bytes.Clone(f.raw), []byte("# NHA055 exact editable source\n")...)
	candidate := nha053WireCandidate(planwire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL, "operational.main", raw)
	plan := f.build(t, candidate)
	stream, err := f.api.WatchConfiguration(f.ctx, nha056SessionRequest(&configuration.WatchConfigurationRequest{Parent: nha203DocumentParent(before.Name), ResumeToken: before.ResumeToken}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Receive() || stream.Msg().GetReady() == nil {
		t.Fatal("watch did not establish common subscriptions", stream.Err())
	}
	request := nha055Request(before, candidate, plan)
	response, err := f.apply.ApplyConfigurationPlan(f.ctx, nha056SessionRequest(request, f.token))
	if err != nil || !response.Msg.Done {
		t.Fatal("actual TLS apply did not publish", err)
	}
	if contracts.AdmitConfigurationApplyResponse(response.Msg) != nil {
		t.Fatal("apply returned an unadmitted response")
	}
	after := f.read(t)
	if after.Version.Revision != before.Version.Revision+1 || after.ActiveGeneration.Revision != before.ActiveGeneration.Revision+1 {
		t.Fatal("apply did not create exactly one document revision and generation")
	}
	exact, err := os.ReadFile(f.path)
	if err != nil || !bytes.Equal(exact, raw) {
		t.Fatal("apply did not edit the actual selected YAML", err)
	}
	if !stream.Receive() || stream.Msg().GetResyncRequired() == nil {
		t.Fatal("activation did not notify the common consumer", stream.Err())
	}
	info, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.apply.ApplyConfigurationPlan(f.ctx, nha056SessionRequest(request, f.token))
	nha055Conflict(t, err, connect.CodeAborted)
	f.unchanged(t, after, raw, info)
	restored, err := f.apply.RollbackConfiguration(f.ctx, nha056SessionRequest(&wire.RollbackConfigurationRequest{Name: after.Name, Expected: after.Version, ExpectedWorkspaceSha256: after.GetWorkspace().SourceSha256, Historical: before.ActiveGeneration}, f.token))
	if err != nil || !restored.Msg.Done {
		t.Fatal("retained rollback failed", err)
	}
	restoredCurrent := f.read(t)
	if restoredCurrent.Version.Revision != after.Version.Revision+1 || restoredCurrent.ActiveGeneration.Revision != after.ActiveGeneration.Revision+1 || !bytes.Equal(restoredCurrent.Version.SourceSha256, before.Version.SourceSha256) {
		t.Fatal("rollback rewound authority or failed to preserve exact historical source")
	}
	exact, err = os.ReadFile(f.path)
	if err != nil || !bytes.Equal(exact, f.raw) {
		t.Fatal("rollback did not restore the actual editable YAML", err)
	}
}

func TestNHA055ActualRunTLSReceiptCannotGrantRightsOrChangeAuthorOrSource(t *testing.T) {
	f := nha055StartTLS(t)
	before := f.read(t)
	info, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	candidate := nha053WireCandidate(planwire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL, "operational.main", append(bytes.Clone(f.raw), []byte("# receipt-bound private source\n")...))
	plan := f.build(t, candidate)
	request := nha055Request(before, candidate, plan)
	for _, fixture := range []struct {
		name string
		role configuration.AccountRole
	}{{"ordinary", configuration.AccountRole_ACCOUNT_ROLE_USER}, {"different-admin", configuration.AccountRole_ACCOUNT_ROLE_ADMIN}} {
		name := "NHA055 " + fixture.name
		const password = "NHA055 synthetic extra account password"
		if _, err := f.accounts.CreateAccount(f.ctx, nha056SessionRequest(&configuration.CreateAccountRequest{Parent: nha203SessionAccount(t, f.tokens.Session).Home().String(), Account: &configuration.Account{LoginName: name, DisplayName: name, Role: fixture.role, InitialPassword: password}}, f.token)); err != nil {
			t.Fatal(err)
		}
		other := nha162RunLogin(t, f.ctx, f.authentication, name, password)
		_, err := f.apply.ApplyConfigurationPlan(f.ctx, nha056SessionRequest(request, other.AccessToken))
		switch fixture.role {
		case configuration.AccountRole_ACCOUNT_ROLE_USER:
			// The common route gate denies this caller before the configuration
			// handler, so only its public Connect error is available.
			var denied *connect.Error
			if !errors.As(err, &denied) || denied.Code() != connect.CodePermissionDenied || denied.Message() != "permission_denied" || len(denied.Details()) != 0 {
				t.Fatal("ordinary account did not receive the outer route permission denial", err)
			}
		case configuration.AccountRole_ACCOUNT_ROLE_ADMIN:
			// This caller reaches Apply; the foreign-author receipt must still
			// produce the common typed configuration rejection.
			nha055Conflict(t, err, connect.CodeNotFound)
		default:
			t.Fatal("unhandled receipt-author fixture role", fixture.role)
		}
		f.unchanged(t, before, f.raw, info)
	}
	altered := proto.Clone(request).(*wire.ApplyConfigurationPlanRequest)
	altered.Candidate.Files[0].ExactSource = append(altered.Candidate.Files[0].ExactSource, '\n')
	_, err = f.apply.ApplyConfigurationPlan(f.ctx, nha056SessionRequest(altered, f.token))
	nha055Conflict(t, err, connect.CodeInvalidArgument)
	f.unchanged(t, before, f.raw, info)
	corrupted := proto.Clone(request).(*wire.ApplyConfigurationPlanRequest)
	corrupted.Name = plan.Name[:len(plan.Name)-4] + "aaaa"
	_, err = f.apply.ApplyConfigurationPlan(f.ctx, nha056SessionRequest(corrupted, f.token))
	nha055Conflict(t, err, connect.CodeNotFound)
	f.unchanged(t, before, f.raw, info)
	changedBase := proto.Clone(request).(*wire.ApplyConfigurationPlanRequest)
	changedBase.Expected.Revision++
	_, err = f.apply.ApplyConfigurationPlan(f.ctx, nha056SessionRequest(changedBase, f.token))
	nha055Conflict(t, err, connect.CodeAborted)
	f.unchanged(t, before, f.raw, info)
	if _, err := f.authentication.RevokeSession(f.ctx, nha056SessionRequest(&configuration.RevokeSessionRequest{Name: f.tokens.Session.Name}, f.token)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.apply.ApplyConfigurationPlan(f.ctx, nha056SessionRequest(request, f.token)); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("revoked session reused a valid receipt", err)
	}
	exact, err := os.ReadFile(f.path)
	if err != nil || !bytes.Equal(exact, f.raw) {
		t.Fatal("denied request changed editable source", err)
	}
}

func TestNHA055ActualRunTLSNamedPlansAndPurgedRollbackFailClosed(t *testing.T) {
	f := nha055StartTLS(t)
	first := f.read(t)
	current := first
	for n := 0; n < 21; n++ {
		raw := append(bytes.Clone(f.raw), []byte(fmt.Sprintf("# NHA055 retained revision %d\n", n))...)
		candidate := nha053WireCandidate(planwire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL, "operational.main", raw)
		response, err := f.apply.ApplyConfigurationPlan(f.ctx, nha056SessionRequest(nha055Request(current, candidate, f.build(t, candidate)), f.token))
		if err != nil || !response.Msg.Done {
			t.Fatal("named-plan YAML CAS/activation failed", n, err)
		}
		current = f.read(t)
	}
	exact, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.apply.RollbackConfiguration(f.ctx, nha056SessionRequest(&wire.RollbackConfigurationRequest{Name: current.Name, Expected: current.Version, ExpectedWorkspaceSha256: current.GetWorkspace().SourceSha256, Historical: first.ActiveGeneration}, f.token))
	nha055Conflict(t, err, connect.CodeFailedPrecondition)
	f.unchanged(t, current, exact, info)
	hash := sha256.Sum256(exact)
	if !bytes.Equal(hash[:], current.Version.SourceSha256) {
		t.Fatal("purged rollback disturbed admitted source")
	}
}

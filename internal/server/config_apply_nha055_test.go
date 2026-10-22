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
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	configuration "github.com/damione1/Homeward/gen/home/configuration/v1"
	"github.com/damione1/Homeward/gen/home/configuration/v1/configurationv1connect"
	wire "github.com/damione1/Homeward/gen/home/configurationapply/v1"
	"github.com/damione1/Homeward/gen/home/configurationapply/v1/configurationapplyv1connect"
	planwire "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	"github.com/damione1/Homeward/gen/home/configurationplan/v1/configurationplanv1connect"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/testpg"
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
	authentication := authv1connect.NewAuthServiceClient(client, base)
	tokens := nha162RunLogin(t, ctx, authentication, "NHA055 admin", password)
	return nha055TLSFixture{
		nha053TLSFixture: nha053TLSFixture{
			nha149TLSFixture: nha149TLSFixture{api: configurationv1connect.NewConfigurationServiceClient(client, base), authentication: authentication, accounts: authv1connect.NewAccountServiceClient(client, base), tokens: tokens, token: tokens.AccessToken, path: path, raw: source.Bytes(), run: run, ctx: ctx},
			plans:            configurationplanv1connect.NewConfigurationPlanServiceClient(client, base), policies: authv1connect.NewAuthorizationPolicyServiceClient(client, base),
		},
		apply: configurationapplyv1connect.NewConfigurationApplyServiceClient(client, base),
	}
}

func nha055Request(before *configuration.ReadDocumentResponse, candidate *planwire.Candidate, receipt *string) *wire.ApplyRequest {
	return &wire.ApplyRequest{Candidate: candidate, Receipt: receipt, Expected: before.Document.Version, ExpectedWorkspaceSha256: before.Document.GetWorkspace().SourceSha256}
}

func nha055Conflict(t *testing.T, err error, code connect.Code) {
	t.Helper()
	if connect.CodeOf(err) != code {
		t.Fatalf("expected structured %v, got %v", code, err)
	}
	var rpc *connect.Error
	if !errors.As(err, &rpc) || len(rpc.Details()) != 1 {
		t.Fatal("rejection lacks typed configuration detail", err)
	}
	value, detailErr := rpc.Details()[0].Value()
	if detailErr != nil {
		t.Fatal(detailErr)
	}
	if _, ok := value.(*configuration.ConfigurationError); !ok {
		t.Fatal("rejection did not use common configuration error")
	}
}

func TestNHA055ActualRunTLSApplyPublishesNotifiesAndRollbackCreatesRevision(t *testing.T) {
	f := nha055StartTLS(t)
	before := f.read(t)
	raw := append(bytes.Clone(f.raw), []byte("# NHA055 exact editable source\n")...)
	candidate := nha053WireCandidate(planwire.SourceFamily_SOURCE_FAMILY_OPERATIONAL, "operational.main", raw)
	plan := f.build(t, candidate)
	stream, err := f.api.WatchConfiguration(f.ctx, nha056SessionRequest(&configuration.WatchConfigurationRequest{ResumeToken: before.ResumeToken}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Receive() || stream.Msg().GetReady() == nil {
		t.Fatal("watch did not establish common subscriptions", stream.Err())
	}
	request := nha055Request(before, candidate, &plan.Receipt)
	response, err := f.apply.Apply(f.ctx, nha056SessionRequest(request, f.token))
	if err != nil || response.Msg.Activation.Outcome != configuration.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED {
		t.Fatal("actual TLS apply did not publish", err)
	}
	if contracts.AdmitConfigurationApplyResponse(response.Msg) != nil {
		t.Fatal("apply returned an unadmitted response")
	}
	after := response.Msg.Current
	if after.Document.Version.Revision != before.Document.Version.Revision+1 || after.ActiveGeneration.Revision != before.ActiveGeneration.Revision+1 {
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
	_, err = f.apply.Apply(f.ctx, nha056SessionRequest(request, f.token))
	nha055Conflict(t, err, connect.CodeAborted)
	f.unchanged(t, after, raw, info)
	restored, err := f.apply.Rollback(f.ctx, nha056SessionRequest(&wire.RollbackRequest{Id: after.Document.Id, Expected: after.Document.Version, ExpectedWorkspaceSha256: after.Document.GetWorkspace().SourceSha256, Historical: before.ActiveGeneration}, f.token))
	if err != nil || restored.Msg.Activation.Outcome != configuration.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED {
		t.Fatal("retained rollback failed", err)
	}
	if restored.Msg.Current.Document.Version.Revision != after.Document.Version.Revision+1 || restored.Msg.Current.ActiveGeneration.Revision != after.ActiveGeneration.Revision+1 || !bytes.Equal(restored.Msg.Current.Document.Version.SourceSha256, before.Document.Version.SourceSha256) {
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
	candidate := nha053WireCandidate(planwire.SourceFamily_SOURCE_FAMILY_OPERATIONAL, "operational.main", append(bytes.Clone(f.raw), []byte("# receipt-bound private source\n")...))
	plan := f.build(t, candidate)
	request := nha055Request(before, candidate, &plan.Receipt)
	for _, fixture := range []struct {
		name string
		role auth.AccountRole
	}{{"ordinary", auth.AccountRole_ACCOUNT_ROLE_USER}, {"different-admin", auth.AccountRole_ACCOUNT_ROLE_ADMIN}} {
		name := "NHA055 " + fixture.name
		const password = "NHA055 synthetic extra account password"
		if _, err := f.accounts.CreateAccount(f.ctx, nha056SessionRequest(&auth.CreateAccountRequest{LoginName: name, DisplayName: name, Role: fixture.role, InitialPassword: password}, f.token)); err != nil {
			t.Fatal(err)
		}
		other := nha162RunLogin(t, f.ctx, f.authentication, name, password)
		_, err := f.apply.Apply(f.ctx, nha056SessionRequest(request, other.AccessToken))
		switch fixture.role {
		case auth.AccountRole_ACCOUNT_ROLE_USER:
			// The common route gate denies this caller before the configuration
			// handler, so only its public Connect error is available.
			var denied *connect.Error
			if !errors.As(err, &denied) || denied.Code() != connect.CodePermissionDenied || denied.Message() != "permission_denied" || len(denied.Details()) != 0 {
				t.Fatal("ordinary account did not receive the outer route permission denial", err)
			}
		case auth.AccountRole_ACCOUNT_ROLE_ADMIN:
			// This caller reaches Apply; the foreign-author receipt must still
			// produce the common typed configuration rejection.
			nha055Conflict(t, err, connect.CodePermissionDenied)
		default:
			t.Fatal("unhandled receipt-author fixture role", fixture.role)
		}
		f.unchanged(t, before, f.raw, info)
	}
	altered := proto.Clone(request).(*wire.ApplyRequest)
	altered.Candidate.Files[0].ExactSource = append(altered.Candidate.Files[0].ExactSource, '\n')
	_, err = f.apply.Apply(f.ctx, nha056SessionRequest(altered, f.token))
	nha055Conflict(t, err, connect.CodeInvalidArgument)
	f.unchanged(t, before, f.raw, info)
	corrupted := proto.Clone(request).(*wire.ApplyRequest)
	bad := plan.Receipt[:len(plan.Receipt)-4] + "AAAA"
	corrupted.Receipt = &bad
	_, err = f.apply.Apply(f.ctx, nha056SessionRequest(corrupted, f.token))
	nha055Conflict(t, err, connect.CodeInvalidArgument)
	f.unchanged(t, before, f.raw, info)
	changedBase := proto.Clone(request).(*wire.ApplyRequest)
	changedBase.Expected.Revision++
	_, err = f.apply.Apply(f.ctx, nha056SessionRequest(changedBase, f.token))
	nha055Conflict(t, err, connect.CodeAborted)
	f.unchanged(t, before, f.raw, info)
	if _, err := f.authentication.RevokeSession(f.ctx, nha056SessionRequest(&auth.RevokeSessionRequest{SessionId: f.tokens.Session.Id}, f.token)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.apply.Apply(f.ctx, nha056SessionRequest(request, f.token)); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("revoked session reused a valid receipt", err)
	}
	exact, err := os.ReadFile(f.path)
	if err != nil || !bytes.Equal(exact, f.raw) {
		t.Fatal("denied request changed editable source", err)
	}
}

func TestNHA055ActualRunTLSOptionalPlanAndPurgedRollbackFailClosed(t *testing.T) {
	f := nha055StartTLS(t)
	first := f.read(t)
	current := first
	for n := 0; n < 21; n++ {
		raw := append(bytes.Clone(f.raw), []byte(fmt.Sprintf("# NHA055 retained revision %d\n", n))...)
		candidate := nha053WireCandidate(planwire.SourceFamily_SOURCE_FAMILY_OPERATIONAL, "operational.main", raw)
		response, err := f.apply.Apply(f.ctx, nha056SessionRequest(nha055Request(current, candidate, nil), f.token))
		if err != nil || response.Msg.Activation.Outcome != configuration.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED {
			t.Fatal("plan-free YAML CAS/activation failed", n, err)
		}
		current = response.Msg.Current
	}
	exact, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.apply.Rollback(f.ctx, nha056SessionRequest(&wire.RollbackRequest{Id: current.Document.Id, Expected: current.Document.Version, ExpectedWorkspaceSha256: current.Document.GetWorkspace().SourceSha256, Historical: first.ActiveGeneration}, f.token))
	nha055Conflict(t, err, connect.CodeFailedPrecondition)
	f.unchanged(t, current, exact, info)
	hash := sha256.Sum256(exact)
	if !bytes.Equal(hash[:], current.Document.Version.SourceSha256) {
		t.Fatal("purged rollback disturbed admitted source")
	}
}

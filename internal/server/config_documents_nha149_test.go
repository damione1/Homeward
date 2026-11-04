package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"github.com/damione1/Homeward/internal/apierrors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	configurationv1connect "github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

type nha149TLSFixture struct {
	http           connect.HTTPClient
	origin, home   string
	api            configurationv1connect.ConfigurationServiceClient
	authentication apiv2connect.AuthServiceClient
	accounts       apiv2connect.AccountServiceClient
	tokens         *wire.SessionTokens
	token, path    string
	raw            []byte
	run            *nha162Running
	ctx            context.Context
}

func nha149StartTLS(t *testing.T, withLegacy ...bool) nha149TLSFixture {
	t.Helper()
	if len(withLegacy) > 1 {
		t.Fatal("multiple legacy fixture choices")
	}
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	cfg := nha181Config(t, cluster.Port, data, "127.0.0.1:0", nha181TLSYAML())
	if len(withLegacy) == 1 && withLegacy[0] {
		nha056ActivateLocalGeneration(t, cfg)
	}
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
	const password = "NHA149 isolated synthetic password"
	nha162RunBootstrap(t, ctx, data, "NHA149 admin", password)
	authentication := apiv2connect.NewAuthServiceClient(client, base)
	tokens := nha162RunLogin(t, ctx, authentication, "NHA149 admin", password)
	return nha149TLSFixture{http: client, origin: base, home: nha203PublicHomeName(t, ctx, client, base, tokens.AccessToken), api: configurationv1connect.NewConfigurationServiceClient(client, base), authentication: authentication, accounts: apiv2connect.NewAccountServiceClient(client, base), tokens: tokens, token: tokens.AccessToken, path: path, raw: source.Bytes(), run: run, ctx: ctx}
}
func (f nha149TLSFixture) read(t *testing.T) *wire.ConfigurationDocument {
	t.Helper()
	response, err := f.api.GetConfigurationDocument(f.ctx, nha056SessionRequest(&wire.GetConfigurationDocumentRequest{Name: nha203PublicDocumentName(t, f.ctx, f.http, f.origin, f.token, "operational.main")}, f.token))
	if err != nil {
		t.Fatal("actual Run/TLS document read", err)
	}
	return response.Msg
}
func (f nha149TLSFixture) write(base *wire.ConfigurationDocument, raw []byte) (*connect.Response[longrunningpb.Operation], error) {
	return f.api.PublishConfigurationDocument(f.ctx, nha056SessionRequest(&wire.PublishConfigurationDocumentRequest{Name: base.Name, Expected: base.Version, ExpectedWorkspaceSha256: base.GetWorkspace().SourceSha256, ExactYamlSource: raw}, f.token))
}
func TestNHA149ActualRunTLSConcurrentCASAndRestoreCreatesNewRevision(t *testing.T) {
	f := nha149StartTLS(t)
	before := f.read(t)
	if before.Kind != wire.DocumentKind_DOCUMENT_KIND_OPERATIONAL_YAML || before.SchemaVersion != "homeward.operational/1" || before.Version.Revision < 1 || before.UpdatedAt == nil || len(before.ProtectedSourceReference) != 32 || before.GetWorkspace().Dirty != wire.DirtyState_DIRTY_STATE_CLEAN {
		t.Fatal("incomplete coherent public document identity")
	}
	if !bytes.Equal(before.Version.SourceSha256, before.GetWorkspace().SourceSha256) {
		t.Fatal("initial admitted/source observation differs")
	}
	start := make(chan struct{})
	type answer struct {
		response *connect.Response[longrunningpb.Operation]
		err      error
	}
	answers := make(chan answer, 2)
	var workers sync.WaitGroup
	for _, comment := range []string{"# CAS candidate A\n", "# CAS candidate B\n"} {
		workers.Add(1)
		go func(comment string) {
			defer workers.Done()
			<-start
			response, err := f.write(before, append(bytes.Clone(f.raw), []byte(comment)...))
			answers <- answer{response, err}
		}(comment)
	}
	close(start)
	workers.Wait()
	close(answers)
	winners, conflicts := 0, 0
	for answer := range answers {
		if answer.err == nil && answer.response.Msg.Done {
			winners++
			continue
		}
		if connect.CodeOf(answer.err) != connect.CodeAborted {
			t.Fatal("concurrent CAS was neither one publication nor structured conflict", answer.err)
		}
		var rpc *connect.Error
		if !errors.As(answer.err, &rpc) || len(rpc.Details()) < 1 {
			t.Fatal("CAS conflict has no typed detail")
		}
		detail, err := rpc.Details()[0].Value()
		if err != nil {
			t.Fatal(err)
		}
		conflict, ok := detail.(*errdetails.ErrorInfo)
		if !ok || conflict.Reason != "ETAG_MISMATCH" || conflict.Domain != "homeward.damiengoehrig.ca" {
			t.Fatal("conflict lost canonical CAS reason")
		}
		conflicts++
	}
	if winners != 1 || conflicts != 1 {
		t.Fatal("concurrent documentary CAS did not have exactly one winner")
	}
	after := f.read(t)
	if after.Version.Revision <= before.Version.Revision || after.ActiveGeneration.Revision <= before.ActiveGeneration.Revision {
		t.Fatal("revision did not advance")
	}
	exact, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(exact)
	if !bytes.Equal(digest[:], after.Version.SourceSha256) {
		t.Fatal("API did not update the actual editable YAML")
	}
	restored, err := f.api.RollbackConfigurationDocument(f.ctx, nha056SessionRequest(&wire.RollbackConfigurationDocumentRequest{Name: after.Name, Expected: after.Version, ExpectedWorkspaceSha256: after.GetWorkspace().SourceSha256, Historical: before.ActiveGeneration}, f.token))
	if err != nil || !restored.Msg.Done {
		t.Fatal("actual 029 historical restoration failed", err)
	}
	current := f.read(t)
	if current.Version.Revision <= after.Version.Revision || current.ActiveGeneration.Revision <= after.ActiveGeneration.Revision || !bytes.Equal(current.Version.SourceSha256, before.Version.SourceSha256) {
		t.Fatal("restore rewound the counter or failed to restore exact historical content")
	}
	exact, err = os.ReadFile(f.path)
	if err != nil || !bytes.Equal(exact, f.raw) {
		t.Fatal("restoration failed to update the same editable YAML", err)
	}
	versions, err := f.api.ListConfigurationRevisions(f.ctx, nha056SessionRequest(&wire.ListConfigurationRevisionsRequest{Parent: before.Name, PageSize: 1}, f.token))
	if err != nil || len(versions.Msg.ConfigurationRevisions) != 1 || versions.Msg.NextPageToken == "" {
		t.Fatal("real version keyset page failed", err)
	}
	schemas, err := f.api.ListConfigurationSchemas(f.ctx, nha056SessionRequest(&wire.ListConfigurationSchemasRequest{Parent: f.home, PageSize: 1}, f.token))
	if err != nil || len(schemas.Msg.ConfigurationSchemas) != 1 || schemas.Msg.NextPageToken == "" {
		t.Fatal("real SQL schema page failed", err)
	}
}
func TestNHA149ActualRunTLSDirtyCorrectionAndReadSubscribeGapAreExplicit(t *testing.T) {
	f := nha149StartTLS(t)
	before := f.read(t)
	stream, err := f.api.WatchConfiguration(f.ctx, nha056SessionRequest(&wire.WatchConfigurationRequest{Parent: nha203DocumentParent(before.Name), ResumeToken: before.ResumeToken}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Receive() || stream.Msg().GetReady() == nil {
		t.Fatal("stream did not confirm both registrations", stream.Err())
	}
	invalid := []byte("# rejected synthetic private argument\nexternal_workers: [\n")
	rejected, err := f.write(before, invalid)
	if connect.CodeOf(err) != connect.CodeInvalidArgument || rejected != nil {
		t.Fatal("invalid candidate lost its activation outcome", err)
	}
	if !stream.Receive() || stream.Msg().GetChanges() == nil || len(stream.Msg().GetChanges().Changes) != 1 || stream.Msg().GetChanges().Changes[0].Dirty != wire.DirtyState_DIRTY_STATE_REJECTED {
		t.Fatal("dirty transition was not reported", stream.Err())
	}
	dirty := f.read(t)
	if dirty.Version.Revision != before.Version.Revision || dirty.ActiveGeneration.Revision != before.ActiveGeneration.Revision || dirty.GetWorkspace().Dirty != wire.DirtyState_DIRTY_STATE_REJECTED {
		t.Fatal("invalid workspace became SQL authority")
	}
	corrected, err := f.write(dirty, append(bytes.Clone(f.raw), []byte("# corrected\n")...))
	if err != nil || !corrected.Msg.Done {
		t.Fatal("dirty correction failed", err)
	}
	if !stream.Receive() || stream.Msg().GetResyncRequired() == nil {
		t.Fatal("coalesced activation pretended to be full documentary history", stream.Err())
	}
	gap, err := f.api.WatchConfiguration(f.ctx, nha056SessionRequest(&wire.WatchConfigurationRequest{Parent: nha203DocumentParent(before.Name), ResumeToken: before.ResumeToken}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	defer gap.Close()
	if !gap.Receive() || gap.Msg().GetResyncRequired() == nil || gap.Msg().GetResyncRequired().Reason != wire.ResyncReason_RESYNC_REASON_AUTHORITY_CHANGED {
		t.Fatal("change between read and subscribe disappeared", gap.Err())
	}
}
func TestNHA149OwnerRejectsPostStagingExternalSourceBeforeReservationOrSQL(t *testing.T) {
	r := nha029StartOwner(t, nha029OwnerParent, nha029OwnerChild, nil)
	stamp, err := r.owner.CurrentConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	revision, manifest, err := stamp.Components()
	if err != nil {
		t.Fatal(err)
	}
	generation, _ := storage.NewConfigGenerationRevision(int64(revision))
	staged := append(bytes.Clone(r.raw), []byte("# staged by CAS149\n")...)
	precondition, err := configactivation.NewWorkspacePrecondition(generation, manifest, sha256.Sum256(staged))
	if err != nil {
		t.Fatal(err)
	}
	request, err := r.request(t, nil, nil, nil).WithWorkspacePrecondition(precondition)
	if err != nil {
		t.Fatal(err)
	}
	external := append(bytes.Clone(r.raw), []byte("# external editor before Owner prepare\n")...)
	if err := os.WriteFile(r.path, external, 0600); err != nil {
		t.Fatal(err)
	}
	starts := r.pilot.startCount()
	driverCalls := len(r.driver.snapshot())
	result, err := r.owner.Activate(context.Background(), request)
	if !errors.Is(err, configactivation.ErrWorkspacePreconditionConflict) || result.Outcome() != configactivation.RejectedBeforeCommit || result.AttemptOutcome() != storage.TransactionNotCommitted || result.Attempt().Valid() {
		t.Fatal("post-staging edit crossed the reservation/SQL boundary", err)
	}
	after, err := r.owner.CurrentConfig(context.Background())
	if err != nil || after != stamp || r.pilot.startCount() != starts || len(r.driver.snapshot()) != driverCalls {
		t.Fatal("precondition conflict touched active generation/pilot", err)
	}
	current, err := os.ReadFile(r.path)
	if err != nil || !bytes.Equal(current, external) {
		t.Fatal("Owner overwrote external source", err)
	}
	r.close(t)
}
func TestNHA149ActualTLSHumanAdminBoundaryAndStreamRevocation(t *testing.T) {
	f := nha149StartTLS(t)
	before := f.read(t)
	const password = "NHA149 synthetic ordinary user password"
	_, err := f.accounts.CreateAccount(f.ctx, nha056SessionRequest(&wire.CreateAccountRequest{Parent: nha203SessionAccount(t, f.tokens.Session).Home().String(), Account: &wire.Account{LoginName: "NHA149 ordinary user", DisplayName: "NHA149 ordinary user", Role: wire.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: password}}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	user := nha162RunLogin(t, f.ctx, f.authentication, "NHA149 ordinary user", password)
	_, err = f.api.GetConfigurationDocument(f.ctx, nha056SessionRequest(&wire.GetConfigurationDocumentRequest{Name: before.Name}, user.AccessToken))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("ordinary user read global configuration", err)
	}
	_, err = f.api.PublishConfigurationDocument(f.ctx, nha056SessionRequest(&wire.PublishConfigurationDocumentRequest{Name: before.Name, Expected: before.Version, ExpectedWorkspaceSha256: before.GetWorkspace().SourceSha256, ExactYamlSource: []byte("external_workers: []\n")}, user.AccessToken))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("ordinary user mutated global configuration", err)
	}
	exact, err := os.ReadFile(f.path)
	if err != nil || !bytes.Equal(exact, f.raw) {
		t.Fatal("denied mutation touched editable YAML", err)
	}
	before = f.read(t)
	stream, err := f.api.WatchConfiguration(f.ctx, nha056SessionRequest(&wire.WatchConfigurationRequest{Parent: nha203DocumentParent(before.Name), ResumeToken: before.ResumeToken}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Receive() || stream.Msg().GetReady() == nil {
		t.Fatal("authorized stream not established", stream.Err())
	}
	if _, err := f.authentication.RevokeSession(f.ctx, nha056SessionRequest(&wire.RevokeSessionRequest{Name: f.tokens.Session.Name}, f.token)); err != nil {
		t.Fatal(err)
	}
	if stream.Receive() || connect.CodeOf(stream.Err()) != connect.CodeUnauthenticated {
		t.Fatal("revoked stream emitted data or silently ended as complete", stream.Err())
	}
	var terminated *connect.Error
	if !errors.As(stream.Err(), &terminated) || len(terminated.Details()) != 1 {
		t.Fatal("revocation lost its typed lease termination")
	}
	detail, err := terminated.Details()[0].Value()
	if err != nil {
		t.Fatal(err)
	}
	if info, ok := detail.(*errdetails.ErrorInfo); !ok || info.Domain != apierrors.Domain() || info.Reason != "AUTHENTICATION_REQUIRED" {
		t.Fatal("stream did not preserve the common141 termination contract")
	}
}
func TestNHA149RealLostCommitACKPreservesAttemptAndPublicationWireOutcomes(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(strconv.FormatBool(rollback), func(t *testing.T) {
			environment := nha042ServerEnvironmentFor(t)
			backendPort := int(environment.observer.Config().Port)
			proxy := nha029OwnerStartCommitACKProxy(t, backendPort)
			proxy.abort = rollback
			captured, err := environment.config.startup.CaptureWorkspace()
			if err != nil {
				t.Fatal(err)
			}
			raw := strings.Replace(string(captured.Bytes()), "  port: "+strconv.Itoa(backendPort)+"\n", "  port: "+strconv.Itoa(proxy.port(t))+"\n", 1)
			if raw == string(captured.Bytes()) {
				t.Fatal("synthetic PostgreSQL transport was not replaced")
			}
			environment.config = nha029RunFixtureFromRaw(t, []byte(raw))
			r := nha029StartOwnerEnvironment(t, environment, nha029OwnerParent, nha029OwnerChild, nil)
			r.edit(t, "# uncertain149 attempt B\n")
			proxy.armed.Store(true)
			pending := nha029OwnerActivate(r, r.request(t, nil, nil, nil))
			nha034WaitSignal(t, proxy.held, "real149 PostgreSQL COMMIT reply withheld")
			r.edit(t, "# current149 winner C\n")
			winner := r.commitCurrentWorkspace(t)
			proxy.allowDrop()
			uncertain, err := nha029OwnerAwait(t, pending)
			if uncertain.Outcome() != configactivation.CommitUnknown || err == nil {
				t.Fatal("real lost ACK did not remain unknown", err)
			}
			if uncertain.AttemptOutcome() != storage.TransactionIndeterminate {
				t.Fatalf("lost ACK domain result omitted indeterminate transaction evidence: outcome=%d transaction=%d", uncertain.Outcome(), uncertain.AttemptOutcome())
			}
			if administrationFault(configactivation.ErrCommitIndeterminate).Reason() != wire.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN {
				t.Fatal("uncertain native commit did not map to its canonical fault")
			}
			published, err := r.owner.Reconcile(context.Background())
			if err != nil || published.Outcome() != configactivation.Published || published.Revision() != winner {
				t.Fatal("actual owner reconciliation failed", err)
			}
			own, present, err := r.store.ReadActivationAttempt(context.Background(), published.Attempt())
			if err != nil || present == rollback {
				t.Fatal("own transaction identity differs from native commit", err)
			}
			if _, proven := published.PublishedEvidence(own); proven {
				t.Fatal("newer winner manufactured publication evidence for this caller's attempt")
			}
			if len(r.driver.snapshot()) != 0 {
				t.Fatal("149 uncertainty mapping replayed a fake driver action")
			}
			r.close(t)
		})
	}
}
func TestNHA149ActualRunTLSReadsEveryDeliveredDocumentKind(t *testing.T) {
	f := nha149StartTLS(t, true)
	operational := f.read(t)
	page, err := f.api.ListConfigurationDocuments(f.ctx, nha056SessionRequest(&wire.ListConfigurationDocumentsRequest{Parent: f.home, PageSize: 100}, f.token))
	if err != nil {
		t.Fatal("generic catalogue read through actual TLS/Run", err)
	}
	kinds := make(map[wire.DocumentKind]bool)
	var yamlRead *wire.ConfigurationDocument
	for _, listed := range page.Msg.ConfigurationDocuments {
		response, err := f.api.GetConfigurationDocument(f.ctx, nha056SessionRequest(&wire.GetConfigurationDocumentRequest{Name: listed.Name}, f.token))
		if err != nil || contracts.AdmitConfigurationResponse(response.Msg) != nil {
			t.Fatal("delivered document kind failed generic read", listed.Kind, err)
		}
		document := response.Msg
		if document.Id.Value != listed.Id.Value || document.Kind != listed.Kind || document.Version.Revision != listed.Version.Revision || !bytes.Equal(document.Version.SourceSha256, listed.Version.SourceSha256) || !document.UpdatedAt.AsTime().Equal(listed.UpdatedAt.AsTime()) || document.Active != listed.Active {
			t.Fatal("generic read blended metadata, revision/hash or active membership")
		}
		if response.Msg.ActiveGeneration.Revision != operational.ActiveGeneration.Revision || !bytes.Equal(response.Msg.ActiveGeneration.ManifestSha256, operational.ActiveGeneration.ManifestSha256) || response.Msg.ResumeToken != operational.ResumeToken {
			t.Fatal("generic document read did not share the authoritative global cut")
		}
		if document.Kind == wire.DocumentKind_DOCUMENT_KIND_OPERATIONAL_YAML {
			if document.GetWorkspace() == nil || document.GetWorkspace().Dirty != wire.DirtyState_DIRTY_STATE_CLEAN {
				t.Fatal("selected workspace observation missing")
			}
		} else if document.GetWorkspaceUnobserved() == nil || document.GetWorkspaceUnobserved().Reason != wire.WorkspaceUnobservedReason_WORKSPACE_UNOBSERVED_REASON_NO_PHYSICAL_LOCATOR {
			t.Fatal("non-workspace document silently claimed clean or invented a physical locator")
		}
		kinds[document.Kind] = true
		if document.Kind == wire.DocumentKind_DOCUMENT_KIND_YAML_SOURCE {
			yamlRead = response.Msg
		}
	}
	for _, kind := range []wire.DocumentKind{wire.DocumentKind_DOCUMENT_KIND_OPERATIONAL_YAML, wire.DocumentKind_DOCUMENT_KIND_YAML_SOURCE, wire.DocumentKind_DOCUMENT_KIND_BOOTSTRAP_LAUNCH, wire.DocumentKind_DOCUMENT_KIND_AUTHORIZATION_POLICY, wire.DocumentKind_DOCUMENT_KIND_MFA_POLICY} {
		if !kinds[kind] {
			t.Fatal("missing positive delivered document kind", kind)
		}
	}
	if yamlRead == nil || len(yamlRead.ProtectedSourceReference) != 32 {
		t.Fatal("retained YAML-source metadata was not projected opaquely")
	}
	active, present, err := f.run.observed.sessions.store.ReadActiveConfig(f.ctx)
	if err != nil || !present {
		t.Fatal(err)
	}
	matched := false
	for _, version := range active.Versions() {
		if version.Ref().Document().String() == yamlRead.Id.Value {
			digest := version.SHA256()
			matched = version.Ref().Revision().Number() == yamlRead.Version.Revision && bytes.Equal(digest[:], yamlRead.Version.SourceSha256)
		}
	}
	if !matched {
		t.Fatal("second genre did not come from the actual retained generation")
	}
	stream, err := f.api.WatchConfiguration(f.ctx, nha056SessionRequest(&wire.WatchConfigurationRequest{Parent: nha203DocumentParent(yamlRead.Name), ResumeToken: yamlRead.ResumeToken}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Receive() || stream.Msg().GetReady() == nil {
		t.Fatal("second-kind read token could not subscribe without a fixed-ID bypass", stream.Err())
	}
}

func TestNHA149ActualRunTLSTwoRejectionsThenCorrection(t *testing.T) {
	f := nha149StartTLS(t)
	admitted := f.read(t)
	base := admitted
	for _, candidate := range [][]byte{[]byte("# rejected B\nexternal_workers: [\n"), []byte("# rejected C\nexternal_workers: {\n")} {
		response, err := f.write(base, candidate)
		if connect.CodeOf(err) != connect.CodeInvalidArgument || response != nil {
			t.Fatal("known rejected dirty write lost its outcome", err)
		}
		base = f.read(t)
		digest := sha256.Sum256(candidate)
		if base.Version.Revision != admitted.Version.Revision || !bytes.Equal(base.Version.SourceSha256, admitted.Version.SourceSha256) || base.ActiveGeneration.Revision != admitted.ActiveGeneration.Revision || base.GetWorkspace().Dirty != wire.DirtyState_DIRTY_STATE_REJECTED || !bytes.Equal(base.GetWorkspace().SourceSha256, digest[:]) {
			t.Fatal("second known rejection became SQL authority or reconciliation")
		}
		active, present, err := f.run.observed.sessions.store.ReadActiveConfig(f.ctx)
		if err != nil || !present || active.Revision().Number() != admitted.ActiveGeneration.Revision {
			t.Fatal("rejected candidate changed real SQL authority", err)
		}
		exact, err := os.ReadFile(f.path)
		if err != nil || !bytes.Equal(exact, candidate) {
			t.Fatal("rejection rolled back physical YAML", err)
		}
	}
	if _, err := f.write(admitted, append(bytes.Clone(f.raw), []byte("# stale workspace\n")...)); connect.CodeOf(err) != connect.CodeAborted {
		t.Fatal("dirty correction weakened workspace CAS", err)
	}
	corrected := append(bytes.Clone(f.raw), []byte("# corrected D after two rejections\n")...)
	response, err := f.write(base, corrected)
	if err != nil || !response.Msg.Done {
		t.Fatal("two rejections blocked actual 029 correction", err)
	}
	current := f.read(t)
	digest := sha256.Sum256(corrected)
	if current.Version.Revision <= admitted.Version.Revision || current.ActiveGeneration.Revision <= admitted.ActiveGeneration.Revision || current.GetWorkspace().Dirty != wire.DirtyState_DIRTY_STATE_CLEAN || !bytes.Equal(current.Version.SourceSha256, digest[:]) {
		t.Fatal("corrected source did not publish as a new exact revision")
	}
}

// The existing 162 writer gate holds only the first ready frame. The SQL C5
// proxy then holds the real Watch.Next metadata read, after its first signal
// check and old journal dequeue. No callback substitutes activation or Read.
type nha149ReadyWriter struct {
	http.ResponseWriter
	ctx     context.Context
	gate    *nha162WatchWriteGate
	flushed bool
}

func (w *nha149ReadyWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *nha149ReadyWriter) Flush()                      { _ = w.FlushError() }
func (w *nha149ReadyWriter) FlushError() error {
	if err := http.NewResponseController(w.ResponseWriter).Flush(); err != nil {
		return err
	}
	if !w.flushed {
		w.flushed = true
		close(w.gate.blocked)
		select {
		case <-w.gate.release:
		case <-w.ctx.Done():
			return w.ctx.Err()
		}
	}
	return nil
}

func TestNHA149ActualOwnerPublicationDuringWatchReadRequiresResync(t *testing.T) {
	environment := nha042ServerEnvironmentFor(t)
	backendPort := int(environment.observer.Config().Port)
	proxy := nha029C5StartSQLProxy(t, backendPort)
	captured, err := environment.config.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.Replace(string(captured.Bytes()), "  port: "+strconv.Itoa(backendPort)+"\n", "  port: "+strconv.Itoa(proxy.port())+"\n", 1)
	if raw == string(captured.Bytes()) {
		t.Fatal("synthetic SQL transport was not replaced")
	}
	environment.config = nha029RunFixtureFromRaw(t, []byte(raw))
	r := nha029StartOwnerEnvironment(t, environment, nha029OwnerParent, nha029OwnerChild, nil)
	defer r.close(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	actor, err := environment.principal.Actor()
	if err != nil {
		t.Fatal(err)
	}
	gate := &nha162WatchWriteGate{blocked: make(chan struct{}), release: make(chan struct{})}
	defer gate.open()
	var firstWatch sync.Once
	// These are the generated handlers retained by the actual Run's compiled
	// routes and their real Service/Owner/Store. This focused TLS harness supplies
	// the fixture's admitted native context; the separate Run/TLS tests exercise
	// common authentication, lease and revocation. It creates no second Service.
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		route, present := r.observation.public.routes[routeLookup{method: request.Method, path: request.URL.Path}]
		if !present || route.handler == nil {
			http.NotFound(writer, request)
			return
		}
		native, err := authorization.WithPrincipal(request.Context(), environment.principal)
		if err != nil {
			http.Error(writer, "invalid synthetic principal", http.StatusInternalServerError)
			return
		}
		native = context.WithValue(native, actorContextKey{}, actor)
		if request.URL.Path == configurationv1connect.ConfigurationServiceWatchConfigurationProcedure {
			firstWatch.Do(func() { writer = &nha149ReadyWriter{ResponseWriter: writer, ctx: native, gate: gate} })
		}
		route.handler.ServeHTTP(writer, request.WithContext(native))
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer func() { gate.open(); server.Close() }()
	client := server.Client()
	client.Timeout = 40 * time.Second
	defer client.CloseIdleConnections()
	api := configurationv1connect.NewConfigurationServiceClient(client, server.URL)
	read := func() *wire.ConfigurationDocument {
		t.Helper()
		response, err := api.GetConfigurationDocument(ctx, connect.NewRequest(&wire.GetConfigurationDocumentRequest{Name: nha203PublicDocumentName(t, ctx, client, server.URL, "", "operational.main")}))
		if err != nil {
			t.Fatal("actual Run-owned metadata handler", err)
		}
		return response.Msg
	}
	before := read()
	stream, err := api.WatchConfiguration(ctx, connect.NewRequest(&wire.WatchConfigurationRequest{Parent: nha203DocumentParent(before.Name), ResumeToken: before.ResumeToken}))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Receive() || stream.Msg().GetReady() == nil {
		t.Fatal("real Watch did not register before ready", stream.Err())
	}
	lastReceived := stream.Msg().GetReady().ResumeToken
	nha034WaitSignal(t, gate.blocked, "Watch ready writer barrier")
	rejected, err := api.PublishConfigurationDocument(ctx, connect.NewRequest(&wire.PublishConfigurationDocumentRequest{Name: before.Name, Expected: before.Version, ExpectedWorkspaceSha256: before.GetWorkspace().SourceSha256, ExactYamlSource: []byte("# old dirty event\nexternal_workers: [\n")}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || rejected != nil {
		t.Fatal("failed to queue old dirty event", err)
	}
	ticket := proxy.arm(t, nha029C5CutAfterStatement, nha029C5SQLDocumentRead)
	defer ticket.finish(true)
	gate.open()
	var barrier nha029C5SQLBarrier
	select {
	case barrier = <-ticket.held:
	case <-ctx.Done():
		t.Fatal("Watch did not reach the real SQL metadata barrier", ctx.Err())
	}
	if barrier.statement != nha029C5SQLDocumentRead || barrier.committed {
		t.Fatal("Watch did not pause at its real metadata snapshot")
	}
	r.edit(t, "# direct029 publication while old event waits\n")
	published, err := r.owner.Activate(ctx, r.request(t, nil, nil, nil))
	if err != nil || published.Outcome() != configactivation.Published || published.Revision().Number() <= before.ActiveGeneration.Revision {
		t.Fatal("real direct029 publication failed", err)
	}
	stamp, err := r.owner.CurrentConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	generation, _, err := stamp.Components()
	if err != nil || int64(generation) != published.Revision().Number() {
		t.Fatal("Owner did not expose the new publication", err)
	}
	// The held metadata transaction saw A, while Owner before/after is A/B.
	// Read must retry, return the coherent B cut, and only then Next checks the
	// queued activation signal. ActivationSignal (rather than a read failure)
	// proves that successful authoritative read preceded the refusal to sign.
	ticket.finish(true)
	if !stream.Receive() || stream.Msg().GetResyncRequired() == nil || stream.Msg().GetResyncRequired().Reason != wire.ResyncReason_RESYNC_REASON_ACTIVATION_SIGNAL {
		t.Fatal("old dirty batch absorbed a new authority/token instead of resync", stream.Err())
	}
	after := read()
	if after.ActiveGeneration.Revision != published.Revision().Number() || after.ResumeToken == lastReceived {
		t.Fatal("new authoritative generation was not observed")
	}
	// Reconnect with the last token actually delivered, without any Refresh149
	// following direct029. Its prior authority must still force explicit resync.
	resumed, err := api.WatchConfiguration(ctx, connect.NewRequest(&wire.WatchConfigurationRequest{Parent: nha203DocumentParent(before.Name), ResumeToken: lastReceived}))
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if !resumed.Receive() || resumed.Msg().GetResyncRequired() == nil || resumed.Msg().GetResyncRequired().Reason != wire.ResyncReason_RESYNC_REASON_AUTHORITY_CHANGED {
		t.Fatal("reconnect silently skipped direct029 publication", resumed.Err())
	}
}

func TestNHA149RealAPILostRollbackACKRecoversPersistentDirtyBase(t *testing.T) {
	environment := nha042ServerEnvironmentFor(t)
	backendPort := int(environment.observer.Config().Port)
	proxy := nha029OwnerStartCommitACKProxy(t, backendPort)
	proxy.abort = true
	captured, err := environment.config.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.Replace(string(captured.Bytes()), "  port: "+strconv.Itoa(backendPort)+"\n", "  port: "+strconv.Itoa(proxy.port(t))+"\n", 1)
	if raw == string(captured.Bytes()) {
		t.Fatal("synthetic SQL transport was not replaced")
	}
	environment.config = nha029RunFixtureFromRaw(t, []byte(raw))
	r := nha029StartOwnerEnvironment(t, environment, nha029OwnerParent, nha029OwnerChild, nil)
	defer r.close(t)
	defer proxy.allowDrop()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	actor, err := environment.principal.Actor()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		route, present := r.observation.public.routes[routeLookup{method: request.Method, path: request.URL.Path}]
		if !present || route.handler == nil {
			http.NotFound(writer, request)
			return
		}
		native, err := authorization.WithPrincipal(request.Context(), environment.principal)
		if err != nil {
			http.Error(writer, "invalid synthetic principal", http.StatusInternalServerError)
			return
		}
		route.handler.ServeHTTP(writer, request.WithContext(context.WithValue(native, actorContextKey{}, actor)))
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	client := server.Client()
	client.Timeout = 40 * time.Second
	defer client.CloseIdleConnections()
	api := configurationv1connect.NewConfigurationServiceClient(client, server.URL)
	read := func() *wire.ConfigurationDocument {
		t.Helper()
		response, err := api.GetConfigurationDocument(ctx, connect.NewRequest(&wire.GetConfigurationDocumentRequest{Name: nha203PublicDocumentName(t, ctx, client, server.URL, "", "operational.main")}))
		if err != nil {
			t.Fatal("actual Run-owned metadata handler", err)
		}
		return response.Msg
	}
	write := func(base *wire.ConfigurationDocument, candidate []byte) (*connect.Response[longrunningpb.Operation], error) {
		return api.PublishConfigurationDocument(ctx, connect.NewRequest(&wire.PublishConfigurationDocumentRequest{Name: base.Name, Expected: base.Version, ExpectedWorkspaceSha256: base.GetWorkspace().SourceSha256, ExactYamlSource: candidate}))
	}
	admitted := read()
	if response, err := write(admitted, []byte("# rejected dirty B\nexternal_workers: [\n")); connect.CodeOf(err) != connect.CodeInvalidArgument || response != nil {
		t.Fatal("initial dirty rejection failed", err)
	}
	dirty := read()
	candidate := append(bytes.Clone(r.raw), []byte("# uncertain candidate C\n")...)
	proxy.armed.Store(true)
	type answer struct {
		response *connect.Response[longrunningpb.Operation]
		err      error
	}
	completed := make(chan answer, 1)
	go func() { response, err := write(dirty, candidate); completed <- answer{response, err} }()
	nha034WaitSignal(t, proxy.held, "149 real rollback acknowledged only to proxy")
	// This independent observer bypasses the synthetic ACK fault. The backend
	// has actually rolled back; no callback fabricates a transaction outcome.
	var sqlRaw []byte
	var generation int64
	err = environment.observer.QueryRow(ctx, `SELECT v.source_hash,a.generation_revision
	 FROM public.config_active a JOIN public.config_generation_entries e ON e.generation_revision=a.generation_revision
	 JOIN public.config_versions v ON v.document_id=e.document_id AND v.revision=e.version_revision
	 WHERE a.singleton AND e.document_id='operational.main'`).Scan(&sqlRaw, &generation)
	if err != nil || generation != admitted.ActiveGeneration.Revision || !bytes.Equal(sqlRaw, admitted.Version.SourceSha256) {
		t.Fatal("lost rollback ACK changed admitted SQL A", err)
	}
	proxy.allowDrop()
	var result answer
	select {
	case result = <-completed:
	case <-ctx.Done():
		t.Fatal("149 lost-ACK request did not return", ctx.Err())
	}
	if connect.CodeOf(result.err) != connect.CodeUnknown || result.response != nil {
		t.Fatal("149 collapsed real unknown COMMIT into known rejection", result.err)
	}
	resolved, err := r.owner.Reconcile(ctx)
	if err != nil || resolved.Outcome() != configactivation.NotCommitted || resolved.AttemptOutcome() != storage.TransactionNotCommitted {
		t.Fatal("029 did not resolve the actual rollback", err)
	}
	if current := read(); current.GetWorkspace().Dirty != wire.DirtyState_DIRTY_STATE_RECONCILING {
		t.Fatal("unknown edit retired its persistent intent before recovery")
	}
	// Close the actual Run owner before reopening its workspace: this is a
	// restart/recovery proof, never a second concurrent workspace writer.
	r.close(t)
	workspace, err := configdocuments.OpenWorkspace(r.path, "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	var sql [32]byte
	if len(sqlRaw) != len(sql) {
		t.Fatal("invalid exact admitted SQL digest")
	}
	copy(sql[:], sqlRaw)
	found, err := workspace.Reconcile(sql)
	if err != nil || !found {
		t.Fatal("persisted physical B/SQL A/candidate C could not resolve rollback", err)
	}
	observation, err := workspace.Observe()
	if err != nil || observation.ReconciliationPending() {
		t.Fatal("resolved rollback retained a blocking intent", err)
	}
	exact, err := os.ReadFile(r.path)
	if err != nil || !bytes.Equal(exact, candidate) {
		t.Fatal("rollback recovery overwrote editable candidate C", err)
	}
	if len(r.driver.snapshot()) != 0 {
		t.Fatal("149 recovery replayed an action")
	}
}

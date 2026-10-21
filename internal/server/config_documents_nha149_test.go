package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	wire "github.com/damione1/Homeward/gen/home/configuration/v1"
	"github.com/damione1/Homeward/gen/home/configuration/v1/configurationv1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

type nha149TLSFixture struct {
	api            configurationv1connect.ConfigurationServiceClient
	authentication authv1connect.AuthServiceClient
	accounts       authv1connect.AccountServiceClient
	tokens         *auth.SessionTokens
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
	authentication := authv1connect.NewAuthServiceClient(client, base)
	tokens := nha162RunLogin(t, ctx, authentication, "NHA149 admin", password)
	return nha149TLSFixture{api: configurationv1connect.NewConfigurationServiceClient(client, base), authentication: authentication, accounts: authv1connect.NewAccountServiceClient(client, base), tokens: tokens, token: tokens.AccessToken, path: path, raw: source.Bytes(), run: run, ctx: ctx}
}
func (f nha149TLSFixture) read(t *testing.T) *wire.ReadDocumentResponse {
	t.Helper()
	response, err := f.api.ReadDocument(f.ctx, nha056SessionRequest(&wire.ReadDocumentRequest{Id: &wire.DocumentId{Value: "operational.main"}}, f.token))
	if err != nil {
		t.Fatal("actual Run/TLS document read", err)
	}
	return response.Msg
}
func (f nha149TLSFixture) write(base *wire.ReadDocumentResponse, raw []byte) (*connect.Response[wire.WriteDocumentResponse], error) {
	return f.api.WriteDocument(f.ctx, nha056SessionRequest(&wire.WriteDocumentRequest{Id: base.Document.Id, Expected: base.Document.Version, ExpectedWorkspaceSha256: base.Document.GetWorkspace().SourceSha256, ExactYamlSource: raw}, f.token))
}
func TestNHA149ActualRunTLSConcurrentCASAndRestoreCreatesNewRevision(t *testing.T) {
	f := nha149StartTLS(t)
	before := f.read(t)
	if before.Document.Kind != wire.DocumentKind_DOCUMENT_KIND_OPERATIONAL_YAML || before.Document.SchemaVersion != "homeward.operational/1" || before.Document.Version.Revision < 1 || before.Document.UpdatedAt == nil || len(before.Document.ProtectedSourceReference) != 32 || before.Document.GetWorkspace().Dirty != wire.DirtyState_DIRTY_STATE_CLEAN {
		t.Fatal("incomplete coherent public document identity")
	}
	if !bytes.Equal(before.Document.Version.SourceSha256, before.Document.GetWorkspace().SourceSha256) {
		t.Fatal("initial admitted/source observation differs")
	}
	start := make(chan struct{})
	type answer struct {
		response *connect.Response[wire.WriteDocumentResponse]
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
		if answer.err == nil && answer.response.Msg.Activation.Outcome == wire.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED {
			winners++
			continue
		}
		if connect.CodeOf(answer.err) != connect.CodeAborted {
			t.Fatal("concurrent CAS was neither one publication nor structured conflict", answer.err)
		}
		var rpc *connect.Error
		if !errors.As(answer.err, &rpc) || len(rpc.Details()) != 1 {
			t.Fatal("CAS conflict has no typed detail")
		}
		detail, err := rpc.Details()[0].Value()
		if err != nil {
			t.Fatal(err)
		}
		conflict, ok := detail.(*wire.ConfigurationError)
		if !ok || conflict.Code != wire.ErrorCode_ERROR_CODE_CONFLICT || conflict.Current == nil {
			t.Fatal("conflict lost current revision/hash")
		}
		conflicts++
	}
	if winners != 1 || conflicts != 1 {
		t.Fatal("concurrent documentary CAS did not have exactly one winner")
	}
	after := f.read(t)
	if after.Document.Version.Revision <= before.Document.Version.Revision || after.ActiveGeneration.Revision <= before.ActiveGeneration.Revision {
		t.Fatal("revision did not advance")
	}
	exact, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(exact)
	if !bytes.Equal(digest[:], after.Document.Version.SourceSha256) {
		t.Fatal("API did not update the actual editable YAML")
	}
	restored, err := f.api.RestoreDocument(f.ctx, nha056SessionRequest(&wire.RestoreDocumentRequest{Id: after.Document.Id, Expected: after.Document.Version, ExpectedWorkspaceSha256: after.Document.GetWorkspace().SourceSha256, Historical: before.ActiveGeneration}, f.token))
	if err != nil || restored.Msg.Activation.Outcome != wire.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED {
		t.Fatal("actual 029 historical restoration failed", err)
	}
	current := f.read(t)
	if current.Document.Version.Revision <= after.Document.Version.Revision || current.ActiveGeneration.Revision <= after.ActiveGeneration.Revision || !bytes.Equal(current.Document.Version.SourceSha256, before.Document.Version.SourceSha256) {
		t.Fatal("restore rewound the counter or failed to restore exact historical content")
	}
	exact, err = os.ReadFile(f.path)
	if err != nil || !bytes.Equal(exact, f.raw) {
		t.Fatal("restoration failed to update the same editable YAML", err)
	}
	versions, err := f.api.ListVersions(f.ctx, nha056SessionRequest(&wire.ListVersionsRequest{Id: before.Document.Id, Page: &wire.PageRequest{PageSize: 1}}, f.token))
	if err != nil || len(versions.Msg.Versions) != 1 || versions.Msg.Continuation.GetNextPageToken() == "" {
		t.Fatal("real version keyset page failed", err)
	}
	schemas, err := f.api.ListSchemas(f.ctx, nha056SessionRequest(&wire.ListSchemasRequest{Page: &wire.PageRequest{PageSize: 1}}, f.token))
	if err != nil || len(schemas.Msg.Schemas) != 1 || schemas.Msg.Continuation.GetNextPageToken() == "" {
		t.Fatal("real SQL schema page failed", err)
	}
}
func TestNHA149ActualRunTLSDirtyCorrectionAndReadSubscribeGapAreExplicit(t *testing.T) {
	f := nha149StartTLS(t)
	before := f.read(t)
	stream, err := f.api.WatchConfiguration(f.ctx, nha056SessionRequest(&wire.WatchConfigurationRequest{ResumeToken: before.ResumeToken}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Receive() || stream.Msg().GetReady() == nil {
		t.Fatal("stream did not confirm both registrations", stream.Err())
	}
	invalid := []byte("# rejected synthetic private argument\nexternal_workers: [\n")
	rejected, err := f.write(before, invalid)
	if err != nil || rejected.Msg.Activation.Outcome != wire.ActivationOutcome_ACTIVATION_OUTCOME_REJECTED_BEFORE_COMMIT {
		t.Fatal("invalid candidate lost its activation outcome", err)
	}
	if !stream.Receive() || stream.Msg().GetChanges() == nil || len(stream.Msg().GetChanges().Changes) != 1 || stream.Msg().GetChanges().Changes[0].Dirty != wire.DirtyState_DIRTY_STATE_REJECTED {
		t.Fatal("dirty transition was not reported", stream.Err())
	}
	dirty := f.read(t)
	if dirty.Document.Version.Revision != before.Document.Version.Revision || dirty.ActiveGeneration.Revision != before.ActiveGeneration.Revision || dirty.Document.GetWorkspace().Dirty != wire.DirtyState_DIRTY_STATE_REJECTED {
		t.Fatal("invalid workspace became SQL authority")
	}
	corrected, err := f.write(dirty, append(bytes.Clone(f.raw), []byte("# corrected\n")...))
	if err != nil || corrected.Msg.Activation.Outcome != wire.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED {
		t.Fatal("dirty correction failed", err)
	}
	if !stream.Receive() || stream.Msg().GetResyncRequired() == nil {
		t.Fatal("coalesced activation pretended to be full documentary history", stream.Err())
	}
	gap, err := f.api.WatchConfiguration(f.ctx, nha056SessionRequest(&wire.WatchConfigurationRequest{ResumeToken: before.ResumeToken}, f.token))
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
	_, err := f.accounts.CreateAccount(f.ctx, nha056SessionRequest(&auth.CreateAccountRequest{LoginName: "NHA149 ordinary user", DisplayName: "NHA149 ordinary user", Role: auth.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: password}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	user := nha162RunLogin(t, f.ctx, f.authentication, "NHA149 ordinary user", password)
	_, err = f.api.ReadDocument(f.ctx, nha056SessionRequest(&wire.ReadDocumentRequest{Id: before.Document.Id}, user.AccessToken))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("ordinary user read global configuration", err)
	}
	_, err = f.api.WriteDocument(f.ctx, nha056SessionRequest(&wire.WriteDocumentRequest{Id: before.Document.Id, Expected: before.Document.Version, ExpectedWorkspaceSha256: before.Document.GetWorkspace().SourceSha256, ExactYamlSource: []byte("external_workers: []\n")}, user.AccessToken))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("ordinary user mutated global configuration", err)
	}
	exact, err := os.ReadFile(f.path)
	if err != nil || !bytes.Equal(exact, f.raw) {
		t.Fatal("denied mutation touched editable YAML", err)
	}
	before = f.read(t)
	stream, err := f.api.WatchConfiguration(f.ctx, nha056SessionRequest(&wire.WatchConfigurationRequest{ResumeToken: before.ResumeToken}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Receive() || stream.Msg().GetReady() == nil {
		t.Fatal("authorized stream not established", stream.Err())
	}
	if _, err := f.authentication.RevokeSession(f.ctx, nha056SessionRequest(&auth.RevokeSessionRequest{SessionId: f.tokens.Session.Id}, f.token)); err != nil {
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
	if _, ok := detail.(*auth.AuthorizationTermination); !ok {
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
			response := &wire.WriteDocumentResponse{Activation: configActivationResult(uncertain, err)}
			if contracts.AdmitConfigurationResponse(response) != nil || response.Activation.Outcome != wire.ActivationOutcome_ACTIVATION_OUTCOME_COMMIT_UNKNOWN || response.Activation.Transaction != wire.TransactionOutcome_TRANSACTION_OUTCOME_COMMIT_UNKNOWN {
				t.Fatal("149 wire collapsed indeterminate COMMIT into rejection/publication")
			}
			published, err := r.owner.Reconcile(context.Background())
			if err != nil || published.Outcome() != configactivation.Published || published.Revision() != winner {
				t.Fatal("actual owner reconciliation failed", err)
			}
			response = &wire.WriteDocumentResponse{Activation: configActivationResult(published, nil)}
			attempt := wire.TransactionOutcome_TRANSACTION_OUTCOME_COMMITTED
			if rollback {
				attempt = wire.TransactionOutcome_TRANSACTION_OUTCOME_NOT_COMMITTED
			}
			if contracts.AdmitConfigurationResponse(response) != nil || response.Activation.Outcome != wire.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED || response.Activation.Transaction != attempt {
				t.Fatal("149 wire confused published current winner with this caller's transaction")
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
	page, err := f.api.ListDocuments(f.ctx, nha056SessionRequest(&wire.ListDocumentsRequest{Page: &wire.PageRequest{PageSize: 100}}, f.token))
	if err != nil {
		t.Fatal("generic catalogue read through actual TLS/Run", err)
	}
	kinds := make(map[wire.DocumentKind]bool)
	var yamlRead *wire.ReadDocumentResponse
	for _, listed := range page.Msg.Documents {
		response, err := f.api.ReadDocument(f.ctx, nha056SessionRequest(&wire.ReadDocumentRequest{Id: listed.Id}, f.token))
		if err != nil || contracts.AdmitConfigurationResponse(response.Msg) != nil {
			t.Fatal("delivered document kind failed generic read", listed.Kind, err)
		}
		document := response.Msg.Document
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
	if yamlRead == nil || len(yamlRead.Document.ProtectedSourceReference) != 32 {
		t.Fatal("retained YAML-source metadata was not projected opaquely")
	}
	active, present, err := f.run.observed.sessions.store.ReadActiveConfig(f.ctx)
	if err != nil || !present {
		t.Fatal(err)
	}
	matched := false
	for _, version := range active.Versions() {
		if version.Ref().Document().String() == yamlRead.Document.Id.Value {
			digest := version.SHA256()
			matched = version.Ref().Revision().Number() == yamlRead.Document.Version.Revision && bytes.Equal(digest[:], yamlRead.Document.Version.SourceSha256)
		}
	}
	if !matched {
		t.Fatal("second genre did not come from the actual retained generation")
	}
	stream, err := f.api.WatchConfiguration(f.ctx, nha056SessionRequest(&wire.WatchConfigurationRequest{ResumeToken: yamlRead.ResumeToken}, f.token))
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
		if err != nil || response.Msg.Activation.Outcome != wire.ActivationOutcome_ACTIVATION_OUTCOME_REJECTED_BEFORE_COMMIT || response.Msg.Activation.Transaction != wire.TransactionOutcome_TRANSACTION_OUTCOME_NOT_COMMITTED {
			t.Fatal("known rejected dirty write lost its outcome", err)
		}
		base = f.read(t)
		digest := sha256.Sum256(candidate)
		if base.Document.Version.Revision != admitted.Document.Version.Revision || !bytes.Equal(base.Document.Version.SourceSha256, admitted.Document.Version.SourceSha256) || base.ActiveGeneration.Revision != admitted.ActiveGeneration.Revision || base.Document.GetWorkspace().Dirty != wire.DirtyState_DIRTY_STATE_REJECTED || !bytes.Equal(base.Document.GetWorkspace().SourceSha256, digest[:]) {
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
	if err != nil || response.Msg.Activation.Outcome != wire.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED {
		t.Fatal("two rejections blocked actual 029 correction", err)
	}
	current := f.read(t)
	digest := sha256.Sum256(corrected)
	if current.Document.Version.Revision <= admitted.Document.Version.Revision || current.ActiveGeneration.Revision <= admitted.ActiveGeneration.Revision || current.Document.GetWorkspace().Dirty != wire.DirtyState_DIRTY_STATE_CLEAN || !bytes.Equal(current.Document.Version.SourceSha256, digest[:]) {
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
	read := func() *wire.ReadDocumentResponse {
		t.Helper()
		response, err := api.ReadDocument(ctx, connect.NewRequest(&wire.ReadDocumentRequest{Id: &wire.DocumentId{Value: "operational.main"}}))
		if err != nil {
			t.Fatal("actual Run-owned metadata handler", err)
		}
		return response.Msg
	}
	before := read()
	stream, err := api.WatchConfiguration(ctx, connect.NewRequest(&wire.WatchConfigurationRequest{ResumeToken: before.ResumeToken}))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Receive() || stream.Msg().GetReady() == nil {
		t.Fatal("real Watch did not register before ready", stream.Err())
	}
	lastReceived := stream.Msg().GetReady().ResumeToken
	nha034WaitSignal(t, gate.blocked, "Watch ready writer barrier")
	rejected, err := api.WriteDocument(ctx, connect.NewRequest(&wire.WriteDocumentRequest{Id: before.Document.Id, Expected: before.Document.Version, ExpectedWorkspaceSha256: before.Document.GetWorkspace().SourceSha256, ExactYamlSource: []byte("# old dirty event\nexternal_workers: [\n")}))
	if err != nil || rejected.Msg.Activation.Outcome != wire.ActivationOutcome_ACTIVATION_OUTCOME_REJECTED_BEFORE_COMMIT {
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
	resumed, err := api.WatchConfiguration(ctx, connect.NewRequest(&wire.WatchConfigurationRequest{ResumeToken: lastReceived}))
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
	read := func() *wire.ReadDocumentResponse {
		t.Helper()
		response, err := api.ReadDocument(ctx, connect.NewRequest(&wire.ReadDocumentRequest{Id: &wire.DocumentId{Value: "operational.main"}}))
		if err != nil {
			t.Fatal("actual Run-owned metadata handler", err)
		}
		return response.Msg
	}
	write := func(base *wire.ReadDocumentResponse, candidate []byte) (*connect.Response[wire.WriteDocumentResponse], error) {
		return api.WriteDocument(ctx, connect.NewRequest(&wire.WriteDocumentRequest{Id: base.Document.Id, Expected: base.Document.Version, ExpectedWorkspaceSha256: base.Document.GetWorkspace().SourceSha256, ExactYamlSource: candidate}))
	}
	admitted := read()
	if response, err := write(admitted, []byte("# rejected dirty B\nexternal_workers: [\n")); err != nil || response.Msg.Activation.Outcome != wire.ActivationOutcome_ACTIVATION_OUTCOME_REJECTED_BEFORE_COMMIT {
		t.Fatal("initial dirty rejection failed", err)
	}
	dirty := read()
	candidate := append(bytes.Clone(r.raw), []byte("# uncertain candidate C\n")...)
	proxy.armed.Store(true)
	type answer struct {
		response *connect.Response[wire.WriteDocumentResponse]
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
	if err != nil || generation != admitted.ActiveGeneration.Revision || !bytes.Equal(sqlRaw, admitted.Document.Version.SourceSha256) {
		t.Fatal("lost rollback ACK changed admitted SQL A", err)
	}
	proxy.allowDrop()
	var result answer
	select {
	case result = <-completed:
	case <-ctx.Done():
		t.Fatal("149 lost-ACK request did not return", ctx.Err())
	}
	if result.err != nil || result.response.Msg.Activation.Outcome != wire.ActivationOutcome_ACTIVATION_OUTCOME_COMMIT_UNKNOWN || result.response.Msg.Activation.Transaction != wire.TransactionOutcome_TRANSACTION_OUTCOME_COMMIT_UNKNOWN {
		t.Fatal("149 collapsed real unknown COMMIT into known rejection", result.err)
	}
	resolved, err := r.owner.Reconcile(ctx)
	if err != nil || resolved.Outcome() != configactivation.NotCommitted || resolved.AttemptOutcome() != storage.TransactionNotCommitted {
		t.Fatal("029 did not resolve the actual rollback", err)
	}
	if current := read(); current.Document.GetWorkspace().Dirty != wire.DirtyState_DIRTY_STATE_RECONCILING {
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

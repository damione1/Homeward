package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/gen/home/observability/v1/observabilityv1connect"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

// This only activates the retained local-helper corpus, with no physical
// driver, cloud account, MQTT broker, or external HA configuration.
func nha056ActivateLocalGeneration(t *testing.T, cfg config.Config) uint64 {
	t.Helper()
	settings, err := cfg.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := bundle.CompileV2(filepath.Join("..", "config", "bundle", "testdata", "nha097"), "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	document, err := storage.NewConfigDocumentID("nha056.local-generation")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := storage.NewConfigDocumentBinding(document, "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	author, err := storage.NewConfigAuthor("nha056 test")
	if err != nil {
		t.Fatal(err)
	}
	source, err := storage.NewConfigSource("isolated local helper corpus")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := storage.NewConfigVersionMetadata(author, source)
	if err != nil {
		t.Fatal(err)
	}
	var revision uint64
	outcome, err := store.InConfigTransactionOutcome(ctx, func(tx *storage.ConfigTx) error {
		refs, err := tx.SaveCandidate(candidate, []storage.ConfigDocumentBinding{binding}, metadata)
		if err != nil {
			return err
		}
		generation, err := tx.RegisterGeneration(candidate, refs)
		if err != nil {
			return err
		}
		if err := tx.ActivateGeneration(storage.ExpectNoConfigActive(), generation); err != nil {
			return err
		}
		revision = uint64(generation.Number())
		return nil
	})
	if err != nil || outcome != storage.TransactionCommitted || revision == 0 {
		t.Fatal("local generation was not actually committed", err)
	}
	return revision
}
func TestNHA056TLSHealthPublishesActualActiveGenerationAndDeniesBroadUser(t *testing.T) {
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	cfg := nha181Config(t, cluster.Port, data, "127.0.0.1:0", nha181TLSYAML())
	revision := nha056ActivateLocalGeneration(t, cfg)
	run := nha162StartDefaultRun(t, cfg)
	client := nha162RunClient(t, roots, run.address)
	base := "https://" + run.address.String()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const password = "NHA056 generation synthetic password"
	nha162RunBootstrap(t, ctx, data, "NHA056 generation admin", password)
	authClient := authv1connect.NewAuthServiceClient(client, base)
	token := nha162RunLogin(t, ctx, authClient, "NHA056 generation admin", password).AccessToken
	reader := observabilityv1connect.NewObservabilityServiceClient(client, base)
	response, err := reader.GetHealth(ctx, nha056SessionRequest(&obs.GetHealthRequest{}, token))
	if err != nil || contracts.AdmitObservabilityResponse(response.Msg) != nil {
		t.Fatal("real active-generation health unavailable", err)
	}
	if response.Msg.Health.Server.Readiness != obs.ServerReadiness_SERVER_READINESS_READY || response.Msg.Health.Server.Generation.GetPresent().GetValue() != revision {
		t.Fatal("Ready did not identify the real committed generation")
	}
	accounts := authv1connect.NewAccountServiceClient(client, base)
	created, err := accounts.CreateAccount(ctx, nha056SessionRequest(&auth.CreateAccountRequest{LoginName: "NHA056 broad user", DisplayName: "NHA056 broad user", Role: auth.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: password}, token))
	if err != nil {
		t.Fatal(err)
	}
	// All entity rights are intentionally broad. Administrative observability
	// still evaluates the current role at its real common TLS/API boundary.
	nha162RunPolicy(t, ctx, authv1connect.NewAuthorizationPolicyServiceClient(client, base), created.Msg.Account.Id, token, `  - scope: {family: state_registry, operation: read}
    objects: {entities: {all: {capabilities: {all: {}}}}}
`)
	user := nha162RunLogin(t, ctx, authClient, "NHA056 broad user", password).AccessToken
	if _, err := reader.GetHealth(ctx, nha056SessionRequest(&obs.GetHealthRequest{}, user)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("entity-wide user gained administrative health", err)
	}
	if _, err := reader.GetMetrics(ctx, nha056SessionRequest(&obs.GetMetricsRequest{}, user)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("entity-wide user gained administrative metrics", err)
	}
}

package server

import (
	"context"
	obsv2 "github.com/damione1/Homeward/gen/home/api/v2"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

// This only activates the retained local-helper corpus, with no physical
// driver, cloud account, MQTT broker, or external HA configuration.
func nha056ActivateLocalGeneration(t *testing.T, cfg nha029RunFixture) uint64 {
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
	// Seed a genuine pre-029 legacy generation only after establishing its
	// existing027 key/DB pair from this admitted exact operational source.
	// An active generation without that pair is HistoryWithoutKey and must
	// remain a startup refusal; Run will perform the real source-bearing
	// LegacyV1-to-CompiledV2 upgrade and preserve these legacy refs below.
	if cfg.startup.Origin() != config.BootstrapOriginLegacyCombined {
		t.Fatal("local legacy fixture requires its admitted combined source")
	}
	captured, err := cfg.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	dataDir, err := cfg.DataDir()
	if err != nil {
		t.Fatal(err)
	}
	admission, err := storage.FreshSecretStartup(captured.SHA256(), storage.SecretStartupLegacyCombined)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapSecretsForStartup(ctx, dataDir, admission); err != nil {
		t.Fatal("local legacy source did not establish the admitted key/DB pair", err)
	}
	if state, err := store.ProbeStartupSecretState(ctx, dataDir); err != nil || state != storage.SecretStartupEstablishedPair {
		t.Fatal("legacy fixture key/DB identity is not established", err)
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
	legacy, present, err := store.ReadActiveConfig(ctx)
	if err != nil || !present || uint64(legacy.Revision().Number()) != revision || legacy.ManifestFormat() != storage.ConfigManifestLegacyV1 || legacy.Availability() != storage.ConfigGenerationComplete {
		t.Fatal("local fixture did not commit a complete real legacy generation", err)
	}
	return revision
}
func nha056AdmittedSQLRevision(t *testing.T, store *storage.Store, prior uint64) uint64 {
	t.Helper()
	active, present, err := store.ReadActiveConfig(context.Background())
	if err != nil || !present || active.Availability() != storage.ConfigGenerationComplete || active.ManifestFormat() != storage.ConfigManifestCompiledV2 || uint64(active.Revision().Number()) <= prior {
		t.Fatalf("source-bearing startup lacks actual complete SQL publication: present=%t prior=%d err=%v", present, prior, err)
	}
	if prior != 0 {
		revision, err := storage.NewConfigGenerationRevision(int64(prior))
		if err != nil {
			t.Fatal(err)
		}
		legacy, err := store.ReadConfigGeneration(context.Background(), revision)
		if err != nil {
			t.Fatal(err)
		}
		for _, old := range legacy.Versions() {
			found := false
			for _, current := range active.Versions() {
				if current.Ref() == old.Ref() && current.SHA256() == old.SHA256() {
					found = true
				}
			}
			if !found {
				t.Fatal("startup generation lost a real legacy source/version/hash")
			}
		}
	}
	return uint64(active.Revision().Number())
}

func TestNHA056TLSHealthPublishesActualActiveGenerationAndDeniesBroadUser(t *testing.T) {
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	cfg := nha181Config(t, cluster.Port, data, "127.0.0.1:0", nha181TLSYAML())
	revision := nha056ActivateLocalGeneration(t, cfg)
	run := nha162StartDefaultRun(t, cfg.startup)
	revision = nha056AdmittedSQLRevision(t, run.observed.sessions.store, revision)
	client := nha162RunClient(t, roots, run.address)
	base := "https://" + run.address.String()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const password = "NHA056 generation synthetic password"
	nha162RunBootstrap(t, ctx, data, "NHA056 generation admin", password)
	authClient := apiv2connect.NewAuthServiceClient(client, base)
	token := nha162RunLogin(t, ctx, authClient, "NHA056 generation admin", password).AccessToken
	reader := apiv2connect.NewObservabilityServiceClient(client, base)
	home := nha203ObservationHome(t, ctx, client, base, token)
	response, err := reader.GetHealth(ctx, nha056SessionRequest(&obsv2.GetHealthRequest{Name: home + "/health"}, token))
	if err != nil || contracts.AdmitObservationResponse(response.Msg) != nil {
		t.Fatal("real active-generation health unavailable", err)
	}
	if response.Msg.Server.Readiness != obsv2.ObservationServerReadiness_OBSERVATION_SERVER_READINESS_READY || response.Msg.Server.Generation.GetPresent().GetValue() != revision {
		t.Fatal("Ready did not identify the real committed generation")
	}
	accounts := apiv2connect.NewAccountServiceClient(client, base)
	created, err := accounts.CreateAccount(ctx, nha056SessionRequest(&obsv2.CreateAccountRequest{Parent: nha203ObservationHome(t, ctx, client, base, token), Account: &obsv2.Account{LoginName: "NHA056 broad user", DisplayName: "NHA056 broad user", Role: obsv2.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: password}}, token))
	if err != nil {
		t.Fatal(err)
	}
	// All entity rights are intentionally broad. Administrative observability
	// still evaluates the current role at its real common TLS/API boundary.
	nha162RunPolicy(t, ctx, apiv2connect.NewAuthorizationPolicyServiceClient(client, base), created.Msg.Name, token, `  - scope: {family: state_registry, operation: read}
    objects: {entities: {all: {capabilities: {all: {}}}}}
`)
	user := nha162RunLogin(t, ctx, authClient, "NHA056 broad user", password).AccessToken
	if _, err := reader.GetHealth(ctx, nha056SessionRequest(&obsv2.GetHealthRequest{Name: home + "/health"}, user)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("entity-wide user gained administrative health", err)
	}
	if _, err := reader.GetMetrics(ctx, nha056SessionRequest(&obsv2.GetMetricsRequest{Name: home + "/metrics"}, user)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("entity-wide user gained administrative metrics", err)
	}
}

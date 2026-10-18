package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

type nha177ServerPilot struct {
	policy    *integrations.CloudPolicy
	admission *storage.CloudAttemptQuota
	transport *http.Transport
	binder    CloudAuthorizationBinder
	store     *storage.Store
	session   *integrations.Session
	client    *integrations.CloudClient
	started   chan error
	stopped   chan error
}

type nha177WaitingProvider struct {
	entered     chan struct{}
	release     chan struct{}
	cooperative bool
}

type nha177FixedProvider struct {
	profile integrations.CloudAuthorizationProfileID
	calls   atomic.Int32
}

func (p *nha177FixedProvider) Authorize(_ context.Context, _ integrations.CloudAuthorizationExchanger, input integrations.CloudAuthorizationInput) (integrations.CloudAuthorizedCredential, error) {
	p.calls.Add(1)
	if !input.Profile().Equal(p.profile) {
		return integrations.CloudAuthorizedCredential{}, errors.New("wrong profile")
	}
	value, err := storage.NewSecretValue([]byte("private-replacement-session-credential"))
	if err != nil {
		return integrations.CloudAuthorizedCredential{}, err
	}
	return integrations.NewCloudAuthorizedCredential(p.profile, value)
}

func TestNHA177StaleUnbindCannotRemoveReplacementSession(t *testing.T) {
	cluster := testpg.Start(t)
	dataDir := shortPrivateDataDir(t)
	cfg := nha051ServerConfig(t, cluster.Port, dataDir)
	settings, err := cfg.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	repository, err := store.BootstrapSecrets(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	host, _, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	if err := host.AttachSecrets(repository); err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha177-replacement")
	if err != nil {
		t.Fatal(err)
	}
	handle := nha051ServerHandle(t, "nha177-replacement", "cloud-credential")
	profile, err := integrations.ParseCloudAuthorizationProfileID("synthetic-replacement")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := integrations.NewCloudPolicy(integrations.DefaultCloudLimits(), nil, integrations.RealCloudClock{}, integrations.RealCloudTimer{}, integrations.RandomJitter{})
	if err != nil {
		t.Fatal(err)
	}
	firstSession, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	firstClient, err := integrations.NewCloudClient(firstSession, policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer firstClient.Close()
	provider := &nha177FixedProvider{profile: profile}
	coordinator, err := newCloudAuthorizationCoordinator(context.Background(), host, integrations.DefaultCloudAuthorizationLimits())
	if err != nil {
		t.Fatal(err)
	}
	registration, err := NewCloudAuthorizationRegistration(instance, handle, profile, policy, provider)
	if err != nil {
		t.Fatal(err)
	}
	binder, err := coordinator.composition().Register(registration)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.freeze()
	if err := binder.Bind(firstClient); err != nil {
		t.Fatal(err)
	}
	if err := binder.Bind(firstClient); !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatalf("duplicate Bind reused one generation: %v", err)
	}
	if err := coordinator.openAdmissions(); err != nil {
		t.Fatal(err)
	}
	secondSession, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstSession.Done():
	default:
		t.Fatal("first pilot session was not retired")
	}
	secondClient, err := integrations.NewCloudClient(secondSession, policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer secondClient.Close()
	if err := binder.Bind(secondClient); err != nil {
		t.Fatal(err)
	}
	binder.Unbind(firstClient)
	inputValue, err := storage.NewSecretValue([]byte("private-input"))
	if err != nil {
		t.Fatal(err)
	}
	input, err := integrations.NewCloudAuthorizationInput(profile, inputValue)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := coordinator.composition().Invoker().Authorize(context.Background(), instance, input)
	if err != nil || !receipt.Published() || provider.calls.Load() != 1 {
		t.Fatalf("stale Unbind removed replacement: receipt=%v err=%v calls=%d", receipt, err, provider.calls.Load())
	}
	binder.Unbind(secondClient)
	if _, err := coordinator.composition().Invoker().Authorize(context.Background(), instance, input); !errors.Is(err, integrations.ErrInactiveSession) {
		t.Fatalf("current Unbind left admission open: %v", err)
	}
	if err := coordinator.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func (p *nha177WaitingProvider) Authorize(ctx context.Context, _ integrations.CloudAuthorizationExchanger, _ integrations.CloudAuthorizationInput) (integrations.CloudAuthorizedCredential, error) {
	close(p.entered)
	if p.cooperative {
		<-ctx.Done()
		return integrations.CloudAuthorizedCredential{}, ctx.Err()
	}
	<-p.release
	return integrations.CloudAuthorizedCredential{}, errors.New("provider finished after cancellation")
}

func TestNHA177ProviderDeadlineAndRealDrain(t *testing.T) {
	cluster := testpg.Start(t)
	dataDir := shortPrivateDataDir(t)
	cfg := nha051ServerConfig(t, cluster.Port, dataDir)
	settings, err := cfg.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	repository, err := store.BootstrapSecrets(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	host, _, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	if err := host.AttachSecrets(repository); err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha177-deadline")
	if err != nil {
		t.Fatal(err)
	}
	handle := nha051ServerHandle(t, "nha177-deadline", "cloud-credential")
	profile, err := integrations.ParseCloudAuthorizationProfileID("synthetic-deadline")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := integrations.NewCloudPolicy(integrations.DefaultCloudLimits(), nil, integrations.RealCloudClock{}, integrations.RealCloudTimer{}, integrations.RandomJitter{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	client, err := integrations.NewCloudClient(session, policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	inputSecret, err := storage.NewSecretValue([]byte("private-input"))
	if err != nil {
		t.Fatal(err)
	}
	input, err := integrations.NewCloudAuthorizationInput(profile, inputSecret)
	if err != nil {
		t.Fatal(err)
	}
	limits, err := integrations.AdmitCloudAuthorizationLimits(integrations.CloudAuthorizationLimitsInput{
		TotalTimeout: 500 * time.Millisecond, MaxExchanges: 1, MaxActive: 1, MaxPerHandle: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, cooperative := range []bool{true, false} {
		provider := &nha177WaitingProvider{entered: make(chan struct{}), release: make(chan struct{}), cooperative: cooperative}
		coordinator, err := newCloudAuthorizationCoordinator(context.Background(), host, limits)
		if err != nil {
			t.Fatal(err)
		}
		registration, err := NewCloudAuthorizationRegistration(instance, handle, profile, policy, provider)
		if err != nil {
			t.Fatal(err)
		}
		binder, err := coordinator.composition().Register(registration)
		if err != nil {
			t.Fatal(err)
		}
		coordinator.freeze()
		if err := binder.Bind(client); err != nil {
			t.Fatal(err)
		}
		if err := coordinator.openAdmissions(); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			_, err := coordinator.composition().Invoker().Authorize(context.Background(), instance, input)
			result <- err
		}()
		_ = nha177Wait(t, provider.entered)
		if cooperative {
			if err := nha177Wait(t, result); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("provider did not receive total deadline: %v", err)
			}
			if err := coordinator.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
		} else {
			select {
			case err := <-result:
				t.Fatalf("noncooperative provider returned before release: %v", err)
			case <-time.After(550 * time.Millisecond):
			}
			coordinator.CloseAdmissions()
			closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			if err := coordinator.Close(closeCtx); !errors.Is(err, ErrCloudAuthorizationJoinTimeout) {
				t.Fatalf("close lost real drain: %v", err)
			}
			cancel()
			select {
			case <-coordinator.Done():
				t.Fatal("Done closed before provider returned")
			default:
			}
			close(provider.release)
			if err := nha177Wait(t, result); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("late provider outcome=%v", err)
			}
			_ = nha177Wait(t, coordinator.Done())
		}
		binder.Unbind(client)
	}
}

func (p *nha177ServerPilot) Start(_ context.Context, host integrations.SessionHost, _ integrations.Tasks) error {
	fail := func(err error) error { p.started <- err; return err }
	session, ok := host.(*integrations.Session)
	if !ok || session == nil {
		return fail(errors.New("pilot did not receive its real session"))
	}
	p.session = session
	client, err := integrations.NewCloudClient(host, p.policy, p.transport, p.admission)
	if err != nil {
		return fail(err)
	}
	p.client = client
	if err := p.binder.Bind(client); err != nil {
		client.Close()
		return fail(err)
	}
	p.started <- nil
	return nil
}
func (p *nha177ServerPilot) Stop(ctx context.Context) error {
	if p.client != nil {
		p.binder.Unbind(p.client)
		defer p.client.Close()
	}
	var err error
	select {
	case <-p.session.Done():
	default:
		err = errors.New("pilot stopped before session retirement")
	}
	if err == nil {
		err = p.store.InTransaction(ctx, func(tx pgx.Tx) error {
			var one int
			return tx.QueryRow(ctx, "SELECT 1").Scan(&one)
		})
	}
	p.stopped <- err
	return err
}

type nha177SyntheticProvider struct {
	target  string
	profile integrations.CloudAuthorizationProfileID
	calls   atomic.Int32
}

func (p *nha177SyntheticProvider) Authorize(ctx context.Context, exchanger integrations.CloudAuthorizationExchanger, input integrations.CloudAuthorizationInput) (integrations.CloudAuthorizedCredential, error) {
	p.calls.Add(1)
	if !input.Profile().Equal(p.profile) {
		return integrations.CloudAuthorizedCredential{}, errors.New("wrong profile")
	}
	requestID, err := integrations.ParseCloudAuthorizationExchangeID("token")
	if err != nil {
		return integrations.CloudAuthorizedCredential{}, err
	}
	request, err := integrations.NewCloudAuthorizationRequest(requestID, http.MethodGet, p.target, nil, nil, integrations.CloudAuthorizationExpectedOK)
	if err != nil {
		return integrations.CloudAuthorizedCredential{}, err
	}
	response, err := exchanger.Exchange(ctx, request)
	if err != nil {
		return integrations.CloudAuthorizedCredential{}, err
	}
	value, err := storage.NewSecretValue(response.Bytes())
	if err != nil {
		return integrations.CloudAuthorizedCredential{}, err
	}
	return integrations.NewCloudAuthorizedCredential(p.profile, value)
}

func nha177Wait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(8 * time.Second):
		t.Fatal("NHA-177 lifecycle barrier timed out")
		var zero T
		return zero
	}
}

func TestNHA177RunCompositionPublishesEncryptedCredential(t *testing.T) {
	cluster := testpg.Start(t)
	dataDir := shortPrivateDataDir(t)
	cfg := nha051ServerConfig(t, cluster.Port, dataDir)
	instance, err := domain.ParseIntegrationInstanceID("nha177-server-synthetic")
	if err != nil {
		t.Fatal(err)
	}
	handle := nha051ServerHandle(t, "nha177-server-synthetic", "cloud-credential")
	profile, err := integrations.ParseCloudAuthorizationProfileID("synthetic-opaque-v1")
	if err != nil {
		t.Fatal(err)
	}
	account, err := integrations.ParseCloudAccountID("nha177-synthetic-account")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := integrations.NewCloudAccountBinding(instance, account)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := integrations.NewCloudPolicy(integrations.DefaultCloudLimits(), []integrations.CloudAccountBinding{binding}, integrations.RealCloudClock{}, integrations.RealCloudTimer{}, integrations.RandomJitter{})
	if err != nil {
		t.Fatal(err)
	}
	const committed = "{\"access\":\"private-access\",\"refresh\":\"private-refresh\"}"
	var requests atomic.Int32
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(committed))
	}))
	defer endpoint.Close()
	provider := &nha177SyntheticProvider{target: endpoint.URL, profile: profile}
	pilot := &nha177ServerPilot{
		policy: policy, transport: endpoint.Client().Transport.(*http.Transport),
		started: make(chan error, 1), stopped: make(chan error, 1),
	}
	spec, err := integrations.NewPilotSpec(instance, func() integrations.Pilot { return pilot })
	if err != nil {
		t.Fatal(err)
	}
	invokerReady := make(chan CloudAuthorizationInvoker, 1)
	factory := func(_ context.Context, store *storage.Store) (RuntimePlan, error) {
		pilot.store = store
		quota, err := storage.NewCloudAttemptQuota(store, "nha177-synthetic-account", 5, "America/Toronto")
		if err != nil {
			return RuntimePlan{}, err
		}
		pilot.admission = quota
		return RuntimePlan{
			Identity: unconfiguredIdentity{}, Pilots: []integrations.PilotSpec{spec},
			ConfigureCloudAuthorization: func(_ context.Context, composition CloudAuthorizationComposition) error {
				registration, err := NewCloudAuthorizationRegistration(instance, handle, profile, policy, provider, quota)
				if err != nil {
					return err
				}
				binder, err := composition.Register(registration)
				if err != nil {
					return err
				}
				pilot.binder = binder
				invokerReady <- composition.Invoker()
				return nil
			},
		}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var lc net.ListenConfig
	go func() {
		done <- RunWithFactory(ctx, cfg, lc.Listen, func(address net.Addr) { ready <- address }, factory)
	}()
	if err := nha177Wait(t, pilot.started); err != nil {
		t.Fatal(err)
	}
	_ = nha177Wait(t, ready)
	invoker := nha177Wait(t, invokerReady)
	if requests.Load() != 0 {
		t.Fatal("pilot Start performed cloud authorization")
	}
	inputSecret, err := storage.NewSecretValue([]byte("ephemeral-operator-input"))
	if err != nil {
		t.Fatal(err)
	}
	input, err := integrations.NewCloudAuthorizationInput(profile, inputSecret)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := invoker.Authorize(context.Background(), instance, input)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Published() || !receipt.AttemptID().Valid() || provider.calls.Load() != 1 || requests.Load() != 1 {
		t.Fatal("production coordinator did not publish one authorized exchange")
	}
	sessionSecrets, err := pilot.session.Secrets()
	if err != nil {
		t.Fatal(err)
	}
	current, err := sessionSecrets.Read(context.Background(), handle)
	if err != nil {
		t.Fatal(err)
	}
	currentRaw, _ := current.Bytes()
	if string(currentRaw) != committed {
		t.Fatal("pilot session did not read committed credential")
	}
	cancel()
	if err := nha177Wait(t, pilot.stopped); err != nil {
		t.Fatal(err)
	}
	if err := nha177Wait(t, done); err != nil {
		t.Fatal(err)
	}

	settings, err := cfg.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	repository, err := reopened.BootstrapSecrets(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	persisted, _, err := repository.ReadCredentialVersioned(context.Background(), handle)
	if err != nil {
		t.Fatal(err)
	}
	persistedRaw, _ := persisted.Bytes()
	if string(persistedRaw) != committed {
		t.Fatal("credential did not survive PG reopen")
	}
	var ciphertext []byte
	if err := reopened.InTransaction(context.Background(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), "SELECT ciphertext FROM public.integration_secrets WHERE instance_id=$1 AND secret_name=$2", "nha177-server-synthetic", "cloud-credential").Scan(&ciphertext)
	}); err != nil {
		t.Fatal(err)
	}
	if len(ciphertext) == 0 || strings.Contains(string(ciphertext), "private-access") {
		t.Fatal("credential was not encrypted at rest")
	}
	quota, err := storage.NewCloudAttemptQuota(reopened, "nha177-synthetic-account", 5, "America/Toronto")
	if err != nil {
		t.Fatal(err)
	}
	count, err := quota.Today(context.Background())
	if err != nil || count != 1 {
		t.Fatalf("durable quota count=%d err=%v", count, err)
	}
}

type nha177HeldProvider struct {
	target  string
	profile integrations.CloudAuthorizationProfileID
	entered chan struct{}
	release chan struct{}
}

func (p *nha177HeldProvider) Authorize(ctx context.Context, exchanger integrations.CloudAuthorizationExchanger, _ integrations.CloudAuthorizationInput) (integrations.CloudAuthorizedCredential, error) {
	id, err := integrations.ParseCloudAuthorizationExchangeID("authorize")
	if err != nil {
		return integrations.CloudAuthorizedCredential{}, err
	}
	request, err := integrations.NewCloudAuthorizationRequest(id, http.MethodGet, p.target, nil, nil, integrations.CloudAuthorizationExpectedOK)
	if err != nil {
		return integrations.CloudAuthorizedCredential{}, err
	}
	response, err := exchanger.Exchange(ctx, request)
	if err != nil {
		return integrations.CloudAuthorizedCredential{}, err
	}
	close(p.entered)
	<-p.release // deliberately ignores cancellation to exercise the real join
	secret, err := storage.NewSecretValue(response.Bytes())
	if err != nil {
		return integrations.CloudAuthorizedCredential{}, err
	}
	return integrations.NewCloudAuthorizedCredential(p.profile, secret)
}

func TestNHA177RunRetainsPostgresUntilAuthorizationActuallyDrains(t *testing.T) {
	cluster := testpg.Start(t)
	dataDir := shortPrivateDataDir(t)
	cfg := nha051ServerConfig(t, cluster.Port, dataDir)
	instance, err := domain.ParseIntegrationInstanceID("nha177-server-drain")
	if err != nil {
		t.Fatal(err)
	}
	handle := nha051ServerHandle(t, "nha177-server-drain", "cloud-credential")
	profile, err := integrations.ParseCloudAuthorizationProfileID("synthetic-drain")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := integrations.NewCloudPolicy(integrations.DefaultCloudLimits(), nil, integrations.RealCloudClock{}, integrations.RealCloudTimer{}, integrations.RandomJitter{})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("private-late-token"))
	}))
	defer endpoint.Close()
	provider := &nha177HeldProvider{
		target: endpoint.URL, profile: profile, entered: make(chan struct{}), release: make(chan struct{}),
	}
	pilot := &nha177ServerPilot{
		policy: policy, transport: endpoint.Client().Transport.(*http.Transport),
		started: make(chan error, 1), stopped: make(chan error, 1),
	}
	spec, err := integrations.NewPilotSpec(instance, func() integrations.Pilot { return pilot })
	if err != nil {
		t.Fatal(err)
	}
	invokerReady := make(chan CloudAuthorizationInvoker, 1)
	factory := func(_ context.Context, store *storage.Store) (RuntimePlan, error) {
		pilot.store = store
		quota, err := storage.NewCloudAttemptQuota(store, "nha177-drain-account", 5, "America/Toronto")
		if err != nil {
			return RuntimePlan{}, err
		}
		pilot.admission = quota
		return RuntimePlan{
			Identity: unconfiguredIdentity{}, Pilots: []integrations.PilotSpec{spec},
			ConfigureCloudAuthorization: func(_ context.Context, composition CloudAuthorizationComposition) error {
				registration, err := NewCloudAuthorizationRegistration(instance, handle, profile, policy, provider, quota)
				if err != nil {
					return err
				}
				binder, err := composition.Register(registration)
				if err != nil {
					return err
				}
				pilot.binder = binder
				invokerReady <- composition.Invoker()
				return nil
			},
		}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var lc net.ListenConfig
	go func() {
		done <- RunWithFactory(ctx, cfg, lc.Listen, func(address net.Addr) { ready <- address }, factory)
	}()
	if err := nha177Wait(t, pilot.started); err != nil {
		t.Fatal(err)
	}
	_ = nha177Wait(t, ready)
	invoker := nha177Wait(t, invokerReady)
	inputSecret, err := storage.NewSecretValue([]byte("private-operator-input"))
	if err != nil {
		t.Fatal(err)
	}
	input, err := integrations.NewCloudAuthorizationInput(profile, inputSecret)
	if err != nil {
		t.Fatal(err)
	}
	authorizationDone := make(chan error, 1)
	go func() { _, err := invoker.Authorize(context.Background(), instance, input); authorizationDone <- err }()
	_ = nha177Wait(t, provider.entered)
	cancel()
	if err := nha177Wait(t, pilot.stopped); err != nil {
		t.Fatal(err)
	}
	runErr := nha177Wait(t, done)
	if !errors.Is(runErr, ErrCloudAuthorizationJoinTimeout) {
		t.Fatalf("Run did not report authorization join timeout: %v", runErr)
	}
	if err := pilot.store.InTransaction(context.Background(), func(tx pgx.Tx) error {
		var one int
		return tx.QueryRow(context.Background(), "SELECT 1").Scan(&one)
	}); err != nil {
		t.Fatalf("PostgreSQL closed before provider finished: %v", err)
	}
	select {
	case err := <-authorizationDone:
		t.Fatalf("authorization finished before provider release: %v", err)
	default:
	}
	close(provider.release)
	if err := nha177Wait(t, authorizationDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("late provider returned visible success: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := pilot.store.InTransaction(context.Background(), func(tx pgx.Tx) error {
			var one int
			return tx.QueryRow(context.Background(), "SELECT 1").Scan(&one)
		})
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("PostgreSQL did not close after authorization drain")
		}
		time.Sleep(10 * time.Millisecond)
	}
	settings, err := cfg.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	repository, err := reopened.BootstrapSecrets(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = repository.ReadCredentialVersioned(context.Background(), handle)
	var secretErr *storage.Error
	if !errors.As(err, &secretErr) || secretErr.Kind() != storage.SecretNotFound {
		t.Fatalf("late authorization published despite shutdown: %v", err)
	}
}

func TestNHA177PreparationFailureClosesAuthorizationBeforeStore(t *testing.T) {
	cluster := testpg.Start(t)
	cfg := nha051ServerConfig(t, cluster.Port, shortPrivateDataDir(t))
	instance, err := domain.ParseIntegrationInstanceID("nha177-prepare")
	if err != nil {
		t.Fatal(err)
	}
	handle := nha051ServerHandle(t, "nha177-prepare", "cloud-credential")
	profile, err := integrations.ParseCloudAuthorizationProfileID("synthetic-prepare")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := integrations.NewCloudPolicy(integrations.DefaultCloudLimits(), nil, integrations.RealCloudClock{}, integrations.RealCloudTimer{}, integrations.RandomJitter{})
	if err != nil {
		t.Fatal(err)
	}
	provider := &nha177WaitingProvider{entered: make(chan struct{}), release: make(chan struct{})}
	sentinel := errors.New("synthetic prepare failure")
	var store *storage.Store
	var invoker CloudAuthorizationInvoker
	factory := func(_ context.Context, opened *storage.Store) (RuntimePlan, error) {
		store = opened
		return RuntimePlan{
			Identity: unconfiguredIdentity{},
			ConfigureCloudAuthorization: func(_ context.Context, composition CloudAuthorizationComposition) error {
				registration, err := NewCloudAuthorizationRegistration(instance, handle, profile, policy, provider)
				if err != nil {
					return err
				}
				if _, err := composition.Register(registration); err != nil {
					return err
				}
				invoker = composition.Invoker()
				return sentinel
			},
		}, nil
	}
	ready := false
	var lc net.ListenConfig
	err = RunWithFactory(context.Background(), cfg, lc.Listen, func(net.Addr) { ready = true }, factory)
	if !errors.Is(err, sentinel) || ready || invoker == nil {
		t.Fatalf("preparation boundary: err=%v ready=%v", err, ready)
	}
	inputSecret, err := storage.NewSecretValue([]byte("private-input"))
	if err != nil {
		t.Fatal(err)
	}
	input, err := integrations.NewCloudAuthorizationInput(profile, inputSecret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := invoker.Authorize(context.Background(), instance, input); !errors.Is(err, ErrCloudAuthorizationClosed) {
		t.Fatalf("prepared invoker remained open: %v", err)
	}
	if err := store.InTransaction(context.Background(), func(tx pgx.Tx) error {
		var one int
		return tx.QueryRow(context.Background(), "SELECT 1").Scan(&one)
	}); err == nil {
		t.Fatal("prepared Store was not closed")
	}
}

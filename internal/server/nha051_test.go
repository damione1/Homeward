package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

func nha051ServerConfig(t *testing.T, port int, dataDir string) config.Config {
	t.Helper()
	data := []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\n", dataDir, port))
	c, err := config.ParseWithEnv(data, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func nha051ServerHandle(t *testing.T, instance, name string) storage.SecretHandle {
	t.Helper()
	i, err := domain.ParseIntegrationInstanceID(instance)
	if err != nil {
		t.Fatal(err)
	}
	n, err := storage.ParseSecretName(name)
	if err != nil {
		t.Fatal(err)
	}
	h, err := storage.NewSecretHandle(i, n)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

type nha051Pilot struct {
	started    chan integrations.SessionHost
	read       chan error
	own, other storage.SecretHandle
	expected   string
}

func (p *nha051Pilot) Start(ctx context.Context, host integrations.SessionHost, _ integrations.Tasks) error {
	provider, err := host.Secrets()
	if err != nil {
		p.read <- err
		return err
	}
	v, err := provider.Read(ctx, p.own)
	if err != nil {
		p.read <- err
		return err
	}
	plain, err := v.Bytes()
	if err != nil {
		p.read <- err
		return err
	}
	if string(plain) != p.expected {
		err = errors.New("fake pilot received wrong credential")
		p.read <- err
		return err
	}
	_, err = provider.Read(ctx, p.other)
	if !errors.Is(err, integrations.ErrForeignSecretHandle) {
		p.read <- errors.New("cross-instance read was admitted")
		return errors.New("cross-instance read was admitted")
	}
	p.read <- nil
	p.started <- host
	return nil
}

func (p *nha051Pilot) Stop(context.Context) error { return nil }

func TestNHA051RunWithFactoryBootstrapsBeforePilotAndReady(t *testing.T) {
	cluster := testpg.Start(t)
	dataDir := filepath.Join(t.TempDir(), "persistent-data")
	c := nha051ServerConfig(t, cluster.Port, dataDir)
	inst, err := domain.ParseIntegrationInstanceID("fake-instance-a")
	if err != nil {
		t.Fatal(err)
	}
	own := nha051ServerHandle(t, "fake-instance-a", "api-token")
	other := nha051ServerHandle(t, "fake-instance-b", "api-token")
	const canary = "NHA051_server_canary_token"
	for run := 0; run < 2; run++ {
		pilot := &nha051Pilot{started: make(chan integrations.SessionHost, 1), read: make(chan error, 1), own: own, other: other, expected: canary}
		spec, err := integrations.NewPilotSpec(inst, func() integrations.Pilot { return pilot })
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		ready := make(chan net.Addr, 1)
		done := make(chan error, 1)
		factory := func(ctx context.Context, store *storage.Store) (RuntimePlan, error) {
			repo, err := store.BootstrapSecrets(ctx, dataDir)
			if err != nil {
				return RuntimePlan{}, err
			}
			if run == 0 {
				v, _ := storage.NewSecretValue([]byte(canary))
				if err := repo.RotateCredential(ctx, own, v); err != nil {
					return RuntimePlan{}, err
				}
			}
			return RuntimePlan{Identity: unconfiguredIdentity{}, Pilots: []integrations.PilotSpec{spec}}, nil
		}
		var lc net.ListenConfig
		go func() { done <- RunWithFactory(ctx, c, lc.Listen, func(a net.Addr) { ready <- a }, factory) }()
		select {
		case err := <-pilot.read:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("pilot could not read secrets")
		}
		select {
		case <-pilot.started:
		case <-time.After(5 * time.Second):
			t.Fatal("pilot did not start")
		}
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			t.Fatal("server was not ready")
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("server did not stop")
		}
	}
	settings, _ := c.PostgresSettings()
	store, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var audit int
	if err := store.InTransaction(context.Background(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM public.secret_access_audit WHERE requesting_instance='fake-instance-a' AND target_instance='fake-instance-b' AND reason='foreign_handle'`).Scan(&audit)
	}); err != nil || audit != 2 {
		t.Fatalf("cross access audit: %d %v", audit, err)
	}
	file := filepath.Join(dataDir, ".env")
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	called := false
	err = RunWithFactory(context.Background(), c, func(context.Context, string, string) (net.Listener, error) { called = true; return nil, nil }, func(net.Addr) { called = true }, func(context.Context, *storage.Store) (RuntimePlan, error) { called = true; return RuntimePlan{}, nil })
	if err == nil || called || !strings.Contains(err.Error(), "persistent key") {
		t.Fatalf("missing key reached ready/composition: %v", err)
	}
	wrong := []byte("HOMEWARD_SECRETS_KEY_V1=" + base64.StdEncoding.EncodeToString(make([]byte, 32)) + "\n")
	if err := os.WriteFile(file, wrong, 0600); err != nil {
		t.Fatal(err)
	}
	err = RunWithFactory(context.Background(), c, func(context.Context, string, string) (net.Listener, error) { called = true; return nil, nil }, func(net.Addr) { called = true }, func(context.Context, *storage.Store) (RuntimePlan, error) { called = true; return RuntimePlan{}, nil })
	if err == nil || called || !strings.Contains(err.Error(), "persistent key") {
		t.Fatalf("wrong key reached ready/composition: %v", err)
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatal("startup error leaked credential")
	}
}

func TestNHA051RevocationDuringBlockedReadReturnsNoValue(t *testing.T) {
	cluster := testpg.Start(t)
	dataDir := filepath.Join(t.TempDir(), "persistent-data")
	c := nha051ServerConfig(t, cluster.Port, dataDir)
	inst, err := domain.ParseIntegrationInstanceID("fake-instance-a")
	if err != nil {
		t.Fatal(err)
	}
	handle := nha051ServerHandle(t, "fake-instance-a", "blocked-token")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan integrations.SessionHost, 1)
	pilot := &nha051AccessPilot{started: started}
	spec, err := integrations.NewPilotSpec(inst, func() integrations.Pilot { return pilot })
	if err != nil {
		t.Fatal(err)
	}
	managerCh := make(chan *integrations.Manager, 1)
	storeCh := make(chan *storage.Store, 1)
	factory := func(ctx context.Context, store *storage.Store) (RuntimePlan, error) {
		repo, err := store.BootstrapSecrets(ctx, dataDir)
		if err != nil {
			return RuntimePlan{}, err
		}
		v, _ := storage.NewSecretValue([]byte("NHA051_blocked_canary"))
		if err := repo.RotateCredential(ctx, handle, v); err != nil {
			return RuntimePlan{}, err
		}
		return RuntimePlan{Identity: unconfiguredIdentity{}, Pilots: []integrations.PilotSpec{spec}, Configure: func(_ context.Context, m *integrations.Manager, s *storage.Store) error {
			managerCh <- m
			storeCh <- s
			return nil
		}}, nil
	}
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var lc net.ListenConfig
	go func() { done <- RunWithFactory(ctx, c, lc.Listen, func(a net.Addr) { ready <- a }, factory) }()
	var host integrations.SessionHost
	select {
	case host = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("pilot did not start")
	}
	manager := <-managerCh
	store := <-storeCh
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not become ready")
	}
	provider, err := host.Secrets()
	if err != nil {
		t.Fatal(err)
	}
	locked := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- store.InTransaction(context.Background(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(context.Background(), `LOCK TABLE public.integration_secrets IN ACCESS EXCLUSIVE MODE`); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("SQL lock not acquired")
	}
	readDone := make(chan struct {
		value storage.SecretValue
		err   error
	}, 1)
	go func() {
		v, e := provider.Read(context.Background(), handle)
		readDone <- struct {
			value storage.SecretValue
			err   error
		}{v, e}
	}()
	// Observe PostgreSQL's blocked read before revoking; no timing guess is used.
	deadline := time.After(5 * time.Second)
	for {
		var waiting bool
		checkErr := store.InTransaction(context.Background(), func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE 'SELECT revision,key_id,suite,salt,ciphertext FROM public.integration_secrets%')`).Scan(&waiting)
		})
		if checkErr != nil {
			t.Fatal(checkErr)
		}
		if waiting {
			break
		}
		select {
		case <-deadline:
			t.Fatal("read did not block on SQL lock")
		case <-time.After(10 * time.Millisecond):
		}
	}
	session, ok := host.(*integrations.Session)
	if !ok {
		t.Fatal("unexpected SessionHost")
	}
	if err := manager.Revoke(session); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-lockDone; err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-readDone:
		if !errors.Is(result.err, integrations.ErrInactiveSession) {
			t.Fatalf("revoked read: %v", result.err)
		}
		if _, err := result.value.Bytes(); err == nil {
			t.Fatal("revoked read returned value")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read did not complete")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}
}

type nha051AccessPilot struct{ started chan integrations.SessionHost }

func (p *nha051AccessPilot) Start(_ context.Context, h integrations.SessionHost, _ integrations.Tasks) error {
	p.started <- h
	return nil
}
func (p *nha051AccessPilot) Stop(context.Context) error { return nil }

type nha051AuditFailurePilot struct {
	target storage.SecretHandle
	result chan struct {
		value storage.SecretValue
		err   error
	}
}

func (p *nha051AuditFailurePilot) Start(ctx context.Context, host integrations.SessionHost, _ integrations.Tasks) error {
	provider, err := host.Secrets()
	if err != nil {
		p.result <- struct {
			value storage.SecretValue
			err   error
		}{storage.SecretValue{}, err}
		return nil
	}
	v, err := provider.Read(ctx, p.target)
	p.result <- struct {
		value storage.SecretValue
		err   error
	}{v, err}
	return nil
}
func (p *nha051AuditFailurePilot) Stop(context.Context) error { return nil }

func TestNHA051AuditInsertFailureClosesCrossInstanceAccess(t *testing.T) {
	cluster := testpg.Start(t)
	dataDir := filepath.Join(t.TempDir(), "persistent-data")
	c := nha051ServerConfig(t, cluster.Port, dataDir)
	inst, err := domain.ParseIntegrationInstanceID("fake-instance-a")
	if err != nil {
		t.Fatal(err)
	}
	other := nha051ServerHandle(t, "fake-instance-b", "api-token")
	pilot := &nha051AuditFailurePilot{target: other, result: make(chan struct {
		value storage.SecretValue
		err   error
	}, 1)}
	spec, err := integrations.NewPilotSpec(inst, func() integrations.Pilot { return pilot })
	if err != nil {
		t.Fatal(err)
	}
	storeCh := make(chan *storage.Store, 1)
	factory := func(context.Context, *storage.Store) (RuntimePlan, error) {
		return RuntimePlan{Identity: unconfiguredIdentity{}, Pilots: []integrations.PilotSpec{spec}, Configure: func(ctx context.Context, _ *integrations.Manager, store *storage.Store) error {
			if err := store.InTransaction(ctx, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `CREATE FUNCTION public.nha051_audit_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'NHA051_audit_SQL_canary'; END $$;
				CREATE TRIGGER nha051_audit_failure BEFORE INSERT ON public.secret_access_audit FOR EACH ROW EXECUTE FUNCTION public.nha051_audit_failure();`)
				return err
			}); err != nil {
				return err
			}
			storeCh <- store
			return nil
		}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var lc net.ListenConfig
	go func() { done <- RunWithFactory(ctx, c, lc.Listen, func(a net.Addr) { ready <- a }, factory) }()
	var result struct {
		value storage.SecretValue
		err   error
	}
	select {
	case result = <-pilot.result:
	case <-time.After(5 * time.Second):
		t.Fatal("pilot did not attempt denied read")
	}
	if _, extractErr := result.value.Bytes(); !errors.Is(result.err, integrations.ErrSecretAuditUnavailable) || extractErr == nil || strings.Contains(result.err.Error(), "NHA051_audit_SQL_canary") {
		t.Fatalf("audit failure leaked/admitted: %v", result.err)
	}
	store := <-storeCh
	var count int
	if err := store.InTransaction(context.Background(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM public.secret_access_audit`).Scan(&count)
	}); err != nil || count != 0 {
		t.Fatalf("failed audit persisted claim: %d %v", count, err)
	}
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}
}

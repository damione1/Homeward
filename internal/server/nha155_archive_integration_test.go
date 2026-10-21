package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/damione1/Homeward/internal/backups"
	backuparchive "github.com/damione1/Homeward/internal/backups/archive"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

func nha155RunStartup(t *testing.T, port int, dataDir, public string) config.StartupInputs {
	t.Helper()
	raw := fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\n", dataDir, port)
	if public != "" {
		raw += fmt.Sprintf("backup_recovery_recipient: %q\n", public)
	}
	startup, err := config.NewDetachedStartup([]byte(raw), "configuration.yaml", nil, nha029TestEnv)
	if err != nil {
		t.Fatal("prepare archive Run fixture")
	}
	return startup
}
func nha155RunSource(t *testing.T) []backuparchive.YAMLSource {
	t.Helper()
	text := "input_boolean:\n  fixture:\n    initial: true\n"
	source, err := backuparchive.NewYAMLSource("configuration.yaml", int64(len(text)), strings.NewReader(text))
	if err != nil {
		t.Fatal("prepare Run YAML source")
	}
	return []backuparchive.YAMLSource{source}
}

func TestNHA155RunBorrowsRegistryAndSameServerKeyBeforeReady(t *testing.T) {
	cluster := testpg.Start(t)
	dataDir := shortPrivateDataDir(t)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal("generate operator recovery fixture")
	}
	private, err := backuparchive.ParseRecoveryPrivate([]byte(identity.String()))
	if err != nil {
		t.Fatal("admit operator recovery fixture")
	}
	startup := nha155RunStartup(t, cluster.Port, dataDir, identity.Recipient().String())
	artifact, _ := backups.ParseArtifactID("15500000000000000000000000000001")
	local, _ := backups.ParseInstanceID("local")
	var registry *backups.Registry
	var borrowed backups.Provider
	var services BackupArchiveServices
	var admittedKey bytes.Buffer
	configured := make(chan struct{}, 1)
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Run archive fixture left a live goroutine")
		}
	}()
	factory := func(callback context.Context, store *storage.Store) (RuntimePlan, error) {
		secrets, err := store.BootstrapSecrets(callback, dataDir)
		if err != nil {
			return RuntimePlan{}, err
		}
		key, err := secrets.BackupKey()
		if err != nil {
			return RuntimePlan{}, err
		}
		if _, err := key.WriteTo(&admittedKey); err != nil {
			return RuntimePlan{}, err
		}
		return RuntimePlan{Identity: unconfiguredIdentity{}, ConfigureBackups: func(_ context.Context, owner *backups.Registry) error { registry = owner; return nil }, ConfigureBackupArchives: func(callback context.Context, archives BackupArchiveServices) error {
			if archives.providers != registry || registry == nil {
				return errors.New("Run did not lend its provider registry")
			}
			services = archives
			provider, err := archives.Provider(local)
			if err != nil {
				return err
			}
			borrowed = provider
			if _, canClose := provider.(backups.ManagedProvider); canClose {
				return errors.New("archive callback acquired registry ownership")
			}
			image, err := archives.Seal(callback, artifact, strings.NewReader("prepared-snapshot"), int64(len("prepared-snapshot")), nha155RunSource(t), backuparchive.DefaultLimits())
			if err != nil {
				return err
			}
			defer image.Close()
			published, err := image.Publish(callback, provider)
			if err != nil || published.State() != backups.Published {
				return errors.New("Run archive publication failed")
			}
			parent := filepath.Join(t.TempDir(), "recovery")
			result, err := backuparchive.RecoverProvider(callback, provider, artifact, private, parent, "restored", backuparchive.DefaultLimits())
			if err != nil || result.State() != backuparchive.Restored {
				return errors.New("Run archive not recoverable")
			}
			recoveredKey, err := os.ReadFile(filepath.Join(parent, "restored", ".env"))
			defer clear(recoveredKey)
			if err != nil || !bytes.Equal(recoveredKey, admittedKey.Bytes()) {
				return errors.New("Run archived a different server key")
			}
			onDisk, err := os.ReadFile(filepath.Join(dataDir, ".env"))
			defer clear(onDisk)
			if err != nil || !bytes.Equal(onDisk, recoveredKey) {
				return errors.New("archive key differs from live fixture .env")
			}
			configured <- struct{}{}
			return nil
		}}, nil
	}
	var listener net.ListenConfig
	go func() {
		defer close(done)
		done <- RunWithStartupFactory(ctx, startup, listener.Listen, func(address net.Addr) { ready <- address }, factory)
	}()
	select {
	case <-ready:
	case <-done:
		t.Fatal("Run failed before archive callback/Ready")
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not become Ready")
	}
	select {
	case <-configured:
	default:
		t.Fatal("Ready preceded archive callback")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("Run failed to drain archive fixture")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run archive fixture did not stop")
	}
	clear(admittedKey.Bytes())
	if _, err := borrowed.Verify(context.Background(), artifact); !errors.Is(err, backups.NewError(backups.Closed)) {
		t.Fatal("Run leaked borrowed provider")
	}
	if _, err := services.Provider(local); !errors.Is(err, backups.NewError(backups.Closed)) {
		t.Fatal("Run leaked archive registry capability")
	}
}

func TestNHA155RunArchiveCallbackFailurePreventsReadyAndCloses(t *testing.T) {
	cluster := testpg.Start(t)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal("generate failure fixture public")
	}
	startup := nha155RunStartup(t, cluster.Port, shortPrivateDataDir(t), identity.Recipient().String())
	stop := errors.New("NHA155 callback refused")
	local, _ := backups.ParseInstanceID("local")
	artifact, _ := backups.ParseArtifactID("15500000000000000000000000000002")
	var borrowed backups.Provider
	var services BackupArchiveServices
	listenCalled, readyCalled := false, false
	err = RunWithStartupFactory(context.Background(), startup, func(context.Context, string, string) (net.Listener, error) { listenCalled = true; return nil, stop }, func(net.Addr) { readyCalled = true }, func(context.Context, *storage.Store) (RuntimePlan, error) {
		return RuntimePlan{Identity: unconfiguredIdentity{}, ConfigureBackupArchives: func(_ context.Context, archives BackupArchiveServices) error {
			services = archives
			var err error
			borrowed, err = archives.Provider(local)
			if err != nil {
				return err
			}
			return stop
		}}, nil
	})
	if !errors.Is(err, stop) || borrowed == nil || listenCalled || readyCalled {
		t.Fatal("archive callback failure did not stop before listener/Ready")
	}
	if _, err := borrowed.Verify(context.Background(), artifact); !errors.Is(err, backups.NewError(backups.Closed)) {
		t.Fatal("failed archive startup leaked provider")
	}
	if _, err := services.Provider(local); !errors.Is(err, backups.NewError(backups.Closed)) {
		t.Fatal("failed archive startup leaked registry")
	}
}

func TestNHA155RunWithoutRecoveryPublicRefusesArchive(t *testing.T) {
	cluster := testpg.Start(t)
	startup := nha155RunStartup(t, cluster.Port, shortPrivateDataDir(t), "")
	artifact, _ := backups.ParseArtifactID("15500000000000000000000000000003")
	stop := errors.New("NHA155 no-public fixture complete")
	called := false
	err := RunWithStartupFactory(context.Background(), startup, func(context.Context, string, string) (net.Listener, error) { return nil, stop }, nil, func(context.Context, *storage.Store) (RuntimePlan, error) {
		return RuntimePlan{Identity: unconfiguredIdentity{}, ConfigureBackupArchives: func(ctx context.Context, archives BackupArchiveServices) error {
			called = true
			image, err := archives.Seal(ctx, artifact, strings.NewReader("snapshot"), 8, nha155RunSource(t), backuparchive.DefaultLimits())
			if image != nil || err == nil {
				if image != nil {
					_ = image.Close()
				}
				return errors.New("archive created without public recovery")
			}
			local, _ := backups.ParseInstanceID("local")
			provider, err := archives.Provider(local)
			if err != nil {
				return err
			}
			request, _ := backups.NewListRequest(1, backups.Cursor{})
			page, err := provider.List(ctx, request)
			if err != nil || len(page.Items()) != 0 {
				return errors.New("unconfigured recovery published an archive")
			}
			return nil
		}}, nil
	})
	if !called || !errors.Is(err, stop) {
		t.Fatal("missing recovery public changed server startup or archive refusal")
	}
}

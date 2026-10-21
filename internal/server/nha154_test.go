package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/backups"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

func nha154RunArchive(ctx context.Context, registry *backups.Registry, instance string, id backups.ArtifactID, payload []byte) (backups.Provider, error) {
	instanceID, _ := backups.ParseInstanceID(instance)
	provider, err := registry.Instance(instanceID)
	if err != nil {
		return nil, err
	}
	if !provider.Capabilities().AtomicPublication() {
		return nil, errors.New("missing atomic backup capability")
	}
	intent, _ := backups.NewWriteIntent(id, int64(len(payload)))
	result, err := provider.Write(ctx, intent, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	if result.State() != backups.Published && result.State() != backups.AlreadyPresent {
		return nil, errors.New("artifact not durable")
	}
	verified, err := provider.Verify(ctx, id)
	if err != nil || !verified.Valid() {
		return nil, errors.New("artifact failed verification")
	}
	reader, err := provider.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if !bytes.Equal(got, payload) {
		return nil, errors.New("opaque payload changed")
	}
	request, _ := backups.NewListRequest(2, backups.Cursor{})
	page, err := provider.List(ctx, request)
	if err != nil || !page.Valid() || len(page.Items()) != 1 || page.Items()[0].ID() != id {
		return nil, errors.New("published fixture absent from bounded page")
	}
	return provider, nil
}

func TestNHA154RunComposition(t *testing.T) {
	cluster := testpg.Start(t)
	dataDir := shortPrivateDataDir(t)
	root := filepath.Join(dataDir, "configured-backups")
	raw := []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\nbackups:\n  instances:\n    - id: local\n      provider: local\n      local:\n        root: %q\n    - id: neighbor\n      provider: local\n      local:\n        root: %q\n", dataDir, cluster.Port, root, root))
	startup, err := config.NewDetachedStartup(raw, "configuration.yaml", nil, nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := backups.ParseArtifactID("15400000000000000000000000000001")
	var previous *backups.Registry
	for run := 0; run < 2; run++ {
		ctx, cancel := context.WithCancel(context.Background())
		var borrowed backups.Provider
		configured := make(chan struct{}, 1)
		ready := make(chan net.Addr, 1)
		done := make(chan error, 1)
		factory := func(context.Context, *storage.Store) (RuntimePlan, error) {
			return RuntimePlan{Identity: unconfiguredIdentity{}, ConfigureBackups: func(callback context.Context, registry *backups.Registry) error {
				if registry == nil || registry == previous {
					return errors.New("Run reused an owner across restarts")
				}
				previous = registry
				provider, err := nha154RunArchive(callback, registry, "local", id, []byte("Run opaque fixture"))
				if err != nil {
					return err
				}
				borrowed = provider
				neighbor, err := nha154RunArchive(callback, registry, "neighbor", id, []byte("isolated neighbor fixture"))
				if err != nil {
					return err
				}
				result, err := provider.Delete(callback, id)
				if err != nil || result.State() != backups.Deleted {
					return errors.New("Run fixture delete failed")
				}
				if _, err := neighbor.Verify(callback, id); err != nil {
					return errors.New("delete crossed instance boundary")
				}
				configured <- struct{}{}
				return nil
			}}, nil
		}
		var listen net.ListenConfig
		go func() {
			done <- RunWithStartupFactory(ctx, startup, listen.Listen, func(address net.Addr) { ready <- address }, factory)
		}()
		select {
		case <-ready:
		case err := <-done:
			cancel()
			t.Fatalf("Run failed before ready: %v", err)
		case <-time.After(15 * time.Second):
			cancel()
			t.Fatal("Run did not become ready")
		}
		select {
		case <-configured:
		default:
			cancel()
			t.Fatal("ready preceded backup composition")
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Run did not drain")
		}
		if _, err := borrowed.Verify(context.Background(), id); !errors.Is(err, backups.NewError(backups.Closed)) {
			t.Fatalf("Run leaked provider ownership: %v", err)
		}
		instanceID, _ := backups.ParseInstanceID("neighbor")
		if _, err := previous.Instance(instanceID); !errors.Is(err, backups.NewError(backups.Closed)) {
			t.Fatal("Run leaked registry")
		}
	}
}

func TestNHA154RunCallbackFailureClosesProvider(t *testing.T) {
	cluster := testpg.Start(t)
	dataDir := shortPrivateDataDir(t)
	fixture := nha051ServerConfig(t, cluster.Port, dataDir)
	stop := errors.New("NHA154 composition refused")
	var borrowed backups.Provider
	listenerCalled := false
	err := RunWithStartupFactory(context.Background(), fixture.startup, func(context.Context, string, string) (net.Listener, error) { listenerCalled = true; return nil, stop }, nil, func(context.Context, *storage.Store) (RuntimePlan, error) {
		return RuntimePlan{Identity: unconfiguredIdentity{}, ConfigureBackups: func(ctx context.Context, registry *backups.Registry) error {
			id, _ := backups.ParseArtifactID("15400000000000000000000000000002")
			provider, err := nha154RunArchive(ctx, registry, backups.Local.String(), id, []byte("default local active"))
			if err != nil {
				return err
			}
			borrowed = provider
			return stop
		}}, nil
	})
	if !errors.Is(err, stop) || listenerCalled || borrowed == nil {
		t.Fatalf("callback failure ordering: %v", err)
	}
	id, _ := backups.ParseArtifactID("15400000000000000000000000000002")
	if _, err := borrowed.Verify(context.Background(), id); !errors.Is(err, backups.NewError(backups.Closed)) {
		t.Fatal("failed startup leaked local provider")
	}
}

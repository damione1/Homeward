package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

func TestNHA168ComposedStartupPreservesEncryptedDatabaseOnMissingOrWrongKey(t *testing.T) {
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	raw := []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\n", data, cluster.Port))
	startup := nha029RunStartupFromRaw(t, raw)
	stop := errors.New("synthetic package boundary reached")
	factoryCalled := false
	readyCalled := false
	ready := func(net.Addr) { readyCalled = true }
	listener := func(context.Context, string, string) (net.Listener, error) {
		t.Fatal("unsafe startup reached listener")
		return nil, stop
	}
	factory := func(context.Context, *storage.Store) (RuntimePlan, error) {
		factoryCalled = true
		return RuntimePlan{}, stop
	}
	if err := RunWithStartupFactory(context.Background(), startup, listener, ready, factory); !errors.Is(err, stop) || !factoryCalled {
		t.Fatal("fresh server did not establish its own key")
	}
	keyPath := filepath.Join(data, ".env")
	original, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(keyPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("runtime server key is not private")
	}
	settings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repository, err := store.BootstrapSecrets(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha168-synthetic")
	if err != nil {
		t.Fatal(err)
	}
	name, err := storage.ParseSecretName("synthetic-token")
	if err != nil {
		t.Fatal(err)
	}
	handle, err := storage.NewSecretHandle(instance, name)
	if err != nil {
		t.Fatal(err)
	}
	value, err := storage.NewSecretValue([]byte("NHA168_SYNTHETIC_ENCRYPTED_CANARY"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.RotateCredential(context.Background(), handle, value); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"missing", "mismatched"} {
		t.Run(scenario, func(t *testing.T) {
			// Keep scenarios independent even if an assertion aborts before the
			// explicit restoration and decryptability proof below.
			t.Cleanup(func() {
				if err := os.WriteFile(keyPath, original, 0600); err != nil {
					t.Error("could not restore synthetic fixture key")
				}
			})
			if err := os.Remove(keyPath); err != nil {
				t.Fatal(err)
			}
			var wrong []byte
			if scenario == "mismatched" {
				wrong = []byte("HOMEWARD_SECRETS_KEY_V1=" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x68}, 32)) + "\n")
				if err := os.WriteFile(keyPath, wrong, 0600); err != nil {
					t.Fatal(err)
				}
			}
			state, err := store.ProbeStartupSecretState(context.Background(), data)
			if err != nil || state != storage.SecretStartupOrphanDB {
				t.Fatal("fixture did not preserve an established database with an invalid key")
			}
			factoryCalled, readyCalled = false, false
			err = RunWithStartupFactory(context.Background(), startup, listener, ready, factory)
			// ReconcileStartup rejects OrphanDB before the lower-level secret
			// bootstrap is called. Its typed public startup error is the oracle.
			if !errors.Is(err, configactivation.ErrStartupIntegrity) || factoryCalled || readyCalled {
				t.Fatal("invalid key reached a runtime owner or was not refused")
			}
			got, readErr := os.ReadFile(keyPath)
			if scenario == "missing" && !os.IsNotExist(readErr) || scenario == "mismatched" && (readErr != nil || !bytes.Equal(got, wrong)) {
				t.Fatal("invalid key silently regenerated or changed")
			}
			if scenario == "mismatched" {
				if err := os.Remove(keyPath); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(keyPath, original, 0600); err != nil {
				t.Fatal(err)
			}
			reopened, err := store.BootstrapSecrets(context.Background(), data)
			if err != nil {
				t.Fatal(err)
			}
			retained, err := reopened.Read(context.Background(), handle)
			if err != nil {
				t.Fatal("encrypted database value was lost")
			}
			plain, err := retained.Bytes()
			if err != nil || string(plain) != "NHA168_SYNTHETIC_ENCRYPTED_CANARY" {
				t.Fatal("restored key cannot decrypt preserved data")
			}
			clear(plain)
		})
	}
	factoryCalled = false
	if err := RunWithStartupFactory(context.Background(), startup, listener, ready, factory); !errors.Is(err, stop) || !factoryCalled || readyCalled {
		t.Fatal("intact recreated server could not admit preserved data")
	}
}

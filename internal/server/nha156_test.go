package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/damione1/Homeward/internal/backups"
	archive "github.com/damione1/Homeward/internal/backups/archive"
	"github.com/damione1/Homeward/internal/backups/s3/s3test"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

func nha156RunRaw(port int, dataDir, endpoint, activation, recipient string) []byte {
	raw := fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\n", dataDir, port)
	if recipient != "" {
		raw += fmt.Sprintf("backup_recovery_recipient: %q\n", recipient)
	}
	if activation != "absent" {
		raw += fmt.Sprintf("backups:\n  instances:\n    - id: offsite\n      provider: s3\n%s      s3:\n        endpoint: %q\n        bucket: nha156-bucket\n        prefix: homeward/test\n        region: us-east-1\n        credential_ref:\n          instance: nha156-credentials\n          name: credential-bundle\n", activation, endpoint)
	}
	return []byte(raw)
}
func nha156Run(t *testing.T, startup config.StartupInputs, factory RuntimeFactory) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var listen net.ListenConfig
	go func() {
		done <- RunWithStartupFactory(ctx, startup, listen.Listen, func(a net.Addr) { ready <- a }, factory)
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("Run did not drain cleanly")
			}
		case <-time.After(10 * time.Second):
			t.Error("Run process did not return")
		}
	}()
	select {
	case <-ready:
	case err := <-done:
		done <- err
		t.Fatal("Run failed before ready")
	case <-time.After(25 * time.Second):
		t.Fatal("Run did not become ready")
	}
}
func TestNHA156RunDisabledNoNetwork(t *testing.T) {
	for _, activation := range []string{"absent", "", "      enabled: false\n"} {
		t.Run(fmt.Sprintf("selection-%d", len(activation)), func(t *testing.T) {
			cluster := testpg.Start(t)
			directory := shortPrivateDataDir(t)
			s3 := s3test.New()
			defer s3.Close()
			raw := nha156RunRaw(cluster.Port, directory, s3.URL(), activation, "")
			startup, err := config.NewDetachedStartup(raw, "configuration.yaml", nil, nha029TestEnv)
			if err != nil {
				t.Fatal("disabled startup")
			}
			configured := false
			nha156Run(t, startup, func(context.Context, *storage.Store) (RuntimePlan, error) {
				return RuntimePlan{Identity: unconfiguredIdentity{}, ConfigureBackups: func(_ context.Context, registry *backups.Registry) error {
					id, _ := backups.ParseInstanceID("offsite")
					if _, err := registry.Instance(id); !errors.Is(err, backups.NewError(backups.NotFound)) {
						return errors.New("disabled S3 exposed")
					}
					if activation == "absent" {
						local, _ := backups.ParseInstanceID("local")
						if _, err := registry.Instance(local); err != nil {
							return errors.New("local default lost")
						}
					}
					configured = true
					return nil
				}}, nil
			})
			if !configured || len(s3.Requests()) != 0 {
				t.Fatal("disabled Run performed network")
			}
		})
	}
}
func TestNHA156RunUsesSameRegistryAndSecrets(t *testing.T) {
	cluster := testpg.Start(t)
	directory := shortPrivateDataDir(t)
	s3 := s3test.New()
	defer s3.Close()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal("recipient fixture")
	}
	raw := nha156RunRaw(cluster.Port, directory, s3.URL(), "      enabled: true\n", identity.Recipient().String())
	parsed, err := config.ParseWithEnv(raw, nha029TestEnv)
	if err != nil {
		t.Fatal("active startup config")
	}
	settings, err := parsed.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal("seed DB")
	}
	if _, err := store.Migrate(context.Background()); err != nil {
		store.Close()
		t.Fatal("migrate DB")
	}
	secrets, err := store.BootstrapSecrets(context.Background(), directory)
	if err != nil {
		store.Close()
		t.Fatal("seed secrets051")
	}
	handle := nha051ServerHandle(t, "nha156-credentials", "credential-bundle")
	value, _ := storage.NewSecretValue([]byte(s3test.Bundle))
	if err := secrets.RotateCredential(context.Background(), handle, value); err != nil {
		store.Close()
		t.Fatal("seed credential reference")
	}
	key, err := secrets.BackupKey()
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	var expectedKey bytes.Buffer
	if _, err := key.WriteTo(&expectedKey); err != nil {
		store.Close()
		t.Fatal(err)
	}
	store.Close()
	startup, err := config.NewDetachedStartup(raw, "configuration.yaml", nil, nha029TestEnv)
	if err != nil {
		t.Fatal("active detached startup")
	}
	var registry *backups.Registry
	var provider backups.Provider
	configured := false
	nha156Run(t, startup, func(context.Context, *storage.Store) (RuntimePlan, error) {
		return RuntimePlan{Identity: unconfiguredIdentity{}, ConfigureBackups: func(_ context.Context, r *backups.Registry) error { registry = r; return nil }, ConfigureBackupArchives: func(ctx context.Context, a BackupArchiveServices) error {
			if a.providers != registry || registry == nil {
				return errors.New("second provider pipeline")
			}
			id, _ := backups.ParseInstanceID("offsite")
			var err error
			provider, err = a.Provider(id)
			if err != nil {
				return err
			}
			if _, owned := provider.(backups.ManagedProvider); owned {
				return errors.New("borrower acquired ownership")
			}
			var admittedKey bytes.Buffer
			if _, err := a.key.WriteTo(&admittedKey); err != nil || !bytes.Equal(admittedKey.Bytes(), expectedKey.Bytes()) {
				return errors.New("archive key differs from051")
			}
			artifact, _ := backups.ParseArtifactID("15600000000000000000000000000001")
			image, err := a.Seal(ctx, artifact, strings.NewReader("NHA156_RUN_DATABASE_CANARY"), int64(len("NHA156_RUN_DATABASE_CANARY")), nha155RunSource(t), archive.DefaultLimits())
			if err != nil {
				return err
			}
			defer image.Close()
			result, err := image.Publish(ctx, provider)
			if err != nil || result.State() != backups.Published {
				return errors.New("Run S3 publication")
			}
			reader, err := provider.Open(ctx, artifact)
			if err != nil {
				return err
			}
			ciphertext, readErr := io.ReadAll(reader)
			closeErr := reader.Close()
			if readErr != nil || closeErr != nil || !bytes.HasPrefix(ciphertext, []byte("age-encryption.org/v1\n")) {
				return errors.New("Run S3 ciphertext read")
			}
			for _, secret := range [][]byte{[]byte("NHA156_RUN_DATABASE_CANARY"), expectedKey.Bytes(), []byte("NHA156SECRET")} {
				if bytes.Contains(ciphertext, secret) {
					return errors.New("Run leaked plaintext")
				}
			}
			request, _ := backups.NewListRequest(1, backups.Cursor{})
			page, err := provider.List(ctx, request)
			if err != nil || len(page.Items()) != 1 {
				return errors.New("Run S3 listing")
			}
			if _, err := provider.Verify(ctx, artifact); err != nil {
				return err
			}
			deleted, err := provider.Delete(ctx, artifact)
			if err != nil || deleted.State() != backups.Deleted {
				return errors.New("Run S3 delete")
			}
			configured = true
			return nil
		}}, nil
	})
	if !configured || len(s3.Requests()) == 0 {
		t.Fatal("Run never used S3")
	}
	for _, request := range s3.Requests() {
		if !request.Signed {
			t.Fatal("Run did not use referenced051 credentials")
		}
	}
	artifact, _ := backups.ParseArtifactID("15600000000000000000000000000001")
	if _, err := provider.Verify(context.Background(), artifact); !errors.Is(err, backups.NewError(backups.Closed)) {
		t.Fatal("Run leaked provider")
	}
}

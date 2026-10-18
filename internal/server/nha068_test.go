package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	mochi "github.com/mochi-mqtt/server/v2"
)

// This uses the public Run composition path, with no RuntimeFactory override.
// NHA-069 has not supplied mappings, so the pilot only subscribes and observes.
func TestNHA068RunActivatesConfiguredMQTTWithoutFactory(t *testing.T) {
	broker := testbroker.Start(t)
	cluster := testpg.Start(t)
	dataDir := filepath.Join(t.TempDir(), "app-data")
	const instance = "nha068-server-pilot"
	data := []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\nmqtt:\n  profile: '5'\n  broker: 'mqtt://%s'\n  instance: '%s'\n  mode: passive\n  username_secret: mqtt-username\n  password_secret: mqtt-password\n  subscriptions: ['nha068/state/#']\n", dataDir, cluster.Port, broker.Address, instance))
	c, err := config.ParseWithEnv(data, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	pg, err := c.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), pg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(context.Background()); err != nil {
		store.Close()
		t.Fatal(err)
	}
	repo, err := store.BootstrapSecrets(context.Background(), dataDir)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	for name, value := range map[string]string{"mqtt-username": testbroker.PilotUser, "mqtt-password": testbroker.PilotPassword} {
		handle := nha051ServerHandle(t, instance, name)
		secret, err := storage.NewSecretValue([]byte(value))
		if err != nil {
			store.Close()
			t.Fatal(err)
		}
		if err := repo.RotateCredential(context.Background(), handle, secret); err != nil {
			store.Close()
			t.Fatal(err)
		}
	}
	store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var lc net.ListenConfig
	go func() { done <- Run(ctx, c, lc.Listen, func(a net.Addr) { ready <- a }) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Run exited before ready: %v", err)
	case <-time.After(6 * time.Second):
		t.Fatal("Run not ready")
	}
	digest := sha256.Sum256([]byte(instance))
	clientID := "homeward-" + hex.EncodeToString(digest[:12])
	deadline := time.Now().Add(5 * time.Second)
	var firstClient *mochi.Client
	for {
		if client, ok := broker.Server.Clients.Get(clientID); ok && client.State.Subscriptions.Len() > 0 {
			firstClient = client
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("configured pilot did not connect and subscribe")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !firstClient.Properties.Clean || firstClient.Properties.Props.SessionExpiryInterval != 0 || firstClient.Properties.Will.Flag != 0 {
		t.Fatal("runtime admitted persistent MQTT session")
	}
	broker.Publish(t, "nha068/state/device", []byte("canary"), false)
	firstClient.Stop(errors.New("synthetic network loss"))
	broker.Publish(t, "nha068/state/device", []byte("offline-during-retry"), false)
	deadline = time.Now().Add(5 * time.Second)
	for {
		client, ok := broker.Server.Clients.Get(clientID)
		if ok && client != firstClient && !client.Closed() && client.State.Subscriptions.Len() > 0 {
			if !client.Properties.Clean || client.Properties.Props.SessionExpiryInterval != 0 {
				t.Fatal("reconnected runtime resumed offline session")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Supervisor did not reconnect and resubscribe")
		}
		time.Sleep(10 * time.Millisecond)
	}
	broker.Publish(t, "nha068/state/device", []byte("after-retry"), false)
	if count := broker.PublishCount(clientID); count != 0 {
		t.Fatalf("Run passive pilot emitted %d application PUBLISH packets", count)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("Run did not join MQTT pilot")
	}
	if count := broker.PublishCount(clientID); count != 0 {
		t.Fatalf("Run emitted %d application PUBLISH packets by joined Stop", count)
	}
}

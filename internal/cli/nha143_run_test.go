package cli

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	wire "github.com/damione1/Homeward/gen/home/integrationadmin/v1"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/server"
	"github.com/damione1/Homeward/internal/testpg"
	"google.golang.org/protobuf/encoding/protojson"
)

// Exercises the shipped Run assembly, generated clients, native authentication,
// PostgreSQL admission, server-side user denial and protected operator socket.
// No driver, home endpoint, SQL client or physical device is injected by the CLI.
func TestNHA143ActualRunTLSAccountsIntegrationsAndLocalRecovery(t *testing.T) {
	cluster := testpg.Start(t)
	data := testpg.ShortPrivateDataDir(t)
	var err error
	data, err = filepath.EvalSymlinks(data)
	if err != nil {
		t.Fatal(err)
	}
	certificate, key := nha143Certificate(t)
	if err := os.WriteFile(filepath.Join(data, "public.crt"), certificate, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "public.key"), key, 0600); err != nil {
		t.Fatal(err)
	}
	clear(key)
	raw := []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npublic:\n  transport: tls\n  tls:\n    certificate_file: public.crt\n    private_key_file: public.key\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\n", data, cluster.Port))
	workspace := filepath.Join(nha143Private(t), "configuration.yaml")
	if err := os.WriteFile(workspace, raw, 0600); err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(workspace, config.DefaultBootstrapChoice(), func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan net.Addr, 1)
	go func() {
		done <- server.RunWithStartup(ctx, startup, (&net.ListenConfig{}).Listen, func(address net.Addr) { ready <- address })
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(20 * time.Second):
			t.Error("actual Run did not drain")
		}
	})
	var address net.Addr
	select {
	case address = <-ready:
	case err := <-done:
		t.Fatal("actual Run", err)
	case <-time.After(15 * time.Second):
		t.Fatal("actual Run readiness timeout")
	}
	dir := nha143Private(t)
	terminal := &nha143Terminal{}
	nha143Run(t, dir, terminal, ExitOK, "profiles", "add", "local", "--socket", filepath.Join(data, "admin.sock"))
	nha143Run(t, dir, &nha143Terminal{values: []string{nha143Password, nha143Password}}, ExitOK, "--profile", "local", "bootstrap", "admin", "--display-name", "Admin")
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, certificate, 0600); err != nil {
		t.Fatal(err)
	}
	base := "https://" + address.String()
	nha143Run(t, dir, terminal, ExitOK, "profiles", "add", "admin", "--url", base, "--ca", ca)
	nha143Login(t, dir, "admin")
	nha143Run(t, dir, terminal, ExitOK, "--profile", "admin", "sessions", "list")
	created := nha143Run(t, dir, &nha143Terminal{values: []string{nha143Password, nha143Password}}, ExitOK, "--profile", "admin", "accounts", "create", "user", "--display-name", "User", "--role", "user")
	var envelope struct {
		Result struct {
			Account struct {
				ID struct {
					Value string `json:"value"`
				} `json:"id"`
			} `json:"account"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(created), &envelope); err != nil || envelope.Result.Account.ID.Value == "" {
		t.Fatal("created account id", err)
	}
	nha143Run(t, dir, terminal, ExitOK, "--profile", "admin", "accounts", "invite", "guest", "--role", "user", "--expires", time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "--save-token", "guest")
	nha143Run(t, dir, terminal, ExitOK, "--profile", "admin", "integrations", "catalogue")
	nha143Run(t, dir, terminal, ExitOK, "--profile", "admin", "integrations", "schema", "workday")
	// Workday is a pure internal calendar; create it disabled and never configure
	// a network/physical driver while testing integration configuration CRUD.
	configFile := filepath.Join(dir, "workday.json")
	configuration := &wire.PublicConfiguration{SchemaVersion: "homeward.integration/workday/1", YamlSource: []byte("timezone: America/Toronto\n")}
	encoded, err := protojson.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configFile, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	readCAS := func() []string {
		t.Helper()
		result := nha143Run(t, dir, terminal, ExitOK, "--profile", "admin", "integrations", "list")
		var response struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal([]byte(result), &response); err != nil {
			t.Fatal(err)
		}
		list := &wire.ListInstancesResponse{}
		if err := protojson.Unmarshal(response.Result, list); err != nil {
			t.Fatal(err)
		}
		if list.Document == nil || list.Document.Document == nil || list.Document.Document.Version == nil || list.Document.Document.GetWorkspace() == nil {
			t.Fatal("physical CAS unavailable")
		}
		document := list.Document.Document
		return []string{"--revision", fmt.Sprint(document.Version.Revision), "--source-sha256", hex.EncodeToString(document.Version.SourceSha256), "--workspace-sha256", hex.EncodeToString(document.GetWorkspace().SourceSha256)}
	}
	mutate := func(command []string) {
		t.Helper()
		argv := append([]string{"--profile", "admin", "integrations"}, command...)
		argv = append(argv, readCAS()...)
		nha143Run(t, dir, terminal, ExitOK, argv...)
	}
	mutate([]string{"create", "workday", "homeward-workday-v1", "--config", configFile})
	nha143Run(t, dir, terminal, ExitOK, "--profile", "admin", "integrations", "get", "homeward-workday-v1")
	nha143Run(t, dir, terminal, ExitOK, "--profile", "admin", "integrations", "health", "homeward-workday-v1")
	configuration.YamlSource = []byte("timezone: America/Montreal\n")
	encoded, err = protojson.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configFile, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	mutate([]string{"update", "workday", "homeward-workday-v1", "--config", configFile})
	mutate([]string{"enable", "homeward-workday-v1"})
	mutate([]string{"disable", "homeward-workday-v1"})
	mutate([]string{"delete", "homeward-workday-v1"})
	nha143Run(t, dir, terminal, ExitOK, "profiles", "add", "user", "--url", base, "--ca", ca)
	nha143Run(t, dir, &nha143Terminal{values: []string{nha143Password}}, ExitOK, "--profile", "user", "login", "user")
	before, err := os.ReadFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	nha143Run(t, dir, terminal, ExitPermission, "--profile", "user", "accounts", "list")
	nha143Run(t, dir, terminal, ExitPermission, "--profile", "user", "integrations", "catalogue")
	after, err := os.ReadFile(workspace)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("denied user changed configuration", err)
	}
	nha143Run(t, dir, &nha143Terminal{values: []string{nha143Password, nha143Password}}, ExitOK, "--profile", "local", "accounts", "reset", envelope.Result.Account.ID.Value)
	nha143Run(t, dir, terminal, ExitAuthentication, "--profile", "user", "logout")
	nha143Run(t, dir, &nha143Terminal{values: []string{nha143Password}}, ExitOK, "--profile", "user", "login", "user")
	nha143Run(t, dir, terminal, ExitOK, "--profile", "user", "logout")
	nha143Run(t, dir, terminal, ExitOK, "--profile", "admin", "logout")
}

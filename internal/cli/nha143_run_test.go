package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	wire "github.com/damione1/Homeward/gen/home/api/v2"
	nativeauth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/server"
	"github.com/damione1/Homeward/internal/testpg"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
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
	decode := func(output string, message proto.Message) {
		t.Helper()
		var envelope struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal([]byte(output), &envelope); err != nil {
			t.Fatal(err)
		}
		if err := protojson.Unmarshal(envelope.Result, message); err != nil {
			t.Fatal(err)
		}
	}
	// The protected operator protocol retains its native account identity.
	// Public account names are never reverse-decoded into native recovery IDs.
	bootstrap := &nativeauth.BootstrapAdminResponse{}
	decode(nha143Run(t, dir, &nha143Terminal{values: []string{nha143Password, nha143Password}}, ExitOK, "--profile", "local", "bootstrap", "admin", "--display-name", "Admin"), bootstrap)
	if bootstrap.GetAccount().GetId().GetValue() == "" {
		t.Fatal("operator bootstrap account missing")
	}
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, certificate, 0600); err != nil {
		t.Fatal(err)
	}
	base := "https://" + address.String()
	nha143Run(t, dir, terminal, ExitOK, "profiles", "add", "admin", "--url", base, "--ca", ca)
	session := &wire.Session{}
	decode(nha143Run(t, dir, &nha143Terminal{values: []string{nha143Password}}, ExitOK, "--profile", "admin", "login", "admin"), session)
	sessionName, err := apitypes.ParseSessionName(session.Name)
	if err != nil {
		t.Fatal(err)
	}
	home := sessionName.Account().Home().String()
	nha143Run(t, dir, terminal, ExitOK, "--profile", "admin", "sessions", "list")
	created := nha143Run(t, dir, &nha143Terminal{values: []string{nha143Password, nha143Password}}, ExitOK, "--profile", "admin", "accounts", "create", "user", "--display-name", "User", "--role", "user")
	account := &wire.Account{}
	decode(created, account)
	accountName, err := apitypes.ParseAccountName(account.Name)
	if err != nil || accountName.Home().String() != home {
		t.Fatal("created account resource", err)
	}
	nha143Run(t, dir, terminal, ExitOK, "--profile", "admin", "accounts", "invite", "guest", "--role", "user", "--expires", time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "--save-token", "guest")
	var integrationType *wire.IntegrationType
	pageToken := ""
	for page := 0; page < 64; page++ {
		catalogue := &wire.ListIntegrationTypesResponse{}
		decode(nha143Run(t, dir, terminal, ExitOK, "--profile", "admin", "integrations", "catalogue", home, "--page-size", "100", "--page-token", pageToken), catalogue)
		for _, candidate := range catalogue.IntegrationTypes {
			if candidate.TypeKey == "workday" {
				integrationType = candidate
			}
		}
		pageToken = catalogue.NextPageToken
		if integrationType != nil || pageToken == "" {
			break
		}
	}
	if integrationType == nil {
		t.Fatal("workday descriptor missing from native catalogue")
	}
	nha143Run(t, dir, terminal, ExitOK, "--profile", "admin", "integrations", "schema", integrationType.Name)
	// Workday is a pure internal calendar; create it disabled and never configure
	// a network/physical driver while testing integration configuration CRUD.
	configFile := filepath.Join(dir, "workday.json")
	configuration := &wire.IntegrationConfiguration{SchemaVersion: integrationType.SchemaVersion, YamlSource: []byte("timezone: America/Toronto\n")}
	encoded, err := protojson.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configFile, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	nha143Run(t, dir, terminal, ExitOK, "--profile", "admin", "integrations", "create", integrationType.Name, "homeward-workday-v1", "--config", configFile)
	listed := &wire.ListIntegrationInstancesResponse{}
	decode(nha143Run(t, dir, terminal, ExitOK, "--profile", "admin", "integrations", "list", home), listed)
	if len(listed.IntegrationInstances) != 1 || listed.NextPageToken != "" {
		t.Fatal("expected the isolated instance")
	}
	instance := listed.IntegrationInstances[0]
	if instance.InstanceKey != "homeward-workday-v1" || instance.IntegrationType != integrationType.Name || instance.DesiredState != wire.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_DISABLED {
		t.Fatal("created instance differs from disabled native declaration")
	}
	readETag := func() string {
		t.Helper()
		current := &wire.IntegrationInstance{}
		decode(nha143Run(t, dir, terminal, ExitOK, "--profile", "admin", "integrations", "get", instance.Name), current)
		if current.Name != instance.Name || current.Etag == "" {
			t.Fatal("current instance etag unavailable")
		}
		return current.Etag
	}
	mutate := func(command []string) {
		t.Helper()
		argv := append([]string{"--profile", "admin", "integrations"}, command...)
		argv = append(argv, "--etag", readETag())
		nha143Run(t, dir, terminal, ExitOK, argv...)
	}
	nha143Run(t, dir, terminal, ExitOK, "--profile", "admin", "integrations", "health", instance.Name)
	configuration.YamlSource = []byte("timezone: America/Montreal\n")
	encoded, err = protojson.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configFile, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	mutate([]string{"update", instance.Name, "--config", configFile})
	mutate([]string{"enable", instance.Name})
	mutate([]string{"disable", instance.Name})
	mutate([]string{"delete", instance.Name})
	nha143Run(t, dir, terminal, ExitOK, "profiles", "add", "user", "--url", base, "--ca", ca)
	nha143Run(t, dir, &nha143Terminal{values: []string{nha143Password}}, ExitOK, "--profile", "user", "login", "user")
	before, err := os.ReadFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	nha143Run(t, dir, terminal, ExitPermission, "--profile", "user", "accounts", "list")
	nha143Run(t, dir, terminal, ExitPermission, "--profile", "user", "integrations", "catalogue", home)
	after, err := os.ReadFile(workspace)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("denied user changed configuration", err)
	}
	nha143Run(t, dir, &nha143Terminal{values: []string{nha143Password, nha143Password}}, ExitOK, "--profile", "local", "accounts", "reset", bootstrap.Account.Id.Value)
	nha143Run(t, dir, terminal, ExitAuthentication, "--profile", "admin", "logout")
	nha143Run(t, dir, &nha143Terminal{values: []string{nha143Password}}, ExitOK, "--profile", "admin", "login", "admin")
	nha143Run(t, dir, terminal, ExitOK, "--profile", "user", "logout")
	nha143Run(t, dir, terminal, ExitOK, "--profile", "admin", "logout")
}

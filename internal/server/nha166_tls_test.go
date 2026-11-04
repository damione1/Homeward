package server

import (
	"bytes"
	"context"
	"encoding/json"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	obsv2 "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/types/known/timestamppb"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
	"github.com/damione1/Homeward/internal/readonlyobservation"
	"github.com/damione1/Homeward/internal/testpg"
)

func TestNHA166TLSAttestationRequiresReadAuthorizationAndCollectorUsesOnlyReads(t *testing.T) {
	broker := testbroker.Start(t)
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	original := nha181Config(t, cluster.Port, data, "127.0.0.1:0", nha181TLSYAML())
	source, err := original.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	mqtt := nha166Config(t, cluster.Port, broker.Address)
	mqttSource, err := mqtt.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	mqttYAML := strings.SplitN(string(mqttSource.Bytes()), "mqtt:\n", 2)[1]
	cfg := nha029RunFixtureFromRaw(t, append(bytes.Replace(source.Bytes(), []byte("public:\n"), []byte("public:\n  mcp: {enabled: true}\n"), 1), []byte("mqtt:\n"+mqttYAML)...))
	nha166Provision(t, cfg)
	running := nha162StartDefaultRun(t, cfg.startup.WithMQTTReadOnlyObservation())
	client := nha162RunClient(t, roots, running.address)
	base := "https://" + running.address.String()
	observationAPI := apiv2connect.NewObservabilityServiceClient(client, base)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	denied, err := observationAPI.GetHealth(ctx, connect.NewRequest(&obsv2.GetHealthRequest{Name: "homes/synthetic/health"}))
	if err == nil || denied != nil {
		t.Fatal("anonymous attestation read admitted")
	}
	// The rejected HTTP response must not carry a guard or build attestation.
	if connectErr, ok := err.(*connect.Error); ok && connectErr.Meta().Get("Homeward-Observation-Profile") != "" {
		t.Fatal("attestation leaked before READ authorization")
	}
	const login = "NHA166 isolated observer admin"
	const password = "NHA166_SYNTHETIC_PASSWORD_CANARY"
	nha162RunBootstrap(t, ctx, data, login, password)
	tokens := nha162RunLogin(t, ctx, apiv2connect.NewAuthServiceClient(client, base), login, password)
	nha162RunPolicy(t, ctx, apiv2connect.NewAuthorizationPolicyServiceClient(client, base), nha203SessionAccount(t, tokens.Session).String(), tokens.AccessToken, `  - scope: {family: state_registry, operation: read}
    objects: {entities: {all: {capabilities: {all: {}}}}}
`)
	home := nha203ObservationHome(t, ctx, client, base, tokens.AccessToken)
	health, err := observationAPI.GetHealth(ctx, nha056SessionRequest(&obsv2.GetHealthRequest{Name: home + "/health"}, tokens.AccessToken))
	if err != nil {
		t.Fatal("authorized health read", err)
	}
	binding := readonlyobservation.Binding{Profile: health.Header().Get("Homeward-Observation-Profile"), Executable: health.Header().Get("Homeward-Executable-SHA256"), Manifest: health.Header().Get("Homeward-Manifest-SHA256"), Source: health.Header().Get("Homeward-Source-SHA256")}
	if binding.Profile != "mqtt-passive-readonly-v1" || len(binding.Executable) != 64 || len(binding.Manifest) != 64 || len(binding.Source) != 64 || health.Header().Get("Homeward-Command-Driver-Entries") != "0" {
		t.Fatal("missing immutable guard/executable/source evidence")
	}
	credential := filepath.Join(t.TempDir(), "credential.json")
	raw, _ := json.Marshal(map[string]string{"login_name": login, "password": password})
	if err := os.WriteFile(credential, raw, 0600); err != nil {
		t.Fatal(err)
	}
	configuration := readonlyobservation.Config{URL: base, CAFile: filepath.Join(data, nha181CertificateName), CredentialFile: credential, Instance: nha203ObservationInstanceFromAPI(t, ctx, client, base, tokens.AccessToken, home, "nha166-passive"), Expected: binding}
	collector, err := readonlyobservation.NewNativeSource(configuration)
	if err != nil {
		t.Fatal(err)
	}
	sample, err := collector.Read(ctx, 0)
	if err != nil {
		t.Fatal("actual HTTPS collector reads", err)
	}
	encoded, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	if sample.Epoch == "" || sample.ConfigRevision == 0 || sample.DriverEntries != 0 || strings.Contains(string(encoded), password) || strings.Contains(string(encoded), broker.Address) || strings.Contains(string(encoded), "nha068/state") {
		t.Fatal("collector lost evidence or retained private payload")
	}

	// Negative capability and MCP attempts target only this loopback broker/runtime.
	// ExecuteCapability uses a structurally valid request. Durable gate precedes any
	// target lookup, SQL admission or driver entry even for an admin session.
	actionAPI := apiv2connect.NewCapabilityServiceClient(client, base)
	action := &api.ExecuteCapabilityRequest{Name: "homes/nha166/entities/switch-nha166-double/capabilities/power", Etag: `W/"nha166-double"`, RequestId: "12345678-1234-4123-8123-123456789abc", ExpireTime: timestamppb.New(time.Now().Add(time.Minute)), Command: &api.ExecuteCapabilityRequest_Power{Power: &api.SetPowerCommand{Value: api.PowerValue_POWER_VALUE_ON}}}
	if _, err := actionAPI.ExecuteCapability(ctx, nha056SessionRequest(action, tokens.AccessToken)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("native action did not refuse before driver", err)
	}
	keyClient := apiv2connect.NewMcpKeyServiceClient(client, base)
	key, err := keyClient.IssueMcpKey(ctx, nha056SessionRequest(&api.IssueMcpKeyRequest{Parent: nha203SessionAccount(t, tokens.Session).String(), ClientName: "nha166-double", Scopes: []*api.Scope{{Family: api.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, Operation: api.ScopeOperation_SCOPE_OPERATION_READ}, {Family: api.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY, Operation: api.ScopeOperation_SCOPE_OPERATION_READ}}}, tokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	fixture := &nha052Fixture{ctx: ctx, base: base, http: client}
	mcpSession, err := fixture.connect(key.Msg.Secret)
	if err != nil {
		t.Fatal("readonly MCP initialization", err)
	}
	defer mcpSession.Close()
	tools, err := mcpSession.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil || len(tools.Tools) == 0 {
		t.Fatal("MCP read catalogue unavailable", err)
	}
	for _, tool := range tools.Tools {
		if strings.Contains(tool.Name, "action") || strings.Contains(tool.Name, "activate") || strings.Contains(tool.Name, "write") {
			t.Fatal("MCP exposed an action tool", tool.Name)
		}
	}
	nha052Call(t, ctx, mcpSession, "homeward.get_health", map[string]any{"name": home + "/health"}, nil)
	if result, err := mcpSession.CallTool(ctx, &mcp.CallToolParams{Name: "homeward.submit_action", Arguments: map[string]any{}}); err == nil && (result == nil || !result.IsError) {
		t.Fatal("MCP accepted nonexistent action tool")
	}
	final, err := observationAPI.GetHealth(ctx, nha056SessionRequest(&obsv2.GetHealthRequest{Name: home + "/health"}, tokens.AccessToken))
	if err != nil || final.Header().Get("Homeward-Command-Driver-Entries") != "0" {
		t.Fatal("API/MCP negative entered physical driver", err)
	}
	wrong := configuration
	wrong.Expected.Source = strings.Repeat("0", 64)
	rejected, err := readonlyobservation.NewNativeSource(wrong)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rejected.Read(ctx, 0); err == nil {
		t.Fatal("collector admitted changed frozen source")
	}
}

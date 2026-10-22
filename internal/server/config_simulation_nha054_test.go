package server

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"github.com/damione1/Homeward/gen/home/configuration/v1/configurationv1connect"
	plan "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	wire "github.com/damione1/Homeward/gen/home/configurationsimulation/v1"
	"github.com/damione1/Homeward/gen/home/configurationsimulation/v1/configurationsimulationv1connect"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/testpg"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func nha054StartTLS(t *testing.T) (nha149TLSFixture, configurationsimulationv1connect.ConfigurationSimulationServiceClient) {
	t.Helper()
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	cfg := nha181Config(t, cluster.Port, data, "127.0.0.1:0", nha181TLSYAML())
	source, err := cfg.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "configuration.yaml")
	if err := os.WriteFile(path, source.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	run := nha162StartDefaultRun(t, startup)
	client := nha162RunClient(t, roots, run.address)
	base := "https://" + run.address.String()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	const password = "NHA054 isolated synthetic password"
	nha162RunBootstrap(t, ctx, data, "NHA054 admin", password)
	authentication := authv1connect.NewAuthServiceClient(client, base)
	tokens := nha162RunLogin(t, ctx, authentication, "NHA054 admin", password)
	f := nha149TLSFixture{api: configurationv1connect.NewConfigurationServiceClient(client, base), authentication: authentication, accounts: authv1connect.NewAccountServiceClient(client, base), tokens: tokens, token: tokens.AccessToken, path: path, raw: source.Bytes(), run: run, ctx: ctx}
	return f, configurationsimulationv1connect.NewConfigurationSimulationServiceClient(client, base)
}
func nha054WireRequest() *wire.SimulatePlanRequest {
	state := "off"
	return &wire.SimulatePlanRequest{Candidate: nha053WireCandidate(plan.SourceFamily_SOURCE_FAMILY_SCRIPT, "script.simulation", []byte("sequence:\n- if:\n  - condition: state\n    entity_id: switch.fixture\n    state: 'off'\n  then:\n  - wait_for_trigger:\n    - trigger: event\n      event_type: nha054_release\n    timeout: {seconds: 2}\n    continue_on_timeout: true\n  - action: light.turn_on\n    target: {entity_id: light.fixture}\n    data: {brightness_pct: 25}\n")), Fixtures: []*wire.Fixture{{Alias: "switch.fixture", State: &state, Supports: []wire.Support{wire.Support_SUPPORT_OBSERVE_STATE}}, {Alias: "light.fixture", State: &state, Supports: []wire.Support{wire.Support_SUPPORT_LIGHT_TURN_ON, wire.Support_SUPPORT_LIGHT_BRIGHTNESS_ABSOLUTE}}}, EventTypes: []string{"nha054_release"}, Now: timestamppb.New(time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)), TimeZone: "UTC", VirtualLimitMillis: 60000}
}
func TestNHA054ActualRunTLSDeterministicSimulationHasNoProductionMutation(t *testing.T) {
	f, client := nha054StartTLS(t)
	before := f.read(t)
	info, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	request := nha054WireRequest()
	first, err := client.SimulatePlan(f.ctx, nha056SessionRequest(request, f.token))
	if err != nil {
		t.Fatal("actual TLS simulation", err)
	}
	if err := contracts.AdmitConfigurationSimulationResponse(first.Msg); err != nil {
		t.Fatal(err)
	}
	report := first.Msg.Report
	if !report.Simulated || report.HardwareValidated || report.Status != wire.Status_STATUS_COMPLETED || len(report.Intents) != 1 || len(report.Nodes) == 0 || report.ElapsedMillis != 2000 || report.Intents[0].GetLight().GetBrightnessPercent() != 25 || report.Intents[0].Position.Line != 12 || report.Intents[0].Position.File != "configuration.yaml" || !report.Intents[0].At.AsTime().Equal(request.Now.AsTime().Add(2*time.Second)) {
		t.Fatalf("TLS report lost fixture/source intent: %v", report)
	}
	second, err := client.SimulatePlan(f.ctx, nha056SessionRequest(request, f.token))
	if err != nil || !proto.Equal(first.Msg, second.Msg) {
		t.Fatal("same fixture produced another report", err)
	}
	partial := nha054WireRequest()
	partial.Candidate = nha053WireCandidate(plan.SourceFamily_SOURCE_FAMILY_SCENE, "scene.simulation", []byte("id: simulation\nname: Simulation\nentities:\n  light.fixture:\n    state: 'on'\n    brightness: 64\n    color_mode: brightness\n  switch.fixture:\n    state: 'on'\n"))
	partial.Fixtures[0].RefuseIntent = true
	result, err := client.SimulatePlan(f.ctx, nha056SessionRequest(partial, f.token))
	if err != nil || result.Msg.Report.Status != wire.Status_STATUS_PARTIAL || len(result.Msg.Report.Intents) != 2 || result.Msg.Report.Intents[0].GetLight().GetBrightness_255() != 64 {
		t.Fatal("actual scene partial result lost", err)
	}
	malicious := nha054WireRequest()
	malicious.Candidate = nha053WireCandidate(plan.SourceFamily_SOURCE_FAMILY_SCRIPT, "script.simulation", []byte("sequence:\n- action: rest_command.NHA054_PRIVATE_CANARY\n  data: {url: 'http://127.0.0.1:9/NO_NETWORK'}\n"))
	refused, err := client.SimulatePlan(f.ctx, nha056SessionRequest(malicious, f.token))
	if err != nil || refused.Msg.Report.Status != wire.Status_STATUS_FAILED || len(refused.Msg.Report.Intents) != 0 || len(refused.Msg.Report.Diagnostics) == 0 {
		t.Fatal("extension did not fail closed", err)
	}
	encoded, err := proto.Marshal(refused.Msg)
	if err != nil || bytes.Contains(encoded, []byte("NHA054_PRIVATE_CANARY")) || bytes.Contains(encoded, []byte("NO_NETWORK")) {
		t.Fatal("source/error content leaked")
	}
	(nha053TLSFixture{nha149TLSFixture: f}).unchanged(t, before, f.raw, info)
	if _, err := client.SimulatePlan(f.ctx, nha056SessionRequest(request, "invalid-synthetic-bearer")); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("invalid session reached simulator", err)
	}
	const password = "NHA054 isolated ordinary password"
	if _, err := f.accounts.CreateAccount(f.ctx, nha056SessionRequest(&auth.CreateAccountRequest{LoginName: "NHA054 user", DisplayName: "NHA054 user", Role: auth.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: password}, f.token)); err != nil {
		t.Fatal(err)
	}
	user := nha162RunLogin(t, f.ctx, f.authentication, "NHA054 user", password)
	if _, err := client.SimulatePlan(f.ctx, nha056SessionRequest(request, user.AccessToken)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("ordinary user reached admin simulator", err)
	}
	if _, err := f.authentication.RevokeSession(f.ctx, nha056SessionRequest(&auth.RevokeSessionRequest{SessionId: f.tokens.Session.Id}, f.token)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SimulatePlan(f.ctx, nha056SessionRequest(request, f.token)); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("revoked session reused simulator", err)
	}
}

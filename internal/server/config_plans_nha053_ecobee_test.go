package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"github.com/damione1/Homeward/gen/home/configuration/v1/configurationv1connect"
	wire "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	"github.com/damione1/Homeward/gen/home/configurationplan/v1/configurationplanv1connect"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/testpg"
	"google.golang.org/protobuf/proto"
)

func nha053StartEcobeeScopeTLS(t *testing.T) nha053TLSFixture {
	t.Helper()
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	cfg := nha181Config(t, cluster.Port, data, "127.0.0.1:0", nha181TLSYAML()+"scope_revision: ecobee_mvp_v1\n")
	source, e := cfg.startup.CaptureWorkspace()
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "configuration.yaml")
	if e := os.WriteFile(path, source.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	startup, e := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029TestEnv)
	if e != nil {
		t.Fatal(e)
	}
	run := nha162StartDefaultRun(t, startup)
	client := nha162RunClient(t, roots, run.address)
	base := "https://" + run.address.String()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	const password = "NHA053 synthetic isolated password"
	nha162RunBootstrap(t, ctx, data, "NHA053 admin", password)
	authentication := authv1connect.NewAuthServiceClient(client, base)
	tokens := nha162RunLogin(t, ctx, authentication, "NHA053 admin", password)
	return nha053TLSFixture{nha149TLSFixture: nha149TLSFixture{api: configurationv1connect.NewConfigurationServiceClient(client, base), authentication: authentication, accounts: authv1connect.NewAccountServiceClient(client, base), tokens: tokens, token: tokens.AccessToken, path: path, raw: source.Bytes(), run: run, ctx: ctx}, plans: configurationplanv1connect.NewConfigurationPlanServiceClient(client, base), policies: authv1connect.NewAuthorizationPolicyServiceClient(client, base)}
}

func TestNHA053ActualRunTLSEcobeePreviewUsesActiveScopeAndNeverDials(t *testing.T) {
	for _, amended := range []bool{false, true} {
		t.Run(fmt.Sprintf("amended-%t", amended), func(t *testing.T) {
			var f nha053TLSFixture
			if amended {
				f = nha053StartEcobeeScopeTLS(t)
			} else {
				f = nha053StartTLS(t)
			}
			before := f.read(t)
			raw, err := os.ReadFile(f.path)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(f.path)
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			source := fmt.Sprintf("ecobee:\n  instance: nha053-private-ecobee\n  address: %q\n  accessory_id: 'CA:FE:00:11:22:33'\n  credential_secret: nha053-absent-pairing\n", listener.Addr().String())
			enabled := nha053WireCandidate(wire.SourceFamily_SOURCE_FAMILY_OPERATIONAL, "operational.main", []byte(source))
			disabled := nha053WireCandidate(wire.SourceFamily_SOURCE_FAMILY_OPERATIONAL, "operational.main", []byte(source+"disabled_integrations: [nha053-private-ecobee]\n"))
			workspace := before.Document.GetWorkspace()
			if workspace == nil {
				t.Fatal("fixture has no observed workspace")
			}
			assertRejected := func(candidate *wire.Candidate, plan *wire.Plan, diagnostic wire.DiagnosticCode) {
				t.Helper()
				digest := sha256.Sum256(candidate.Files[0].ExactSource)
				if plan == nil || plan.Base == nil || plan.State != wire.PlanState_PLAN_STATE_REJECTED || plan.Author != f.tokens.Session.AccountId.Value || plan.Family != wire.SourceFamily_SOURCE_FAMILY_OPERATIONAL || plan.Identity != "operational.main" || plan.SchemaVersion != "homeward.operational/1" || !bytes.Equal(plan.SourceSha256, digest[:]) || plan.SemanticSha256 != nil || len(plan.Changes) != 0 || plan.Effect != wire.Effect_EFFECT_CONFIGURATION || len(plan.Diagnostics) != 1 || plan.Diagnostics[0] != diagnostic || len(plan.Receipt) < 44 || len(plan.Receipt) > 4200 || plan.ApplicationAvailable {
					t.Fatal("rejected candidate lost its signed diagnostic metadata or became applicable")
				}
				if plan.Base.Kind != wire.BaseKind_BASE_KIND_ACTIVE_SQL || plan.Base.GetRevision() != before.Document.Version.Revision || !bytes.Equal(plan.Base.SourceSha256, before.Document.Version.SourceSha256) || plan.GenerationRevision != before.ActiveGeneration.Revision || !bytes.Equal(plan.ManifestSha256, before.ActiveGeneration.ManifestSha256) || !bytes.Equal(plan.ObservedWorkspaceSha256, workspace.SourceSha256) {
					t.Fatal("rejected candidate changed active base, generation, manifest or workspace provenance")
				}
				encoded, err := proto.Marshal(plan)
				if err != nil {
					t.Fatal(err)
				}
				for _, private := range []string{"nha053-private-ecobee", "nha053-absent-pairing", "CA:FE:00:11:22:33", listener.Addr().String()} {
					if bytes.Contains(encoded, []byte(private)) {
						t.Fatal("private Ecobee settings leaked into rejected response/receipt")
					}
				}
				fresh, err := f.plans.RevalidatePlan(f.ctx, nha056SessionRequest(&wire.RevalidatePlanRequest{Candidate: candidate, Receipt: plan.Receipt}, f.token))
				if err != nil || fresh.Msg.Plan.State != wire.PlanState_PLAN_STATE_REJECTED || fresh.Msg.Plan.ApplicationAvailable || fresh.Msg.Plan.SemanticSha256 != nil || len(fresh.Msg.Plan.Changes) != 0 || len(fresh.Msg.Plan.Diagnostics) != 1 || fresh.Msg.Plan.Diagnostics[0] != diagnostic || !bytes.Equal(fresh.Msg.Plan.SourceSha256, digest[:]) || fresh.Msg.Plan.Receipt == "" {
					t.Fatal("signed rejection did not revalidate as a non-applicable rejection", err)
				}
				f.unchanged(t, before, raw, info)
			}
			for _, candidate := range []*wire.Candidate{enabled, disabled} {
				plan := f.build(t, candidate)
				if !amended {
					assertRejected(candidate, plan, wire.DiagnosticCode_DIAGNOSTIC_CODE_EXCLUDED_REFERENCE)
					continue
				}
				if plan.State != wire.PlanState_PLAN_STATE_ADMITTED || plan.Effect != wire.Effect_EFFECT_CONFIGURATION || plan.ApplicationAvailable {
					t.Fatal("amended inert Ecobee preview refused or acquired application authority")
				}
				var ecobeeField bool
				for _, change := range plan.Changes {
					if change.Field == "ecobee" {
						ecobeeField = change.Effect == wire.Effect_EFFECT_CONFIGURATION
					}
				}
				if !ecobeeField {
					t.Fatal("admitted Ecobee lost its public semantic effect")
				}
				encoded, err := proto.Marshal(plan)
				if err != nil {
					t.Fatal(err)
				}
				for _, private := range []string{"nha053-private-ecobee", "nha053-absent-pairing", "CA:FE:00:11:22:33", listener.Addr().String()} {
					if bytes.Contains(encoded, []byte(private)) {
						t.Fatal("private Ecobee settings leaked into response/receipt")
					}
				}
				fresh, err := f.plans.RevalidatePlan(f.ctx, nha056SessionRequest(&wire.RevalidatePlanRequest{Candidate: candidate, Receipt: plan.Receipt}, f.token))
				if err != nil || fresh.Msg.Plan.State != wire.PlanState_PLAN_STATE_ADMITTED {
					t.Fatal("unchanged inert Ecobee plan could not revalidate", err)
				}
			}
			if !amended {
				selector := nha053WireCandidate(wire.SourceFamily_SOURCE_FAMILY_OPERATIONAL, "operational.main", []byte("scope_revision: ecobee_mvp_v1\n"+source))
				assertRejected(selector, f.build(t, selector), wire.DiagnosticCode_DIAGNOSTIC_CODE_EXCLUDED_REFERENCE)
			} else {
				invalid := nha053WireCandidate(wire.SourceFamily_SOURCE_FAMILY_OPERATIONAL, "operational.main", []byte("ecobee: {instance: nha053-private-ecobee, address: 'localhost:12345', accessory_id: fixture, credential_secret: absent}\ndisabled_integrations: [nha053-private-ecobee]\n"))
				assertRejected(invalid, f.build(t, invalid), wire.DiagnosticCode_DIAGNOSTIC_CODE_INVALID_SCHEMA)
			}
			if err := listener.SetDeadline(time.Now().Add(25 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			connection, err := listener.AcceptTCP()
			if connection != nil {
				connection.Close()
				t.Fatal("preview dialed the synthetic HAP endpoint")
			}
			if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
				t.Fatal("loopback absence witness failed", err)
			}
			f.unchanged(t, before, raw, info)
		})
	}
}

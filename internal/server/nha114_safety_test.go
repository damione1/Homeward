package server

import (
	"bytes"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	configuration "github.com/damione1/Homeward/gen/home/configuration/v1"
	planwire "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/scope"
)

func TestNHA114LifecycleAndUnboundManifestTargetProduceNoCommands(t *testing.T) {
	f := nha114Start(t)
	f.installAutomation(t)
	f.noCommands(t, 0)
	f.reconnectMQTT(t)
	f.noCommands(t, 0)
	before := f.read(t)
	raw := append(bytes.Clone(f.raw), []byte("# NHA114 hot publication without action\n")...)
	candidate := nha053WireCandidate(planwire.SourceFamily_SOURCE_FAMILY_OPERATIONAL, "operational.main", raw)
	plan := f.build(t, candidate)
	if plan.State != planwire.PlanState_PLAN_STATE_ADMITTED || plan.Receipt == "" {
		t.Fatalf("hot refresh plan was not admitted: state=%s diagnostics=%v", plan.State, plan.Diagnostics)
	}
	r, err := f.apply.Apply(f.ctx, nha056SessionRequest(nha055Request(before, candidate, &plan.Receipt), f.token))
	if err != nil || r.Msg.Activation.Outcome != configuration.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED {
		t.Fatal("actual hot refresh", err)
	}
	f.refreshPublicView(t)
	f.waitBindings(t)
	f.noCommands(t, 0)
	// This is a frozen manifest identity deliberately left unbound. It is not
	// a renamed synthetic route and the authenticated policy permits actions.
	const foreign = "switch.arc_soundbar_surround_enabled"
	manifest, err := registry.ParseManifest(scope.ManifestV1())
	if err != nil {
		t.Fatal(err)
	}
	alias, err := registry.ParseHAAlias(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if source, included := manifest.SourceFor(alias); !included {
		t.Fatal("negative target is outside frozen manifest")
	} else if _, valid := source.Value(); !valid {
		t.Fatal("negative target has no manifest provenance")
	}
	// The actual route resolver has no entity for this admitted alias. Its
	// failure precedes construction of a permission object, and is intentionally
	// hidden behind the public permission-denied boundary.
	if _, err := f.actual.sequence.ResolveCommandAlias(alias); !errors.Is(err, integrations.ErrCommandRouteUnavailable) {
		t.Fatal("negative target unexpectedly has a route", err)
	}
	policy, err := f.policies.GetPolicy(f.ctx, nha056SessionRequest(&auth.GetPolicyRequest{AccountId: f.tokens.Session.AccountId}, f.token))
	if err != nil || policy.Msg.GetPresent() == nil || !bytes.Equal(policy.Msg.GetPresent().YamlSource, f.policySource) {
		t.Fatal("public policy no longer proves actions-all permission", err)
	}
	command := f.command(t, "nha114-unbound-foreign-source", foreign, true)
	if err := contracts.AdmitSubmitActionRequest(&actions.SubmitActionRequest{Command: command}); err != nil {
		t.Fatal("negative command is malformed", err)
	}
	_, err = f.actions.SubmitAction(f.ctx, nha056SessionRequest(&actions.SubmitActionRequest{Command: command}, f.token))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("unbound manifest resolution did not preserve its exact public denial", err)
	}
	f.noCommands(t, 0)
	if err := f.run.join(t); err != nil {
		t.Fatal("stop before same-catalog restart", err)
	}
	f.broker.WaitSessionRemoved(t, f.clientID)
	f.noCommands(t, 0)
	f.startRun(t)
	f.login(t)
	f.refreshClients(t)
	f.waitBindings(t)
	f.noCommands(t, 0)
	metrics, err := f.observation.GetMetrics(f.ctx, nha056SessionRequest(&obs.GetMetricsRequest{}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	handle := metrics.Msg.Metrics.Runs.GetInstalled().GetController()
	if handle == nil {
		t.Fatal("published automation was not restored")
	}
	trace, err := f.observation.ReadRunTrace(f.ctx, nha056SessionRequest(&obs.ReadRunTraceRequest{Controller: handle, RunId: 1}, f.token))
	if err != nil || trace.Msg.GetNotRetained() == nil {
		t.Fatal("startup/reconnect/hot refresh fabricated an automation occurrence", err)
	}
	f.noCommands(t, 0)
	f.drainAndCount(t, 0)
}

package server

import (
	"bytes"
	"errors"
	"testing"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/scope"
	"strings"
)

func TestNHA114LifecycleAndUnboundManifestTargetProduceNoCommands(t *testing.T) {
	f := nha114Start(t)
	f.installAutomation(t)
	f.noCommands(t, 0)
	f.reconnectMQTT(t)
	f.noCommands(t, 0)
	before := f.read(t)
	raw := append(bytes.Clone(f.raw), []byte("# NHA114 hot publication without action\n")...)
	candidate := nha053WireCandidate(api.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL, "operational.main", raw)
	plan := f.build(t, candidate)
	if plan.State != api.ConfigurationPlanState_CONFIGURATION_PLAN_STATE_ADMITTED || plan.Receipt == "" {
		t.Fatalf("hot refresh plan was not admitted: state=%s diagnostics=%v", plan.State, plan.Diagnostics)
	}
	r, err := f.apply.ApplyConfigurationPlan(f.ctx, nha056SessionRequest(nha055Request(before, candidate, plan), f.token))
	if err != nil || !r.Msg.Done || r.Msg.GetError() != nil || f.read(t).ActiveGeneration.Revision != before.ActiveGeneration.Revision+1 {
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
	// unavailable in the native resolver. The installation administrator may
	// receive canonical NOT_FOUND; an unprivileged caller remains indistinguishably denied.
	if _, err := f.actual.sequence.ResolveCommandAlias(alias); !errors.Is(err, integrations.ErrCommandRouteUnavailable) {
		t.Fatal("negative target unexpectedly has a route", err)
	}
	policy, err := f.policies.GetAuthorizationPolicy(f.ctx, nha056SessionRequest(&api.GetAuthorizationPolicyRequest{Name: nha203SessionAccount(t, f.tokens.Session).String() + "/authorizationPolicy"}, f.token))
	if err != nil || policy.Msg == nil || !bytes.Equal(policy.Msg.YamlSource, f.policySource) {
		t.Fatal("public policy no longer proves actions-all permission", err)
	}
	// Aliases no longer form public command targets. Exercise the same denied
	// boundary with a syntactically valid canonical identity lacking a native binding.
	command := f.command(t, "nha114-unbound-foreign-source", nha114Aliases[0], true)
	command.Name = f.entityName(t, "nha114-unbound-manifest-target") + "/capabilities/" + command.Name[strings.LastIndex(command.Name, "/")+1:]
	if _, err := apitypes.ParseCapabilityName(command.Name); err != nil {
		t.Fatal("negative canonical target malformed", err)
	}
	_, err = f.capabilities.ExecuteCapability(f.ctx, nha056SessionRequest(command, f.token))
	if connect.CodeOf(err) != connect.CodeNotFound || nha200Info(t, err).Reason != "RESOURCE_NOT_FOUND" {
		t.Fatal("installation administrator lost canonical absence diagnostic", err)
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
	metrics, err := f.observation.GetMetrics(f.ctx, nha056SessionRequest(&api.GetMetricsRequest{Name: f.home + "/metrics"}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	handle := metrics.Msg.Runs.GetInstalled().GetController()
	if handle == "" {
		t.Fatal("published automation was not restored")
	}
	trace, err := f.observation.QueryRunTrace(f.ctx, nha056SessionRequest(&api.QueryRunTraceRequest{Parent: nha203ObservationRun(t, handle, 1)}, f.token))
	if connect.CodeOf(err) != connect.CodeNotFound || trace != nil {
		t.Fatal("startup/reconnect/hot refresh fabricated an automation occurrence", err)
	}
	f.noCommands(t, 0)
	f.drainAndCount(t, 0)
}

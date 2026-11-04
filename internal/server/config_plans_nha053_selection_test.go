package server

import (
	"bytes"
	"connectrpc.com/connect"
	"crypto/sha256"
	"errors"
	"fmt"
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"google.golang.org/protobuf/proto"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNHA053ActualRunTLSIntegrationSelectionIsSemanticPrivateAndInert(t *testing.T) {
	f := nha053StartTLS(t)
	before := f.read(t)
	raw, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	const template = "external_workers:\n - instance: nha053-private-selection-worker\n   executable: /synthetic-nha053-never-launch\n   bindings:\n    - device: synthetic-device\n      entity: synthetic-entity\n      alias: light.basement_lights\n"
	executable := filepath.Join(t.TempDir(), "synthetic-worker")
	marker := filepath.Join(t.TempDir(), "MUST_NOT_EXIST")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 91\n"), 0700); err != nil {
		t.Fatal(err)
	}
	source := strings.Replace(template, "/synthetic-nha053-never-launch", fmt.Sprintf("%q", executable), 1)
	enabled := nha053WireCandidate(wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL, "operational.main", []byte(source))
	disabled := nha053WireCandidate(wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL, "operational.main", []byte(source+"disabled_integrations: [nha053-private-selection-worker]\n"))
	a, b := f.build(t, enabled), f.build(t, disabled)
	if a.State != wire.ConfigurationPlanState_CONFIGURATION_PLAN_STATE_ADMITTED || b.State != wire.ConfigurationPlanState_CONFIGURATION_PLAN_STATE_ADMITTED || bytes.Equal(a.SourceSha256, b.SourceSha256) || bytes.Equal(a.SemanticSha256, b.SemanticSha256) {
		t.Fatal("real selector did not change admitted source/semantic proof")
	}
	var selection bool
	for _, change := range b.Changes {
		if change.Field == "disabled_integrations" {
			selection = change.Effect == wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_CONFIGURATION
		}
	}
	if !selection || b.ApplicationAvailable {
		t.Fatal("selection lost public effect or invented activation")
	}
	encoded, err := proto.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"nha053-private-selection-worker", executable, "synthetic-device", "synthetic-entity"} {
		if bytes.Contains(encoded, []byte(private)) {
			t.Fatal("selection private declaration leaked")
		}
	}
	fresh, err := f.plans.RevalidateConfigurationPlan(f.ctx, nha056SessionRequest(&wire.RevalidateConfigurationPlanRequest{Candidate: disabled, Name: b.Name, Etag: b.Etag}, f.token))
	if err != nil || fresh.Msg.State != wire.ConfigurationPlanState_CONFIGURATION_PLAN_STATE_ADMITTED {
		t.Fatal("unchanged selected plan became stale", err)
	}
	b = fresh.Msg
	if _, err := f.plans.RevalidateConfigurationPlan(f.ctx, nha056SessionRequest(&wire.RevalidateConfigurationPlanRequest{Candidate: enabled, Name: b.Name, Etag: b.Etag}, f.token)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatal("selection receipt authenticated another operational selection", err)
	}
	excluded := strings.Replace(source, "light.basement_lights", "light.excluded_outside_manifest", 1) + "disabled_integrations: [nha053-private-selection-worker]\n"
	refused := f.build(t, nha053WireCandidate(wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL, "operational.main", []byte(excluded)))
	digest := sha256.Sum256([]byte(excluded))
	if refused.State != wire.ConfigurationPlanState_CONFIGURATION_PLAN_STATE_REJECTED || len(refused.Diagnostics) != 1 || refused.Diagnostics[0] != wire.ConfigurationPlanDiagnosticCode_CONFIGURATION_PLAN_DIAGNOSTIC_CODE_EXCLUDED_REFERENCE || refused.SemanticSha256 != nil || len(refused.Changes) != 0 || refused.Effect != wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_CONFIGURATION || !bytes.Equal(refused.SourceSha256, digest[:]) || len(refused.Receipt) < 44 || len(refused.Receipt) > 4200 || refused.ApplicationAvailable {
		t.Fatal("disabled excluded binding lost signed rejection metadata or became applicable")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preview launched selected worker", err)
	}
	f.unchanged(t, before, raw, info)
}

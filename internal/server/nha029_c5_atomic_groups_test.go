package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
)

// All subqueries share one independent PostgreSQL statement snapshot. The
// preceding C5 digest includes sources/manifests/membership/active/audits and
// binding/alias rows; these are the remaining tables written by this candidate.
const nha029C5AtomicDigestSQL = `SELECT sha256((` + nha029C5PrecommitDigestSQL + `) ||
 convert_to(jsonb_build_array(
 (SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb) FROM public.secret_salts x),
 (SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb) FROM public.registry_devices x),
 (SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb) FROM public.registry_device_sources x),
 (SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb) FROM public.registry_entities x),
 (SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb) FROM public.authorization_policy_active x),
 (SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb) FROM public.config_bootstrap_active x),
 (SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb) FROM public.command_intents x)
 )::text,'UTF8'))`

// These are the actual statements used by writePreparedActivation, including
// distinct source variants on shared tables. Operational/policy documents
// already exist: their ON CONFLICT INSERT is not a successful new row write;
// their actual revision UPDATE is separately observed instead.
func TestNHA029C5OwnerRollsBackEveryAtomicWriteGroup(t *testing.T) {
	for _, point := range []struct {
		name, table, event, when string
	}{
		{"operational_revision", "config_documents", "UPDATE", "NEW.kind::text='operational_yaml'"},
		{"operational_salt", "secret_salts", "INSERT", "true"},
		{"operational_version", "config_versions", "INSERT", "NEW.source_representation::text='protected_operational_v1'"},
		{"declarative_document", "config_documents", "INSERT", "NEW.kind::text='yaml_source'"},
		{"declarative_revision", "config_documents", "UPDATE", "NEW.kind::text='yaml_source'"},
		{"declarative_version", "config_versions", "INSERT", "NEW.schema_version::text='ha-declarative-1'"},
		{"generation_metadata", "config_generations", "INSERT", "true"},
		{"manifest_membership", "config_generation_entries", "INSERT", "NEW.document_id='operational.main'"},
		{"manifest_seal", "config_generations", "UPDATE", "NEW.sealed AND NOT OLD.sealed"},
		{"registry_device", "registry_devices", "INSERT", "true"},
		{"registry_device_source", "registry_device_sources", "INSERT", "true"},
		{"registry_entity", "registry_entities", "INSERT", "true"},
		{"registry_binding", "registry_source_bindings", "INSERT", "true"},
		{"registry_alias", "registry_aliases", "INSERT", "true"},
		{"policy_revision", "config_documents", "UPDATE", "NEW.kind::text='authorization_policy'"},
		{"policy_version", "config_versions", "INSERT", "NEW.schema_version::text='homeward.authorization/1'"},
		{"policy_active", "authorization_policy_active", "UPDATE", "true"},
		{"active_generation", "config_active", "UPDATE", "true"},
		{"attempt_audit", "config_activation_attempts", "INSERT", "true"},
		{"policy_change_audit", "config_activation_policy_changes", "INSERT", "true"},
	} {
		t.Run(point.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			r := nha029StartOwner(t, nha029OwnerParent, nha029OwnerChild, nil)
			r.waitBoth(t)
			original := r.composition.scripts.interpreter
			ids := nha029C5HistoryWaits(t, original)
			waits := nha029C5PrecommitWaitingNodes(t, original, ids)
			controller := r.controller()
			a := nha029C5HistoryActive(t, ctx, r)
			stamp, err := r.owner.CurrentConfig(ctx)
			if err != nil {
				t.Fatal(err)
			}
			policy, err := r.store.GetAuthorizationPolicy(ctx, r.environment.account, r.environment.account)
			if err != nil {
				t.Fatal(err)
			}
			oldVersion, err := policy.Version()
			if err != nil {
				t.Fatal(err)
			}
			expectedPolicy, err := authorization.ExpectVersion(oldVersion)
			if err != nil {
				t.Fatal(err)
			}
			policySource, err := authorization.EmptySource(r.environment.account)
			if err != nil {
				t.Fatal(err)
			}
			change, err := configactivation.NewPolicyChange(policySource, expectedPolicy)
			if err != nil {
				t.Fatal(err)
			}
			declarative := []byte("group:\n  c5_atomic:\n    name: Atomic SQL group\n    entities: [input_boolean.away_mode]\ninput_boolean:\n  away_mode:\n    name: Atomic SQL helper\n    initial: false\n")
			file, err := parser.NewSourceFile("c5-atomic-definitions.yaml", declarative)
			if err != nil {
				t.Fatal(err)
			}
			candidate := nha029C5RestoreCompile(t, parser.SyntaxProfileDeclarative, file.Path(), []parser.SourceFile{file})
			document, err := storage.NewConfigDocumentID("nha029.c5.atomic.declarative")
			if err != nil {
				t.Fatal(err)
			}
			binding, err := storage.NewConfigDocumentBinding(document, file.Path())
			if err != nil {
				t.Fatal(err)
			}
			definitions, err := configactivation.ReplaceDefinitions(candidate, []storage.ConfigDocumentBinding{binding})
			if err != nil {
				t.Fatal(err)
			}
			instance, err := domain.ParseIntegrationInstanceID("nha029-c5-atomic-pilot")
			if err != nil {
				t.Fatal(err)
			}
			key, err := integrations.NewSourceKey("nha029-c5-atomic-device", "occupancy")
			if err != nil {
				t.Fatal(err)
			}
			author, err := storage.NewConfigAuthor("nha029:c5-atomic-operator")
			if err != nil {
				t.Fatal(err)
			}
			source, err := storage.NewConfigSource("nha029:c5-atomic-workspace")
			if err != nil {
				t.Fatal(err)
			}
			request, err := configactivation.NewWorkspaceRequest(definitions, author, source, []configactivation.PolicyChange{change}, nil, func(writer *registry.ConfigWriter) error {
				_, err := writer.ConfigureBinding(instance, key, "binary_sensor.bathroom_motion_occupancy")
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			r.edit(t, "# candidate B includes declarations, technical binding and explicit human policy\n")
			workspace, err := r.environment.config.startup.CaptureWorkspace()
			if err != nil {
				t.Fatal(err)
			}
			// AFTER sees a real successful row write. Sequence advancement survives
			// the intentional transaction rollback and proves this exact point
			// was reached, rather than accepting any earlier unrelated refusal.
			setup := fmt.Sprintf(`CREATE SEQUENCE public.nha029_atomic_reached START WITH 1;
CREATE FUNCTION public.nha029_atomic_abort() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN PERFORM nextval('public.nha029_atomic_reached');
RAISE EXCEPTION 'nha029 atomic fixture abort' USING ERRCODE='P0001'; END $$;
CREATE TRIGGER nha029_atomic_abort AFTER %s ON public.%s
FOR EACH ROW WHEN (%s) EXECUTE FUNCTION public.nha029_atomic_abort()`, point.event, point.table, point.when)
			if _, err := r.environment.observer.Exec(ctx, setup); err != nil {
				t.Fatal(err)
			}
			before := nha029C5RestoreDigest(t, ctx, r.environment.observer, nha029C5AtomicDigestSQL)
			counts := nha029C5HistoryCounts(t, ctx, r)
			result, refusal := r.owner.Activate(ctx, request)
			if refusal == nil || result.Outcome() != configactivation.NotCommitted || result.AttemptOutcome() != storage.TransactionNotCommitted || !result.Attempt().Valid() {
				t.Fatalf("actual SQL abort did not roll back: outcome=%v transaction=%v err=%v", result.Outcome(), result.AttemptOutcome(), refusal)
			}
			var reached bool
			var writes int64
			if err := r.environment.observer.QueryRow(ctx, `SELECT last_value,is_called FROM public.nha029_atomic_reached`).Scan(&writes, &reached); err != nil || !reached || writes != 1 {
				t.Fatalf("intended write-group trigger was not reached exactly once: writes=%d reached=%t err=%v", writes, reached, err)
			}
			if nha029C5RestoreDigest(t, ctx, r.environment.observer, nha029C5AtomicDigestSQL) != before || nha029C5HistoryCounts(t, ctx, r) != counts || nha029C5HistoryActive(t, ctx, r) != a {
				t.Fatal("one write group escaped the full atomic rollback")
			}
			if _, present, err := r.store.ReadActivationAttempt(ctx, result.Attempt()); err != nil || present {
				t.Fatalf("rolled-back candidate retained a success audit: present=%t err=%v", present, err)
			}
			afterStamp, err := r.owner.CurrentConfig(ctx)
			if err != nil || afterStamp != stamp || r.controller() != controller || !reflect.DeepEqual(nha029C5PrecommitWaitingNodes(t, original, ids), waits) {
				t.Fatalf("rollback replaced stamp/Controller/original live waits: %v", err)
			}
			if _, err := r.identity.Sequence().Capture(); err != nil || len(r.driver.snapshot()) != 0 {
				t.Fatalf("rollback failed to reopen old capture or dispatched a command: %v", err)
			}
			// Same candidate/expected policy, after removing only the test abort.
			// Every earlier write must now commit together in the actual Owner.
			if _, err := r.environment.observer.Exec(ctx, "DROP TRIGGER nha029_atomic_abort ON public."+point.table); err != nil {
				t.Fatal(err)
			}
			published, err := r.owner.Activate(ctx, request)
			if err != nil || published.Outcome() != configactivation.Published || published.AttemptOutcome() != storage.TransactionCommitted || published.Revision().Number() <= a.revision.Number() {
				t.Fatalf("same complete candidate without abort did not commit/publish: outcome=%v err=%v", published.Outcome(), err)
			}
			active, present, err := r.store.ReadActiveConfig(ctx)
			if err != nil || !present || active.Availability() != storage.ConfigGenerationComplete || active.Revision() != published.Revision() || len(active.Versions()) != 2 {
				t.Fatalf("positive control did not commit exact two-source generation: present=%t err=%v", present, err)
			}
			var operational, declared bool
			for _, version := range active.Versions() {
				kind, _ := version.Kind()
				switch kind {
				case storage.ConfigDocumentKindOperationalYAML:
					operational = version.SHA256() == workspace.SHA256() && version.Ref().Revision().Number() > a.ref.Revision().Number()
				case storage.ConfigDocumentKindYAMLSource:
					raw, plain := version.Bytes()
					declared = plain && version.Ref().Document() == document && version.SHA256() == sha256.Sum256(declarative) && bytes.Equal(raw, declarative)
				default:
					t.Fatal("human policy entered the operational manifest")
				}
			}
			if !operational || !declared {
				t.Fatal("positive control lost exact operational or declarative source")
			}
			instanceText, _ := instance.Value()
			var bindings, attempts, policies int
			if err := r.environment.observer.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM public.registry_source_bindings WHERE instance_id=$1 AND source_device='nha029-c5-atomic-device' AND source_entity='occupancy'),
 (SELECT count(*) FROM public.config_activation_attempts WHERE target_generation=$2),
 (SELECT count(*) FROM public.config_activation_policy_changes p JOIN public.config_activation_attempts a USING(attempt_id) WHERE a.target_generation=$2)`, instanceText, published.Revision().Number()).Scan(&bindings, &attempts, &policies); err != nil || bindings != 1 || attempts != 1 || policies != 1 {
				t.Fatalf("binding/attempt/policy audit did not commit together: %d/%d/%d err=%v", bindings, attempts, policies, err)
			}
			audit, present, err := r.store.ReadActivationAttempt(ctx, published.Attempt())
			if err != nil || !present || audit.Target() != active.Revision() || audit.ManifestSHA256() != active.ManifestSHA256() {
				t.Fatalf("positive control audit was not linked to its exact active manifest: present=%t err=%v", present, err)
			}
			policyAfter, err := r.store.GetAuthorizationPolicy(ctx, r.environment.account, r.environment.account)
			if err != nil {
				t.Fatal(err)
			}
			policyVersion, err := policyAfter.Version()
			if err != nil {
				t.Fatal(err)
			}
			_, policyHash, err := policyVersion.Values()
			wantedPolicyHash, hashErr := policySource.SHA256()
			if err != nil || hashErr != nil || policyHash != wantedPolicyHash {
				t.Fatal("positive control did not commit the explicitly touched human policy")
			}
			visible, err := r.owner.CurrentConfig(ctx)
			if err != nil {
				t.Fatal(err)
			}
			revision, manifest, err := visible.Components()
			if err != nil || revision != uint64(active.Revision().Number()) || manifest != active.ManifestSHA256() || r.controller() != controller || !reflect.DeepEqual(nha029C5PrecommitWaitingNodes(t, original, ids), waits) || len(r.driver.snapshot()) != 0 {
				t.Fatalf("positive control exposed mixed authority or replaced untouched waits: %v", err)
			}
			r.close(t)
			if traces, _ := original.NodeTraces(); len(traces) != 0 {
				t.Fatal("atomicity observer retained the old native image after Close")
			}
		})
	}
}

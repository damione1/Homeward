package server

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
)

func nha203PlanMetadata() *api.ConfigurationPlan {
	return &api.ConfigurationPlan{State: api.ConfigurationPlanState_CONFIGURATION_PLAN_STATE_ADMITTED, Author: "homes/catalog/accounts/admin", Family: api.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DASHBOARD, Identity: "synthetic.dashboard", SchemaVersion: "homeward-dashboard-1", SourceSha256: bytes.Repeat([]byte{1}, 32), SemanticSha256: bytes.Repeat([]byte{2}, 32), Base: &api.ConfigurationPlanBase{Kind: api.ConfigurationPlanBaseKind_CONFIGURATION_PLAN_BASE_KIND_NO_ACTIVE_DOCUMENT}, GenerationRevision: 1, ManifestSha256: bytes.Repeat([]byte{3}, 32), ObservedWorkspaceSha256: bytes.Repeat([]byte{4}, 32), Effect: api.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_NONE, Receipt: strings.Repeat("r", 80)}
}
func nha203PlanBinding(t *testing.T, digest byte) configDocumentsBinding {
	t.Helper()
	account, err := contracts.ParseAccountID("catalog-admin")
	if err != nil {
		t.Fatal(err)
	}
	return configDocumentsBinding{account: account, digest: [32]byte{digest}}
}
func nha203AssertPlanFault(t *testing.T, err error, reason api.ApiErrorReason) {
	t.Helper()
	var fault apierrors.Fault
	if !errors.As(err, &fault) || fault.Reason() != reason {
		t.Fatal("unexpected typed catalogue fault", err)
	}
}

func TestNHA203PlanCatalogueBoundsLifetimeAndNeverRetainsCandidate(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	catalog, err := newConfigurationPlanCatalog(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	home, _ := apitypes.ParseHomeName("homes/synthetic")
	binding := nha203PlanBinding(t, 1)
	metadata := nha203PlanMetadata()
	nativeExpiry := now.Add(5 * time.Minute)
	record, err := catalog.add(home, binding, metadata, nativeExpiry)
	if err != nil || !record.expires.Equal(nativeExpiry) {
		t.Fatal("capture extended native proof", err)
	}
	metadata.SourceSha256[0] = 99
	record.metadata.SourceSha256[0] = 88
	stored, err := catalog.get(record.name, binding)
	if err != nil || stored.metadata.SourceSha256[0] != 1 {
		t.Fatal("catalogue retained caller mutation aliases", err)
	}
	source := nha203PlanMetadata()
	source.Candidate = &api.ConfigurationPlanCandidate{Identity: "SYNTHETIC_PRIVATE_SOURCE_CANARY"}
	if _, err := catalog.add(home, binding, source, nativeExpiry); err == nil {
		t.Fatal("candidate/source bytes entered catalogue")
	}
	source = nha203PlanMetadata()
	source.ProtoReflect().SetUnknown([]byte{0xf8, 0x07, 0x01})
	if _, err := catalog.add(home, binding, source, nativeExpiry); err == nil {
		t.Fatal("unknown metadata entered catalogue")
	}
	now = now.Add(time.Minute)
	replacement, err := catalog.replace(record.name, binding, record.version, nha203PlanMetadata(), now.Add(30*time.Minute))
	if err != nil || !replacement.expires.Equal(nativeExpiry) {
		t.Fatal("revalidation extended capture lifetime", err)
	}
	wire := configurationPlanResource(replacement)
	if wire.Candidate != nil || contracts.AdmitConfigurationPlanResponse(wire) != nil || len(wire.Etag) != 66 || !strings.HasPrefix(wire.Etag, `"`) || !strings.HasSuffix(wire.Etag, `"`) {
		t.Fatal("catalogue did not emit an admitted metadata-only resource")
	}
	if _, err := catalog.replace(record.name, binding, record.version, nha203PlanMetadata(), now.Add(time.Minute)); err == nil {
		t.Fatal("stale catalogue version replaced a concurrently changed capture")
	} else {
		nha203AssertPlanFault(t, err, api.ApiErrorReason_API_ERROR_REASON_ETAG_MISMATCH)
	}
	now = nativeExpiry
	_, err = catalog.get(record.name, binding)
	nha203AssertPlanFault(t, err, api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND)
}

func TestNHA203PlanPageTokensBindCredentialAndRelevantCapture(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	catalog, err := newConfigurationPlanCatalog(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	home, _ := apitypes.ParseHomeName("homes/synthetic")
	binding := nha203PlanBinding(t, 1)
	other := nha203PlanBinding(t, 2)
	var first configurationPlanRecord
	for i := 0; i < 3; i++ {
		record, e := catalog.add(home, binding, nha203PlanMetadata(), now.Add(30*time.Minute))
		if e != nil {
			t.Fatal(e)
		}
		if i == 0 {
			first = record
		}
	}
	page, token, err := catalog.list(home, binding, 1, "")
	if err != nil || len(page) != 1 || token == "" {
		t.Fatal("bounded first capture", err)
	}
	if _, err := catalog.add(home, other, nha203PlanMetadata(), now.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if next, _, err := catalog.list(home, binding, 1, token); err != nil || len(next) != 1 {
		t.Fatal("another credential invalidated/leaked into this capture", err)
	}
	_, _, err = catalog.list(home, other, 1, token)
	nha203AssertPlanFault(t, err, api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED)
	_, err = catalog.get(first.name, other)
	nha203AssertPlanFault(t, err, api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND)
	metadata := nha203PlanMetadata()
	metadata.Receipt = strings.Repeat("s", 80)
	if _, err := catalog.replace(first.name, binding, first.version, metadata, now.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	_, _, err = catalog.list(home, binding, 1, token)
	nha203AssertPlanFault(t, err, api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED)
	restarted, err := newConfigurationPlanCatalog(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = restarted.list(home, binding, 1, token)
	nha203AssertPlanFault(t, err, api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED)
	_, err = restarted.get(first.name, binding)
	nha203AssertPlanFault(t, err, api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND)
}

func TestNHA203PlanCatalogueHasPerCredentialAndGlobalBudgets(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	catalog, err := newConfigurationPlanCatalog(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	home, _ := apitypes.ParseHomeName("homes/synthetic")
	for credential := byte(1); credential <= 4; credential++ {
		binding := nha203PlanBinding(t, credential)
		for i := 0; i < configurationPlanCredentialCapacity; i++ {
			if _, err := catalog.add(home, binding, nha203PlanMetadata(), now.Add(30*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
		_, err := catalog.add(home, binding, nha203PlanMetadata(), now.Add(30*time.Minute))
		nha203AssertPlanFault(t, err, api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
	}
	_, err = catalog.add(home, nha203PlanBinding(t, 5), nha203PlanMetadata(), now.Add(30*time.Minute))
	nha203AssertPlanFault(t, err, api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
	var zero configurationPlanCatalog
	if _, _, err := zero.list(home, nha203PlanBinding(t, 1), 1, ""); err == nil {
		t.Fatal("zero catalogue admitted")
	}
}

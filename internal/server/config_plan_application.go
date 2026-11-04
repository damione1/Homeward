package server

import (
	"time"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configplans"
)

// Only an admitted, currently owned capture constructs this value. The native
// receipt remains private and is released only for the exact source candidate
// and current caller binding. A stale/expired preview cannot select the native
// writer's separate ordinary-YAML-edit path through an empty receipt.
type configurationPlanApplication struct {
	valid    bool
	binding  configDocumentsBinding
	family   configplans.Family
	identity string
	source   [32]byte
	receipt  string
	expires  time.Time
	now      func() time.Time
}

func (c *configurationPlanCatalog) forApplication(name apitypes.ConfigurationPlanName, binding configDocumentsBinding, etag string) (configurationPlanApplication, error) {
	record, err := c.get(name, binding)
	if err != nil {
		return configurationPlanApplication{}, err
	}
	resource := configurationPlanResource(record)
	if resource == nil {
		return configurationPlanApplication{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
	}
	if resource.Etag != etag {
		return configurationPlanApplication{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ETAG_MISMATCH)
	}
	metadata := record.metadata
	if metadata.State != api.ConfigurationPlanState_CONFIGURATION_PLAN_STATE_ADMITTED || len(metadata.Receipt) < 44 || len(metadata.Receipt) > 4200 {
		return configurationPlanApplication{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STATE_PRECONDITION_FAILED)
	}
	family, err := configPlanFamily(metadata.Family)
	if err != nil || len(metadata.SourceSha256) != 32 {
		return configurationPlanApplication{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
	}
	var source [32]byte
	copy(source[:], metadata.SourceSha256)
	return configurationPlanApplication{valid: true, binding: binding, family: family, identity: metadata.Identity, source: source, receipt: metadata.Receipt, expires: record.expires, now: c.now}, nil
}

func (p configurationPlanApplication) nativeReceipt(candidate configplans.Candidate, binding configDocumentsBinding) (string, error) {
	if !p.valid || p.now == nil || !candidate.Valid() {
		return "", configplans.ErrInvalid
	}
	if p.binding != binding {
		return "", authorization.ErrDenied
	}
	if !p.now().Before(p.expires) {
		return "", apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND)
	}
	if p.family != candidate.Family() || p.identity != candidate.Identity() || p.source != candidate.SourceSHA256() {
		return "", configplans.ErrInvalid
	}
	return p.receipt, nil
}

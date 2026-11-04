package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"sort"

	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apimutations"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

type configurationPlanMutationResult struct {
	resource *api.ConfigurationPlan
	binding  configDocumentsBinding
}

func encodeConfigurationPlanMutation(value configurationPlanMutationResult) ([]byte, error) {
	if !value.binding.valid() || value.resource == nil || proto.Size(value.resource) > configurationPlanMetadataBytes || contracts.AdmitConfigurationPlanResponse(value.resource) != nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
	}
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(value.resource)
	if err != nil {
		return nil, err
	}
	// The opaque authority/credential capture is not a public resource field.
	result := append([]byte{1}, value.binding.digest[:]...)
	return append(result, raw...), nil
}

func decodeConfigurationPlanMutation(data []byte, binding configDocumentsBinding) (*api.ConfigurationPlan, error) {
	if len(data) < 34 || len(data) > configurationPlanMetadataBytes+33 || data[0] != 1 {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
	}
	if !binding.valid() || !bytes.Equal(data[1:33], binding.digest[:]) {
		return nil, authorization.ErrDenied
	}
	resource := new(api.ConfigurationPlan)
	if proto.Unmarshal(data[33:], resource) != nil || contracts.AdmitConfigurationPlanResponse(resource) != nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
	}
	return resource, nil
}

func configurationPlanMutationContext(ctx context.Context, method, id string, raw proto.Message) (context.Context, error) {
	if id == "" {
		return ctx, nil
	}
	if _, err := apitypes.ParseRequestID(id); err != nil {
		if raw == nil {
			return ctx, contracts.ErrInvalidConfigurationPlanContract
		}
		violation, buildErr := apierrors.NewViolation(raw.ProtoReflect().Descriptor(), "request_id", api.ValidationReason_VALIDATION_REASON_INVALID_REQUEST_ID)
		if buildErr != nil {
			return ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
		}
		return ctx, apierrors.Validation(violation)
	}
	copy := proto.Clone(raw)
	var candidate *api.ConfigurationPlanCandidate
	switch value := copy.(type) {
	case *api.CreateConfigurationPlanRequest:
		if value == nil || value.ConfigurationPlan == nil {
			return ctx, contracts.ErrInvalidConfigurationPlanContract
		}
		candidate = value.ConfigurationPlan.Candidate
		value.ConfigurationPlan = &api.ConfigurationPlan{Candidate: candidate}
		value.RequestId = ""
	case *api.RevalidateConfigurationPlanRequest:
		if value == nil {
			return ctx, contracts.ErrInvalidConfigurationPlanContract
		}
		candidate = value.Candidate
		value.RequestId = ""
	default:
		return ctx, contracts.ErrInvalidConfigurationPlanContract
	}
	if candidate == nil {
		return ctx, contracts.ErrInvalidConfigurationPlanContract
	}
	for _, file := range candidate.Files {
		if file == nil {
			return ctx, contracts.ErrInvalidConfigurationPlanContract
		}
	}
	sort.Slice(candidate.Files, func(i, j int) bool { return candidate.Files[i].LogicalPath < candidate.Files[j].LogicalPath })
	normalized, err := (proto.MarshalOptions{Deterministic: true}).Marshal(copy)
	if err != nil {
		return ctx, err
	}
	digest := sha256.Sum256(normalized)
	request, err := apimutations.New(ctx, method, id, digest[:])
	if err != nil {
		return ctx, err
	}
	return apimutations.WithResultEncoder(ctx, request, encodeConfigurationPlanMutation)
}

func (h *configPlansHandler) planMutationEvidence(ctx context.Context) (authorization.MetadataEvidence, error) {
	permit, err := h.documents.mutationPermit(ctx)
	if err != nil {
		return authorization.MetadataEvidence{}, err
	}
	defer permit.Release()
	if !permit.Ready() {
		return authorization.MetadataEvidence{}, authorization.ErrDenied
	}
	return permit.EntityMetadataEvidence()
}

// This transaction contains only the current-rights fence and a ledger probe.
// Native Build/Revalidate and their SQL/Owner captures run after it has ended.
func (h *configPlansHandler) probePlanMutationSQL(ctx context.Context, evidence authorization.MetadataEvidence) ([]byte, bool, error) {
	rights, err := evidence.Rights()
	if err != nil {
		return nil, false, err
	}
	var data []byte
	var present bool
	var decision error
	outcome, err := h.documents.service.Store().InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		if e := authorization.FenceSQL(ctx, tx, rights); e != nil {
			decision = e
			return e
		}
		data, present, decision = apimutations.Probe[configurationPlanMutationResult](ctx, tx)
		return decision
	})
	if decision != nil {
		return nil, false, decision
	}
	if err != nil || outcome != storage.TransactionCommitted {
		return nil, false, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	return data, present, nil
}

// The existing catalogue mutex is the publication barrier. A duplicate cannot
// return committed resource bytes before the first call publishes its capture.
// Only an exact process-local pending capture can be promoted after a lost SQL
// acknowledgement. Expiry or restart drops that ability; replay never rebuilds
// a native receipt, extends its lifetime or resurrects a lost plan.
func (c *configurationPlanCatalog) admitPlanReplayLocked(data []byte, binding configDocumentsBinding) (*api.ConfigurationPlan, error) {
	resource, err := decodeConfigurationPlanMutation(data, binding)
	if err != nil {
		return nil, err
	}
	c.purgeLocked()
	if pending, exists := c.pending[resource.Name]; exists && pending.binding == binding && proto.Equal(configurationPlanResource(pending), resource) {
		c.publishLocked(pending)
	}
	return resource, nil
}

func (h *configPlansHandler) probePlanMutation(ctx context.Context, evidence authorization.MetadataEvidence, binding configDocumentsBinding) (*api.ConfigurationPlan, bool, error) {
	data, present, err := h.probePlanMutationSQL(ctx, evidence)
	if err != nil || !present {
		return nil, present, err
	}
	h.catalog.mu.Lock()
	defer h.catalog.mu.Unlock()
	resource, err := h.catalog.admitPlanReplayLocked(data, binding)
	return resource, true, err
}

func (h *configPlansHandler) planMutationPreparationError(ctx context.Context, evidence authorization.MetadataEvidence, binding configDocumentsBinding, cause error) (*api.ConfigurationPlan, error) {
	// A concurrent first request may have committed while this read-only
	// diagnostic observed a newer plan/CAS. Prefer its exact original result.
	if resource, present, err := h.probePlanMutation(ctx, evidence, binding); err != nil {
		return nil, err
	} else if present {
		return resource, nil
	}
	return nil, cause
}

func (h *configPlansHandler) persistPlanMutation(ctx context.Context, evidence authorization.MetadataEvidence, binding configDocumentsBinding, prepare func() (configurationPlanRecord, error)) (*api.ConfigurationPlan, error) {
	c := h.catalog
	c.mu.Lock()
	defer c.mu.Unlock()
	// Recheck before local CAS/capacity preparation, under the publication
	// barrier. The eventual Execute independently rechecks in its own tx.
	if data, present, err := h.probePlanMutationSQL(ctx, evidence); err != nil {
		return nil, err
	} else if present {
		return c.admitPlanReplayLocked(data, binding)
	}
	record, err := prepare()
	if err != nil {
		return nil, err
	}
	resource := configurationPlanResource(record)
	if resource == nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
	}
	c.pending[record.name.String()] = record.clone()
	rights, err := evidence.Rights()
	if err != nil {
		delete(c.pending, record.name.String())
		return nil, err
	}
	var decision error
	outcome, err := h.documents.service.Store().InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		if e := authorization.FenceSQL(ctx, tx, rights); e != nil {
			decision = e
			return e
		}
		_, decision = apimutations.Execute(ctx, tx, func() (configurationPlanMutationResult, error) {
			return configurationPlanMutationResult{resource: resource, binding: binding}, nil
		})
		return decision
	})
	var replay *apimutations.Error
	if errors.As(decision, &replay) {
		if data, present := replay.ReplayResult(); present {
			delete(c.pending, record.name.String())
			return c.admitPlanReplayLocked(data, binding)
		}
	}
	if outcome == storage.TransactionNotCommitted {
		delete(c.pending, record.name.String())
	}
	if decision != nil {
		return nil, decision
	}
	if err != nil || outcome != storage.TransactionCommitted {
		// Retain only bounded redacted pending metadata. A subsequent probe
		// can publish it only after reading this exact committed SQL result.
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	c.publishLocked(record)
	return resource, nil
}

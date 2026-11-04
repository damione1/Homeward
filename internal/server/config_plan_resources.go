package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/configplans"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type configurationRPCDependencies struct {
	names      administrationNames
	plans      *configurationPlanCatalog
	operations *configurationOperationProvider
}

func firstConfigurationRPC(values []configurationRPCDependencies) configurationRPCDependencies {
	if len(values) != 1 {
		return configurationRPCDependencies{}
	}
	return values[0]
}
func configurationPlanResource(record configurationPlanRecord) *api.ConfigurationPlan {
	if record.metadata == nil || !record.name.Valid() || record.created.IsZero() || !record.expires.After(record.created) {
		return nil
	}
	result := proto.Clone(record.metadata).(*api.ConfigurationPlan)
	result.Name = record.name.String()
	result.CreateTime = timestamppb.New(record.created)
	result.UpdateTime = timestamppb.New(record.updated)
	result.ExpireTime = timestamppb.New(record.expires)
	result.Candidate = nil
	result.Etag = ""
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(result)
	if err != nil {
		return nil
	}
	digest := sha256.Sum256(raw)
	result.Etag = `"` + hex.EncodeToString(digest[:]) + `"`
	return result
}
func (h *configPlansHandler) GetConfigurationPlan(ctx context.Context, r *connect.Request[api.GetConfigurationPlanRequest]) (*connect.Response[api.ConfigurationPlan], error) {
	binding, err := h.admit(ctx, nil)
	if err != nil {
		return nil, configPlanError(ctx, err)
	}
	if r == nil || contracts.AdmitConfigurationPlanRequest(r.Msg) != nil {
		return nil, configPlanError(ctx, configplans.ErrInvalid)
	}
	name, err := apitypes.ParseConfigurationPlanName(r.Msg.Name)
	if err != nil {
		return nil, configPlanError(ctx, configplans.ErrInvalid)
	}
	if err := h.documents.names.parent(ctx, name.Home().String()); err != nil {
		return nil, configPlanError(ctx, err)
	}
	record, err := h.catalog.get(name, binding)
	if err != nil {
		return nil, configPlanError(ctx, err)
	}
	if _, err := h.admit(ctx, &binding); err != nil {
		return nil, configPlanError(ctx, err)
	}
	return configPlanResponse(ctx, configurationPlanResource(record))
}
func (h *configPlansHandler) ListConfigurationPlans(ctx context.Context, r *connect.Request[api.ListConfigurationPlansRequest]) (*connect.Response[api.ListConfigurationPlansResponse], error) {
	binding, err := h.admit(ctx, nil)
	if err != nil {
		return nil, configPlanError(ctx, err)
	}
	if r == nil || contracts.AdmitConfigurationPlanRequest(r.Msg) != nil {
		return nil, configPlanError(ctx, configplans.ErrInvalid)
	}
	if err := h.documents.names.parent(ctx, r.Msg.Parent); err != nil {
		return nil, configPlanError(ctx, err)
	}
	home, err := apitypes.ParseHomeName(r.Msg.Parent)
	if err != nil {
		return nil, configPlanError(ctx, configplans.ErrInvalid)
	}
	records, next, err := h.catalog.list(home, binding, r.Msg.PageSize, r.Msg.PageToken)
	if err != nil {
		return nil, configPlanError(ctx, err)
	}
	result := &api.ListConfigurationPlansResponse{NextPageToken: next}
	for _, record := range records {
		result.ConfigurationPlans = append(result.ConfigurationPlans, configurationPlanResource(record))
	}
	if _, err := h.admit(ctx, &binding); err != nil {
		return nil, configPlanError(ctx, err)
	}
	return configPlanResponse(ctx, result)
}

package server

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"math"
	"strings"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	native "github.com/damione1/Homeward/gen/home/integrationadmin/v1"
	host "github.com/damione1/Homeward/gen/home/integrations/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func integrationResourceIntegrity() error {
	return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
}

// Descriptor keys are immutable natural keys, not labels. A lossless encoding
// preserves their exact identity; instances use the native transaction's durable
// random mapping instead. Neither naming mechanism grants permission.
func integrationTypeName(home apitypes.HomeName, key string) (apitypes.IntegrationTypeName, error) {
	if !home.Valid() || key == "" {
		return apitypes.IntegrationTypeName{}, integrationResourceIntegrity()
	}
	id := "t-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(key)))
	name, err := apitypes.ParseIntegrationTypeName(home.String() + "/integrationTypes/" + id)
	if err != nil {
		return apitypes.IntegrationTypeName{}, integrationResourceIntegrity()
	}
	return name, nil
}

func integrationTypeResource(home apitypes.HomeName, value *native.IntegrationType) (*api.IntegrationType, error) {
	if value == nil {
		return nil, integrationResourceIntegrity()
	}
	name, err := integrationTypeName(home, value.TypeId)
	if err != nil {
		return nil, err
	}
	return &api.IntegrationType{Name: name.String(), TypeKey: value.TypeId, ManifestVersion: value.ManifestVersion,
		SchemaVersion: value.SchemaVersion, JsonSchema: append([]byte(nil), value.JsonSchema...),
		SchemaSha256: append([]byte(nil), value.SchemaSha256...), CapabilitySchemaIds: append([]string(nil), value.CapabilitySchemaIds...)}, nil
}

func integrationSecrets(values []*native.SecretReference) ([]*api.IntegrationSecretReference, error) {
	out := make([]*api.IntegrationSecretReference, 0, len(values))
	for _, value := range values {
		if value == nil {
			return nil, integrationResourceIntegrity()
		}
		out = append(out, &api.IntegrationSecretReference{Field: value.Field, InstanceKey: value.IntegrationInstanceId, SecretKey: value.Name})
	}
	return out, nil
}

func integrationDesiredState(value native.DesiredState) (api.IntegrationDesiredState, error) {
	switch value {
	case native.DesiredState_DESIRED_STATE_ENABLED:
		return api.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_ENABLED, nil
	case native.DesiredState_DESIRED_STATE_DISABLED:
		return api.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_DISABLED, nil
	case native.DesiredState_DESIRED_STATE_UNSPECIFIED:
		return 0, integrationResourceIntegrity()
	default:
		return 0, integrationResourceIntegrity()
	}
}
func integrationLifecycle(value obs.PilotPhase) (api.IntegrationLifecycle, error) {
	switch value {
	case obs.PilotPhase_PILOT_PHASE_STARTING:
		return api.IntegrationLifecycle_INTEGRATION_LIFECYCLE_STARTING, nil
	case obs.PilotPhase_PILOT_PHASE_RUNNING:
		return api.IntegrationLifecycle_INTEGRATION_LIFECYCLE_RUNNING, nil
	case obs.PilotPhase_PILOT_PHASE_RETRYING:
		return api.IntegrationLifecycle_INTEGRATION_LIFECYCLE_RETRYING, nil
	case obs.PilotPhase_PILOT_PHASE_DEGRADED:
		return api.IntegrationLifecycle_INTEGRATION_LIFECYCLE_DEGRADED, nil
	case obs.PilotPhase_PILOT_PHASE_STOPPED:
		return api.IntegrationLifecycle_INTEGRATION_LIFECYCLE_STOPPED, nil
	case obs.PilotPhase_PILOT_PHASE_UNSPECIFIED:
		return 0, integrationResourceIntegrity()
	default:
		return 0, integrationResourceIntegrity()
	}
}

// Health and supervision are separate observations. This converter never turns
// Running, Enabled, absence or a current configuration into a Ready report.
func integrationObservations(out *api.IntegrationInstance, value *native.IntegrationInstance) error {
	if out == nil || value == nil || value.Report == nil || value.Supervision == nil {
		return integrationResourceIntegrity()
	}
	switch report := value.Report.Result.(type) {
	case *obs.IntegrationReportPresence_NoReport:
		if report == nil || report.NoReport == nil {
			return integrationResourceIntegrity()
		}
		out.ObservedHealth = &api.IntegrationInstance_NoHealthReport{NoHealthReport: &emptypb.Empty{}}
	case *obs.IntegrationReportPresence_Present:
		if report == nil || report.Present == nil {
			return integrationResourceIntegrity()
		}
		projected := &api.IntegrationHealthReport{}
		switch report.Present.Status {
		case host.HealthStatus_HEALTH_STATUS_READY:
			projected.Status = api.IntegrationHealthStatus_INTEGRATION_HEALTH_STATUS_READY
		case host.HealthStatus_HEALTH_STATUS_DEGRADED:
			projected.Status = api.IntegrationHealthStatus_INTEGRATION_HEALTH_STATUS_DEGRADED
		case host.HealthStatus_HEALTH_STATUS_UNAVAILABLE:
			projected.Status = api.IntegrationHealthStatus_INTEGRATION_HEALTH_STATUS_UNAVAILABLE
		case host.HealthStatus_HEALTH_STATUS_UNSPECIFIED:
			return integrationResourceIntegrity()
		default:
			return integrationResourceIntegrity()
		}
		switch evidence := report.Present.AdmissionTime.(type) {
		case *obs.IntegrationReport_AdmittedAt:
			if evidence == nil || evidence.AdmittedAt == nil || evidence.AdmittedAt.CheckValid() != nil {
				return integrationResourceIntegrity()
			}
			projected.Admission = &api.IntegrationHealthReport_AdmissionTime{AdmissionTime: proto.Clone(evidence.AdmittedAt).(*timestamppb.Timestamp)}
		case *obs.IntegrationReport_NotRecorded:
			if evidence == nil || evidence.NotRecorded == nil {
				return integrationResourceIntegrity()
			}
			projected.Admission = &api.IntegrationHealthReport_AdmissionNotRecorded{AdmissionNotRecorded: &emptypb.Empty{}}
		default:
			return integrationResourceIntegrity()
		}
		out.ObservedHealth = &api.IntegrationInstance_HealthReport{HealthReport: projected}
	default:
		return integrationResourceIntegrity()
	}
	switch supervised := value.Supervision.Result.(type) {
	case *obs.IntegrationSupervision_NotSupervised:
		if supervised == nil || supervised.NotSupervised == nil {
			return integrationResourceIntegrity()
		}
		out.ObservedSupervision = &api.IntegrationInstance_NotSupervised{NotSupervised: &emptypb.Empty{}}
	case *obs.IntegrationSupervision_Supervised:
		if supervised == nil || supervised.Supervised == nil || supervised.Supervised.Attempts > math.MaxInt64 || supervised.Supervised.Tasks > math.MaxInt64 {
			return integrationResourceIntegrity()
		}
		lifecycle, err := integrationLifecycle(supervised.Supervised.Phase)
		if err != nil {
			return err
		}
		out.ObservedSupervision = &api.IntegrationInstance_Supervision{Supervision: &api.IntegrationSupervisionSnapshot{Lifecycle: lifecycle, Attempts: int64(supervised.Supervised.Attempts), Tasks: int64(supervised.Supervised.Tasks)}}
	default:
		return integrationResourceIntegrity()
	}
	return nil
}

type integrationDefinitionVersion struct {
	ref        storage.ConfigVersionRef
	hash       [32]byte
	generation int64
	manifest   [32]byte
	workspace  [32]byte
	observed   bool
	pending    bool
}

func (h *integrationProjection) integrationResource(ctx context.Context, home apitypes.HomeName, value *native.IntegrationInstance, version integrationDefinitionVersion, observations bool) (*api.IntegrationInstance, error) {
	if value == nil || !home.Valid() || !version.ref.Valid() || version.hash == ([32]byte{}) || version.generation < 1 || version.manifest == ([32]byte{}) {
		return nil, integrationResourceIntegrity()
	}
	name, err := h.names.name(ctx, storage.APIIntegrationInstances, value.IntegrationInstanceId)
	if err != nil {
		return nil, err
	}
	admitted, err := apitypes.ParseIntegrationInstanceName(name)
	if err != nil || admitted.Home() != home {
		return nil, integrationResourceIntegrity()
	}
	source, err := h.names.name(ctx, storage.APIConfigurationDocuments, version.ref.Document().String())
	if err != nil {
		return nil, err
	}
	desired, err := integrationDesiredState(value.DesiredState)
	if err != nil {
		return nil, err
	}
	out := &api.IntegrationInstance{Name: name, InstanceKey: value.IntegrationInstanceId, DesiredState: desired, ConfigurationDocument: source}
	if value.TypeId != "" {
		typ, err := integrationTypeName(home, value.TypeId)
		if err != nil {
			return nil, err
		}
		out.IntegrationType = typ.String()
	}
	switch configuration := value.Configuration.(type) {
	case *native.IntegrationInstance_Editable:
		if configuration == nil || configuration.Editable == nil || out.IntegrationType == "" {
			return nil, integrationResourceIntegrity()
		}
		secrets, err := integrationSecrets(configuration.Editable.Secrets)
		if err != nil {
			return nil, err
		}
		out.Configuration = &api.IntegrationInstance_Editable{Editable: &api.IntegrationConfiguration{SchemaVersion: configuration.Editable.SchemaVersion, YamlSource: append([]byte(nil), configuration.Editable.YamlSource...), Secrets: secrets}}
	case *native.IntegrationInstance_Opaque:
		if configuration == nil || configuration.Opaque == nil {
			return nil, integrationResourceIntegrity()
		}
		secrets, err := integrationSecrets(configuration.Opaque.Secrets)
		if err != nil {
			return nil, err
		}
		out.Configuration = &api.IntegrationInstance_Opaque{Opaque: &api.OpaqueIntegrationConfiguration{Secrets: secrets}}
	default:
		return nil, integrationResourceIntegrity()
	}
	// Source revision distinguishes ABA even when the authorized definition bytes
	// happen to match. The etag is compared opaquely; nobody decodes this digest.
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(out)
	if err != nil {
		return nil, err
	}
	digest := sha256.New()
	_, _ = digest.Write(raw)
	_, _ = digest.Write(version.hash[:])
	var revision [8]byte
	binary.BigEndian.PutUint64(revision[:], uint64(version.ref.Revision().Number()))
	_, _ = digest.Write(revision[:])
	binary.BigEndian.PutUint64(revision[:], uint64(version.generation))
	_, _ = digest.Write(revision[:])
	_, _ = digest.Write(version.manifest[:])
	_, _ = digest.Write(version.workspace[:])
	flags := byte(0)
	if version.observed {
		flags |= 1
	}
	if version.pending {
		flags |= 2
	}
	_, _ = digest.Write([]byte{flags})
	out.Etag = `W/"` + hex.EncodeToString(digest.Sum(nil)) + `"`
	if observations {
		if err := integrationObservations(out, value); err != nil {
			return nil, err
		}
	}
	return out, nil
}

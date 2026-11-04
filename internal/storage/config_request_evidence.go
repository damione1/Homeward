package storage

import (
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
)

// The journal retains the family of the native authorization, not a generic
// administrator flag. Its closed variants cannot be selected by public input.
type configurationRequestEvidenceKind uint8

const (
	configurationRequestGlobal configurationRequestEvidenceKind = iota + 1
	configurationRequestIntegration
)

type configurationRequestEvidence struct {
	kind          configurationRequestEvidenceKind
	configuration authorization.MetadataEvidence
	integration   authorization.IntegrationAdministrationEvidence
}

func (e configurationRequestEvidence) rights(method ConfigurationAPIMethod) (authorization.RightsEvidence, error) {
	switch method {
	case ConfigurationPublishDocument, ConfigurationRollbackDocument, ConfigurationApplyPlan, ConfigurationRollback:
		if e.kind == configurationRequestGlobal {
			return e.configuration.Rights()
		}
	case ConfigurationCreateIntegration, ConfigurationUpdateIntegration, ConfigurationDeleteIntegration:
		if e.kind == configurationRequestIntegration {
			return e.integration.Rights()
		}
	}
	return authorization.RightsEvidence{}, authorization.ErrDenied
}

func (e configurationRequestEvidence) actor(method ConfigurationAPIMethod) (authorization.RightsEvidence, string, error) {
	rights, err := e.rights(method)
	if err != nil {
		return authorization.RightsEvidence{}, "", authorization.ErrDenied
	}
	account, err := rights.Account()
	if err != nil {
		return authorization.RightsEvidence{}, "", authorization.ErrDenied
	}
	actor, err := account.Value()
	if err != nil {
		return authorization.RightsEvidence{}, "", authorization.ErrDenied
	}
	return rights, actor, nil
}

// IntegrationRequestID represents a deliberately omitted UUID separately from
// an invalid/uninitialized value. Absence never requests deduplication.
type IntegrationRequestID struct {
	id    apitypes.RequestID
	valid bool
}

func WithoutIntegrationRequestID() IntegrationRequestID { return IntegrationRequestID{valid: true} }
func WithIntegrationRequestID(id apitypes.RequestID) (IntegrationRequestID, error) {
	if !id.Valid() {
		return IntegrationRequestID{}, configRequestError(ConfigurationRequestInvalid)
	}
	return IntegrationRequestID{id: id, valid: true}, nil
}
func (id IntegrationRequestID) Valid() bool { return id.valid }
func integrationAPIMethod(method ConfigurationAPIMethod) bool {
	switch method {
	case ConfigurationCreateIntegration, ConfigurationUpdateIntegration, ConfigurationDeleteIntegration:
		return true
	default:
		return false
	}
}
func configurationRequestUUID(id apitypes.RequestID) *string {
	if !id.Valid() {
		return nil
	}
	value := id.String()
	return &value
}

func configurationIntegrationKey(target ConfigurationIntegrationTarget) *string {
	if !target.Valid() {
		return nil
	}
	key, _ := target.Instance().Value()
	return &key
}

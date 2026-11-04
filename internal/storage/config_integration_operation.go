package storage

import (
	"context"

	"github.com/damione1/Homeward/internal/domain"
)

// ConfigurationIntegrationTarget binds the result vocabulary to one admitted
// native integration identity. It contains no source, credential, compiler
// receipt or capability to execute an activation.
type ConfigurationIntegrationTarget struct {
	method   ConfigurationAPIMethod
	instance domain.IntegrationInstanceID
}

func NewConfigurationIntegrationTarget(method ConfigurationAPIMethod, instance domain.IntegrationInstanceID) (ConfigurationIntegrationTarget, error) {
	switch method {
	case ConfigurationCreateIntegration, ConfigurationUpdateIntegration, ConfigurationDeleteIntegration:
	default:
		return ConfigurationIntegrationTarget{}, &Error{kind: ConfigInvalid}
	}
	if key, err := instance.Value(); err != nil || len(key) > 256 {
		return ConfigurationIntegrationTarget{}, &Error{kind: ConfigInvalid}
	}
	return ConfigurationIntegrationTarget{method: method, instance: instance}, nil
}
func (t ConfigurationIntegrationTarget) Method() ConfigurationAPIMethod         { return t.method }
func (t ConfigurationIntegrationTarget) Instance() domain.IntegrationInstanceID { return t.instance }
func (t ConfigurationIntegrationTarget) Valid() bool {
	_, err := NewConfigurationIntegrationTarget(t.method, t.instance)
	return err == nil
}

type configurationIntegrationTargetKey struct{}

// WithConfigurationIntegrationTarget is a private composition port. The native
// owner still checks actual rights, author, source CAS and publication. The
// result binding is independent of the optional deduplication request_id.
func WithConfigurationIntegrationTarget(ctx context.Context, target ConfigurationIntegrationTarget) (context.Context, error) {
	if ctx == nil || !target.Valid() {
		return nil, &Error{kind: ConfigInvalid}
	}
	if _, present := configurationIntegrationTargetFromContext(ctx); present {
		return nil, &Error{kind: ConfigInvalid}
	}
	return context.WithValue(ctx, configurationIntegrationTargetKey{}, target), nil
}
func configurationIntegrationTargetFromContext(ctx context.Context) (ConfigurationIntegrationTarget, bool) {
	if ctx == nil {
		return ConfigurationIntegrationTarget{}, false
	}
	target, present := ctx.Value(configurationIntegrationTargetKey{}).(ConfigurationIntegrationTarget)
	return target, present && target.Valid()
}

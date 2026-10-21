package config

import (
	"fmt"

	"github.com/damione1/Homeward/scope"
	"go.yaml.in/yaml/v3"
)

// The omitted selector preserves the published v1 default. An explicit null,
// alias, non-string or unknown revision must never silently select a scope.
func parseScopeRevision(node yaml.Node) (scope.Revision, error) {
	if node.Kind == 0 {
		return scope.BaselineV1(), nil
	}
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return scope.Revision{}, fmt.Errorf("configuration: scope_revision: %w", scope.ErrInvalidRevision)
	}
	revision, err := scope.ParseRevision(node.Value)
	if err != nil {
		return scope.Revision{}, fmt.Errorf("configuration: scope_revision: %w", err)
	}
	return revision, nil
}

// ScopeRevision is a cold-start selection, independent of configuration
// document revisions. An uninitialized Config grants no default admission.
func (c Config) ScopeRevision() (scope.Revision, error) {
	if _, err := c.scopeRevision.Value(); err != nil {
		return scope.Revision{}, ErrUninitialized
	}
	return c.scopeRevision, nil
}

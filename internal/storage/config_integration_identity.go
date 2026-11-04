package storage

import (
	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/authorization"
)

// This is an origin binding, not a bearer credential or delegated authority.
// Only integrations use it. Global configuration retains its existing account
// ownership. A refresh preserves a session ID; a new login does not.
type configurationIntegrationCredential struct {
	kind authorization.IntegrationCredentialKind
	id   string
}

func (c configurationIntegrationCredential) valid() bool {
	if len(c.id) < 1 || len(c.id) > 255 {
		return false
	}
	switch c.kind {
	case authorization.IntegrationNativeSession:
		_, err := contracts.ParseSessionID(c.id)
		return err == nil
	case authorization.IntegrationMCPKey:
		_, err := contracts.ParseMcpKeyID(c.id)
		return err == nil && len(c.id) == 32
	default:
		return false
	}
}
func integrationCredential(e authorization.IntegrationAdministrationEvidence) (configurationIntegrationCredential, error) {
	c := configurationIntegrationCredential{kind: e.CredentialKind()}
	switch c.kind {
	case authorization.IntegrationNativeSession:
		session, err := e.Session()
		if err != nil {
			return configurationIntegrationCredential{}, authorization.ErrDenied
		}
		c.id, err = session.Value()
		if err != nil {
			return configurationIntegrationCredential{}, authorization.ErrDenied
		}
	case authorization.IntegrationMCPKey:
		key, err := e.MCPKey()
		if err != nil {
			return configurationIntegrationCredential{}, authorization.ErrDenied
		}
		c.id, err = key.Value()
		if err != nil {
			return configurationIntegrationCredential{}, authorization.ErrDenied
		}
	default:
		return configurationIntegrationCredential{}, authorization.ErrDenied
	}
	if !c.valid() {
		return configurationIntegrationCredential{}, authorization.ErrDenied
	}
	return c, nil
}
func (c configurationIntegrationCredential) sqlKind() *int16 {
	if !c.valid() {
		return nil
	}
	kind := int16(c.kind)
	return &kind
}
func (c configurationIntegrationCredential) sqlID() *string {
	if !c.valid() {
		return nil
	}
	id := c.id
	return &id
}

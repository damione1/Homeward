package integrations

import (
	"context"
	"errors"

	"github.com/damione1/Homeward/internal/storage"
)

var ErrForeignSecretHandle = errors.New("secret handle belongs to another integration instance")
var ErrSecretAuditUnavailable = errors.New("secret access refusal could not be audited")

// AttachSecrets is called only by trusted composition before serving pilots.
func (h *Manager) AttachSecrets(repo *storage.SecretRepository) error {
	if h == nil || !repo.Valid() {
		return ErrInvalidInput
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.composition != compositionOpen || h.secrets != nil || h.supervisor != nil {
		return ErrSupervisorState
	}
	h.secrets = repo
	return nil
}

type sessionSecretProvider struct{ session *Session }

func (p *sessionSecretProvider) String() string   { return "integration: secret provider [REDACTED]" }
func (p *sessionSecretProvider) GoString() string { return p.String() }

func (s *Session) Secrets() (SecretProvider, error) {
	if s == nil || s.host == nil {
		return nil, ErrInactiveSession
	}
	h := s.host
	h.mu.Lock()
	defer h.mu.Unlock()
	if !s.live() || h.admission != admissionOpen {
		return nil, ErrInactiveSession
	}
	if h.secrets == nil {
		return nil, ErrInvalidInput
	}
	return &sessionSecretProvider{session: s}, nil
}

func (p *sessionSecretProvider) Read(ctx context.Context, handle storage.SecretHandle) (storage.SecretValue, error) {
	if ctx == nil || p == nil || p.session == nil || p.session.host == nil {
		return storage.SecretValue{}, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return storage.SecretValue{}, err
	}
	target, _, err := handle.Components()
	if err != nil {
		return storage.SecretValue{}, err
	}
	s := p.session
	h := s.host
	h.mu.Lock()
	repo := h.secrets
	live := s.live() && h.admission == admissionOpen
	requester := s.instance
	h.mu.Unlock()
	if repo == nil {
		return storage.SecretValue{}, ErrInvalidInput
	}
	if !live {
		if err := repo.AuditDenied(ctx, requester, handle, storage.SecretDenialInactiveSession); err != nil {
			return storage.SecretValue{}, ErrSecretAuditUnavailable
		}
		return storage.SecretValue{}, ErrInactiveSession
	}
	if target != requester {
		if err := repo.AuditDenied(ctx, requester, handle, storage.SecretDenialForeignHandle); err != nil {
			return storage.SecretValue{}, ErrSecretAuditUnavailable
		}
		return storage.SecretValue{}, ErrForeignSecretHandle
	}
	value, readErr := repo.Read(ctx, handle) // Manager.mu is not held through SQL or crypto.
	h.mu.Lock()
	live = s.live() && h.admission == admissionOpen
	h.mu.Unlock()
	if !live {
		if err := repo.AuditDenied(ctx, requester, handle, storage.SecretDenialInactiveSession); err != nil {
			return storage.SecretValue{}, ErrSecretAuditUnavailable
		}
		return storage.SecretValue{}, ErrInactiveSession
	}
	if readErr != nil {
		return storage.SecretValue{}, readErr
	}
	if err := ctx.Err(); err != nil {
		return storage.SecretValue{}, err
	}
	return value, nil
}

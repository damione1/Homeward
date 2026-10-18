package integrations

import (
	"context"
	"errors"
	"time"

	"github.com/damione1/Homeward/internal/storage"
)

var ErrForeignSecretHandle = errors.New("secret handle belongs to another integration instance")
var ErrSecretAuditUnavailable = errors.New("secret access refusal could not be audited")
var ErrCredentialCommittedRetired = errors.New("credential committed but integration session is inactive")
var ErrCredentialCommittedUnobserved = errors.New("credential committed after caller cancellation")

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

// readVersioned is reserved for the shared cloud policy. The public
// SecretProvider still grants read-only access to ordinary pilots.
func (p *sessionSecretProvider) readVersioned(ctx context.Context, handle storage.SecretHandle) (storage.SecretValue, storage.SecretRevision, error) {
	repo, s, err := p.admitCredential(ctx, handle)
	if err != nil {
		return storage.SecretValue{}, storage.SecretRevision{}, err
	}
	value, revision, readErr := repo.ReadCredentialVersioned(ctx, handle)
	if err := p.checkCredentialAfterSQL(ctx, repo, s, handle); err != nil {
		return storage.SecretValue{}, storage.SecretRevision{}, err
	}
	if readErr != nil {
		return storage.SecretValue{}, storage.SecretRevision{}, readErr
	}
	return value, revision, nil
}

// rotateIfRevision linearizes refresh admission under Host.mu after the HTTP
// response was received. The mutex is released before PostgreSQL I/O. A
// rotation admitted before retirement can still commit; a retired caller
// receives no value and an indeterminate commit is preserved explicitly.
func (p *sessionSecretProvider) rotateIfRevision(ctx context.Context, handle storage.SecretHandle, revision storage.SecretRevision, value storage.SecretValue) error {
	repo, s, err := p.admitCredential(ctx, handle)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithCancel(ctx)
	joined := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-s.Done():
			cancel()
		case <-joined:
		}
	}()
	rotationErr := repo.RotateCredentialIfRevision(writeCtx, handle, revision, value)
	close(joined)
	cancel()
	<-watcherDone
	if err := p.checkCredentialAfterSQL(context.Background(), repo, s, handle); err != nil {
		var storageErr *storage.Error
		if errors.As(rotationErr, &storageErr) && storageErr.Kind() == storage.SecretCommitUnknown {
			return rotationErr
		}
		if rotationErr == nil {
			return ErrCredentialCommittedRetired
		}
		return err
	}
	if rotationErr == nil && ctx.Err() != nil {
		return ErrCredentialCommittedUnobserved
	}
	return rotationErr
}

func (p *sessionSecretProvider) admitCredential(ctx context.Context, handle storage.SecretHandle) (*storage.SecretRepository, *Session, error) {
	if ctx == nil || p == nil || p.session == nil || p.session.host == nil {
		return nil, nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	target, _, err := handle.Components()
	if err != nil {
		return nil, nil, err
	}
	s := p.session
	h := s.host
	h.mu.Lock()
	repo := h.secrets
	live := s.live() && h.admission == admissionOpen
	requester := s.instance
	h.mu.Unlock()
	if repo == nil {
		return nil, nil, ErrInvalidInput
	}
	if !live {
		if err := repo.AuditDenied(ctx, requester, handle, storage.SecretDenialInactiveSession); err != nil {
			return nil, nil, ErrSecretAuditUnavailable
		}
		return nil, nil, ErrInactiveSession
	}
	if target != requester {
		if err := repo.AuditDenied(ctx, requester, handle, storage.SecretDenialForeignHandle); err != nil {
			return nil, nil, ErrSecretAuditUnavailable
		}
		return nil, nil, ErrForeignSecretHandle
	}
	return repo, s, nil
}

func (p *sessionSecretProvider) checkCredentialAfterSQL(ctx context.Context, repo *storage.SecretRepository, s *Session, handle storage.SecretHandle) error {
	h := s.host
	h.mu.Lock()
	live := s.live() && h.admission == admissionOpen
	h.mu.Unlock()
	if !live {
		auditCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := repo.AuditDenied(auditCtx, s.instance, handle, storage.SecretDenialInactiveSession); err != nil {
			return ErrSecretAuditUnavailable
		}
		return ErrInactiveSession
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

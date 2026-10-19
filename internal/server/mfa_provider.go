package server

import (
	"context"
	"sync"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/storage"
)

type mfaRuntimeState uint8

const (
	mfaRuntimeInvalid mfaRuntimeState = iota
	mfaRuntimePolicyOnly
	mfaRuntimeReady
)

// The runtime borrows the same 027 protector and coordinator as Run. No key,
// account registry, listener, revision or policy is created beside them.
type mfaRuntime struct {
	repository  *storage.MFARepository
	coordinator *sessionCoordinator
	crypto      *authmfa.CryptoGate
	party       authmfa.RelyingParty
	verifier    *authmfa.Verifier
	state       mfaRuntimeState
	closeOnce   sync.Once
	done        chan struct{}
}

func newMFARuntime(ctx context.Context, coordinator *sessionCoordinator, protector *storage.SecretRepository, settings config.PublicSettings) (*mfaRuntime, error) {
	if !validMFAPolicyContext(ctx) || coordinator == nil || coordinator.store == nil || coordinator.policy == nil {
		return nil, authmfa.NewError(authmfa.ErrorInvalidInput)
	}
	repository, err := coordinator.store.NewMFARepository(protector)
	if err != nil {
		return nil, err
	}
	runtime := &mfaRuntime{repository: repository, coordinator: coordinator, crypto: authmfa.NewCryptoGate(), state: mfaRuntimePolicyOnly, done: make(chan struct{})}
	canonical, present := settings.CanonicalURL()
	if !present || canonical.Scheme != "https" {
		return runtime, nil
	}
	party, err := repository.BootstrapMFAInstance(ctx, canonical.String())
	if err != nil {
		return nil, err
	}
	verifier, err := authmfa.NewVerifier(party)
	if err != nil {
		return nil, err
	}
	runtime.party, runtime.verifier, runtime.state = party, verifier, mfaRuntimeReady
	return runtime, nil
}

func (m *mfaRuntime) requireReady() error {
	if m == nil || m.state != mfaRuntimeReady || m.repository == nil || m.verifier == nil {
		return authmfa.NewError(authmfa.ErrorUnavailable)
	}
	return nil
}

func (m *mfaRuntime) Prepare(ctx context.Context, account contracts.AccountID, id contracts.ChallengeID) (factorPreparation, error) {
	if m == nil || m.repository == nil || m.coordinator == nil {
		return factorPreparation{}, authmfa.NewError(authmfa.ErrorUnavailable)
	}
	state, err := m.repository.AccountState(ctx, account)
	if err != nil {
		return factorPreparation{}, err
	}
	requirement, revision, policy, err := state.Requirement()
	if err != nil {
		return factorPreparation{}, err
	}
	result := factorPreparation{revision: revision, policy: policy}
	switch requirement {
	case storage.MFALoginPasswordOnly:
		result.kind = factorPreparationAuthenticated
		return result, nil
	case storage.MFALoginEnrollment:
		if err := m.requireReady(); err != nil {
			return factorPreparation{}, err
		}
		result.kind = factorPreparationEnrollment
		return result, nil
	case storage.MFALoginFactorChallenge:
		if err := m.requireReady(); err != nil {
			return factorPreparation{}, err
		}
		instance, _, _, err := m.party.Values()
		if err != nil {
			return factorPreparation{}, err
		}
		binding, err := authmfa.NewBinding(account, id, instance)
		if err != nil {
			return factorPreparation{}, err
		}
		user, err := m.repository.UserSnapshot(ctx, account, m.party)
		if err != nil {
			return factorPreparation{}, err
		}
		now := m.coordinator.clock.Now()
		ceremony, err := m.verifier.BeginLogin(ctx, binding, user, now)
		if err != nil {
			return factorPreparation{}, err
		}
		options, err := ceremony.AssertionOptions(now)
		if err != nil {
			return factorPreparation{}, err
		}
		result.kind, result.ceremony, result.started = factorPreparationChallenge, ceremony, now
		result.options = []*authv1.FactorOption{{Method: &authv1.FactorOption_Fido2{Fido2: options}}}
		recovery, err := m.repository.RecoveryAvailable(ctx, account)
		if err != nil {
			return factorPreparation{}, err
		}
		if recovery {
			result.options = append(result.options, &authv1.FactorOption{Method: &authv1.FactorOption_RecoveryCode{RecoveryCode: &authv1.RecoveryCodeChallenge{}}})
		}
		return result, nil
	case storage.MFALoginUnspecified:
		return factorPreparation{}, authmfa.NewError(authmfa.ErrorIntegrity)
	}
	return factorPreparation{}, authmfa.NewError(authmfa.ErrorIntegrity)
}

func (m *mfaRuntime) Verify(ctx context.Context, snapshot storage.MFALoginSnapshot, proof *authv1.FactorProof) (authmfa.VerifiedLogin, error) {
	if !validMFAPolicyContext(ctx) {
		return authmfa.VerifiedLogin{}, authmfa.NewError(authmfa.ErrorInvalidInput)
	}
	if err := m.requireReady(); err != nil {
		return authmfa.VerifiedLogin{}, err
	}
	binding, user, ceremony, batch, candidates, err := snapshot.Verification()
	if err != nil {
		return authmfa.VerifiedLogin{}, err
	}
	party, err := ceremony.RelyingParty()
	if err != nil {
		return authmfa.VerifiedLogin{}, err
	}
	instance, rp, origin, err := party.Values()
	expectedInstance, expectedRP, expectedOrigin, expectedErr := m.party.Values()
	if err != nil || expectedErr != nil || instance != expectedInstance || rp != expectedRP || origin != expectedOrigin || proof == nil || len(proof.ProtoReflect().GetUnknown()) != 0 {
		return authmfa.VerifiedLogin{}, authmfa.NewError(authmfa.ErrorInvalidProof)
	}
	switch value := proof.Proof.(type) {
	case *authv1.FactorProof_Fido2:
		if value == nil {
			return authmfa.VerifiedLogin{}, authmfa.NewError(authmfa.ErrorInvalidProof)
		}
		return m.verifier.VerifyLogin(ctx, user, ceremony, value.Fido2, m.coordinator.clock.Now())
	case *authv1.FactorProof_RecoveryCode:
		if value == nil {
			return authmfa.VerifiedLogin{}, authmfa.NewError(authmfa.ErrorInvalidProof)
		}
		code, err := authmfa.ParseRecoveryCode(value.RecoveryCode)
		if err != nil {
			return authmfa.VerifiedLogin{}, authmfa.NewError(authmfa.ErrorInvalidProof)
		}
		return authmfa.VerifyRecoveryCode(binding, ceremony, batch, code, candidates)
	default:
		return authmfa.VerifiedLogin{}, authmfa.NewError(authmfa.ErrorInvalidProof)
	}
}

func (m *mfaRuntime) beginClose() <-chan struct{} {
	if m == nil {
		return nil
	}
	m.closeOnce.Do(func() {
		m.crypto.CloseAdmissions()
		m.coordinator.policy.closeAdmissions()
		go func() {
			// Background observes the actual owners after a bounded Run drain expires;
			// it never asserts that a still-running verifier or transaction is stopped.
			_ = m.crypto.Drain(context.Background())
			<-m.coordinator.policy.Done()
			close(m.done)
		}()
	})
	return m.done
}
func (m *mfaRuntime) Done() <-chan struct{} {
	if m == nil {
		return nil
	}
	return m.done
}
func (m *mfaRuntime) close(ctx context.Context) error {
	done := m.beginClose()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

package integrations

import (
	"crypto/sha256"
	"errors"
	"github.com/damione1/Homeward/internal/authorization"

	"github.com/damione1/Homeward/internal/domain"
)

var (
	ErrCommandPolicyConflict   = errors.New("command ID already belongs to another durability policy")
	ErrCommandPolicyLedgerFull = errors.New("command policy ledger is full for this Host epoch")
)

const maxCommandPolicyEntries = 65536

type commandPolicy uint8

const (
	commandPolicyUnknown commandPolicy = iota
	commandPolicyVolatile
	commandPolicyDurable
)

// claimCommandPolicy is the shared in-process authority for all Host command
// paths. Claims remain until Host shutdown, including when a durable commit is
// uncertain; a DB outage cannot downgrade an ID to volatile dispatch.
func (h *Manager) claimCommandPolicy(id domain.CommandID, policy commandPolicy) error {
	if h.MQTTReadOnlyObservation() {
		return ErrCommandAuthorizationDenied
	}
	if h == nil || h.commandPolicies == nil {
		return ErrInvalidInput
	}
	switch policy {
	case commandPolicyVolatile, commandPolicyDurable:
		// Both policies are valid claims.
	case commandPolicyUnknown:
		return ErrInvalidInput
	default:
		return ErrInvalidInput
	}
	if _, err := id.Value(); err != nil {
		return ErrInvalidInput
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.admission != admissionOpen {
		return ErrAdmissionsClosed
	}
	if prior, exists := h.commandPolicies[id]; exists {
		if prior.policy != policy {
			return ErrCommandPolicyConflict
		}
		return nil
	}
	if len(h.commandPolicies) >= maxCommandPolicyEntries {
		return ErrCommandPolicyLedgerFull
	}
	h.commandPolicies[id] = commandPolicyClaim{policy: policy}
	return nil
}

// The bounded native forclusion entry also owns the one admitted API flight.
// This is completion evidence only, never a response cache or another ledger.
type commandPolicyClaim struct {
	policy    commandPolicy
	apiFlight *commandAPIFlight
}
type commandAPIFlight struct {
	id                     domain.CommandID
	epoch                  string
	target                 authorization.CommandTarget
	fingerprint            [32]byte
	finished               bool
	persistenceUnavailable bool
}

func (h *Manager) beginAPICommandFlight(id domain.CommandID, epoch string, target authorization.CommandTarget, fingerprint []byte) (*commandAPIFlight, error) {
	if h == nil || epoch == "" || len(fingerprint) == 0 {
		return nil, ErrInvalidInput
	}
	if _, _, err := target.Values(); err != nil {
		return nil, ErrInvalidInput
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	claim, found := h.commandPolicies[id]
	if !found || claim.policy != commandPolicyDurable {
		return nil, ErrCommandPolicyConflict
	}
	if claim.apiFlight != nil {
		return nil, ErrCommandIDConflict
	}
	flight := &commandAPIFlight{id: id, epoch: epoch, target: target, fingerprint: sha256.Sum256(fingerprint)}
	claim.apiFlight = flight
	h.commandPolicies[id] = claim
	return flight, nil
}
func (h *Manager) finishAPICommandFlight(flight *commandAPIFlight, persistenceUnavailable bool) {
	if h == nil || flight == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	claim := h.commandPolicies[flight.id]
	if claim.policy != commandPolicyDurable || claim.apiFlight != flight || flight.finished {
		return
	}
	flight.finished = true
	flight.persistenceUnavailable = persistenceUnavailable
}

// Sample before SQL: a finished flight guarantees that its last SQL attempt
// preceded the authoritative read. A concurrently finishing flight is simply
// observed by the next reconciliation request, never guessed from global counts.
func (h *Manager) sampleAPICommandFlight(id domain.CommandID) commandAPIFlight {
	if h == nil {
		return commandAPIFlight{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	claim := h.commandPolicies[id]
	if claim.policy != commandPolicyDurable || claim.apiFlight == nil {
		return commandAPIFlight{}
	}
	return *claim.apiFlight
}
func (flight commandAPIFlight) unavailable(id domain.CommandID, epoch string, target authorization.CommandTarget, fingerprint []byte) bool {
	return flight.finished && flight.persistenceUnavailable && flight.id == id && flight.epoch == epoch && flight.target == target && flight.fingerprint == sha256.Sum256(fingerprint)
}

package integrations

import (
	"errors"

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
	if prior, exists := h.commandPolicies[id]; exists {
		if prior != policy {
			return ErrCommandPolicyConflict
		}
		return nil
	}
	if len(h.commandPolicies) >= maxCommandPolicyEntries {
		return ErrCommandPolicyLedgerFull
	}
	h.commandPolicies[id] = policy
	return nil
}

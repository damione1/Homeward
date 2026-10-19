package server

import (
	"context"
	"time"

	"github.com/damione1/Homeward/internal/storage"
)

const creationReconciliationLimit = 16
const creationReconciliationTimeout = 2 * time.Second

// reconcilePrimary owns exactly this gate. The primary's singleton/row locks
// settle an earlier unknown transaction; refresh then publishes its exact
// policy or removes an absent account. It never retries a creation INSERT.
func (g *sessionGuard) reconcilePrimary(ctx context.Context) error {
	if err := g.coordinator.store.ConfirmAccountSettled(ctx, g.gate.account); err != nil {
		return err
	}
	if err := g.refreshAuthorization(ctx); err != nil {
		return err
	}
	c := g.coordinator
	c.mu.Lock()
	g.gate.quarantined = false
	if g.gate.pendingCreation != nil {
		c.pendingCreations.Remove(g.gate.pendingCreation)
		g.gate.pendingCreation = nil
	}
	c.mu.Unlock()
	return nil
}

// reconcilePendingCreations is called only before a creation takes its new
// account gate. A rotating queue selects at most 16 unpublished creation IDs.
// Gates are tried without waiting and released individually; no recursive
// enter, multiple held gates, background worker, or SQL under c.mu is needed.
func (c *sessionCoordinator) reconcilePendingCreations(ctx context.Context) error {
	if c == nil || c.store == nil || ctx == nil || ctx.Err() != nil {
		return storage.ErrSessionUnavailable
	}
	c.mu.Lock()
	candidates := make([]string, 0, min(creationReconciliationLimit, c.pendingCreations.Len()))
	for count := min(creationReconciliationLimit, c.pendingCreations.Len()); count > 0; count-- {
		element := c.pendingCreations.Front()
		candidates = append(candidates, element.Value.(string)) // private queue stores only account keys
		c.pendingCreations.MoveToBack(element)
	}
	c.mu.Unlock()
	if len(candidates) == 0 {
		return nil
	}
	boundedCtx, cancel := context.WithTimeout(ctx, creationReconciliationTimeout)
	defer cancel()
	for _, key := range candidates {
		c.mu.Lock()
		gate := c.accounts[key]
		if gate == nil || gate.pendingCreation == nil {
			c.mu.Unlock()
			continue
		}
		select {
		case <-gate.permit:
			gate.refs++
		default:
			// This candidate remains in the rotated queue; other accounts do
			// not wait behind a gate already owned by another admission.
			c.mu.Unlock()
			continue
		}
		c.mu.Unlock()
		guard := &sessionGuard{coordinator: c, key: key, gate: gate}
		err := guard.reconcilePrimary(boundedCtx)
		guard.release()
		if err != nil {
			return storage.ErrSessionUnavailable
		}
	}
	c.mu.Lock()
	pending := c.pendingCreations.Len() != 0
	c.mu.Unlock()
	if pending {
		return storage.ErrSessionUnavailable
	}
	return nil
}

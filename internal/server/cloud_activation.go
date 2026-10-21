package server

import (
	"context"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

// One configured Hilo registration is replaced in the existing 096 owner.
// Reservation keeps A usable; only the post-COMMIT quiescence closes it.
type cloudRegistrationReplacement struct {
	owner       *cloudAuthorizationCoordinator
	previous    domain.IntegrationInstanceID
	hasPrevious bool
	next        *CloudAuthorizationRegistration
	quiescing   bool
	joined      bool
	published   bool
	released    bool
	replacing   bool
	blocked     []domain.IntegrationInstanceID
}

func (r *cloudRegistrationReplacement) touches(id domain.IntegrationInstanceID) bool {
	for _, blocked := range r.blocked {
		if blocked == id {
			return true
		}
	}
	return r.hasPrevious && r.previous == id || r.next != nil && r.next.instance == id
}
func (c *cloudAuthorizationCoordinator) replacingRegistration() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pendingReplacement != nil && c.pendingReplacement.replacing
}
func (c *cloudAuthorizationCoordinator) reserveReplacement(previous *domain.IntegrationInstanceID, next *CloudAuthorizationRegistration) (*cloudRegistrationReplacement, CloudAuthorizationBinder, error) {
	if c == nil {
		return nil, nil, integrations.ErrInvalidInput
	}
	if previous != nil {
		if _, err := previous.Value(); err != nil {
			return nil, nil, err
		}
	}
	if next != nil {
		if _, err := NewCloudAuthorizationRegistration(next.instance, next.handle, next.profile, next.policy, next.provider); err != nil {
			return nil, nil, err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || !c.frozen || c.pendingReplacement != nil && !c.pendingReplacement.replacing {
		return nil, nil, ErrCloudAuthorizationClosed
	}
	if before := c.pendingReplacement; before != nil && before.replacing {
		// A cloud registration may have reached B before another operational
		// publication failed. Rebase on this owner's actual committed swap,
		// rather than assuming the still-visible configuration owns that row.
		previous = nil
		var actual domain.IntegrationInstanceID
		if before.published {
			if before.next != nil {
				actual = before.next.instance
			}
		} else if before.hasPrevious {
			actual = before.previous
		}
		if c.registrations[actual] != nil {
			previous = &actual
		}
	} else if previous == nil && next == nil {
		return nil, nil, integrations.ErrInvalidInput
	}
	if previous != nil && c.registrations[*previous] == nil {
		return nil, nil, integrations.ErrInvalidInput
	}
	if next != nil && (previous == nil || *previous != next.instance) && c.registrations[next.instance] != nil {
		return nil, nil, integrations.ErrInvalidInput
	}
	if next != nil && previous == nil && len(c.registrations) == maxCloudAuthorizationRegistrations {
		return nil, nil, ErrCloudAuthorizationBusy
	}
	r := &cloudRegistrationReplacement{owner: c}
	if before := c.pendingReplacement; before != nil {
		blocked := make(map[domain.IntegrationInstanceID]bool)
		for _, id := range before.blocked {
			if c.registrations[id] != nil {
				blocked[id] = true
			}
		}
		if before.hasPrevious && c.registrations[before.previous] != nil {
			blocked[before.previous] = true
		}
		if before.next != nil && c.registrations[before.next.instance] != nil {
			blocked[before.next.instance] = true
		}
		for id := range blocked {
			r.blocked = append(r.blocked, id)
		}
		if len(r.blocked) > maxCloudAuthorizationRegistrations {
			return nil, nil, ErrCloudAuthorizationBusy
		}
		r.quiescing, r.replacing = true, true
	}
	if previous != nil {
		r.previous, r.hasPrevious = *previous, true
	}
	var binder CloudAuthorizationBinder
	if next != nil {
		value := *next
		r.next = &value
		binder = cloudAuthorizationBinderView{coordinator: c, instance: next.instance}
	}
	c.pendingReplacement = r
	return r, binder, nil
}
func (r *cloudRegistrationReplacement) Quiesce(ctx context.Context) error {
	if r == nil || r.owner == nil || nilReactiveInterface(ctx) {
		return integrations.ErrInvalidInput
	}
	c := r.owner
	c.mu.Lock()
	if r.released || r.published || c.pendingReplacement != r || c.closed {
		c.mu.Unlock()
		return ErrCloudAuthorizationClosed
	}
	if r.joined {
		c.mu.Unlock()
		return nil
	}
	r.quiescing = true
	active := make([]*cloudAuthorizationInvocation, 0, len(c.active))
	for handle, invocation := range c.active {
		for id, binding := range c.registrations {
			if r.touches(id) && handle == binding.registration.handle {
				active = append(active, invocation)
				break
			}
		}
	}
	c.mu.Unlock()
	for _, invocation := range active {
		invocation.cancel()
	}
	for _, invocation := range active {
		select {
		case <-invocation.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.mu.Lock()
	r.joined = true
	c.mu.Unlock()
	return nil
}
func (r *cloudRegistrationReplacement) Publish() error {
	if r == nil || r.owner == nil {
		return integrations.ErrInvalidInput
	}
	c := r.owner
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.published {
		return nil
	}
	if r.released || !r.joined || c.pendingReplacement != r || c.closed {
		return ErrCloudAuthorizationClosed
	}
	if r.hasPrevious {
		if old := c.registrations[r.previous]; old != nil && old.client != nil {
			return ErrCloudAuthorizationBusy
		}
		delete(c.registrations, r.previous)
	}
	if r.next != nil {
		c.registrations[r.next.instance] = &cloudAuthorizationBinding{registration: *r.next}
	}
	r.published = true // the owner releases this closed human gate after stamp
	return nil
}
func (r *cloudRegistrationReplacement) Release() {
	if r == nil || r.owner == nil {
		return
	}
	c := r.owner
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.released {
		return
	}
	if r.published {
		if c.pendingReplacement == r {
			c.pendingReplacement = nil
		}
		r.released = true
		return
	}
	if c.pendingReplacement == r && !r.quiescing {
		c.pendingReplacement = nil
		r.released = true
	}
	// A post-COMMIT unresolved replacement remains closed until publication
	// or terminal coordinator close. Release cannot silently restore A.
}

func (r *cloudRegistrationReplacement) Supersede(ctx context.Context) ([]domain.IntegrationInstanceID, error) {
	if r == nil || r.owner == nil || nilReactiveInterface(ctx) {
		return nil, integrations.ErrInvalidInput
	}
	c := r.owner
	c.mu.Lock()
	if r.released {
		c.mu.Unlock()
		return nil, nil
	}
	quiescing, published := r.quiescing, r.published
	c.mu.Unlock()
	if !quiescing && !published {
		r.Release()
		return nil, nil
	}
	if !published {
		if err := r.Quiesce(ctx); err != nil {
			return nil, err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pendingReplacement != r || c.closed {
		return nil, ErrCloudAuthorizationClosed
	}
	r.replacing = true
	forced := append([]domain.IntegrationInstanceID(nil), r.blocked...)
	if r.hasPrevious {
		forced = append(forced, r.previous)
	}
	if r.next != nil {
		forced = append(forced, r.next.instance)
	}
	return forced, nil
}

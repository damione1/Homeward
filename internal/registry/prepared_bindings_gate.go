package registry

import "context"

// PrepareBindings borrows this already-held repository exclusion while the
// activation compiler reserves prospective identities. It uses the ordinary
// registry SQL read and preparation logic without acquiring its own admission
// token a second time. No write, publication or live discovery occurs here.
func (g *ActivationGate) PrepareBindings(ctx context.Context, requests []BindingRequest) (*PreparedBindings, error) {
	if g == nil || g.repo == nil || nilContext(ctx) || len(requests) > 4096 ||
		!g.phase.CompareAndSwap(uint32(activationHeld), uint32(activationPreparing)) {
		return nil, ErrInvalidInput
	}
	defer g.phase.Store(uint32(activationHeld))
	records, err := g.repo.listUnlocked(ctx)
	if err != nil {
		return nil, err
	}
	return g.repo.prepareBindings(records, requests, true)
}

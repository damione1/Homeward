package fake

import (
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

// CaptureLightRoute keeps the command fixture on the same sequencer authority
// as production. It has no independent state or revision for a test to trust.
func (f *Fixture) CaptureLightRoute(route integrations.CommandRoute) (domain.ObservedState, bool, error) {
	if f == nil || f.sequence == nil {
		return domain.ObservedState{}, false, integrations.ErrInvalidInput
	}
	return f.sequence.CaptureLightRoute(route)
}

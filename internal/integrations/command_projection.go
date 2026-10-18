package integrations

import (
	"errors"

	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
)

var (
	ErrCommandRouteUnavailable = errors.New("command route is unavailable")
	ErrCommandRouteAmbiguous   = errors.New("command route has multiple candidate owners")
	ErrCommandRevisionChanged  = errors.New("command target state revision changed")
	ErrCommandEpochChanged     = errors.New("command admission epoch changed")
	ErrCommandRouteBusy        = errors.New("command endpoint has an in-flight dispatch")
)

// CommandRoute is assembled by Host after resolving a session-owned binding.
// The sequencer rechecks every field against its current SQL-backed projection.
type CommandRoute struct {
	Entity           domain.EntityID
	Alias            *domain.HAAlias
	Binding          Binding
	Description      *wireregistry.CapabilityDescription
	Designated       domain.EntityID
	ExpectedRevision *uint64
	Epoch            string
}

// CommandProjection has no SQL or driver I/O. AdmitCommandRoute is called under
// Host and catalogue locks; its own lock is the route linearization point.
type CommandProjection interface {
	ResolveCommandAlias(domain.HAAlias) (domain.EntityID, error)
	AdmitCommandRoute(CommandRoute) error
}

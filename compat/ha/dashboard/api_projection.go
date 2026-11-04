package dashboard

import (
	"github.com/damione1/Homeward/internal/apiresources"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/calendarnames"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/tasknames"
)

// APIProjection supplies one already-authorized immutable cut. Callbacks perform
// no device I/O, mutate no owner and never return hidden identities. Query uses
// the exact EntityView incarnation; Get/List use currently filtered declarations.
// The projector consumes definitions, not observation values or action results.
type APIProjection struct {
	Home         apitypes.HomeName
	Entity       func(domain.HAAlias) (apitypes.EntityName, bool)
	Capabilities func(domain.HAAlias) []apiresources.Capability
	Dashboard    func(authorization.DashboardID) (apitypes.DashboardName, bool)
	Calendar     func(domain.EntityID) (calendarnames.CalendarName, bool)
	TaskList     func(domain.ListID) (tasknames.ListName, bool)
	Navigation   *NavigationIndex
}

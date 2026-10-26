package server

// The development handler uses the same pure native projection as future
// production composition; this adapter does not introduce a persistence service.
import (
	imported "github.com/damione1/Homeward/compat/ha/dashboard"
	dashboardv1 "github.com/damione1/Homeward/gen/home/dashboard/v1"
	"github.com/damione1/Homeward/internal/domain"
)

func projectDashboardLayout(result imported.Result, support *dashboardv1.ClientLayoutSupport,
	resolve func(domain.HAAlias) (domain.EntityID, bool)) (*dashboardv1.NativeLayout, error) {
	layout, err := imported.ProjectNativeLayout(result, imported.LayoutProjection{Support: support, Entity: resolve})
	if err != nil {
		return nil, ErrInvalidPublicPlan
	}
	return layout, nil
}

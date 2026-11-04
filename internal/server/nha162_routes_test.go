package server

import (
	"net/http"
	"testing"

	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
)

func TestNHA162EveryTaskRouteRequiresTLSAndExactPermission(t *testing.T) {
	plan, err := NewPublicPlan(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := registerTaskRoutes(plan, &taskHandler{}); err != nil {
		t.Fatal(err)
	}
	if len(plan.connect) != 1 || len(plan.routes) != len(taskRoutes) {
		t.Fatalf("unexpected task route registration: handlers=%d routes=%d", len(plan.connect), len(plan.routes))
	}
	for _, expected := range taskRoutes {
		route, ok := plan.routes[routeLookup{method: http.MethodPost, path: expected.path}]
		if !ok || route.key.kind != RouteKindConnect || route.access != RouteAccessProtected ||
			route.flow != expected.flow || route.transport != RouteTransportTLSRequired {
			t.Errorf("task route is missing or weak: %s", expected.path)
			continue
		}
		family, operation, err := route.key.permission.Values()
		if err != nil || family != authv1.ScopeFamily_SCOPE_FAMILY_LISTS || operation != expected.op {
			t.Errorf("wrong permission on %s: %v", expected.path, err)
		}
	}
}

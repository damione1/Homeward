package server

import (
	"net/http"
	"testing"

	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
)

func TestNHA162EveryListRouteRequiresTLSAndExactPermission(t *testing.T) {
	plan, err := NewPublicPlan(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := registerListRoutes(plan, &listHandler{}); err != nil {
		t.Fatal(err)
	}
	if len(plan.connect) != 1 || len(plan.routes) != len(listRoutes) {
		t.Fatalf("unexpected list route registration: handlers=%d routes=%d", len(plan.connect), len(plan.routes))
	}
	for _, expected := range listRoutes {
		route, ok := plan.routes[routeLookup{method: http.MethodPost, path: expected.path}]
		if !ok || route.key.kind != RouteKindConnect || route.access != RouteAccessProtected ||
			route.flow != expected.flow || route.transport != RouteTransportTLSRequired {
			t.Errorf("list route is missing or weak: %s", expected.path)
			continue
		}
		family, operation, err := route.key.permission.Values()
		if err != nil || family != authv1.ScopeFamily_SCOPE_FAMILY_LISTS || operation != expected.op {
			t.Errorf("wrong permission on %s: %v", expected.path, err)
		}
	}
}

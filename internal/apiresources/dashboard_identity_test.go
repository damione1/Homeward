package apiresources

import (
	"github.com/damione1/Homeward/internal/apitypes"
	"testing"

	"github.com/damione1/Homeward/internal/authorization"
)

func TestDashboardNamesKeepNativeIdentityAndHomeWithoutInverseLookup(t *testing.T) {
	home, _ := apitypes.ParseHomeName("homes/synthetic")
	otherHome, _ := apitypes.ParseHomeName("homes/other")
	id, _ := authorization.ParseDashboardID("native_dashboard_with_underscores")
	other, _ := authorization.ParseDashboardID("native-dashboard-with-underscores")
	first, err := DashboardNameForIdentity(home, id)
	if err != nil || !first.Valid() || first.Home() != home {
		t.Fatal(first, err)
	}
	repeated, err := DashboardNameForIdentity(home, id)
	if err != nil || repeated != first {
		t.Fatal("unstable identity", err)
	}
	different, _ := DashboardNameForIdentity(home, other)
	foreign, _ := DashboardNameForIdentity(otherHome, id)
	if different == first || foreign == first {
		t.Fatal("native identity or home collapsed")
	}
	parsed, err := apitypes.ParseDashboardName(first.String())
	if err != nil || parsed != first {
		t.Fatal("canonical round trip", err)
	}
	for _, value := range []string{"native_dashboard_with_underscores", "homes/synthetic/entities/example", "homes/synthetic/dashboards/../example"} {
		if _, err := apitypes.ParseDashboardName(value); err == nil {
			t.Fatalf("admitted %q", value)
		}
	}
	if _, err := DashboardNameForIdentity(apitypes.HomeName{}, id); err == nil {
		t.Fatal("zero home")
	}
	if _, err := DashboardNameForIdentity(home, authorization.DashboardID{}); err == nil {
		t.Fatal("zero native identity")
	}
}

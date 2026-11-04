package apitypes

import (
	"strings"
)

// DashboardName identifies a configured resource; it never grants permission.
type DashboardName struct{ value string }

func ParseDashboardName(value string) (DashboardName, error) {
	if err := resourceName(value, "homes", "dashboards"); err != nil {
		return DashboardName{}, err
	}
	return DashboardName{value}, nil
}
func (n DashboardName) String() string { return n.value }
func (n DashboardName) Valid() bool    { return n.value != "" }
func (n DashboardName) Home() HomeName {
	if !n.Valid() {
		return HomeName{}
	}
	return HomeName{n.value[:strings.LastIndex(n.value, "/dashboards/")]}
}

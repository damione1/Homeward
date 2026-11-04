package apitypes

import "strings"

// IntegrationTypeName identifies an admitted API resource, independently
// from native source bindings, source digests and SQL revisions.
type IntegrationTypeName struct {
	home HomeName
	id   string
}

func ParseIntegrationTypeName(value string) (IntegrationTypeName, error) {
	if err := resourceName(value, "homes", "integrationTypes"); err != nil {
		return IntegrationTypeName{}, err
	}
	index := strings.LastIndex(value, "/integrationTypes/")
	return IntegrationTypeName{home: HomeName{value: value[:index]}, id: value[index+len("/integrationTypes/"):]}, nil
}
func (v IntegrationTypeName) Valid() bool        { return v.home.Valid() && v.id != "" }
func (v IntegrationTypeName) Home() HomeName     { return v.home }
func (v IntegrationTypeName) ResourceID() string { return v.id }
func (v IntegrationTypeName) String() string {
	if !v.Valid() {
		return ""
	}
	return v.home.String() + "/integrationTypes/" + v.id
}

// IntegrationInstanceName identifies an admitted API resource, independently
// from native source bindings, source digests and SQL revisions.
type IntegrationInstanceName struct {
	home HomeName
	id   string
}

func ParseIntegrationInstanceName(value string) (IntegrationInstanceName, error) {
	if err := resourceName(value, "homes", "integrationInstances"); err != nil {
		return IntegrationInstanceName{}, err
	}
	index := strings.LastIndex(value, "/integrationInstances/")
	return IntegrationInstanceName{home: HomeName{value: value[:index]}, id: value[index+len("/integrationInstances/"):]}, nil
}
func (v IntegrationInstanceName) Valid() bool        { return v.home.Valid() && v.id != "" }
func (v IntegrationInstanceName) Home() HomeName     { return v.home }
func (v IntegrationInstanceName) ResourceID() string { return v.id }
func (v IntegrationInstanceName) String() string {
	if !v.Valid() {
		return ""
	}
	return v.home.String() + "/integrationInstances/" + v.id
}

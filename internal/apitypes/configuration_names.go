package apitypes

import "strings"

// ConfigurationDocumentName identifies an admitted API resource, independently
// from native source bindings, source digests and SQL revisions.
type ConfigurationDocumentName struct {
	home HomeName
	id   string
}

func ParseConfigurationDocumentName(value string) (ConfigurationDocumentName, error) {
	if err := resourceName(value, "homes", "configurationDocuments"); err != nil {
		return ConfigurationDocumentName{}, err
	}
	index := strings.LastIndex(value, "/configurationDocuments/")
	return ConfigurationDocumentName{home: HomeName{value: value[:index]}, id: value[index+len("/configurationDocuments/"):]}, nil
}
func (v ConfigurationDocumentName) Valid() bool        { return v.home.Valid() && v.id != "" }
func (v ConfigurationDocumentName) Home() HomeName     { return v.home }
func (v ConfigurationDocumentName) ResourceID() string { return v.id }
func (v ConfigurationDocumentName) String() string {
	if !v.Valid() {
		return ""
	}
	return v.home.String() + "/configurationDocuments/" + v.id
}

// ConfigurationPlanName identifies a bounded diagnostic capture, never an
// authorization token or activation receipt.
type ConfigurationPlanName struct {
	home HomeName
	id   string
}

func ParseConfigurationPlanName(value string) (ConfigurationPlanName, error) {
	if err := resourceName(value, "homes", "configurationPlans"); err != nil {
		return ConfigurationPlanName{}, err
	}
	index := strings.LastIndex(value, "/configurationPlans/")
	return ConfigurationPlanName{home: HomeName{value: value[:index]}, id: value[index+len("/configurationPlans/"):]}, nil
}
func (v ConfigurationPlanName) Valid() bool    { return v.home.Valid() && v.id != "" }
func (v ConfigurationPlanName) Home() HomeName { return v.home }
func (v ConfigurationPlanName) String() string {
	if !v.Valid() {
		return ""
	}
	return v.home.String() + "/configurationPlans/" + v.id
}

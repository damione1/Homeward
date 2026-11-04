package apitypes

import (
	"strconv"
	"strings"
)

type ConfigurationRevisionName struct {
	value    string
	parent   ConfigurationDocumentName
	revision int64
}
type ConfigurationGenerationName struct {
	value    string
	parent   HomeName
	revision int64
}
type ConfigurationSchemaName struct {
	value  string
	parent HomeName
}

func (n ConfigurationRevisionName) String() string                    { return n.value }
func (n ConfigurationRevisionName) Parent() ConfigurationDocumentName { return n.parent }
func (n ConfigurationRevisionName) Revision() int64                   { return n.revision }
func (n ConfigurationGenerationName) String() string                  { return n.value }
func (n ConfigurationGenerationName) Parent() HomeName                { return n.parent }
func (n ConfigurationGenerationName) Revision() int64                 { return n.revision }
func (n ConfigurationSchemaName) String() string                      { return n.value }
func (n ConfigurationSchemaName) Parent() HomeName                    { return n.parent }
func configurationRevisionID(id, prefix string) (int64, error) {
	if !strings.HasPrefix(id, prefix) {
		return 0, ErrResourceName
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(id, prefix), 10, 64)
	if err != nil || n < 1 || id != prefix+strconv.FormatInt(n, 10) {
		return 0, ErrResourceName
	}
	return n, nil
}
func ParseConfigurationRevisionName(value string) (ConfigurationRevisionName, error) {
	if resourceName(value, "homes", "configurationDocuments", "configurationRevisions") != nil {
		return ConfigurationRevisionName{}, ErrResourceName
	}
	parent, id, _ := strings.Cut(value, "/configurationRevisions/")
	p, err := ParseConfigurationDocumentName(parent)
	if err != nil {
		return ConfigurationRevisionName{}, err
	}
	n, err := configurationRevisionID(id, "r-")
	if err != nil {
		return ConfigurationRevisionName{}, err
	}
	return ConfigurationRevisionName{value, p, n}, nil
}
func ParseConfigurationGenerationName(value string) (ConfigurationGenerationName, error) {
	if resourceName(value, "homes", "configurationGenerations") != nil {
		return ConfigurationGenerationName{}, ErrResourceName
	}
	parent, id, _ := strings.Cut(value, "/configurationGenerations/")
	p, err := ParseHomeName(parent)
	if err != nil {
		return ConfigurationGenerationName{}, err
	}
	n, err := configurationRevisionID(id, "g-")
	if err != nil {
		return ConfigurationGenerationName{}, err
	}
	return ConfigurationGenerationName{value, p, n}, nil
}
func ParseConfigurationSchemaName(value string) (ConfigurationSchemaName, error) {
	if resourceName(value, "homes", "configurationSchemas") != nil {
		return ConfigurationSchemaName{}, ErrResourceName
	}
	parent, _, _ := strings.Cut(value, "/configurationSchemas/")
	p, err := ParseHomeName(parent)
	if err != nil {
		return ConfigurationSchemaName{}, err
	}
	return ConfigurationSchemaName{value, p}, nil
}

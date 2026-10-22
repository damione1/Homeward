package configactivation

import (
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/storage"
)

// ReplaceWorkspaceDefinitions binds the exact namespace used by 149 to the
// same replacement that 029 persists. Retention copies current SQL metadata;
// ordinary non-workspace ReplaceDefinitions deliberately carries no locator.
func ReplaceWorkspaceDefinitions(group storage.ConfigDocumentID, candidate bundle.Candidate, bindings []storage.ConfigDocumentBinding) (DefinitionChoice, error) {
	choice, err := ReplaceDefinitions(candidate, bindings)
	if err != nil {
		return DefinitionChoice{}, err
	}
	locator, err := storage.NewDeclarativeWorkspaceLocator(group)
	if err != nil {
		return DefinitionChoice{}, ErrInvalidStartup
	}
	choice.workspace = locator
	return choice, nil
}

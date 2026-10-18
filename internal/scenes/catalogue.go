// Package scenes prepares bounded, in-memory scene intentions. It owns no
// device state, command ledger or background activation loop.
package scenes

import (
	"errors"
	"io/fs"
	"strings"

	hascene "github.com/damione1/Homeward/compat/ha/scene"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/logicselection"
)

var ErrInvalidScene = errors.New("scene definition or reference is invalid")

// Catalogue retains only the twelve NHA-158 scene definitions; NHA-158 itself
// supplies the alias, source path and digest. No second manifest parser exists.
type Catalogue struct {
	definitions map[domain.HAAlias]hascene.Definition
	order       []domain.HAAlias
}

func CompileSelected(selection logicselection.Selection, sources fs.FS) (Catalogue, error) {
	if sources == nil {
		return Catalogue{}, ErrInvalidScene
	}
	definitions := make(map[domain.HAAlias]hascene.Definition, 12)
	order := make([]domain.HAAlias, 0, 12)
	for _, selected := range selection.Retained() {
		id := selected.ID()
		name, err := id.Value()
		if err != nil {
			return Catalogue{}, ErrInvalidScene
		}
		if !strings.HasPrefix(name, "scene.") {
			continue
		}
		path := selected.Path()
		ref, err := hascene.NewSourceRef(id, path, selected.Digest())
		if err != nil || !strings.HasPrefix(path, "scope/logic_mvp_sources/") {
			return Catalogue{}, ErrInvalidScene
		}
		data, err := fs.ReadFile(sources, strings.TrimPrefix(path, "scope/"))
		if err != nil {
			return Catalogue{}, err
		}
		result, err := hascene.Load(ref, data)
		if err != nil || len(result.Rejected()) != 0 || len(result.Accepted()) != 1 {
			return Catalogue{}, ErrInvalidScene
		}
		if _, exists := definitions[id]; exists {
			return Catalogue{}, ErrInvalidScene
		}
		definitions[id] = result.Accepted()[0]
		order = append(order, id)
	}
	if len(definitions) != 12 {
		return Catalogue{}, ErrInvalidScene
	}
	return Catalogue{definitions: definitions, order: order}, nil
}

func (c Catalogue) Definition(id domain.HAAlias) (hascene.Definition, bool) {
	definition, ok := c.definitions[id]
	if !ok {
		return hascene.Definition{}, false
	}
	// Definition has no exported mutable fields; its Members accessor returns
	// a detached slice with detached optional values.
	return definition, true
}

func (c Catalogue) IDs() []domain.HAAlias {
	return append([]domain.HAAlias(nil), c.order...)
}

package automationruns

import (
	"crypto/sha256"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/storage"
)

// DefinitionProvenance identifies the exact admitted program image. A fixture
// has an explicit variant; neither a Controller generation nor the currently
// active SQL generation can be substituted for a bound image.
type DefinitionProvenance struct {
	kind     ProvenanceKind
	revision storage.ConfigGenerationRevision
	manifest [sha256.Size]byte
}

type ProvenanceKind uint8

const (
	ProvenanceInvalid ProvenanceKind = iota
	UnboundFixturePlan
	BoundProgram
)

func NewBoundProgram(revision storage.ConfigGenerationRevision, manifest [sha256.Size]byte) (DefinitionProvenance, error) {
	if !revision.Valid() || manifest == [sha256.Size]byte{} {
		return DefinitionProvenance{}, ErrInvalidPlan
	}
	return DefinitionProvenance{kind: BoundProgram, revision: revision, manifest: manifest}, nil
}

func FixtureProvenance() DefinitionProvenance {
	return DefinitionProvenance{kind: UnboundFixturePlan}
}

func (p DefinitionProvenance) Valid() bool {
	switch p.kind {
	case UnboundFixturePlan:
		return !p.revision.Valid() && p.manifest == [sha256.Size]byte{}
	case BoundProgram:
		return p.revision.Valid() && p.manifest != [sha256.Size]byte{}
	default:
		return false
	}
}

func (p DefinitionProvenance) Kind() ProvenanceKind { return p.kind }

func (p DefinitionProvenance) Bound() (storage.ConfigGenerationRevision, [sha256.Size]byte, bool) {
	if !p.Valid() || p.kind != BoundProgram {
		return storage.ConfigGenerationRevision{}, [sha256.Size]byte{}, false
	}
	return p.revision, p.manifest, true
}

// WithProvenance fixes the stamp before the plan is handed to an Interpreter or
// Controller. The future SQL-to-IR composition (NHA-115) must call this with
// the generation whose exact stored sources it compiled and admitted.
func (p Plan) WithProvenance(stamp DefinitionProvenance) (Plan, error) {
	if !p.Valid() || !stamp.Valid() {
		return Plan{}, ErrInvalidPlan
	}
	items := make(map[string]automationSpec, len(p.items))
	for id, spec := range p.items {
		spec.provenance = stamp
		items[id] = spec
	}
	scripts := make(map[ir.ScriptID]automationSpec, len(p.scripts))
	for id, spec := range p.scripts {
		spec.provenance = stamp
		scripts[id] = spec
	}
	p.items, p.scripts = items, scripts
	p.provenance = stamp
	return p, nil
}

func (p Plan) Provenance() DefinitionProvenance { return p.provenance }

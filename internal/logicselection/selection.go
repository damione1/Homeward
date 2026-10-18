// Package logicselection admits the documentary NHA-158 MVP logic amendment.
// Admission selects source definitions only: it neither qualifies execution
// support nor activates or dispatches any automation, script, or scene.
package logicselection

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"reflect"
	"sort"
	"strings"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/scope"
)

var (
	ErrInvalidAmendment     = errors.New("invalid MVP logic amendment")
	ErrBaselineMismatch     = errors.New("MVP logic baseline mismatch")
	ErrMissingLogicID       = errors.New("missing MVP logic ID")
	ErrDuplicateLogicID     = errors.New("duplicate MVP logic ID")
	ErrOverlappingLogicID   = errors.New("overlapping MVP logic ID")
	ErrUnknownLogicID       = errors.New("unknown MVP logic ID")
	ErrDefinitionProvenance = errors.New("invalid MVP definition provenance")
	ErrLogicActivation      = errors.New("MVP logic activation forbidden")
)

// Disposition is a closed selection result; zero means no admitted decision.
type Disposition uint8

const (
	DispositionUnspecified Disposition = iota
	DispositionRetained
	DispositionDeferred
)

// DecisionRef records the accepted documentary decision for a selection.
type DecisionRef uint8

const (
	DecisionUnspecified DecisionRef = iota
	DecisionQ32
	DecisionQ33
	DecisionQ34
	DecisionQ37
)

type sourceKind uint8

const (
	sourceUnspecified sourceKind = iota
	sourceAutomation
	sourceScript
	sourceScene
)

// Definition has no executable representation. The digest identifies the
// exact, embedded source YAML block; callers receive values, not mutable data.
type Definition struct {
	id             domain.HAAlias
	digest         [sha256.Size]byte
	sourceDocument string
	decision       DecisionRef
}

func (d Definition) ID() domain.HAAlias        { return d.id }
func (d Definition) Digest() [sha256.Size]byte { return d.digest }
func (d Definition) SourceDocument() string    { return d.sourceDocument }
func (d Definition) Decision() DecisionRef     { return d.decision }

// Selection is an immutable, validated partition of the frozen logic corpus.
// Its zero value admits no identity.
type Selection struct {
	retained []Definition
	byID     map[domain.HAAlias]Disposition
	valid    bool
}

func (s Selection) Retained() []Definition {
	if !s.valid {
		return nil
	}
	return append([]Definition(nil), s.retained...)
}

func (s Selection) DispositionFor(id domain.HAAlias) (Disposition, bool) {
	if !s.valid {
		return DispositionUnspecified, false
	}
	value, ok := s.byID[id]
	return value, ok
}

type rawAmendment struct {
	Version             int           `json:"version"`
	BaselineLogicSHA256 string        `json:"baseline_logic_sha256"`
	ManifestIDsSHA256   string        `json:"manifest_ids_sha256"`
	DecisionDocument    rawDocument   `json:"decision_document"`
	LotDocument         rawDocument   `json:"lot_document"`
	SourceDocuments     []rawDocument `json:"source_documents"`
	Retained            []rawRetained `json:"retained"`
	Deferred            []rawDeferred `json:"deferred"`
	DeferredDecisionRef string        `json:"deferred_decision_ref"`
}

type rawDocument struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

type rawRetained struct {
	EntityID    string        `json:"entity_id"`
	Active      *bool         `json:"active"`
	DecisionRef string        `json:"decision_ref"`
	Definition  rawDefinition `json:"definition"`
}

type rawDeferred struct {
	EntityID string `json:"entity_id"`
	Active   *bool  `json:"active"`
}

type rawDefinition struct {
	Path           string `json:"path"`
	SHA256         string `json:"sha256"`
	SourceDocument string `json:"source_document"`
	SourceSection  string `json:"source_section"`
}

type rawBaseline struct {
	Version int `json:"version"`
	Objects []struct {
		EntityID string `json:"entity_id"`
		Source   string `json:"source"`
		Active   *bool  `json:"active"`
		Decision string `json:"decision"`
	} `json:"objects"`
}

// LoadV1 reads the embedded assets and the verified v1 manifest. It performs
// no provider, database, network, or action work and is not wired into Run.
func LoadV1() (Selection, error) {
	manifest, err := registry.ParseManifest(scope.ManifestV1())
	if err != nil {
		return Selection{}, err
	}
	return Parse(scope.LogicMVPV1(), scope.LogicV1(), manifest, scope.LogicMVPSourcesV1())
}

// Parse admits only an exact, inactive partition of the NHA-002 logic corpus.
// The independent Python scope gate checks source-document fingerprints
// against compat/ha/catalogue.json; source block digests are checked here.
func Parse(data, baseline []byte, manifest registry.Manifest, sources fs.FS) (Selection, error) {
	if nilFS(sources) {
		return Selection{}, ErrInvalidAmendment
	}
	manifestDigest, ok := manifest.Digest()
	if !ok {
		return Selection{}, ErrBaselineMismatch
	}
	var raw rawAmendment
	if err := decodeStrict(data, &raw); err != nil || raw.Version != 1 {
		return Selection{}, ErrInvalidAmendment
	}
	if !equalDigest(raw.ManifestIDsSHA256, manifestDigest[:]) ||
		!equalDigest(raw.BaselineLogicSHA256, sha256Sum(baseline)) {
		return Selection{}, ErrBaselineMismatch
	}
	if raw.DecisionDocument.File != "MVP serveur - décisions et questions.md" ||
		!validDigest(raw.DecisionDocument.SHA256) ||
		raw.LotDocument.File != "MVP serveur - premier lot de logique proposé.md" ||
		!validDigest(raw.LotDocument.SHA256) || parseDecision(raw.DeferredDecisionRef) != DecisionQ37 ||
		len(raw.SourceDocuments) != 2 {
		return Selection{}, ErrInvalidAmendment
	}
	docs := make(map[string]struct{}, 2)
	for _, document := range raw.SourceDocuments {
		if document.File != "Automatisations.md" && document.File != "Scripts et scènes.md" {
			return Selection{}, ErrInvalidAmendment
		}
		if !validDigest(document.SHA256) {
			return Selection{}, ErrInvalidAmendment
		}
		if _, exists := docs[document.File]; exists {
			return Selection{}, ErrInvalidAmendment
		}
		docs[document.File] = struct{}{}
	}
	if len(docs) != 2 {
		return Selection{}, ErrInvalidAmendment
	}
	var frozen rawBaseline
	if err := decodeStrict(baseline, &frozen); err != nil || frozen.Version != 1 || len(frozen.Objects) != 89 {
		return Selection{}, ErrBaselineMismatch
	}
	known := make(map[domain.HAAlias]sourceKind, 89)
	counts := map[sourceKind]int{}
	for _, object := range frozen.Objects {
		id, err := domain.ParseHAAlias(object.EntityID)
		kind := parseSource(object.Source, object.EntityID)
		if err != nil || kind == sourceUnspecified ||
			object.Active == nil || *object.Active || object.Decision != "pending" {
			return Selection{}, ErrBaselineMismatch
		}
		if _, exists := known[id]; exists {
			return Selection{}, ErrBaselineMismatch
		}
		known[id] = kind
		counts[kind]++
	}
	if counts[sourceAutomation] != 50 || counts[sourceScript] != 22 || counts[sourceScene] != 17 {
		return Selection{}, ErrBaselineMismatch
	}
	selection := Selection{retained: make([]Definition, 0, 17), byID: make(map[domain.HAAlias]Disposition, 89)}
	retainedCounts := map[sourceKind]int{}
	for _, item := range raw.Retained {
		id, kind, err := admitID(item.EntityID, known)
		if err != nil {
			return Selection{}, err
		}
		if _, exists := selection.byID[id]; exists {
			return Selection{}, fmt.Errorf("%w: %s", ErrDuplicateLogicID, item.EntityID)
		}
		if item.Active == nil {
			return Selection{}, ErrInvalidAmendment
		}
		if *item.Active {
			return Selection{}, fmt.Errorf("%w: %s", ErrLogicActivation, item.EntityID)
		}
		decision := parseDecision(item.DecisionRef)
		if decision == DecisionUnspecified || decision != expectedDecision(item.EntityID) ||
			(kind != sourceAutomation && kind != sourceScene) {
			return Selection{}, ErrInvalidAmendment
		}
		definition, err := admitDefinition(item.Definition, item.EntityID, kind, docs, sources)
		if err != nil {
			return Selection{}, err
		}
		definition.id = id
		definition.decision = decision
		selection.retained = append(selection.retained, definition)
		selection.byID[id] = DispositionRetained
		retainedCounts[kind]++
	}
	for _, item := range raw.Deferred {
		id, _, err := admitID(item.EntityID, known)
		if err != nil {
			return Selection{}, err
		}
		if previous, exists := selection.byID[id]; exists {
			if previous == DispositionRetained {
				return Selection{}, fmt.Errorf("%w: %s", ErrOverlappingLogicID, item.EntityID)
			}
			return Selection{}, fmt.Errorf("%w: %s", ErrDuplicateLogicID, item.EntityID)
		}
		if item.Active == nil {
			return Selection{}, ErrInvalidAmendment
		}
		if *item.Active {
			return Selection{}, fmt.Errorf("%w: %s", ErrLogicActivation, item.EntityID)
		}
		selection.byID[id] = DispositionDeferred
	}
	missing := make([]string, 0)
	for id := range known {
		if _, present := selection.byID[id]; !present {
			value, _ := id.Value()
			missing = append(missing, value)
		}
	}
	if len(missing) != 0 {
		sort.Strings(missing)
		return Selection{}, fmt.Errorf("%w: %s", ErrMissingLogicID, missing[0])
	}
	if len(raw.Retained) != 17 || len(raw.Deferred) != 72 ||
		retainedCounts[sourceScene] != 12 || retainedCounts[sourceAutomation] != 5 {
		return Selection{}, ErrInvalidAmendment
	}
	files, err := fs.Glob(sources, "logic_mvp_sources/*.yaml")
	if err != nil || len(files) != 17 {
		return Selection{}, ErrDefinitionProvenance
	}
	for _, file := range files {
		idText := strings.TrimSuffix(strings.TrimPrefix(file, "logic_mvp_sources/"), ".yaml")
		id, err := domain.ParseHAAlias(idText)
		if err != nil || selection.byID[id] != DispositionRetained {
			return Selection{}, ErrDefinitionProvenance
		}
	}
	selection.valid = true
	return selection, nil
}

func admitID(value string, known map[domain.HAAlias]sourceKind) (domain.HAAlias, sourceKind, error) {
	id, err := domain.ParseHAAlias(value)
	if err != nil {
		return domain.HAAlias{}, sourceUnspecified, ErrUnknownLogicID
	}
	kind, exists := known[id]
	if !exists {
		return domain.HAAlias{}, sourceUnspecified, fmt.Errorf("%w: %s", ErrUnknownLogicID, value)
	}
	return id, kind, nil
}

func admitDefinition(raw rawDefinition, alias string, kind sourceKind, docs map[string]struct{}, sources fs.FS) (Definition, error) {
	wantDoc := "Automatisations.md"
	if kind == sourceScene {
		wantDoc = "Scripts et scènes.md"
	}
	wantPath := "scope/logic_mvp_sources/" + alias + ".yaml"
	if raw.Path != wantPath || raw.SourceSection != alias || raw.SourceDocument != wantDoc {
		return Definition{}, ErrDefinitionProvenance
	}
	if _, exists := docs[raw.SourceDocument]; !exists {
		return Definition{}, ErrDefinitionProvenance
	}
	if !validDigest(raw.SHA256) {
		return Definition{}, ErrDefinitionProvenance
	}
	digest, _ := hex.DecodeString(raw.SHA256)
	contents, err := fs.ReadFile(sources, strings.TrimPrefix(raw.Path, "scope/"))
	if err != nil || len(contents) == 0 {
		return Definition{}, ErrDefinitionProvenance
	}
	actual := sha256.Sum256(contents)
	if !bytes.Equal(digest, actual[:]) {
		return Definition{}, ErrDefinitionProvenance
	}
	return Definition{digest: actual, sourceDocument: raw.SourceDocument}, nil
}

func parseSource(value, alias string) sourceKind {
	switch {
	case value == "automation" && strings.HasPrefix(alias, "automation."):
		return sourceAutomation
	case value == "script" && strings.HasPrefix(alias, "script."):
		return sourceScript
	case value == "homeassistant" && strings.HasPrefix(alias, "scene."):
		return sourceScene
	default:
		return sourceUnspecified
	}
}

func parseDecision(value string) DecisionRef {
	switch value {
	case "Q32":
		return DecisionQ32
	case "Q33":
		return DecisionQ33
	case "Q34":
		return DecisionQ34
	case "Q37":
		return DecisionQ37
	default:
		return DecisionUnspecified
	}
}

func expectedDecision(alias string) DecisionRef {
	switch alias {
	case "automation.heat_the_office_on_work_day":
		return DecisionQ33
	case "automation.turn_on_lights_at_sunset":
		return DecisionQ34
	case "automation.ikea_kitchen_lights_remote",
		"automation.turn_off_basement_climate_the_evening",
		"automation.turn_off_heating_in_guest_bedroom_the_day",
		"scene.kitchen_cooking_mode",
		"scene.kitchen_cosy_mode",
		"scene.night_mode",
		"scene.cosy",
		"scene.living_room_daylight",
		"scene.living_room_in_between",
		"scene.turn_off_living_room",
		"scene.turn_off_main_floor",
		"scene.ceilling_fan_day",
		"scene.ceilling_fan_night",
		"scene.climate_day_temperature",
		"scene.climate_night_temperature":
		return DecisionQ32
	default:
		return DecisionUnspecified
	}
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return ErrInvalidAmendment
	}
	return nil
}

func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

func equalDigest(value string, expected []byte) bool {
	return validDigest(value) && value == hex.EncodeToString(expected)
}

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

func nilFS(source fs.FS) bool {
	if source == nil {
		return true
	}
	value := reflect.ValueOf(source)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

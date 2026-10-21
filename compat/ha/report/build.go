package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"reflect"
	"sort"
	"strings"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/nha047"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/compat/ha/scene"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/logicselection"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/scope"
)

type Input struct {
	Selection     logicselection.Selection
	Sources       fs.FS
	Manifest      registry.Manifest
	Catalogue     ir.Catalogue
	Configuration ConfigurationRef
	Declarations  []Declaration
	Evidence      nha047.Evidence
	Candidates    []Candidate
	Blueprints    []parser.SourceFile
}

func BuildMVP(catalogue ir.Catalogue, configuration ConfigurationRef, declarations []Declaration, evidence nha047.Evidence, candidates []Candidate, blueprints []parser.SourceFile) (Report, error) {
	selection, err := logicselection.LoadV1()
	if err != nil {
		return Report{}, err
	}
	manifest, err := registry.ParseManifest(scope.ManifestV1())
	if err != nil {
		return Report{}, err
	}
	return Build(Input{selection, scope.LogicMVPSourcesV1(), manifest, catalogue, configuration, declarations, evidence, candidates, blueprints})
}

type builder struct {
	input        Input
	entities     map[domain.HAAlias]ir.Entity
	declarations map[domain.HAAlias]Declaration
	candidates   map[domain.HAAlias]Candidate
	nodes        int
}

func nilSource(source fs.FS) bool {
	if source == nil {
		return true
	}
	v := reflect.ValueOf(source)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// readSource admits metadata before materialization and also bounds streams
// whose reported size is absent or inaccurate. The extra byte detects a limit
// breach; no ReadFileFS shortcut can bypass this boundary.
func readSource(source fs.FS, name string, maximum int) (data []byte, err error) {
	file, err := source.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info == nil || info.IsDir() {
		return nil, fs.ErrInvalid
	}
	if info.Size() > int64(maximum) {
		return nil, ErrLimit
	}
	data, err = io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	if len(data) > maximum {
		return nil, ErrLimit
	}
	return data, err
}

func Build(input Input) (Report, error) {
	if nilSource(input.Sources) || !input.Catalogue.Valid() || !input.Configuration.valid() || !input.Evidence.Valid() {
		return Report{}, ErrInvalidInput
	}
	manifestDigest, ok := input.Manifest.Digest()
	if !ok {
		return Report{}, ErrInvalidInput
	}
	canonical, err := logicselection.LoadV1()
	if err != nil {
		return Report{}, err
	}
	entries := input.Selection.Entries()
	if len(entries) != 89 || len(input.Selection.Retained()) != 17 {
		return Report{}, ErrInvalidInput
	}
	for _, entry := range entries {
		want, present := canonical.DispositionFor(entry.ID())
		if !present || want != entry.Disposition() {
			return Report{}, ErrInvalidInput
		}
	}
	canonicalDefinitions := make(map[domain.HAAlias]logicselection.Definition, 17)
	for _, definition := range canonical.Retained() {
		canonicalDefinitions[definition.ID()] = definition
	}
	for _, definition := range input.Selection.Retained() {
		want, present := canonicalDefinitions[definition.ID()]
		if !present || definition.Path() != want.Path() || definition.Digest() != want.Digest() || definition.SourceDocument() != want.SourceDocument() || definition.Decision() != want.Decision() {
			return Report{}, ErrInvalidInput
		}
	}
	b := builder{input: input, entities: map[domain.HAAlias]ir.Entity{}, declarations: map[domain.HAAlias]Declaration{}, candidates: map[domain.HAAlias]Candidate{}}
	for _, entity := range input.Catalogue.Entities() {
		b.entities[entity.Alias()] = entity
	}
	if len(input.Declarations) > 850 || len(input.Candidates) > 17 || len(input.Blueprints) > 256 {
		return Report{}, ErrLimit
	}
	for _, d := range input.Declarations {
		if _, err := NewDeclaration(d.id, d.description, d.conversion); err != nil {
			return Report{}, err
		}
		if _, exists := b.declarations[d.id]; exists {
			return Report{}, ErrInvalidInput
		}
		b.declarations[d.id] = d
	}
	for _, c := range input.Candidates {
		if !c.valid() {
			return Report{}, ErrInvalidInput
		}
		if disposition, ok := input.Selection.DispositionFor(c.id); !ok || disposition != logicselection.DispositionRetained {
			return Report{}, ErrInvalidInput
		}
		if _, exists := b.candidates[c.id]; exists {
			return Report{}, ErrInvalidInput
		}
		b.candidates[c.id] = c
	}
	result := Report{configuration: input.Configuration, baseline: nha047.Hash(scope.LogicV1()), manifest: nha047.Digest(manifestDigest), objects: make([]Object, 0, 17), deferred: make([]domain.HAAlias, 0, 72)}
	// Hash the admitted partition plus definition identities, not unrelated JSON
	// bytes. The renderer names this digest kind explicitly.
	type selectionRow struct {
		ID          string
		Disposition logicselection.Disposition
		Path        string
		Digest      string
	}
	rows := make([]selectionRow, 0, 89)
	definitions := map[domain.HAAlias]logicselection.Definition{}
	for _, d := range input.Selection.Retained() {
		definitions[d.ID()] = d
	}
	for _, entry := range entries {
		id, _ := entry.ID().Value()
		row := selectionRow{ID: id, Disposition: entry.Disposition()}
		if d, ok := definitions[entry.ID()]; ok {
			row.Path = d.Path()
			row.Digest = nha047.Digest(d.Digest()).String()
		}
		rows = append(rows, row)
		if entry.Disposition() == logicselection.DispositionDeferred {
			result.deferred = append(result.deferred, entry.ID())
		}
	}
	encoded, err := json.Marshal(rows)
	if err != nil {
		return Report{}, err
	}
	result.selection = nha047.Hash(encoded)
	total := 0
	for _, file := range input.Blueprints {
		size := file.ByteLen()
		if size > MaxSourceBytes || size > MaxInputBytes-total {
			return Report{}, ErrLimit
		}
		total += size
	}
	for _, c := range input.Candidates {
		// The pair's original is an additional input, distinct from both the
		// canonical documentary source and the proposed destination snapshot.
		if c.origin == ExplicitImportPair {
			size := c.originalFile.ByteLen()
			if size > MaxSourceBytes || size > MaxInputBytes-total {
				return Report{}, ErrLimit
			}
			total += size
		}
		size := c.file.ByteLen()
		if size > MaxSourceBytes || size > MaxInputBytes-total {
			return Report{}, ErrLimit
		}
		total += size
	}
	for _, d := range input.Selection.Retained() {
		data, readErr := readSource(input.Sources, strings.TrimPrefix(d.Path(), "scope/"), min(MaxSourceBytes, MaxInputBytes-total))
		if errors.Is(readErr, ErrLimit) {
			return Report{}, ErrLimit
		}
		total += len(data)
		object := Object{definition: d, verdict: Refused, activation: BusinessPending, nodes: []Construction{}}
		candidate, adapted := b.candidates[d.ID()]
		if adapted {
			object.candidateSource = candidate.CandidateSource()
			if candidate.origin == ExplicitImportPair {
				object.originalSource = candidate.OriginalSource()
			}
		}
		if readErr != nil {
			object.refusal = SourceUnavailable
			result.objects = append(result.objects, object)
			continue
		}
		if nha047.Hash(data) != nha047.Digest(d.Digest()) {
			object.refusal = InvalidInput
			result.objects = append(result.objects, object)
			continue
		}
		file, err := parser.NewSourceFile(d.Path(), data)
		if err != nil {
			object.refusal = Syntax
			result.objects = append(result.objects, object)
			continue
		}
		if !object.originalSource.Valid() {
			object.originalSource = SourceProvenance{Frozen158, file.Path(), nha047.Digest(file.SHA256())}
		}
		if adapted {
			if !candidate.validFor(d, data) {
				object.refusal = AdaptationUnvalidated
				result.objects = append(result.objects, object)
				continue
			}
			file = candidate.file
			object.candidate = nha047.Digest(file.SHA256())
		}
		id, _ := d.ID().Value()
		if strings.HasPrefix(id, "scene.") {
			b.scene(&object, file)
		} else {
			b.automation(&object, file)
		}
		if adapted {
			b.add(&object, nha047.LegacyServiceAlias, 0, 0, Location{File: file.Path(), Line: 1, Column: 1, Path: "$.actions"}, nil, RefusalNone)
		}
		b.finish(&object, adapted)
		result.objects = append(result.objects, object)
		if b.nodes > MaxNodes {
			return Report{}, ErrLimit
		}
	}
	sort.Slice(result.objects, func(i, j int) bool {
		a, _ := result.objects[i].ID().Value()
		c, _ := result.objects[j].ID().Value()
		return a < c
	})
	result.valid = true
	if _, err := JSON(result); err != nil {
		return Report{}, err
	}
	return result, nil
}

// validFor validates the complete supplied delta against the verified frozen
// destination. It never rewrites either snapshot or constructs an original.
func (c Candidate) validFor(definition logicselection.Definition, canonical []byte) bool {
	if c.file.Path() != definition.Path() || nha047.Digest(c.file.SHA256()) != nha047.Digest(definition.Digest()) {
		return false
	}
	proposed := c.file.Bytes()
	if !bytes.Equal(proposed, canonical) {
		return false
	}
	switch c.origin {
	case Frozen158:
		// The frozen destination is already canonical. This bounded profile
		// requires an explicit distinct original to validate a syntax delta.
		return false
	case ExplicitImportPair:
		return c.originalFile.Path() == definition.Path() && c.original == nha047.Digest(c.originalFile.SHA256()) && serviceAliasDelta(c.originalFile.Bytes(), proposed)
	case SourceOriginInvalid, ExplicitCandidate:
		return false
	default:
		return false
	}
}

func serviceAliasDelta(original, candidate []byte) bool {
	lines := strings.Split(string(original), "\n")
	count := 0
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "- service: switch.turn_on" {
			lines[i] = strings.TrimSuffix(line, trimmed) + "- action: switch.turn_on"
			count++
		}
	}
	return count == 1 && bytes.Equal([]byte(strings.Join(lines, "\n")), candidate)
}

func (b *builder) automation(object *Object, file parser.SourceFile) {
	id, _ := object.ID().Value()
	var result ir.Result
	var err error
	// Probe the existing invocation grammar, never a second YAML parser.
	if _, invocationErr := parser.LoadSources(file.Path(), []parser.SourceFile{file}, parser.SyntaxProfileBlueprintInstance); invocationErr == nil {
		files := append([]parser.SourceFile{file}, b.input.Blueprints...)
		expanded, expandErr := parser.ExpandBlueprintSources(file.Path(), files)
		if expandErr != nil {
			object.refusal = SourceUnavailable
			b.add(object, nha047.BlueprintSourceMissing, 0, 0, Location{File: file.Path(), Line: 1, Column: 1, Path: "$.use_blueprint"}, nil, SourceUnavailable)
			return
		}
		result, err = ir.CompileExpanded(expanded, b.input.Catalogue)
	} else {
		result, err = ir.CompileSources(id, file.Path(), []parser.SourceFile{file}, b.input.Catalogue)
	}
	if err != nil {
		object.refusal = InvalidInput
		return
	}
	if rejected := result.Rejected(); len(rejected) != 0 {
		d := rejected[0]
		object.compilerCode = d.Code().String()
		object.refusal = Semantic
		switch d.Code() {
		case ir.CodeExcludedReference:
			object.refusal = ExcludedReference
		case ir.CodeMissingCapability:
			object.refusal = MissingDeclaredCapability
		case ir.CodeUnknownField, ir.CodeUnsupportedForm:
			object.refusal = Syntax
		case ir.CodeInvalid, ir.CodeInvalidInput, ir.CodeInvalidValue, ir.CodeUnknownReference, ir.CodeUnknownService, ir.CodeUnsupportedTemplate:
		}
		source := d.Source()
		at := source.At()
		object.nodes = append(object.nodes, Construction{source: location(parser.Position{File: at.File, Line: at.Line, Column: at.Column}, source.Path()), coverage: Absent, refusal: object.refusal})
		return
	}
	accepted := result.Accepted()
	if len(accepted) != 1 {
		object.refusal = Semantic
		return
	}
	b.automationFeatures(object, accepted[0])
}

func (b *builder) scene(object *Object, file parser.SourceFile) {
	ref, err := scene.NewSourceRef(object.ID(), file.Path(), file.SHA256())
	if err != nil {
		object.refusal = InvalidInput
		return
	}
	result, err := scene.Load(ref, file.Bytes())
	if err != nil {
		object.refusal = Syntax
		return
	}
	if len(result.Rejected()) != 0 || len(result.Accepted()) != 1 {
		object.refusal = Syntax
		return
	}
	b.sceneFeatures(object, result.Accepted()[0])
}

func (b *builder) finish(object *Object, adapted bool) {
	for _, node := range object.nodes {
		if object.refusal == RefusalNone && node.refusal != RefusalNone {
			object.refusal = node.refusal
		}
	}
	if len(object.nodes) == 0 && object.refusal == RefusalNone {
		object.refusal = CoverageMissing
	}
	if object.refusal != RefusalNone {
		object.verdict = Refused
		return
	}
	object.verdict = AcceptedUnchanged
	if adapted {
		object.verdict = AcceptedAdapted
	}
}

func (b *builder) reference(id domain.HAAlias) RefusalKind {
	if _, err := id.Value(); err != nil {
		return InvalidInput
	}
	if _, ok := b.input.Manifest.SourceFor(id); !ok {
		return ExcludedReference
	}
	entity, ok := b.entities[id]
	if !ok {
		return MissingDeclaredCapability
	}
	if entity.Status() != ir.ReferencePresent {
		return ExcludedReference
	}
	return RefusalNone
}

func (b *builder) add(object *Object, kind nha047.Construction, value, secondary float64, source Location, refs []domain.HAAlias, refusal RefusalKind) {
	b.nodes++
	feature, err := nha047.NewFeature(kind, value, secondary)
	if err != nil && refusal == RefusalNone {
		refusal = UnsupportedConstruction
	}
	for _, id := range refs {
		if r := b.reference(id); r != RefusalNone && refusal == RefusalNone {
			refusal = r
		}
	}
	proofs := b.input.Evidence.Cover(feature)
	coverage := Absent
	for _, p := range proofs {
		role := nha047.SameSource
		if kind == nha047.LegacyServiceAlias {
			role = nha047.OriginalToCandidate
		}
		if p.Kind() == nha047.DifferentialEquivalent && p.Role() == role {
			coverage = Covered
			break
		}
		coverage = Partial
	}
	// A projection proof for another declared conversion is related evidence,
	// not full coverage of this member's execution preconditions.
	if refusal == CoverageMissing && coverage == Covered {
		coverage = Partial
	}
	if coverage != Covered && refusal == RefusalNone {
		refusal = CoverageMissing
	}
	object.nodes = append(object.nodes, Construction{feature, source, append([]domain.HAAlias(nil), refs...), proofs, coverage, refusal})
}

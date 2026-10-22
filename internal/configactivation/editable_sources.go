package configactivation

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"hash"
	"sort"

	"github.com/damione1/Homeward/compat/ha/dashboard"
	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	hascene "github.com/damione1/Homeward/compat/ha/scene"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configcompilation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/heatingownership"
	"github.com/damione1/Homeward/internal/logicselection"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
)

// SourceGroup owns one complete configuration entry and its exact includes.
// The descriptor selects a closed compiler; logical paths do not grant any
// filesystem authority. Bindings and sources are detached immutable values.
type SourceGroup struct {
	descriptor storage.SourceDescriptor
	files      []parser.SourceFile
	bindings   []storage.ConfigDocumentBinding
	hash       [32]byte
}

func NewSourceGroup(descriptor storage.SourceDescriptor, files []parser.SourceFile, bindings []storage.ConfigDocumentBinding) (SourceGroup, error) {
	if !descriptor.Valid() || len(files) < 1 || len(files) > 64 || len(files) != len(bindings) {
		return SourceGroup{}, ErrInvalidStartup
	}
	byPath := map[string]storage.ConfigDocumentBinding{}
	ids := map[storage.ConfigDocumentID]bool{}
	for _, binding := range bindings {
		if !binding.ID().Valid() || binding.Path() == "" || ids[binding.ID()] {
			return SourceGroup{}, ErrInvalidStartup
		}
		if _, present := byPath[binding.Path()]; present {
			return SourceGroup{}, ErrInvalidStartup
		}
		ids[binding.ID()] = true
		byPath[binding.Path()] = binding
	}
	result := SourceGroup{descriptor: descriptor}
	total := 0
	seen := map[string]bool{}
	for _, file := range files {
		binding, present := byPath[file.Path()]
		if !present || seen[file.Path()] {
			return SourceGroup{}, ErrInvalidStartup
		}
		owned, err := parser.CloneSourceFile(file)
		if descriptor.Kind() == storage.ConfigDocumentKindOwnershipSource {
			if _, pathErr := parser.NewJSONSourceFile(file.Path(), nil); pathErr != nil {
				return SourceGroup{}, ErrInvalidStartup
			}
		} else if _, pathErr := parser.NewSourceFile(file.Path(), nil); pathErr != nil {
			return SourceGroup{}, ErrInvalidStartup
		}
		total += len(file.Bytes())
		if err != nil || len(file.Bytes()) < 1 || len(file.Bytes()) > 1<<20 || owned.SHA256() != file.SHA256() || total > 4<<20 {
			return SourceGroup{}, ErrInvalidStartup
		}
		seen[file.Path()] = true
		result.files = append(result.files, owned)
		result.bindings = append(result.bindings, binding)
	}
	if !seen[descriptor.Entry()] {
		return SourceGroup{}, ErrInvalidStartup
	}
	ordered := append([]parser.SourceFile(nil), result.files...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path() < ordered[j].Path() })
	h := sha256.New()
	writeSourcePart(h, []byte(descriptor.Kind().String()))
	writeSourcePart(h, []byte(descriptor.Identity()))
	writeSourcePart(h, []byte(descriptor.Entry()))
	for _, file := range ordered {
		writeSourcePart(h, []byte(file.Path()))
		digest := file.SHA256()
		writeSourcePart(h, digest[:])
	}
	copy(result.hash[:], h.Sum(nil))
	return result, nil
}

func writeSourcePart(h hash.Hash, raw []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(raw)))
	_, _ = h.Write(size[:])
	_, _ = h.Write(raw)
}

func (g SourceGroup) Descriptor() storage.SourceDescriptor { return g.descriptor }
func (g SourceGroup) Files() []parser.SourceFile           { return append([]parser.SourceFile(nil), g.files...) }
func (g SourceGroup) Bindings() []storage.ConfigDocumentBinding {
	return append([]storage.ConfigDocumentBinding(nil), g.bindings...)
}
func (g SourceGroup) SHA256() [32]byte { return g.hash }
func (g SourceGroup) valid() bool {
	return g.descriptor.Valid() && len(g.files) > 0 && len(g.files) == len(g.bindings) && g.hash != ([32]byte{})
}
func (g SourceGroup) String() string               { return "[REDACTED CONFIG SOURCE GROUP]" }
func (g SourceGroup) GoString() string             { return g.String() }
func (g SourceGroup) MarshalJSON() ([]byte, error) { return json.Marshal(g.String()) }

// SourceCompilation is rebuilt from exact SQL source bytes at restart. It is
// never serialized as opaque IR. The supplied semantic catalogue is a trusted
// registry/capability snapshot; source strings cannot invent executable
// provider support. Command admission remains owned by the existing Host/DCS.
type SourceCompilation struct {
	compilationContext configcompilation.Context
	groups             []SourceGroup
	automations        []ir.Automation
	scripts            []ir.Script
	scenes             []hascene.Definition
	dashboards         map[authorization.DashboardID]dashboard.Definition
	ownership          *heatingownership.Decision
	hash               [32]byte
	admitted           bool
}

func CompileSourceGroups(groups []SourceGroup, manifest registry.Manifest, catalogue ir.Catalogue) (SourceCompilation, error) {
	if _, valid := manifest.Revision(); !valid || len(groups) > 64 {
		return SourceCompilation{}, ErrInvalidStartup
	}
	selection, err := logicselection.LoadV1()
	if err != nil {
		return SourceCompilation{}, ErrInvalidStartup
	}
	ordered := append([]SourceGroup(nil), groups...)
	sort.Slice(ordered, func(i, j int) bool {
		left, right := ordered[i].descriptor, ordered[j].descriptor
		if left.Kind() != right.Kind() {
			return left.Kind() < right.Kind()
		}
		return left.Identity() < right.Identity()
	})
	result := SourceCompilation{dashboards: map[authorization.DashboardID]dashboard.Definition{}}
	seen := map[string]bool{}
	var scripts []ir.ScriptID
	totalFiles, totalBytes := 0, 0
	for _, group := range ordered {
		if !group.valid() {
			return SourceCompilation{}, ErrInvalidStartup
		}
		totalFiles += len(group.files)
		for _, file := range group.files {
			totalBytes += len(file.Bytes())
		}
		if totalFiles > 256 || totalBytes > 4<<20 {
			return SourceCompilation{}, ErrInvalidStartup
		}
		if group.descriptor.Kind() == storage.ConfigDocumentKindScriptSource {
			id, err := ir.ParseScriptID(group.descriptor.Identity())
			if err != nil {
				return SourceCompilation{}, ErrInvalidStartup
			}
			scripts = append(scripts, id)
		}
	}
	if len(scripts) > 0 {
		catalogue, err = catalogue.WithScripts(scripts)
		if err != nil {
			return SourceCompilation{}, ErrInvalidStartup
		}
	}
	h := sha256.New()
	writeSourcePart(h, []byte("homeward.editable-sources/1"))
	for _, group := range ordered {
		descriptor := group.descriptor
		key := descriptor.Kind().String() + "\x00" + descriptor.Identity()
		if seen[key] {
			return SourceCompilation{}, ErrInvalidStartup
		}
		seen[key] = true
		switch descriptor.Kind() {
		case storage.ConfigDocumentKindDashboardSource:
			id, err := authorization.ParseDashboardID(descriptor.Identity())
			if err != nil {
				return SourceCompilation{}, ErrInvalidStartup
			}
			source, err := dashboard.NewSource(id, "homeward", descriptor.Entry())
			if err != nil {
				return SourceCompilation{}, ErrInvalidStartup
			}
			compiled, err := dashboard.Restore(source, group.files)
			if err != nil || compiled.Status() != dashboard.StatusSupported {
				return SourceCompilation{}, ErrInvalidStartup
			}
			definition, present := compiled.Definition()
			if !present || !dashboardReferencesWithinManifest(definition, manifest) {
				return SourceCompilation{}, ErrInvalidStartup
			}
			result.dashboards[id] = definition
		case storage.ConfigDocumentKindAutomationSource:
			if !selectedSourceIdentity(selection, descriptor.Identity()) {
				return SourceCompilation{}, ErrInvalidStartup
			}
			compiled, err := ir.CompileSources(descriptor.Identity(), descriptor.Entry(), group.files, catalogue)
			if err != nil || len(compiled.Rejected()) != 0 || len(compiled.Accepted()) != 1 {
				return SourceCompilation{}, ErrInvalidStartup
			}
			result.automations = append(result.automations, compiled.Accepted()[0])
		case storage.ConfigDocumentKindScriptSource:
			if !selectedSourceIdentity(selection, descriptor.Identity()) {
				return SourceCompilation{}, ErrInvalidStartup
			}
			compiled, err := ir.CompileScriptSources(descriptor.Identity(), descriptor.Entry(), group.files, catalogue)
			if err != nil || len(compiled.Rejected()) != 0 || len(compiled.Accepted()) != 1 {
				return SourceCompilation{}, ErrInvalidStartup
			}
			result.scripts = append(result.scripts, compiled.Accepted()[0])
		case storage.ConfigDocumentKindSceneSource:
			if !selectedSourceIdentity(selection, descriptor.Identity()) || len(group.files) != 1 {
				return SourceCompilation{}, ErrInvalidStartup
			}
			id, err := domain.ParseHAAlias(descriptor.Identity())
			if err != nil {
				return SourceCompilation{}, ErrInvalidStartup
			}
			ref, err := hascene.NewSourceRef(id, descriptor.Entry(), group.files[0].SHA256())
			if err != nil {
				return SourceCompilation{}, ErrInvalidStartup
			}
			compiled, err := hascene.Load(ref, group.files[0].Bytes())
			if err != nil || len(compiled.Rejected()) != 0 || len(compiled.Accepted()) != 1 {
				return SourceCompilation{}, ErrInvalidStartup
			}
			for _, member := range compiled.Accepted()[0].Members() {
				if _, present := manifest.SourceFor(member.Alias()); !present {
					return SourceCompilation{}, ErrInvalidStartup
				}
			}
			result.scenes = append(result.scenes, compiled.Accepted()[0])
		case storage.ConfigDocumentKindOwnershipSource:
			if descriptor.Identity() != "heating.ownership" || result.ownership != nil || len(group.files) != 1 {
				return SourceCompilation{}, ErrInvalidStartup
			}
			decision, err := heatingownership.ParseForManifest(group.files[0].Bytes(), manifest)
			if err != nil {
				return SourceCompilation{}, ErrInvalidStartup
			}
			result.ownership = &decision
		default:
			return SourceCompilation{}, ErrInvalidStartup
		}
		writeSourcePart(h, group.hash[:])
		result.groups = append(result.groups, group)
	}
	copy(result.hash[:], h.Sum(nil))
	result.admitted = true
	return result, nil
}

func selectedSourceIdentity(selection logicselection.Selection, identity string) bool {
	id, err := domain.ParseHAAlias(identity)
	if err != nil {
		return false
	}
	disposition, known := selection.DispositionFor(id)
	return !known || disposition == logicselection.DispositionRetained
}

func (s SourceCompilation) Groups() []SourceGroup { return append([]SourceGroup(nil), s.groups...) }
func (s SourceCompilation) SHA256() [32]byte      { return s.hash }
func (s SourceCompilation) Automations() []ir.Automation {
	return append([]ir.Automation(nil), s.automations...)
}
func (s SourceCompilation) Scripts() []ir.Script { return append([]ir.Script(nil), s.scripts...) }
func (s SourceCompilation) Scenes() []hascene.Definition {
	return append([]hascene.Definition(nil), s.scenes...)
}
func (s SourceCompilation) Valid() bool { return s.admitted && s.hash != ([32]byte{}) }
func (s SourceCompilation) Dashboard(id authorization.DashboardID) (dashboard.Definition, bool) {
	value, present := s.dashboards[id]
	return value, present && s.Valid()
}
func (s SourceCompilation) Ownership() (heatingownership.Decision, bool) {
	if !s.Valid() || s.ownership == nil {
		return heatingownership.Decision{}, false
	}
	return *s.ownership, true
}
func (s SourceCompilation) String() string               { return "[REDACTED COMPILED CONFIG SOURCES]" }
func (s SourceCompilation) GoString() string             { return s.String() }
func (s SourceCompilation) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

func sourceGroupsFromSQL(values []storage.EditableSourceSnapshot) ([]SourceGroup, error) {
	byIdentity := map[string][]storage.EditableSourceSnapshot{}
	for _, value := range values {
		descriptor := value.Descriptor()
		if !descriptor.Valid() {
			return nil, ErrStartupIntegrity
		}
		key := descriptor.Kind().String() + "\x00" + descriptor.Identity()
		byIdentity[key] = append(byIdentity[key], value)
	}
	keys := make([]string, 0, len(byIdentity))
	for key := range byIdentity {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	groups := make([]SourceGroup, 0, len(keys))
	for _, key := range keys {
		members := byIdentity[key]
		descriptor := members[0].Descriptor()
		files := make([]parser.SourceFile, 0, len(members))
		bindings := make([]storage.ConfigDocumentBinding, 0, len(members))
		for _, member := range members {
			if member.Descriptor() != descriptor {
				return nil, ErrStartupIntegrity
			}
			files = append(files, member.File())
			bindings = append(bindings, member.Binding())
		}
		group, err := NewSourceGroup(descriptor, files, bindings)
		if err != nil {
			return nil, ErrStartupIntegrity
		}
		groups = append(groups, group)
	}
	return groups, nil
}

func compiledSourceGroupsHash(base [32]byte, groups []SourceGroup) [32]byte {
	if len(groups) == 0 {
		return base
	}
	ordered := append([]SourceGroup(nil), groups...)
	sort.Slice(ordered, func(i, j int) bool {
		left, right := ordered[i].descriptor, ordered[j].descriptor
		if left.Kind() != right.Kind() {
			return left.Kind() < right.Kind()
		}
		return left.Identity() < right.Identity()
	})
	h := sha256.New()
	writeSourcePart(h, []byte("homeward.compiled-operational/2"))
	writeSourcePart(h, base[:])
	for _, group := range ordered {
		writeSourcePart(h, group.hash[:])
	}
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}

func dashboardReferencesWithinManifest(definition dashboard.Definition, manifest registry.Manifest) bool {
	admit := func(alias domain.HAAlias) bool { _, present := manifest.SourceFor(alias); return present }
	visibility := func(values []dashboard.Visibility) bool {
		for _, value := range values {
			if alias, present := value.Entity(); present && !admit(alias) {
				return false
			}
		}
		return true
	}
	var card func(dashboard.Card) bool
	card = func(value dashboard.Card) bool {
		if alias, present := value.Entity(); present && !admit(alias) {
			return false
		}
		if !visibility(value.Visibility()) {
			return false
		}
		for _, action := range value.Actions() {
			if alias, present := action.Entity(); present && !admit(alias) {
				return false
			}
		}
		for _, child := range value.Children() {
			if !card(child) {
				return false
			}
		}
		return true
	}
	for _, view := range definition.Views() {
		if !visibility(view.Visibility()) {
			return false
		}
		for _, section := range view.Sections() {
			if !visibility(section.Visibility()) {
				return false
			}
			for _, item := range section.Cards() {
				if !card(item) {
					return false
				}
			}
		}
	}
	return true
}

// replaceSourceGroup preserves every untouched source group. A replacement
// cannot silently change kind/identity or steal another document binding.
func replaceSourceGroup(current []SourceGroup, replacement *SourceGroup) ([]SourceGroup, error) {
	result := append([]SourceGroup(nil), current...)
	if replacement == nil {
		return result, nil
	}
	if !replacement.valid() {
		return nil, ErrInvalidStartup
	}
	found := false
	for index, group := range result {
		if group.descriptor.Kind() == replacement.descriptor.Kind() && group.descriptor.Identity() == replacement.descriptor.Identity() {
			if found {
				return nil, ErrInvalidStartup
			}
			result[index] = *replacement
			found = true
		}
	}
	if !found {
		result = append(result, *replacement)
	}
	ids := map[storage.ConfigDocumentID]bool{}
	for _, group := range result {
		for _, binding := range group.bindings {
			if ids[binding.ID()] {
				return nil, ErrInvalidStartup
			}
			ids[binding.ID()] = true
		}
	}
	return result, nil
}

func sourceCompilationDigest(groups []SourceGroup) [32]byte {
	ordered := append([]SourceGroup(nil), groups...)
	sort.Slice(ordered, func(i, j int) bool {
		left, right := ordered[i].descriptor, ordered[j].descriptor
		if left.Kind() != right.Kind() {
			return left.Kind() < right.Kind()
		}
		return left.Identity() < right.Identity()
	})
	h := sha256.New()
	writeSourcePart(h, []byte("homeward.editable-sources/1"))
	for _, group := range ordered {
		writeSourcePart(h, group.hash[:])
	}
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}

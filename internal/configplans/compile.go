package configplans

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/damione1/Homeward/compat/ha/dashboard"
	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/compat/ha/scene"
	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/ecobee"
	"github.com/damione1/Homeward/internal/heatingownership"
	"github.com/damione1/Homeward/internal/registry"
	"go.yaml.in/yaml/v3"
)

// image is a private semantic projection, produced only after admission by the
// existing domain compiler. Its digests/values never leave this package.
type image struct {
	digest       [32]byte
	fields       map[string][32]byte
	fieldEffects map[string]Effect
	refs         map[string]bool
	effect       Effect
}

// operationalBindingImage contains only already-admitted binding behavior.
// Profile predicates come from worker.ConfiguredBinding, the same owner used by
// the Host handshake/discovery path; no source string invents profile authority.
type operationalBindingImage struct {
	Device, Entity, Alias string
	CasetaHA100V1         bool
	CasetaHAFan4V1        bool
}

func familyEffect(f Family) Effect {
	switch f {
	case Dashboard:
		return DashboardEffect
	case Authorization:
		return RightsEffect
	case Ownership:
		return OwnershipEffect
	case Automation, Script, Scene:
		return RunTargetsEffect
	case Operational, Declarative1, Declarative2:
		return ConfigurationEffect
	case FamilyInvalid:
		return EffectInvalid
	default:
		return EffectInvalid
	}
}

func compile(candidate Candidate, manifest registry.Manifest) (image, Diagnostic) {
	if !candidate.Valid() {
		return image{}, InvalidSchema
	}
	if _, ok := manifest.Revision(); !ok {
		return image{}, BaseUnavailable
	}
	result := image{fields: map[string][32]byte{}, fieldEffects: map[string]Effect{}, refs: map[string]bool{}, effect: familyEffect(candidate.family)}
	var doc *parser.Document
	var err error
	switch candidate.family {
	case Operational:
		if candidate.identity != "operational.main" || len(candidate.files) != 1 {
			return image{}, InvalidSchema
		}
		configuration, e := config.ParseOperationalWithEnv(candidate.files[0].Bytes(), func(string) (string, bool) { return "", false })
		if e != nil {
			return image{}, InvalidSchema
		}
		// Selection never hides an excluded alias in an admitted declaration.
		if diagnostic := declaredOperationalReferences(configuration.DeclaredOperationalSettings(), manifest); diagnostic != DiagnosticInvalid {
			return image{}, diagnostic
		}
		bindingImage := map[string][]operationalBindingImage{}
		for _, worker := range configuration.ExternalWorkers() {
			instance, e := worker.Instance()
			if e != nil {
				return image{}, InvalidSchema
			}
			instanceName, e := instance.Value()
			if e != nil {
				return image{}, InvalidSchema
			}
			bindings := []operationalBindingImage{}
			for _, binding := range worker.Bindings() {
				key, name, e := binding.Components()
				if e != nil || !admitAlias(name, manifest) {
					return image{}, ExcludedReference
				}
				device, entity, e := key.Components()
				if e != nil {
					return image{}, InvalidSchema
				}
				bindings = append(bindings, operationalBindingImage{Device: device, Entity: entity, Alias: name, CasetaHA100V1: binding.CasetaHA100V1(), CasetaHAFan4V1: binding.CasetaHAFan4V1()})
				result.refs[name] = true
			}
			sort.Slice(bindings, func(i, j int) bool {
				if bindings[i].Device != bindings[j].Device {
					return bindings[i].Device < bindings[j].Device
				}
				if bindings[i].Entity != bindings[j].Entity {
					return bindings[i].Entity < bindings[j].Entity
				}
				return bindings[i].Alias < bindings[j].Alias
			})
			bindingImage[instanceName] = bindings
		}
		for _, binding := range configuration.EnergyBindings() {
			for _, alias := range []string{binding.SourceAlias(), binding.OutputAlias()} {
				if !admitAlias(alias, manifest) {
					return image{}, ExcludedReference
				}
				result.refs[alias] = true
			}
		}
		if settings, present := configuration.EcobeeSettings(); present {
			if !settings.Valid() {
				return image{}, InvalidSchema
			}
			if !admitAlias(ecobee.Alias, manifest) {
				return image{}, ExcludedReference
			}
			result.refs[ecobee.Alias] = true
		}
		if settings, present := configuration.NeviwebSettings(); present {
			for _, binding := range settings.Bindings() {
				if !admitAlias(binding.Alias(), manifest) {
					return image{}, ExcludedReference
				}
				result.refs[binding.Alias()] = true
			}
		}
		if settings, present := configuration.HiloSettings(); present {
			for _, binding := range settings.Bindings() {
				if !admitAlias(binding.Alias(), manifest) {
					return image{}, ExcludedReference
				}
				result.refs[binding.Alias()] = true
			}
		}
		if settings, present := configuration.RemoteCalendarSettings(); present {
			// The delivered101 parser owns this fixed source identity and opaque
			// credential references. Preview checks membership only: it never
			// resolves a handle, builds a CloudClient or starts/fetches a source.
			_, source, err := settings.Identity()
			if err != nil {
				return image{}, InvalidSchema
			}
			_, alias, err := source.Components()
			if err != nil || !admitAlias(alias, manifest) {
				return image{}, ExcludedReference
			}
			result.refs[alias] = true
		}
		// Operational parsing above owns schema admission. This bounded projection
		// only normalizes its already-admitted source; it grants no new grammar.
		node, e := admittedYAML(candidate.files[0].Bytes())
		if e != nil {
			return image{}, InvalidSchema
		}
		operationalFields := map[string]bool{"mqtt": true, "ecobee": true, "neviweb": true, "hilo": true, "energy": true, "derived_power": true, "solar": true, "workday": true, "external_workers": true, "remote_calendar": true, "disabled_integrations": true}
		launchFields := map[string]bool{"scope_revision": true, "backup_recovery_recipient": true, "backups": true, "listen": true, "postgres": true, "data_dir": true, "public": true, "recorder": true}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i].Value
			if operationalFields[key] {
				if key == "disabled_integrations" {
					// Existing148 admission owns declaration/duplicate checks. Selection
					// is a set: omission, an empty set and reordered entries are inert.
					selection := []string{}
					for _, entry := range node.Content[i+1].Content {
						selection = append(selection, entry.Value)
					}
					if len(selection) > 0 {
						sort.Strings(selection)
						raw, _ := json.Marshal(selection)
						result.fields[key] = sha256.Sum256(raw)
					}
				} else if key == "external_workers" {
					bindingsRaw, _ := json.Marshal(bindingImage)
					result.fields["external_worker_bindings"] = sha256.Sum256(bindingsRaw)
					result.fieldEffects["external_worker_bindings"] = OwnershipEffect
					processes := []any{}
					for _, workerNode := range node.Content[i+1].Content {
						projection := map[string]any{}
						for i := 0; i < len(workerNode.Content); i += 2 {
							if workerNode.Content[i].Value != "bindings" {
								projection[workerNode.Content[i].Value] = yamlValue(workerNode.Content[i+1])
							}
						}
						processes = append(processes, projection)
					}
					raw, _ := json.Marshal(processes)
					result.fields["external_worker_processes"] = sha256.Sum256(raw)
				} else {
					result.fields[key] = yamlDigest(node.Content[i+1])
				}
			} else if launchFields[key] {
				// Historical combined launch fields are inert under operational replay.
				// They remain covered by the exact source hash, never an invented effect.
				// Launch bytes are outside this operational semantic image. Their exact
				// source hash still changes and invalidates a prior receipt.
			} else {
				return image{}, InvalidSchema
			}
		}
	case Declarative1, Declarative2:
		var admitted bundleCandidate
		admitted, err = compileBundle(candidate)
		if err != nil {
			return image{}, InvalidSchema
		}
		for _, name := range admitted.aliases {
			if !admitAlias(name, manifest) {
				return image{}, ExcludedReference
			}
			result.refs[name] = true
		}
		profile := parser.SyntaxProfileDeclarative
		if candidate.family == Declarative2 {
			profile = parser.SyntaxProfileDeclarative2
		}
		doc, err = parser.LoadSources(candidate.entry, candidate.files, profile)
	case Authorization:
		if len(candidate.files) != 1 {
			return image{}, InvalidSchema
		}
		account, e := contracts.ParseAccountID(candidate.identity)
		if e != nil {
			return image{}, InvalidSchema
		}
		source, e := authorization.AdmitPolicySource(account, candidate.files[0].Bytes())
		if e != nil {
			return image{}, InvalidSchema
		}
		sum, e := source.SemanticSHA256()
		if e != nil {
			return image{}, InvalidSchema
		}
		result.fields["human_policy"] = sum
		// The policy compiler owns typed object IDs (Homeward IDs, not HA aliases).
	case Ownership:
		if candidate.identity != "heating.ownership" || len(candidate.files) != 1 {
			return image{}, InvalidSchema
		}
		if !uniqueJSON(candidate.files[0].Bytes()) {
			return image{}, InvalidSchema
		}
		decision, e := heatingownership.ParseForManifest(candidate.files[0].Bytes(), manifest)
		if e != nil {
			return image{}, InvalidSchema
		}
		pairs := []string{}
		for _, pair := range decision.Pairs() {
			a, b := pair.Aliases()
			at, _ := a.Value()
			bt, _ := b.Value()
			pairs = append(pairs, fmt.Sprintf("%s\x00%s\x00%d\x00%d", at, bt, pair.Kind(), pair.Relation()))
			result.refs[at] = true
			result.refs[bt] = true
		}
		sort.Strings(pairs)
		data, _ := json.Marshal(struct {
			Heating heatingownership.Provider
			Action  heatingownership.ActionPolicy
			Energy  heatingownership.EnergyRelation
			Pairs   []string
		}{decision.HeatingOwner(), decision.HiloActionPolicy(), decision.EnergyRelation(), pairs})
		result.fields["technical_ownership"] = sha256.Sum256(data)
	case Dashboard:
		id, e := authorization.ParseDashboardID(candidate.identity)
		if e != nil {
			return image{}, InvalidSchema
		}
		source, e := dashboard.NewSource(id, "preview", candidate.entry)
		if e != nil {
			return image{}, InvalidSchema
		}
		compiled, e := dashboard.Restore(source, candidate.files)
		if e != nil || compiled.Status() != dashboard.StatusSupported {
			return image{}, UnsupportedSource
		}
		definition, present := compiled.Definition()
		if !present {
			return image{}, InvalidSchema
		}
		if !dashboardReferences(definition, manifest, result.refs) {
			return image{}, ExcludedReference
		}
		doc, err = parser.LoadSources(candidate.entry, candidate.files, parser.SyntaxProfileLovelace)
	case Automation, Script:
		if (candidate.family == Automation && !strings.HasPrefix(candidate.identity, "automation.")) || (candidate.family == Script && !strings.HasPrefix(candidate.identity, "script.")) {
			return image{}, InvalidSchema
		}
		doc, err = parser.LoadSources(candidate.entry, candidate.files, parser.SyntaxProfileLogic)
		if err != nil {
			return image{}, InvalidSchema
		}
		catalogue, e := diagnosticCatalogue(doc.Root(), manifest, candidate.identity)
		if e != nil {
			return image{}, InvalidSchema
		}
		if candidate.family == Automation {
			compiled, e := ir.CompileSources(candidate.identity, candidate.entry, candidate.files, catalogue)
			if e != nil || len(compiled.Rejected()) != 0 || len(compiled.Accepted()) != 1 {
				return image{}, ExcludedReference
			}
		} else {
			compiled, e := ir.CompileScriptSources(candidate.identity, candidate.entry, candidate.files, catalogue)
			if e != nil || len(compiled.Rejected()) != 0 || len(compiled.Accepted()) != 1 {
				return image{}, ExcludedReference
			}
		}
		collectReferenceSyntax(doc.Root(), "", result.refs)
		for ref := range result.refs {
			if !admitAlias(ref, manifest) {
				return image{}, ExcludedReference
			}
		}
	case Scene:
		alias, e := domain.ParseHAAlias(candidate.identity)
		if e != nil || len(candidate.files) != 1 {
			return image{}, InvalidSchema
		}
		ref, e := scene.NewSourceRef(alias, candidate.entry, candidate.files[0].SHA256())
		if e != nil {
			return image{}, InvalidSchema
		}
		compiled, e := scene.Load(ref, candidate.files[0].Bytes())
		if e != nil || len(compiled.Rejected()) != 0 || len(compiled.Accepted()) != 1 {
			return image{}, InvalidSchema
		}
		for _, member := range compiled.Accepted()[0].Members() {
			name, e := member.Alias().Value()
			if e != nil || len(member.Diagnostics()) != 0 || !admitAlias(name, manifest) {
				return image{}, ExcludedReference
			}
			result.refs[name] = true
		}
		doc, err = parser.LoadSources(candidate.entry, candidate.files, parser.SyntaxProfileScene)
	case FamilyInvalid:
		return image{}, InvalidSchema
	default:
		return image{}, InvalidSchema
	}
	if err != nil {
		return image{}, InvalidSchema
	}
	if doc != nil {
		root := doc.Root()
		if root.Kind() != parser.KindMapping {
			return image{}, InvalidSchema
		}
		grouped := map[string]map[string][32]byte{}
		for _, entry := range root.Entries() {
			key := publicField(candidate.family, entry.Key)
			sum, e := syntaxDigest(entry.Value)
			if e != nil {
				return image{}, InvalidSchema
			}
			if grouped[key] == nil {
				grouped[key] = map[string][32]byte{}
			}
			grouped[key][entry.Key] = sum
		}
		for key, entries := range grouped {
			names := make([]string, 0, len(entries))
			for name := range entries {
				names = append(names, name)
			}
			sort.Strings(names)
			h := sha256.New()
			for _, name := range names {
				part(h, []byte(name))
				sum := entries[name]
				part(h, sum[:])
			}
			var sum [32]byte
			copy(sum[:], h.Sum(nil))
			result.fields[key] = sum
		}
	}
	// Publicly visible fields are a closed set, never caller/source identifiers.
	keys := make([]string, 0, len(result.fields))
	for key := range result.fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	h := sha256.New()
	part(h, []byte(candidate.Schema()))
	for _, key := range keys {
		part(h, []byte(key))
		sum := result.fields[key]
		part(h, sum[:])
	}
	copy(result.digest[:], h.Sum(nil))
	return result, DiagnosticInvalid
}
func admitAlias(name string, manifest registry.Manifest) bool {
	alias, e := domain.ParseHAAlias(name)
	if e != nil {
		return false
	}
	_, ok := manifest.SourceFor(alias)
	return ok
}
func mergeDigest(left, right [32]byte) [32]byte {
	if left == ([32]byte{}) {
		return right
	}
	return sha256.Sum256(append(left[:], right[:]...))
}
func publicField(f Family, key string) string {
	switch f {
	case Declarative1, Declarative2:
		switch key {
		case "input_boolean", "input_number", "counter", "input_button", "input_datetime", "input_select", "input_text", "group":
			return key
		}
	case Dashboard:
		switch key {
		case "views":
			return "dashboard_views"
		case "strategy":
			return "dashboard_strategy"
		}
	case Automation, Script:
		switch key {
		case "triggers", "trigger":
			return "run_triggers"
		case "conditions", "condition":
			return "run_conditions"
		case "actions", "action", "sequence":
			return "run_actions"
		case "mode", "max", "max_exceeded":
			return "run_policy"
		case "variables":
			return "run_variables"
		default:
			return "run_metadata"
		}
	case Scene:
		switch key {
		case "entities", "scene":
			return "scene_members"
		default:
			return "scene_metadata"
		}
	}
	return "definition"
}

// diagnosticCatalogue describes supported syntax for allowlisted aliases only.
// Like ir.Catalogue itself this is NOT live provider/capability qualification;
// the returned plan never claims application or command dispatch is available.
func diagnosticCatalogue(root parser.Node, manifest registry.Manifest, self string) (ir.Catalogue, error) {
	values := map[string]bool{}
	var visit func(parser.Node)
	visit = func(n parser.Node) {
		if s, ok := n.Scalar(); ok && s.Kind() == parser.ScalarString {
			if _, e := domain.ParseHAAlias(s.Text()); e == nil {
				values[s.Text()] = true
			}
		}
		for _, e := range n.Entries() {
			if _, err := domain.ParseHAAlias(e.Key); err == nil {
				values[e.Key] = true
			}
			visit(e.Value)
		}
		for _, v := range n.Items() {
			visit(v)
		}
	}
	visit(root)
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	entities := make([]ir.Entity, 0, len(names))
	for _, name := range names {
		status := ir.ReferenceExcluded
		var supports []ir.DeclaredSupport
		if admitAlias(name, manifest) {
			status = ir.ReferencePresent
			supports = []ir.DeclaredSupport{ir.SupportObserveState}
			switch strings.SplitN(name, ".", 2)[0] {
			case "climate":
				supports = append(supports, ir.SupportClimateSetpoint)
			case "switch":
				supports = append(supports, ir.SupportSwitchTurnOn)
			case "light":
				supports = append(supports, ir.SupportLightTurnOn, ir.SupportLightTurnOff, ir.SupportLightBrightnessAbsolute, ir.SupportLightBrightnessRelative, ir.SupportLightTransition)
			case "scene":
				supports = append(supports, ir.SupportSceneTurnOn, ir.SupportSceneTransition)
			}
		}
		entity, e := ir.NewEntity(name, status, supports...)
		if e != nil {
			return ir.Catalogue{}, e
		}
		entities = append(entities, entity)
	}
	catalogue, err := ir.NewCatalogue(entities, nil, nil)
	if err != nil {
		return ir.Catalogue{}, err
	}
	if strings.HasPrefix(self, "script.") {
		// NHA042's compiler requires the definition's own ScriptID in its
		// catalogue, even when its sequence contains only a stop. This is
		// declaration admission; no referenced foreign script is inferred
		// from source strings or granted executable/runtime authority.
		id, err := ir.ParseScriptID(self)
		if err != nil {
			return ir.Catalogue{}, err
		}
		return catalogue.WithScripts([]ir.ScriptID{id})
	}
	return catalogue, nil
}
func collectReferenceSyntax(n parser.Node, context string, refs map[string]bool) {
	if context == "entity_id" || context == "entity" {
		if s, ok := n.Scalar(); ok && s.Kind() == parser.ScalarString {
			refs[s.Text()] = true
		}
	}
	for _, entry := range n.Entries() {
		collectReferenceSyntax(entry.Value, entry.Key, refs)
	}
	for _, item := range n.Items() {
		collectReferenceSyntax(item, context, refs)
	}
}
func dashboardReferences(def dashboard.Definition, manifest registry.Manifest, refs map[string]bool) bool {
	check := func(alias domain.HAAlias) bool {
		name, e := alias.Value()
		if e != nil || !admitAlias(name, manifest) {
			return false
		}
		refs[name] = true
		return true
	}
	visibility := func(values []dashboard.Visibility) bool {
		for _, v := range values {
			if alias, ok := v.Entity(); ok && !check(alias) {
				return false
			}
		}
		return true
	}
	var card func(dashboard.Card) bool
	card = func(value dashboard.Card) bool {
		if alias, ok := value.Entity(); ok && !check(alias) {
			return false
		}
		if !visibility(value.Visibility()) {
			return false
		}
		for _, action := range value.Actions() {
			if alias, ok := action.Entity(); ok && !check(alias) {
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
	for _, view := range def.Views() {
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

func syntaxDigest(root parser.Node) ([32]byte, error) {
	value, e := syntaxValue(root)
	if e != nil {
		return [32]byte{}, e
	}
	raw, e := json.Marshal(value)
	if e != nil {
		return [32]byte{}, e
	}
	return sha256.Sum256(raw), nil
}
func syntaxValue(n parser.Node) (any, error) {
	switch n.Kind() {
	case parser.KindMapping:
		values := map[string]any{}
		for _, entry := range n.Entries() {
			value, e := syntaxValue(entry.Value)
			if e != nil {
				return nil, e
			}
			values[entry.Key] = value
		}
		return values, nil
	case parser.KindSequence:
		values := []any{}
		for _, item := range n.Items() {
			v, e := syntaxValue(item)
			if e != nil {
				return nil, e
			}
			values = append(values, v)
		}
		return values, nil
	case parser.KindScalar:
		s, ok := n.Scalar()
		if !ok {
			return nil, ErrInvalid
		}
		return normalizedScalar(s.Kind(), s.Text()), nil
	case parser.KindSecretRef:
		key, ok := n.SecretKey()
		if !ok {
			return nil, ErrInvalid
		}
		return []any{"opaque_secret_reference", key}, nil
	case parser.KindInvalid, parser.KindInputRef:
		return nil, ErrInvalid
	default:
		return nil, ErrInvalid
	}
}
func normalizedScalar(kind parser.ScalarKind, text string) any {
	switch kind {
	case parser.ScalarBool:
		return []any{"bool", strings.EqualFold(text, "true")}
	case parser.ScalarInteger:
		if n, e := strconv.ParseInt(text, 10, 64); e == nil {
			return []any{"number", strconv.FormatInt(n, 10)}
		}
	case parser.ScalarFloat:
		if n, e := strconv.ParseFloat(text, 64); e == nil {
			return []any{"number", strconv.FormatFloat(n, 'g', -1, 64)}
		}
	case parser.ScalarNull:
		return []any{"null"}
	case parser.ScalarString:
		return []any{"string", text}
	case parser.ScalarInvalid:
		return []any{"invalid"}
	}
	return []any{"scalar", text}
}

func admittedYAML(raw []byte) (*yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var root yaml.Node
	if e := decoder.Decode(&root); e != nil || len(root.Content) != 1 {
		return nil, ErrInvalid
	}
	var extra yaml.Node
	if e := decoder.Decode(&extra); e != io.EOF {
		return nil, ErrInvalid
	}
	if !uniqueYAML(root.Content[0], 0, map[*yaml.Node]bool{}) {
		return nil, ErrInvalid
	}
	return root.Content[0], nil
}
func yamlDigest(n *yaml.Node) [32]byte {
	raw, _ := json.Marshal(yamlValue(n))
	return sha256.Sum256(raw)
}
func yamlValue(n *yaml.Node) any {
	switch n.Kind {
	case yaml.MappingNode:
		value := map[string]any{}
		for i := 0; i < len(n.Content); i += 2 {
			value[n.Content[i].Value] = yamlValue(n.Content[i+1])
		}
		return value
	case yaml.SequenceNode:
		value := []any{}
		for _, item := range n.Content {
			value = append(value, yamlValue(item))
		}
		return value
	case yaml.AliasNode:
		return yamlValue(n.Alias)
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!bool":
			return []any{"bool", strings.EqualFold(n.Value, "true")}
		case "!!int":
			if v, e := strconv.ParseInt(n.Value, 0, 64); e == nil {
				return []any{"number", strconv.FormatInt(v, 10)}
			}
		case "!!float":
			if v, e := strconv.ParseFloat(n.Value, 64); e == nil {
				return []any{"number", strconv.FormatFloat(v, 'g', -1, 64)}
			}
		case "!!null":
			return []any{"null"}
		}
		return []any{n.Tag, n.Value}
	}
	return nil
}

// declaredOperationalReferences checks only typed getters from the single
// configuration owner. The active image above still uses selected settings.
func declaredOperationalReferences(configuration config.Config, manifest registry.Manifest) Diagnostic {
	for _, worker := range configuration.ExternalWorkers() {
		for _, binding := range worker.Bindings() {
			_, alias, err := binding.Components()
			if err != nil || !admitAlias(alias, manifest) {
				return ExcludedReference
			}
		}
	}
	for _, binding := range configuration.EnergyBindings() {
		if !admitAlias(binding.SourceAlias(), manifest) || !admitAlias(binding.OutputAlias(), manifest) {
			return ExcludedReference
		}
	}
	if settings, present := configuration.EcobeeSettings(); present {
		if !settings.Valid() {
			return InvalidSchema
		}
		if !admitAlias(ecobee.Alias, manifest) {
			return ExcludedReference
		}
	}
	if settings, present := configuration.NeviwebSettings(); present {
		for _, binding := range settings.Bindings() {
			if !admitAlias(binding.Alias(), manifest) {
				return ExcludedReference
			}
		}
	}
	if settings, present := configuration.HiloSettings(); present {
		for _, binding := range settings.Bindings() {
			if !admitAlias(binding.Alias(), manifest) {
				return ExcludedReference
			}
		}
	}
	if settings, present := configuration.RemoteCalendarSettings(); present {
		_, source, err := settings.Identity()
		if err != nil {
			return InvalidSchema
		}
		_, alias, err := source.Components()
		if err != nil || !admitAlias(alias, manifest) {
			return ExcludedReference
		}
	}
	return DiagnosticInvalid
}

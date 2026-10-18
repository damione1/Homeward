package scene

import (
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/domain"
)

// Load recompiles exact bytes through the common bounded HA parser. A source
// mismatch rejects the whole definition; a member mismatch remains local.
func Load(ref SourceRef, data []byte) (Result, error) {
	if _, err := ref.ID(); err != nil {
		return Result{}, err
	}
	file, err := parser.NewSourceFile(ref.Path(), data)
	if err != nil {
		return Result{}, err
	}
	if file.SHA256() != ref.Digest() {
		return Result{}, ErrInvalidInput
	}
	doc, err := parser.LoadSources(ref.Path(), []parser.SourceFile{file}, parser.SyntaxProfileScene)
	if err != nil {
		var d *parser.Diagnostic
		if errors.As(err, &d) {
			return Result{rejected: []Diagnostic{{code: CodeSyntax, field: d.Path, at: d.At, path: d.Path, reason: d.Code.String()}}}, nil
		}
		return Result{}, err
	}
	return Compile(ref, doc)
}

// Compile binds one already admitted source ID to its syntax tree. Neither
// Home Assistant's numeric id nor the filename supplies the scene ID.
func Compile(ref SourceRef, doc *parser.Document) (Result, error) {
	if _, err := ref.ID(); err != nil || doc == nil || doc.Profile() != parser.SceneProfile {
		return Result{}, ErrInvalidInput
	}
	files := doc.Sources()
	if len(files) != 1 || files[0].Path() != ref.Path() || files[0].SHA256() != ref.Digest() {
		return Result{}, ErrInvalidInput
	}
	root := doc.Root()
	if wrapped, ok := root.Lookup("scene"); ok {
		if len(root.Entries()) != 1 || wrapped.Kind() != parser.KindSequence || len(wrapped.Items()) != 1 {
			return Result{rejected: []Diagnostic{diagnostic(CodeInvalidValue, domain.HAAlias{}, "scene", root, "one scene is required")}}, nil
		}
		root = wrapped.Items()[0]
	}
	if root.Kind() != parser.KindMapping {
		return Result{rejected: []Diagnostic{diagnostic(CodeInvalidValue, domain.HAAlias{}, "scene", root, "scene must be a mapping")}}, nil
	}
	idNode, hasID := root.Lookup("id")
	yamlID, idOK := scalarString(idNode)
	nameNode, hasName := root.Lookup("name")
	name, nameOK := scalarString(nameNode)
	entities, hasEntities := root.Lookup("entities")
	if !hasID || !idOK || yamlID == "" || !hasName || !nameOK || strings.TrimSpace(name) == "" || !hasEntities || entities.Kind() != parser.KindMapping {
		return Result{rejected: []Diagnostic{diagnostic(CodeMissingValue, domain.HAAlias{}, "scene", root, "id, name and entities are required")}}, nil
	}
	def := Definition{ref: ref, yamlID: yamlID, name: name}
	for _, entry := range entities.Entries() {
		alias, err := domain.ParseHAAlias(entry.Key)
		if err != nil {
			def.members = append(def.members, Member{source: entry.KeySource, diagnostics: []Diagnostic{diagnostic(CodeInvalidValue, domain.HAAlias{}, entry.Key, entry.Value, "invalid member alias")}})
			continue
		}
		def.members = append(def.members, compileMember(alias, entry.Value))
	}
	if len(def.members) == 0 {
		return Result{rejected: []Diagnostic{diagnostic(CodeMissingValue, domain.HAAlias{}, "entities", entities, "scene has no members")}}, nil
	}
	return Result{accepted: []Definition{def}}, nil
}

func diagnostic(code Code, alias domain.HAAlias, field string, node parser.Node, reason string) Diagnostic {
	return Diagnostic{code: code, alias: alias, field: field, at: node.Source(), path: node.Path(), reason: reason}
}

func compileMember(alias domain.HAAlias, node parser.Node) Member {
	name, _ := alias.Value()
	m := Member{alias: alias, source: node.Source()}
	switch {
	case strings.HasPrefix(name, "light."):
		m.kind = MemberLight
	case strings.HasPrefix(name, "switch."):
		m.kind = MemberSwitch
	case strings.HasPrefix(name, "fan."):
		m.kind = MemberFan
	case strings.HasPrefix(name, "climate."):
		m.kind = MemberClimate
	default:
		m.diagnostics = append(m.diagnostics, diagnostic(CodeUnsupportedValue, alias, "domain", node, "member domain cannot be reproduced"))
		return m
	}
	if node.Kind() != parser.KindMapping && node.Kind() != parser.KindScalar {
		m.diagnostics = append(m.diagnostics, diagnostic(CodeInvalidValue, alias, "state", node, "member must be a state or state mapping"))
		return m
	}
	stateNode := node
	if node.Kind() == parser.KindMapping {
		var found bool
		stateNode, found = node.Lookup("state")
		if !found {
			m.diagnostics = append(m.diagnostics, diagnostic(CodeMissingValue, alias, "state", node, "state is required"))
			return m
		}
	}
	state, valid := scalarString(stateNode)
	if !valid {
		m.diagnostics = append(m.diagnostics, diagnostic(CodeInvalidValue, alias, "state", stateNode, "state must be a string"))
		return m
	}
	switch m.kind {
	case MemberLight:
		m.compileLight(node, state)
	case MemberSwitch:
		if state != "on" && state != "off" {
			m.diagnostics = append(m.diagnostics, diagnostic(CodeUnsupportedValue, alias, "state", stateNode, "switch state must be on or off"))
		} else {
			m.switchOn = state == "on"
		}
	case MemberFan:
		m.compileFan(node, state)
	case MemberClimate:
		m.compileClimate(node, state)
	}
	if node.Kind() == parser.KindMapping {
		for _, entry := range node.Entries() {
			if !knownAttribute(m.kind, entry.Key) {
				m.diagnostics = append(m.diagnostics, diagnostic(CodeUnknownField, alias, entry.Key, entry.Value, "unknown exported attribute; effect cannot be ruled out"))
			}
		}
	}
	return m
}

func (m *Member) compileLight(node parser.Node, state string) {
	if state != "on" && state != "off" {
		m.diagnostics = append(m.diagnostics, diagnostic(CodeUnsupportedValue, m.alias, "state", node, "light state must be on or off"))
		return
	}
	if state == "off" {
		return
	} // HA does not reapply stale OFF attributes.
	m.light.on = true
	b, present := node.Lookup("brightness")
	if !present || isNull(b) {
		m.diagnostics = append(m.diagnostics, diagnostic(CodeMissingValue, m.alias, "brightness", node, "ON brightness is required"))
	} else if value, ok := integer(b, 0, 255); !ok {
		m.diagnostics = append(m.diagnostics, diagnostic(CodeInvalidValue, m.alias, "brightness", b, "brightness must be integer 0..255"))
	} else if value == 0 {
		m.light.on = false // HA light.turn_on brightness zero becomes OFF.
		return             // The service does not use the other ON attributes.
	} else {
		value := uint8(value)
		m.light.brightness = &value
	}
	if effect, ok := node.Lookup("effect"); ok && !isNull(effect) {
		m.diagnostics = append(m.diagnostics, diagnostic(CodeUnsupportedValue, m.alias, "effect", effect, "effect cannot be reproduced"))
	}
	modeNode, present := node.Lookup("color_mode")
	if !present || isNull(modeNode) {
		m.diagnostics = append(m.diagnostics, diagnostic(CodeMissingValue, m.alias, "color_mode", node, "ON color mode is required"))
		return
	}
	mode, ok := scalarString(modeNode)
	if !ok || mode != "brightness" && mode != "color_temp" {
		m.diagnostics = append(m.diagnostics, diagnostic(CodeUnsupportedValue, m.alias, "color_mode", modeNode, "color mode cannot be reproduced"))
		return
	}
	if mode == "color_temp" && m.light.on {
		kelvinNode, present := node.Lookup("color_temp_kelvin")
		if !present || isNull(kelvinNode) {
			m.diagnostics = append(m.diagnostics, diagnostic(CodeMissingValue, m.alias, "color_temp_kelvin", node, "color temperature is required"))
			return
		}
		kelvin, ok := integer(kelvinNode, 1, 1000000)
		if !ok {
			m.diagnostics = append(m.diagnostics, diagnostic(CodeInvalidValue, m.alias, "color_temp_kelvin", kelvinNode, "Kelvin must be a positive integer"))
			return
		}
		ct := uint32(1000000 / kelvin) // HA floor(1e6/K).
		m.light.ctMireds = &ct
	}
}

func (m *Member) compileFan(node parser.Node, state string) {
	if state != "on" && state != "off" {
		m.diagnostics = append(m.diagnostics, diagnostic(CodeUnsupportedValue, m.alias, "state", node, "fan state must be on or off"))
		return
	}
	if state == "off" {
		return // HA does not restore stale percentage on OFF.
	}
	m.fan.on = true
	if preset, ok := node.Lookup("preset_mode"); ok && !isNull(preset) {
		m.diagnostics = append(m.diagnostics, diagnostic(CodeUnsupportedValue, m.alias, "preset_mode", preset, "preset cannot be reproduced"))
	}
	pct, present := node.Lookup("percentage")
	if !present || isNull(pct) {
		m.diagnostics = append(m.diagnostics, diagnostic(CodeMissingValue, m.alias, "percentage", node, "ON percentage is required"))
		return
	}
	value, ok := integer(pct, 0, 100)
	if !ok {
		m.diagnostics = append(m.diagnostics, diagnostic(CodeInvalidValue, m.alias, "percentage", pct, "percentage must be integer 0..100"))
		return
	}
	m.fan.percentage = uint8(value)
}

func (m *Member) compileClimate(node parser.Node, state string) {
	switch state {
	case "off":
		m.climate.mode = domain.ClimateModeOff
	case "heat":
		m.climate.mode = domain.ClimateModeHeat
	case "cool":
		m.climate.mode = domain.ClimateModeCool
	case "auto", "heat_cool":
		m.climate.mode = domain.ClimateModeAuto
	case "dry":
		m.climate.mode = domain.ClimateModeDry
	case "fan_only":
		m.climate.mode = domain.ClimateModeFanOnly
	default:
		m.diagnostics = append(m.diagnostics, diagnostic(CodeUnsupportedValue, m.alias, "state", node, "climate mode cannot be reproduced"))
	}
	for _, field := range []string{"temperature", "humidity"} {
		value, present := node.Lookup(field)
		if !present || isNull(value) {
			continue
		}
		number, ok := finiteNumber(value)
		if !ok {
			m.diagnostics = append(m.diagnostics, diagnostic(CodeInvalidValue, m.alias, field, value, "climate value must be numeric"))
			continue
		}
		if field == "temperature" {
			m.climate.temperature = &number
		} else {
			m.climate.humidity = &number
		}
	}
	for _, field := range []string{"fan_mode", "preset_mode"} {
		value, present := node.Lookup(field)
		if !present || isNull(value) {
			continue
		}
		name, ok := scalarString(value)
		if !ok {
			m.diagnostics = append(m.diagnostics, diagnostic(CodeInvalidValue, m.alias, field, value, "climate option must be string"))
			continue
		}
		if field == "fan_mode" {
			m.climate.fanMode = &name
		} else {
			m.climate.presetMode = &name
		}
	}
	for _, field := range []string{"target_temp_high", "target_temp_low", "swing_mode", "swing_horizontal_mode", "aux_heat"} {
		if value, present := node.Lookup(field); present && !isNull(value) {
			m.diagnostics = append(m.diagnostics, diagnostic(CodeUnsupportedValue, m.alias, field, value, "operative climate attribute is outside command profile"))
		}
	}
}

func scalarString(node parser.Node) (string, bool) {
	scalar, ok := node.Scalar()
	return scalar.Text(), ok && scalar.Kind() == parser.ScalarString
}
func isNull(node parser.Node) bool {
	scalar, ok := node.Scalar()
	return ok && scalar.Kind() == parser.ScalarNull
}
func integer(node parser.Node, min, max int64) (int64, bool) {
	scalar, ok := node.Scalar()
	if !ok || scalar.Kind() != parser.ScalarInteger {
		return 0, false
	}
	value, err := strconv.ParseInt(scalar.Text(), 10, 64)
	return value, err == nil && value >= min && value <= max
}
func finiteNumber(node parser.Node) (float64, bool) {
	scalar, ok := node.Scalar()
	if !ok || scalar.Kind() != parser.ScalarInteger && scalar.Kind() != parser.ScalarFloat {
		return 0, false
	}
	value, err := strconv.ParseFloat(scalar.Text(), 64)
	return value, err == nil && !math.IsNaN(value) && !math.IsInf(value, 0)
}

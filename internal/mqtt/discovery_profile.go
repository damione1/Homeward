package mqtt

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/damione1/Homeward/compat/ha/template"
)

// ProfileHA2026101 is the deliberately narrow Home Assistant 2026.10.1 MQTT
// discovery profile. New fields require a reviewed profile change.
type DiscoveryProfile uint8

const (
	DiscoveryProfileUnspecified DiscoveryProfile = iota
	ProfileHA2026101
)

type componentKind uint8

const (
	componentUnspecified componentKind = iota
	componentBinary
	componentSwitch
	componentSensor
	componentLight
)

type availabilityMode uint8

const (
	availabilityUnspecified availabilityMode = iota
	availabilityAll
	availabilityAny
	availabilityLatest
)

type scalarKind uint8

const (
	scalarUnspecified scalarKind = iota
	scalarText
	scalarBool
)

type scalar struct {
	kind    scalarKind
	text    string
	boolean bool
}

type availabilityConfig struct {
	topic       Topic
	program     template.Program
	hasTemplate bool
	available   scalar
	unavailable scalar
}

type discoveryComponent struct {
	fingerprint           [sha256.Size]byte
	kind                  componentKind
	uniqueID              string
	identifiers           []string
	name                  string
	stateTopic            Topic
	commandTopic          Topic
	hasCommand            bool
	valueProgram          template.Program
	hasValueTemplate      bool
	availability          []availabilityConfig
	availabilityMode      availabilityMode
	payloadOn             scalar
	payloadOff            scalar
	deviceClass           string
	stateClass            string
	unit                  string
	brightness            bool
	brightnessScale       float64
	colorTemp             bool
	minMireds             uint32
	maxMireds             uint32
	unsupportedEffects    bool
	unsupportedColorModes bool
}

var errDiscoveryProfile = errors.New("mqtt: discovery outside ha-2026.10.1/core-1 profile")

type discoveryComponentError struct{ key string }

func (e discoveryComponentError) Error() string { return errDiscoveryProfile.Error() }
func (e discoveryComponentError) Unwrap() error { return errDiscoveryProfile }

// The decoder rejects duplicate keys, trailing data, excess nesting and a
// large component graph before any untrusted object becomes domain input.
func decodeDiscoveryJSON(payload []byte) (map[string]any, error) {
	if len(payload) == 0 || len(payload) > maxPayload || !utf8.Valid(payload) {
		return nil, errDiscoveryProfile
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	nodes := 0
	value, err := decodeDiscoveryNode(dec, 0, &nodes)
	if err != nil {
		return nil, errDiscoveryProfile
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errDiscoveryProfile
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errDiscoveryProfile
	}
	return object, nil
}

func decodeDiscoveryNode(dec *json.Decoder, depth int, nodes *int) (any, error) {
	*nodes++
	if depth > 8 || *nodes > 2048 {
		return nil, errDiscoveryProfile
	}
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := token.(type) {
	case json.Delim:
		switch v {
		case '{':
			object := make(map[string]any)
			for dec.More() {
				if len(object) >= 256 {
					return nil, errDiscoveryProfile
				}
				keyToken, err := dec.Token()
				key, ok := keyToken.(string)
				if err != nil || !ok || key == "" || len(key) > 128 {
					return nil, errDiscoveryProfile
				}
				if _, duplicate := object[key]; duplicate {
					return nil, errDiscoveryProfile
				}
				item, err := decodeDiscoveryNode(dec, depth+1, nodes)
				if err != nil {
					return nil, err
				}
				object[key] = item
			}
			if end, err := dec.Token(); err != nil || end != json.Delim('}') {
				return nil, errDiscoveryProfile
			}
			return object, nil
		case '[':
			items := make([]any, 0)
			for dec.More() {
				if len(items) >= 256 {
					return nil, errDiscoveryProfile
				}
				item, err := decodeDiscoveryNode(dec, depth+1, nodes)
				if err != nil {
					return nil, err
				}
				items = append(items, item)
			}
			if end, err := dec.Token(); err != nil || end != json.Delim(']') {
				return nil, errDiscoveryProfile
			}
			return items, nil
		default:
			return nil, errDiscoveryProfile
		}
	case nil, bool, string, json.Number:
		return v, nil
	default:
		return nil, errDiscoveryProfile
	}
}

var discoveryAliases = map[string]string{
	"avty": "availability", "avty_mode": "availability_mode", "avty_t": "availability_topic", "avty_tpl": "availability_template",
	"bri_scl": "brightness_scale", "cmd_t": "command_topic", "cmps": "components", "def_ent_id": "default_entity_id",
	"dev": "device", "dev_cla": "device_class", "en": "enabled_by_default", "ent_cat": "entity_category",
	"fx_list": "effect_list", "max_mirs": "max_mireds", "min_mirs": "min_mireds", "o": "origin",
	"p": "platform", "pl_avail": "payload_available", "pl_not_avail": "payload_not_available", "pl_off": "payload_off", "pl_on": "payload_on",
	"stat_cla": "state_class", "stat_t": "state_topic", "sup_clrm": "supported_color_modes", "t": "topic",
	"uniq_id": "unique_id", "unit_of_meas": "unit_of_measurement", "val_tpl": "value_template",
}

var discoveryFields = map[string]struct{}{
	"availability": {}, "availability_mode": {}, "availability_topic": {}, "availability_template": {},
	"brightness": {}, "brightness_scale": {}, "command_topic": {}, "components": {}, "default_entity_id": {},
	"device": {}, "device_class": {}, "enabled_by_default": {}, "entity_category": {}, "effect": {}, "effect_list": {},
	"icon": {}, "max_mireds": {}, "min_mireds": {}, "name": {}, "object_id": {}, "origin": {}, "platform": {},
	"payload_available": {}, "payload_not_available": {}, "payload_off": {}, "payload_on": {},
	"schema": {}, "state_class": {}, "state_topic": {}, "supported_color_modes": {}, "topic": {},
	"unique_id": {}, "unit_of_measurement": {}, "value_template": {}, "~": {},
}

func normalizeDiscovery(object map[string]any) (map[string]any, error) {
	result := make(map[string]any, len(object))
	for key, value := range object {
		if alias, ok := discoveryAliases[key]; ok {
			key = alias
		}
		if _, known := discoveryFields[key]; !known {
			return nil, errDiscoveryProfile
		}
		if _, duplicate := result[key]; duplicate {
			return nil, errDiscoveryProfile
		}
		result[key] = value
	}
	return result, nil
}

func stringField(object map[string]any, key string, required bool) (string, error) {
	value, exists := object[key]
	if !exists || value == nil {
		if required {
			return "", errDiscoveryProfile
		}
		return "", nil
	}
	text, ok := value.(string)
	if !ok || len(text) > 4096 || (required && text == "") {
		return "", errDiscoveryProfile
	}
	return text, nil
}

func boolField(object map[string]any, key string) (bool, error) {
	value, exists := object[key]
	if !exists {
		return false, nil
	}
	boolean, ok := value.(bool)
	if !ok {
		return false, errDiscoveryProfile
	}
	return boolean, nil
}

func uintField(object map[string]any, key string) (uint32, error) {
	value, exists := object[key]
	if !exists {
		return 0, nil
	}
	n, ok := value.(json.Number)
	if !ok {
		return 0, errDiscoveryProfile
	}
	parsed, err := strconv.ParseUint(string(n), 10, 32)
	if err != nil {
		return 0, errDiscoveryProfile
	}
	return uint32(parsed), nil
}

func scalarField(object map[string]any, key string, fallback scalar) (scalar, error) {
	value, exists := object[key]
	if !exists {
		return fallback, nil
	}
	switch v := value.(type) {
	case string:
		if v == "" || len(v) > 256 {
			return scalar{}, errDiscoveryProfile
		}
		return scalar{kind: scalarText, text: v}, nil
	case bool:
		return scalar{kind: scalarBool, boolean: v}, nil
	default:
		return scalar{}, errDiscoveryProfile
	}
}

func parseDiscoveryTopic(topic Topic) (componentKind, bool, error) {
	text, err := topic.Value()
	if err != nil {
		return componentUnspecified, false, err
	}
	parts := strings.Split(text, "/")
	if len(parts) < 4 || len(parts) > 5 || parts[0] != "homeassistant" || parts[len(parts)-1] != "config" {
		return componentUnspecified, false, errDiscoveryProfile
	}
	if parts[1] == "device" {
		return componentUnspecified, true, nil
	}
	switch parts[1] {
	case "binary_sensor":
		return componentBinary, false, nil
	case "switch":
		return componentSwitch, false, nil
	case "sensor":
		return componentSensor, false, nil
	case "light":
		return componentLight, false, nil
	default:
		return componentUnspecified, false, errDiscoveryProfile
	}
}

func parseKind(text string) (componentKind, error) {
	switch text {
	case "binary_sensor":
		return componentBinary, nil
	case "switch":
		return componentSwitch, nil
	case "sensor":
		return componentSensor, nil
	case "light":
		return componentLight, nil
	default:
		return componentUnspecified, errDiscoveryProfile
	}
}

func parseIdentifiers(value any) ([]string, error) {
	device, ok := value.(map[string]any)
	if !ok {
		return nil, errDiscoveryProfile
	}
	var raw any
	if ids, exists := device["identifiers"]; exists {
		raw = ids
	}
	if ids, exists := device["ids"]; exists {
		if raw != nil {
			return nil, errDiscoveryProfile
		}
		raw = ids
	}
	for key := range device {
		switch key {
		case "identifiers", "ids", "name", "manufacturer", "mf", "model", "mdl", "model_id", "mdl_id", "hw_version", "hw", "sw_version", "sw", "serial_number", "sn", "suggested_area", "sa", "configuration_url", "cu", "via_device":
		case "connections", "cns": // Connections are not stable identifiers in this profile.
			return nil, errDiscoveryProfile
		default:
			return nil, errDiscoveryProfile
		}
	}
	var identifiers []string
	switch ids := raw.(type) {
	case string:
		identifiers = []string{ids}
	case []any:
		if len(ids) == 0 || len(ids) > 16 {
			return nil, errDiscoveryProfile
		}
		for _, id := range ids {
			text, ok := id.(string)
			if !ok {
				return nil, errDiscoveryProfile
			}
			identifiers = append(identifiers, text)
		}
	default:
		return nil, errDiscoveryProfile
	}
	seen := make(map[string]struct{}, len(identifiers))
	for _, id := range identifiers {
		if id == "" || len(id) > 256 {
			return nil, errDiscoveryProfile
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, errDiscoveryProfile
		}
		seen[id] = struct{}{}
	}
	return identifiers, nil
}

func validateOrigin(value any) error {
	object, ok := value.(map[string]any)
	if !ok {
		return errDiscoveryProfile
	}
	name, ok := object["name"].(string)
	if !ok || name == "" || len(name) > 256 {
		return errDiscoveryProfile
	}
	for key, value := range object {
		switch key {
		case "name", "sw", "sw_version", "url", "support_url":
			if _, ok := value.(string); !ok {
				return errDiscoveryProfile
			}
		default:
			return errDiscoveryProfile
		}
	}
	return nil
}

func validateComponentFields(object map[string]any, kind componentKind) error {
	for key := range object {
		switch key {
		case "components":
			return errDiscoveryProfile
		case "schema", "brightness", "brightness_scale", "supported_color_modes", "min_mireds", "max_mireds", "effect", "effect_list":
			if kind != componentLight {
				return errDiscoveryProfile
			}
		case "state_class", "unit_of_measurement":
			if kind != componentSensor {
				return errDiscoveryProfile
			}
		case "payload_on", "payload_off":
			if kind == componentSensor {
				return errDiscoveryProfile
			}
		case "command_topic":
			if kind == componentSensor || kind == componentBinary {
				return errDiscoveryProfile
			}
		}
	}
	return nil
}

func expandDiscoveryTopic(text, base string) (Topic, error) {
	if text == "" {
		return Topic{}, errDiscoveryProfile
	}
	if strings.Contains(text, "~") {
		if base == "" {
			return Topic{}, errDiscoveryProfile
		}
		switch {
		case text == "~":
			text = base
		case strings.HasPrefix(text, "~") && strings.Count(text, "~") == 1:
			text = base + text[1:]
		case strings.HasSuffix(text, "~") && strings.Count(text, "~") == 1:
			text = text[:len(text)-1] + base
		default:
			return Topic{}, errDiscoveryProfile
		}
	}
	return ParseTopic(text)
}

func parseAvailability(object map[string]any, base string) ([]availabilityConfig, availabilityMode, error) {
	mode := availabilityLatest
	if raw, exists := object["availability_mode"]; exists {
		text, ok := raw.(string)
		if !ok {
			return nil, availabilityUnspecified, errDiscoveryProfile
		}
		switch text {
		case "all":
			mode = availabilityAll
		case "any":
			mode = availabilityAny
		case "latest":
			mode = availabilityLatest
		default:
			return nil, availabilityUnspecified, errDiscoveryProfile
		}
	}
	var entries []any
	if raw, exists := object["availability"]; exists {
		switch v := raw.(type) {
		case []any:
			entries = v
		case map[string]any:
			entries = []any{v}
		default:
			return nil, availabilityUnspecified, errDiscoveryProfile
		}
	}
	if raw, exists := object["availability_topic"]; exists {
		if len(entries) != 0 {
			return nil, availabilityUnspecified, errDiscoveryProfile
		}
		entry := map[string]any{"topic": raw}
		for _, key := range []string{"availability_template", "payload_available", "payload_not_available"} {
			if value, present := object[key]; present {
				if key == "availability_template" {
					entry["value_template"] = value
				} else {
					entry[key] = value
				}
			}
		}
		entries = []any{entry}
	}
	if len(entries) > 16 {
		return nil, availabilityUnspecified, errDiscoveryProfile
	}
	if len(entries) == 0 {
		if _, exists := object["availability_mode"]; exists {
			return nil, availabilityUnspecified, errDiscoveryProfile
		}
		return nil, mode, nil
	}
	result := make([]availabilityConfig, 0, len(entries))
	seen := make(map[Topic]struct{})
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			return nil, availabilityUnspecified, errDiscoveryProfile
		}
		normalized, err := normalizeDiscovery(entry)
		if err != nil {
			return nil, availabilityUnspecified, err
		}
		for key := range normalized {
			switch key {
			case "topic", "value_template", "payload_available", "payload_not_available":
			default:
				return nil, availabilityUnspecified, errDiscoveryProfile
			}
		}
		text, err := stringField(normalized, "topic", true)
		if err != nil {
			return nil, availabilityUnspecified, err
		}
		topic, err := expandDiscoveryTopic(text, base)
		if err != nil {
			return nil, availabilityUnspecified, err
		}
		if _, duplicate := seen[topic]; duplicate {
			return nil, availabilityUnspecified, errDiscoveryProfile
		}
		seen[topic] = struct{}{}
		available, err := scalarField(normalized, "payload_available", scalar{kind: scalarText, text: "online"})
		if err != nil {
			return nil, availabilityUnspecified, err
		}
		unavailable, err := scalarField(normalized, "payload_not_available", scalar{kind: scalarText, text: "offline"})
		if err != nil || available == unavailable {
			return nil, availabilityUnspecified, errDiscoveryProfile
		}
		item := availabilityConfig{topic: topic, available: available, unavailable: unavailable}
		templateText, err := stringField(normalized, "value_template", false)
		if err != nil {
			return nil, availabilityUnspecified, err
		}
		if templateText != "" {
			item.program, err = template.Compile(templateText)
			if err != nil {
				return nil, availabilityUnspecified, errDiscoveryProfile
			}
			item.hasTemplate = true
		}
		result = append(result, item)
	}
	return result, mode, nil
}

func parseDiscoveryComponent(raw map[string]any, inheritedKind componentKind, sharedDevice any, shared map[string]any) (discoveryComponent, error) {
	object, err := normalizeDiscovery(raw)
	if err != nil {
		return discoveryComponent{}, err
	}
	if origin, present := object["origin"]; present {
		if sharedDevice != nil || validateOrigin(origin) != nil {
			return discoveryComponent{}, errDiscoveryProfile
		}
	}
	for _, key := range []string{"state_topic", "command_topic", "availability", "availability_mode", "availability_topic", "availability_template", "payload_available", "payload_not_available", "~"} {
		if _, present := object[key]; !present {
			if value, exists := shared[key]; exists {
				object[key] = value
			}
		}
	}
	if _, present := object["device"]; !present && sharedDevice != nil {
		object["device"] = sharedDevice
	}
	kind := inheritedKind
	if platform, present := object["platform"]; present {
		name, ok := platform.(string)
		if !ok {
			return discoveryComponent{}, errDiscoveryProfile
		}
		parsed, err := parseKind(name)
		if err != nil || (kind != componentUnspecified && kind != parsed) {
			return discoveryComponent{}, errDiscoveryProfile
		}
		kind = parsed
	}
	if kind == componentUnspecified {
		return discoveryComponent{}, errDiscoveryProfile
	}
	canonical, err := json.Marshal(object)
	if err != nil {
		return discoveryComponent{}, errDiscoveryProfile
	}
	if err := validateComponentFields(object, kind); err != nil {
		return discoveryComponent{}, err
	}
	uniqueID, err := stringField(object, "unique_id", true)
	if err != nil || len(uniqueID) > 256 {
		return discoveryComponent{}, errDiscoveryProfile
	}
	identifiers, err := parseIdentifiers(object["device"])
	if err != nil {
		return discoveryComponent{}, err
	}
	base, err := stringField(object, "~", false)
	if err != nil {
		return discoveryComponent{}, err
	}
	stateText, err := stringField(object, "state_topic", true)
	if err != nil {
		return discoveryComponent{}, err
	}
	stateTopic, err := expandDiscoveryTopic(stateText, base)
	if err != nil {
		return discoveryComponent{}, err
	}
	component := discoveryComponent{kind: kind, uniqueID: uniqueID, identifiers: identifiers, stateTopic: stateTopic, brightnessScale: 255}
	component.fingerprint = sha256.Sum256(canonical)
	component.name, err = stringField(object, "name", false)
	if err != nil {
		return discoveryComponent{}, err
	}
	commandText, err := stringField(object, "command_topic", false)
	if err != nil {
		return discoveryComponent{}, err
	}
	if commandText != "" {
		component.commandTopic, err = expandDiscoveryTopic(commandText, base)
		if err != nil {
			return discoveryComponent{}, err
		}
		component.hasCommand = true
	}
	component.availability, component.availabilityMode, err = parseAvailability(object, base)
	if err != nil {
		return discoveryComponent{}, err
	}
	templateText, err := stringField(object, "value_template", false)
	if err != nil {
		return discoveryComponent{}, err
	}
	if templateText != "" {
		component.valueProgram, err = template.Compile(templateText)
		if err != nil {
			return discoveryComponent{}, errDiscoveryProfile
		}
		component.hasValueTemplate = true
	}
	component.deviceClass, err = stringField(object, "device_class", false)
	if err != nil {
		return discoveryComponent{}, err
	}
	component.stateClass, err = stringField(object, "state_class", false)
	if err != nil {
		return discoveryComponent{}, err
	}
	component.unit, err = stringField(object, "unit_of_measurement", false)
	if err != nil {
		return discoveryComponent{}, err
	}
	component.payloadOn, err = scalarField(object, "payload_on", scalar{kind: scalarText, text: "ON"})
	if err != nil {
		return discoveryComponent{}, err
	}
	component.payloadOff, err = scalarField(object, "payload_off", scalar{kind: scalarText, text: "OFF"})
	if err != nil || component.payloadOn == component.payloadOff {
		return discoveryComponent{}, errDiscoveryProfile
	}
	if kind == componentLight {
		if schema, err := stringField(object, "schema", true); err != nil || schema != "json" {
			return discoveryComponent{}, errDiscoveryProfile
		}
		component.brightness, err = boolField(object, "brightness")
		if err != nil {
			return discoveryComponent{}, err
		}
		if _, exists := object["brightness_scale"]; exists {
			var scale uint32
			scale, err = uintField(object, "brightness_scale")
			component.brightnessScale = float64(scale)
			if err != nil || component.brightnessScale < 1 || component.brightnessScale > 65535 {
				return discoveryComponent{}, errDiscoveryProfile
			}
		}
		if rawModes, exists := object["supported_color_modes"]; exists {
			modes, ok := rawModes.([]any)
			if !ok || len(modes) == 0 || len(modes) > 16 {
				return discoveryComponent{}, errDiscoveryProfile
			}
			seenModes := make(map[string]struct{}, len(modes))
			for _, mode := range modes {
				text, ok := mode.(string)
				if !ok {
					return discoveryComponent{}, errDiscoveryProfile
				}
				if _, repeated := seenModes[text]; repeated {
					return discoveryComponent{}, errDiscoveryProfile
				}
				seenModes[text] = struct{}{}
				switch text {
				case "color_temp":
					component.colorTemp = true
				case "brightness", "onoff":
				case "hs", "rgb", "rgbw", "rgbww", "xy", "white":
					component.unsupportedColorModes = true
				default:
					return discoveryComponent{}, errDiscoveryProfile
				}
			}
		}
		component.minMireds, err = uintField(object, "min_mireds")
		if err != nil {
			return discoveryComponent{}, err
		}
		component.maxMireds, err = uintField(object, "max_mireds")
		if err != nil {
			return discoveryComponent{}, err
		}
		if component.colorTemp && (component.minMireds == 0 || component.maxMireds < component.minMireds || component.maxMireds > 1000) {
			return discoveryComponent{}, errDiscoveryProfile
		}
		if rawEffect, exists := object["effect"]; exists {
			active, ok := rawEffect.(bool)
			if !ok {
				return discoveryComponent{}, errDiscoveryProfile
			}
			component.unsupportedEffects = active
		}
		if rawEffects, exists := object["effect_list"]; exists {
			effects, ok := rawEffects.([]any)
			if !ok || len(effects) > 32 {
				return discoveryComponent{}, errDiscoveryProfile
			}
			for _, effect := range effects {
				if _, ok := effect.(string); !ok {
					return discoveryComponent{}, errDiscoveryProfile
				}
			}
			component.unsupportedEffects = component.unsupportedEffects || len(effects) != 0
		}
	} else if _, exists := object["brightness"]; exists {
		return discoveryComponent{}, errDiscoveryProfile
	}
	return component, nil
}

func excludedPlatform(name string) bool {
	switch name {
	case "number", "select", "update", "button", "event", "device_automation", "scene", "climate", "fan", "cover", "lock", "vacuum", "humidifier", "alarm_control_panel", "notify", "text", "siren", "camera", "device_tracker", "date", "datetime", "image", "lawn_mower", "valve", "water_heater":
		return true
	default:
		return false
	}
}

func parseDiscoveryDocument(topic Topic, payload []byte) ([]discoveryComponent, []string, error) {
	kind, deviceBased, err := parseDiscoveryTopic(topic)
	if err != nil {
		return nil, nil, err
	}
	root, err := decodeDiscoveryJSON(payload)
	if err != nil {
		return nil, nil, err
	}
	normalized, err := normalizeDiscovery(root)
	if err != nil {
		return nil, nil, err
	}
	if !deviceBased {
		component, err := parseDiscoveryComponent(normalized, kind, nil, nil)
		if err != nil {
			return nil, nil, err
		}
		return []discoveryComponent{component}, nil, nil
	}
	if _, present := normalized["origin"]; !present {
		return nil, nil, errDiscoveryProfile
	}
	if err := validateOrigin(normalized["origin"]); err != nil {
		return nil, nil, err
	}
	if _, err := parseIdentifiers(normalized["device"]); err != nil {
		return nil, nil, err
	}
	components, ok := normalized["components"].(map[string]any)
	if !ok || len(components) == 0 || len(components) > 64 {
		return nil, nil, errDiscoveryProfile
	}
	result := make([]discoveryComponent, 0, len(components))
	excluded := make([]string, 0)
	seen := make(map[string]struct{})
	ids := make([]string, 0, len(components))
	for id := range components {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		raw := components[id]
		if id == "" || len(id) > 128 {
			return nil, nil, errDiscoveryProfile
		}
		object, ok := raw.(map[string]any)
		if !ok {
			return nil, nil, discoveryComponentError{key: id}
		}
		platform, ok := object["platform"].(string)
		if !ok {
			if abbreviated, has := object["p"].(string); has {
				platform = abbreviated
			} else {
				return nil, nil, discoveryComponentError{key: id}
			}
		}
		if excludedPlatform(platform) {
			// Excluded neighbors have no capability route. Their identity still
			// participates in duplicate checks before an omission can retire.
			unique, err := stringField(object, "unique_id", false)
			if unique == "" {
				unique, err = stringField(object, "uniq_id", true)
			}
			if err != nil || unique == "" || len(unique) > 256 {
				return nil, nil, discoveryComponentError{key: id}
			}
			if _, duplicate := seen[unique]; duplicate {
				return nil, nil, discoveryComponentError{key: id}
			}
			seen[unique] = struct{}{}
			excluded = append(excluded, id)
			continue
		}
		component, err := parseDiscoveryComponent(object, componentUnspecified, normalized["device"], normalized)
		if err != nil {
			return nil, nil, discoveryComponentError{key: id}
		}
		if _, duplicate := seen[component.uniqueID]; duplicate {
			return nil, nil, discoveryComponentError{key: id}
		}
		seen[component.uniqueID] = struct{}{}
		result = append(result, component)
	}
	return result, excluded, nil
}

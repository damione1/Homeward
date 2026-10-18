package mqtt

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

// This is the closed set observed in the redacted Z2M action inventory. The
// position is the private domain value; the spelling is declared only here.
type z2mAction uint8

var z2mActionNames = [...]string{
	"", "on", "off", "toggle", "brightness_move_up", "brightness_move_down", "brightness_stop",
	"arrow_left_click", "arrow_right_click", "volume_up", "volume_down", "volume_up_hold",
	"volume_down_hold", "track_next", "track_previous", "dots_1_initial_press",
	"dots_1_long_press", "dots_1_double_press", "dots_1_short_release", "dots_1_long_release",
	"dots_2_initial_press", "dots_2_long_press", "dots_2_double_press",
	"dots_2_short_release", "dots_2_long_release",
}

func parseZ2MAction(raw string) (z2mAction, bool) {
	for i := 1; i < len(z2mActionNames); i++ {
		if raw == z2mActionNames[i] {
			return z2mAction(i), true
		}
	}
	return 0, false
}

func (a z2mAction) code() (string, bool) {
	if a == 0 || int(a) >= len(z2mActionNames) {
		return "", false
	}
	return "z2m." + z2mActionNames[a], true
}

type actionSourceKind uint8

const (
	actionSourceUnspecified actionSourceKind = iota
	actionTopic
	actionJSON
)

type actionDeclaration struct {
	identifiers []string
	topic       Topic
	kind        actionSourceKind
	action      z2mAction // Present only for a device trigger.
}

// The legacy action sensor is a declaration of an edge source, not an entity
// admitted to the manifest. Z2M 2.14.1 uses the bracket form.
func isLegacyActionSensor(raw map[string]any) bool {
	value, ok := raw["value_template"].(string)
	if !ok {
		value, ok = raw["val_tpl"].(string)
	}
	if !ok {
		return false
	}
	value = strings.TrimSpace(value)
	return value == `{{ value_json["action"] }}` || value == "{{ value_json.action }}" || value == "{{ value_json['action'] }}"
}

func parseActionDeclaration(raw map[string]any, sharedDevice any, shared map[string]any, kind actionSourceKind) (actionDeclaration, error) {
	object, err := normalizeDiscovery(raw)
	if err != nil {
		return actionDeclaration{}, err
	}
	if err := validateActionFields(object, kind); err != nil {
		return actionDeclaration{}, err
	}
	if _, present := object["device"]; !present && sharedDevice != nil {
		object["device"] = sharedDevice
	}
	if shared != nil {
		keys := []string{"~"}
		if kind == actionJSON {
			keys = append(keys, "state_topic", "availability", "availability_mode", "availability_topic", "availability_template", "payload_available", "payload_not_available")
		}
		for _, key := range keys {
			if _, present := object[key]; !present {
				if inherited, exists := shared[key]; exists {
					object[key] = inherited
				}
			}
		}
	}
	ids, err := parseIdentifiers(object["device"])
	if err != nil {
		return actionDeclaration{}, err
	}
	base, err := stringField(object, "~", false)
	if err != nil {
		return actionDeclaration{}, err
	}
	if kind == actionJSON {
		if _, _, err := parseAvailability(object, base); err != nil {
			return actionDeclaration{}, err
		}
	}
	var topicText string
	var action z2mAction
	switch kind {
	case actionTopic:
		if object["automation_type"] != "trigger" || object["type"] != "action" {
			return actionDeclaration{}, errDiscoveryProfile
		}
		payload, err := stringField(object, "payload", true)
		if err != nil || object["subtype"] != payload {
			return actionDeclaration{}, errDiscoveryProfile
		}
		var known bool
		action, known = parseZ2MAction(payload)
		if !known {
			return actionDeclaration{}, errDiscoveryProfile
		}
		topicText, err = stringField(object, "topic", true)
		if err != nil {
			return actionDeclaration{}, err
		}
	case actionJSON:
		if !isLegacyActionSensor(raw) {
			return actionDeclaration{}, errDiscoveryProfile
		}
		topicText, err = stringField(object, "state_topic", true)
		if err != nil {
			return actionDeclaration{}, err
		}
	default:
		return actionDeclaration{}, errDiscoveryProfile
	}
	topic, err := expandDiscoveryTopic(topicText, base)
	if err != nil {
		return actionDeclaration{}, err
	}
	text, _ := topic.Value()
	if !strings.HasPrefix(text, "zigbee2mqtt/") || kind == actionTopic && !strings.HasSuffix(text, "/action") || kind == actionJSON && strings.HasSuffix(text, "/action") {
		return actionDeclaration{}, errDiscoveryProfile
	}
	return actionDeclaration{identifiers: append([]string(nil), ids...), topic: topic, kind: kind, action: action}, nil
}

func validateActionFields(object map[string]any, kind actionSourceKind) error {
	if origin, present := object["origin"]; present {
		if err := validateOrigin(origin); err != nil {
			return err
		}
	}
	for key := range object {
		switch kind {
		case actionTopic:
			switch key {
			case "automation_type", "type", "subtype", "payload", "topic", "device", "origin", "platform", "~":
			default:
				return errDiscoveryProfile
			}
		case actionJSON:
			switch key {
			case "unique_id", "device", "origin", "platform", "name", "object_id", "default_entity_id", "icon", "entity_category", "enabled_by_default", "state_topic", "value_template", "availability", "availability_mode", "availability_topic", "availability_template", "payload_available", "payload_not_available", "~":
			default:
				return errDiscoveryProfile
			}
		default:
			return errDiscoveryProfile
		}
	}
	if platform, exists := object["platform"]; exists {
		wanted := "device_automation"
		if kind == actionJSON {
			wanted = "sensor"
		}
		if platform != wanted {
			return errDiscoveryProfile
		}
	}
	if kind == actionJSON {
		if unique, err := stringField(object, "unique_id", true); err != nil || len(unique) > 256 {
			return errDiscoveryProfile
		}
		for _, key := range []string{"name", "object_id", "default_entity_id", "icon"} {
			if _, err := stringField(object, key, false); err != nil {
				return err
			}
		}
		if category, err := stringField(object, "entity_category", false); err != nil || category != "" && category != "diagnostic" {
			return errDiscoveryProfile
		}
		if _, err := boolField(object, "enabled_by_default"); err != nil {
			return err
		}
	}
	return nil
}

type actionSelectionKind uint8

const (
	selectionUnspecified actionSelectionKind = iota
	selectionActive
	selectionDisabled
)

type actionSource struct {
	kind  actionSourceKind
	topic Topic
}

type actionDevice struct {
	selection actionSelectionKind
	source    actionSource
	anchor    integrations.SourceKey
	entity    domain.EntityID
	binding   integrations.Binding
}

type actionRoute struct {
	device  domain.DeviceID
	source  actionSource
	binding integrations.Binding
	allowed map[z2mAction]struct{} // Only device-trigger routes have explicit payloads.
}

func (m *DiscoveryMapper) setActionDocument(topic Topic, declared []actionDeclaration) error {
	count := len(declared)
	perIdentifier := make(map[string]int)
	for existingTopic, declarations := range m.actionDocuments {
		if existingTopic == topic {
			continue
		}
		count += len(declarations)
		for _, item := range declarations {
			for _, identifier := range item.identifiers {
				perIdentifier[identifier]++
			}
		}
	}
	for _, item := range declared {
		for _, identifier := range item.identifiers {
			perIdentifier[identifier]++
		}
	}
	if count > 1024 {
		m.diagnose(DiagnosticActionOverflow, topic, "")
		return ErrObservationGap
	}
	for _, count := range perIdentifier {
		if count > 64 {
			m.diagnose(DiagnosticActionOverflow, topic, "")
			return ErrObservationGap
		}
	}
	if len(declared) == 0 {
		delete(m.actionDocuments, topic)
	} else {
		m.actionDocuments[topic] = append([]actionDeclaration(nil), declared...)
	}
	return m.rebuildActionRoutes()
}

func (m *DiscoveryMapper) rebuildActionRoutes() error {
	byDevice := make(map[domain.DeviceID][]actionDeclaration)
	byWireID := make(map[string]map[domain.DeviceID]struct{})
	anchorsByDevice := make(map[domain.DeviceID][]*componentRuntime)
	for _, runtime := range m.entities {
		wireID, _, err := runtime.key.Components()
		if err != nil {
			return err
		}
		device, _, err := runtime.binding.IDs()
		if err != nil {
			return err
		}
		if byWireID[wireID] == nil {
			byWireID[wireID] = make(map[domain.DeviceID]struct{})
		}
		byWireID[wireID][device] = struct{}{}
		anchorsByDevice[device] = append(anchorsByDevice[device], runtime)
	}
	for discoveryTopic, declarations := range m.actionDocuments {
		for _, declaration := range declarations {
			matched := make(map[domain.DeviceID]struct{})
			for _, id := range declaration.identifiers {
				for resolved := range byWireID[id] {
					matched[resolved] = struct{}{}
				}
			}
			if len(matched) != 1 {
				code := DiagnosticUnconfiguredActionAnchor
				if len(matched) > 1 {
					code = DiagnosticAmbiguousActionDevice
				}
				m.diagnose(code, discoveryTopic, "")
				continue
			}
			for device := range matched {
				byDevice[device] = append(byDevice[device], declaration)
				if len(byDevice[device]) > 64 {
					m.diagnose(DiagnosticActionOverflow, discoveryTopic, "")
					return ErrObservationGap
				}
			}
		}
	}
	newDevices := 0
	for device := range byDevice {
		if m.actionDevices[device] == nil {
			newDevices++
		}
	}
	if len(m.actionDevices)+newDevices > 1024 {
		m.diagnose(DiagnosticActionOverflow, Topic{}, "")
		return ErrObservationGap
	}
	for device, declarations := range byDevice {
		selected := m.actionDevices[device]
		if selected == nil {
			selected = &actionDevice{}
			m.actionDevices[device] = selected
		}
		if selected.selection == selectionDisabled {
			continue
		}
		if selected.selection == selectionUnspecified {
			anchors := anchorsByDevice[device]
			sort.Slice(anchors, func(i, j int) bool {
				_, a, _ := anchors[i].key.Components()
				_, b, _ := anchors[j].key.Components()
				return a < b
			})
			if len(anchors) == 0 {
				continue
			}
			selected.anchor, selected.entity, selected.binding = anchors[0].key, anchors[0].entity, anchors[0].binding
			sort.Slice(declarations, func(i, j int) bool {
				if declarations[i].kind != declarations[j].kind {
					return declarations[i].kind < declarations[j].kind
				}
				a, _ := declarations[i].topic.Value()
				b, _ := declarations[j].topic.Value()
				return a < b
			})
			selected.source = actionSource{kind: declarations[0].kind, topic: declarations[0].topic}
			selected.selection = selectionActive
		}
	}
	routes := make(map[Topic][]actionRoute)
	for device, selected := range m.actionDevices {
		if selected.selection != selectionActive {
			continue
		}
		anchor := m.entities[selected.entity]
		if anchor == nil || anchor.key != selected.anchor || anchor.binding != selected.binding {
			selected.selection = selectionDisabled
			continue
		}
		var matches []actionDeclaration
		for _, declaration := range byDevice[device] {
			if declaration.kind == selected.source.kind && declaration.topic == selected.source.topic {
				matches = append(matches, declaration)
			}
		}
		if len(matches) == 0 {
			selected.selection = selectionDisabled
			continue
		}
		route := actionRoute{device: device, source: selected.source, binding: selected.binding, allowed: make(map[z2mAction]struct{})}
		for _, declaration := range matches {
			if selected.source.kind == actionTopic {
				route.allowed[declaration.action] = struct{}{}
			}
		}
		routes[selected.source.topic] = append(routes[selected.source.topic], route)
	}
	for topic, candidates := range routes {
		if len(candidates) > 1 {
			m.diagnose(DiagnosticAmbiguousActionDevice, topic, "")
			delete(routes, topic)
		}
	}
	m.actionRoutes = routes
	return nil
}

func (m *DiscoveryMapper) observeAction(ctx context.Context, host integrations.SessionHost, observation Observation, topic Topic, payload []byte) (bool, error) {
	text, _ := topic.Value()
	routes := m.actionRoutes[topic]
	if len(routes) == 0 {
		// The action channel is never a snapshot, even before discovery.
		return strings.HasPrefix(text, "zigbee2mqtt/") && strings.HasSuffix(text, "/action"), nil
	}
	_, _, err := observation.Event()
	if errors.Is(err, ErrNotAnEvent) {
		return routes[0].source.kind == actionTopic, nil
	}
	if err != nil {
		return true, err
	}
	route := routes[0]
	var raw string
	switch route.source.kind {
	case actionTopic:
		raw = string(payload)
	case actionJSON:
		object, err := decodeDiscoveryJSON(payload)
		if err != nil {
			m.diagnose(DiagnosticInvalidAction, topic, "")
			return false, nil
		}
		value, present := object["action"]
		if !present || value == "" {
			return false, nil
		}
		var ok bool
		raw, ok = value.(string)
		if !ok {
			m.diagnose(DiagnosticInvalidAction, topic, "")
			return false, nil
		}
	default:
		return true, errDiscoveryProfile
	}
	action, known := parseZ2MAction(raw)
	if !known {
		m.diagnose(DiagnosticInvalidAction, topic, "")
		return route.source.kind == actionTopic, nil
	}
	if route.source.kind == actionTopic {
		if _, declared := route.allowed[action]; !declared {
			m.diagnose(DiagnosticInvalidAction, topic, "")
			return true, nil
		}
	}
	code, _ := action.code()
	event, err := integrations.NewDeviceEvent(code, time.Now().UTC(), nil)
	if err != nil {
		return true, err
	}
	if err := host.Emit(ctx, route.binding, event); err != nil {
		return true, errors.Join(errDiscoveryHostPublish, err)
	}
	return route.source.kind == actionTopic, nil
}

package ir

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/mqtt"
)

func (c *semanticCompiler) automation(node parser.Node) (Automation, Diagnostic) {
	if d := c.mapping(node, "id", "alias", "description", "category", "mode", "max", "max_exceeded", "variables", "triggers", "conditions", "actions"); d.Valid() {
		return Automation{}, d
	}
	id, d := c.stringField(node, "id", true)
	if d.Valid() {
		return Automation{}, d
	}
	alias, d := c.stringField(node, "alias", false)
	if d.Valid() {
		return Automation{}, d
	}
	description, d := c.stringField(node, "description", false)
	if d.Valid() {
		return Automation{}, d
	}
	category, d := c.stringField(node, "category", false)
	if d.Valid() {
		return Automation{}, d
	}
	mode, d := c.stringField(node, "mode", false)
	if d.Valid() {
		return Automation{}, d
	}
	modeNode, modeSpecified := node.Lookup("mode")
	if !modeSpecified {
		mode = "single"
	}
	parsedMode := parseAutomationMode(mode)
	if parsedMode == AutomationModeInvalid {
		return Automation{}, c.fail(CodeInvalidValue, "mode", modeNode, "mode outside admitted profile")
	}
	maxExceeded, d := c.stringField(node, "max_exceeded", false)
	if d.Valid() {
		return Automation{}, d
	}
	parsedExceeded := parseMaxExceededMode(maxExceeded)
	exceededNode, exceededSpecified := node.Lookup("max_exceeded")
	if exceededSpecified && maxExceeded == "" {
		parsedExceeded = MaxExceededInvalid
	}
	if parsedExceeded == MaxExceededInvalid {
		return Automation{}, c.fail(CodeInvalidValue, "max_exceeded", exceededNode, "max_exceeded outside admitted profile")
	}
	automation := Automation{id: id, alias: alias, description: description, category: category, mode: parsedMode, maxExceeded: parsedExceeded, source: c.source(node)}
	if maximum, ok := node.Lookup("max"); ok {
		count, valid := scalarInt(maximum)
		if !valid || count < 1 || count > 10000 {
			return Automation{}, c.fail(CodeInvalidValue, "max", maximum, "positive max of at most 10000 required")
		}
		automation.max = int(count)
		automation.hasMax = true
	}
	c.triggerIDs = make(map[string]bool)
	triggers, d := c.required(node, "triggers")
	if d.Valid() {
		return Automation{}, d
	}
	automation.triggers, d = c.triggerList(triggers, true, true)
	if d.Valid() {
		return Automation{}, d
	}
	if variables, ok := node.Lookup("variables"); ok {
		automation.variables, d = c.variables(variables)
		if d.Valid() {
			return Automation{}, d
		}
	}
	if conditions, ok := node.Lookup("conditions"); ok {
		automation.conditions, d = c.conditionList(conditions)
		if d.Valid() {
			return Automation{}, d
		}
	}
	actions, d := c.required(node, "actions")
	if d.Valid() {
		return Automation{}, d
	}
	compiled, d := c.actionList(actions, true)
	if d.Valid() {
		return Automation{}, d
	}
	automation.actions = append(automation.actions, compiled...)
	c.need(RequirementNHA022Authorization)
	c.need(RequirementNHA115Execution)
	automation.requirements = append([]Requirement(nil), c.requirements...)
	return automation, Diagnostic{}
}

func (c *semanticCompiler) triggerList(node parser.Node, required, registerAutomationIDs bool) ([]Trigger, Diagnostic) {
	if node.Kind() != parser.KindSequence {
		return nil, c.fail(CodeUnsupportedForm, "triggers", node, "trigger list required")
	}
	items := node.Items()
	if required && len(items) == 0 {
		return nil, c.fail(CodeInvalidValue, "triggers", node, "at least one trigger required")
	}
	result := make([]Trigger, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		trigger, d := c.trigger(item)
		if d.Valid() {
			return nil, d
		}
		if trigger.id != "" {
			if seen[trigger.id] || registerAutomationIDs && c.triggerIDs[trigger.id] {
				return nil, c.fail(CodeInvalidValue, "id", item, "duplicate trigger ID")
			}
			seen[trigger.id] = true
			if registerAutomationIDs {
				c.triggerIDs[trigger.id] = true
			}
		}
		result = append(result, trigger)
	}
	return result, Diagnostic{}
}

func (c *semanticCompiler) trigger(node parser.Node) (Trigger, Diagnostic) {
	if node.Kind() != parser.KindMapping {
		return Trigger{}, c.fail(CodeUnsupportedForm, "trigger", node, "trigger mapping required")
	}
	selection, d := c.stringField(node, "trigger", true)
	if d.Valid() {
		return Trigger{}, d
	}
	id, d := c.stringField(node, "id", false)
	if d.Valid() {
		return Trigger{}, d
	}
	trigger := Trigger{id: id, source: c.source(node)}
	switch selection {
	case "time":
		if d = c.mapping(node, "trigger", "id", "at"); d.Valid() {
			return Trigger{}, d
		}
		at, diag := c.required(node, "at")
		if diag.Valid() {
			return Trigger{}, diag
		}
		value, valid := plain(at)
		if !valid {
			return Trigger{}, c.fail(CodeInvalidValue, "at", at, "time must be HH:MM:SS")
		}
		if containsTemplateSyntax(value) {
			return Trigger{}, c.fail(CodeUnsupportedTemplate, "at", at, "templated time is outside static time profile")
		}
		clock, err := time.Parse("15:04:05", value)
		if err != nil || clock.Format("15:04:05") != value {
			return Trigger{}, c.fail(CodeInvalidValue, "at", at, "time must be HH:MM:SS")
		}
		trigger.kind = TriggerTime
		trigger.secondsOfDay = clock.Hour()*3600 + clock.Minute()*60 + clock.Second()
	case "sun":
		if d = c.mapping(node, "trigger", "id", "event", "offset"); d.Valid() {
			return Trigger{}, d
		}
		event, diag := c.stringField(node, "event", true)
		if diag.Valid() {
			return Trigger{}, diag
		}
		if event != "sunset" {
			return Trigger{}, c.fail(CodeUnsupportedForm, "event", node, "only sunset is in the selected lot")
		}
		trigger.kind = TriggerSunset
		c.need(RequirementNHA105Sun)
		if offset, ok := node.Lookup("offset"); ok {
			value, valid := plain(offset)
			if !valid {
				return Trigger{}, c.fail(CodeInvalidValue, "offset", offset, "offset must be signed HH:MM:SS")
			}
			seconds, valid := clockOffset(value)
			if !valid {
				return Trigger{}, c.fail(CodeInvalidValue, "offset", offset, "offset must be signed HH:MM:SS within 24 hours")
			}
			trigger.offsetSeconds = seconds
		}
	case "event":
		if d = c.mapping(node, "trigger", "id", "event_type", "event_data"); d.Valid() {
			return Trigger{}, d
		}
		name, diag := c.stringField(node, "event_type", true)
		if diag.Valid() {
			return Trigger{}, diag
		}
		nameNode, _ := node.Lookup("event_type")
		if containsTemplateSyntax(name) {
			return Trigger{}, c.fail(CodeUnsupportedTemplate, "event_type", nameNode, "templated event type is outside static event profile")
		}
		eventType, diag := c.requireEvent(name, nameNode)
		if diag.Valid() {
			return Trigger{}, diag
		}
		trigger.kind = TriggerEvent
		trigger.eventType = eventType
		c.need(RequirementNHA048EventOrMQTT)
		if data, ok := node.Lookup("event_data"); ok {
			trigger.eventData, d = c.eventData(data)
			if d.Valid() {
				return Trigger{}, d
			}
		}
	case "mqtt":
		if d = c.mapping(node, "trigger", "id", "topic", "payload"); d.Valid() {
			return Trigger{}, d
		}
		topic, diag := c.stringField(node, "topic", true)
		if diag.Valid() {
			return Trigger{}, diag
		}
		topicNode, _ := node.Lookup("topic")
		if containsTemplateSyntax(topic) {
			return Trigger{}, c.fail(CodeUnsupportedTemplate, "topic", topicNode, "templated MQTT topic is outside static filter profile")
		}
		filter, err := mqtt.ParseTopicFilter(topic)
		if err != nil {
			return Trigger{}, c.fail(CodeInvalidValue, "topic", topicNode, "invalid MQTT topic filter")
		}
		trigger.kind = TriggerMQTT
		trigger.topic = filter
		c.need(RequirementNHA048EventOrMQTT)
		trigger.payload, d = c.stringField(node, "payload", false)
		if d.Valid() {
			return Trigger{}, d
		}
		if payloadNode, ok := node.Lookup("payload"); ok {
			if containsTemplateSyntax(trigger.payload) {
				return Trigger{}, c.fail(CodeUnsupportedTemplate, "payload", payloadNode, "templated MQTT payload is outside static filter profile")
			}
			trigger.hasPayload = true
		}
	case "device":
		if d = c.mapping(node, "trigger", "id", "domain", "device_id", "type", "subtype"); d.Valid() {
			return Trigger{}, d
		}
		domain, diag := c.stringField(node, "domain", true)
		if diag.Valid() {
			return Trigger{}, diag
		}
		kind, diag := c.stringField(node, "type", true)
		if diag.Valid() {
			return Trigger{}, diag
		}
		if domain != "mqtt" || kind != "action" {
			return Trigger{}, c.fail(CodeUnsupportedForm, "domain", node, "only MQTT device action triggers are admitted")
		}
		deviceID, diag := c.stringField(node, "device_id", true)
		if diag.Valid() {
			return Trigger{}, diag
		}
		deviceNode, _ := node.Lookup("device_id")
		deviceRef, diag := c.requireDevice(deviceID, deviceNode)
		if diag.Valid() {
			return Trigger{}, diag
		}
		subtype, diag := c.stringField(node, "subtype", true)
		if diag.Valid() {
			return Trigger{}, diag
		}
		trigger.kind = TriggerDeviceMQTT
		trigger.deviceID = deviceRef
		trigger.subtype = subtype
		c.need(RequirementNHA048EventOrMQTT)
	default:
		return Trigger{}, c.fail(CodeUnsupportedForm, "trigger", node, "trigger variant outside selected lot")
	}
	return trigger, Diagnostic{}
}

func clockOffset(value string) (int, bool) {
	sign := 1
	if strings.HasPrefix(value, "-") {
		sign = -1
		value = value[1:]
	} else if strings.HasPrefix(value, "+") {
		value = value[1:]
	}
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		return 0, false
	}
	for _, part := range parts {
		if len(part) != 2 {
			return 0, false
		}
		for i := 0; i < 2; i++ {
			if part[i] < '0' || part[i] > '9' {
				return 0, false
			}
		}
	}
	h, errH := strconv.Atoi(parts[0])
	m, errM := strconv.Atoi(parts[1])
	s, errS := strconv.Atoi(parts[2])
	if errH != nil || errM != nil || errS != nil || h > 23 || m > 59 || s > 59 {
		return 0, false
	}
	return sign * (h*3600 + m*60 + s), true
}

func (c *semanticCompiler) eventData(node parser.Node) (EventData, Diagnostic) {
	if d := c.mapping(node, "device_id", "command", "cluster_id", "endpoint_id", "args"); d.Valid() {
		return EventData{}, d
	}
	result := EventData{}
	var d Diagnostic
	if value, ok := node.Lookup("device_id"); ok {
		deviceID, diag := c.stringField(node, "device_id", true)
		if diag.Valid() {
			return EventData{}, diag
		}
		if containsTemplateSyntax(deviceID) {
			return EventData{}, c.fail(CodeUnsupportedTemplate, "device_id", value, "templated event device is outside static event-data profile")
		}
		result.deviceID, diag = c.requireDevice(deviceID, value)
		if diag.Valid() {
			return EventData{}, diag
		}
	}
	result.command, d = c.stringField(node, "command", false)
	if d.Valid() {
		return EventData{}, d
	}
	if commandNode, ok := node.Lookup("command"); ok {
		if containsTemplateSyntax(result.command) {
			return EventData{}, c.fail(CodeUnsupportedTemplate, "command", commandNode, "templated event command is outside static event-data profile")
		}
		result.hasCommand = true
	}
	for _, field := range []string{"cluster_id", "endpoint_id"} {
		value, ok := node.Lookup(field)
		if !ok {
			continue
		}
		if literal, valid := plain(value); valid && containsTemplateSyntax(literal) {
			return EventData{}, c.fail(CodeUnsupportedTemplate, field, value, "templated event data is outside static event-data profile")
		}
		number, valid := scalarInt(value)
		if !valid || number < 0 || number > 65535 {
			return EventData{}, c.fail(CodeInvalidValue, field, value, "nonnegative 16-bit integer required")
		}
		if field == "cluster_id" {
			result.clusterID = number
			result.hasCluster = true
		} else {
			result.endpointID = number
			result.hasEndpoint = true
		}
	}
	if args, ok := node.Lookup("args"); ok {
		if args.Kind() != parser.KindSequence {
			return EventData{}, c.fail(CodeUnsupportedForm, "args", args, "integer sequence required")
		}
		result.hasArgs = true
		for _, item := range args.Items() {
			if literal, valid := plain(item); valid && containsTemplateSyntax(literal) {
				return EventData{}, c.fail(CodeUnsupportedTemplate, "args", item, "templated event argument is outside static event-data profile")
			}
			number, valid := scalarInt(item)
			if !valid {
				return EventData{}, c.fail(CodeInvalidValue, "args", item, "integer event argument required")
			}
			result.args = append(result.args, number)
		}
	}
	return result, Diagnostic{}
}

func (c *semanticCompiler) conditionList(node parser.Node) ([]Condition, Diagnostic) {
	if node.Kind() != parser.KindSequence {
		return nil, c.fail(CodeUnsupportedForm, "conditions", node, "condition list required")
	}
	result := make([]Condition, 0, len(node.Items()))
	for _, item := range node.Items() {
		condition, d := c.condition(item)
		if d.Valid() {
			return nil, d
		}
		result = append(result, condition)
	}
	return result, Diagnostic{}
}
func (c *semanticCompiler) condition(node parser.Node) (Condition, Diagnostic) {
	if node.Kind() != parser.KindMapping {
		return Condition{}, c.fail(CodeUnsupportedForm, "condition", node, "condition mapping required")
	}
	selection, d := c.stringField(node, "condition", true)
	if d.Valid() {
		return Condition{}, d
	}
	condition := Condition{source: c.source(node)}
	switch selection {
	case "state":
		if d = c.mapping(node, "condition", "entity_id", "state"); d.Valid() {
			return Condition{}, d
		}
		id, diag := c.stringField(node, "entity_id", true)
		if diag.Valid() {
			return Condition{}, diag
		}
		idNode, _ := node.Lookup("entity_id")
		entityAlias, diag := c.requireEntity(id, SupportObserveState, idNode)
		if diag.Valid() {
			return Condition{}, diag
		}
		state, diag := c.stringField(node, "state", true)
		if diag.Valid() {
			return Condition{}, diag
		}
		condition.kind = ConditionState
		condition.entityID = entityAlias
		condition.state = state
	case "trigger":
		if d = c.mapping(node, "condition", "id"); d.Valid() {
			return Condition{}, d
		}
		ids, diag := c.required(node, "id")
		if diag.Valid() {
			return Condition{}, diag
		}
		var nodes []parser.Node
		if ids.Kind() == parser.KindSequence {
			nodes = ids.Items()
		} else {
			nodes = []parser.Node{ids}
		}
		if len(nodes) == 0 {
			return Condition{}, c.fail(CodeInvalidValue, "id", ids, "trigger ID list cannot be empty")
		}
		for _, item := range nodes {
			value, valid := plain(item)
			if !valid || value == "" {
				return Condition{}, c.fail(CodeInvalidValue, "id", item, "trigger ID string required")
			}
			if !c.triggerIDs[value] {
				return Condition{}, c.fail(CodeUnknownReference, "id", item, fmt.Sprintf("trigger ID %q is not declared", value))
			}
			condition.triggerIDs = append(condition.triggerIDs, value)
		}
		condition.kind = ConditionTrigger
	case "template":
		if d = c.mapping(node, "condition", "value_template"); d.Valid() {
			return Condition{}, d
		}
		value, diag := c.required(node, "value_template")
		if diag.Valid() {
			return Condition{}, diag
		}
		source, valid := plain(value)
		if !valid || source == "" {
			return Condition{}, c.fail(CodeInvalidValue, "value_template", value, "template string required")
		}
		program, err := template.Compile(source)
		if err != nil {
			return Condition{}, c.fail(CodeUnsupportedTemplate, "value_template", value, err.Error())
		}
		condition.kind = ConditionTemplate
		condition.templateSource = source
		condition.program = program
	default:
		return Condition{}, c.fail(CodeUnsupportedForm, "condition", node, "condition variant outside selected lot")
	}
	return condition, Diagnostic{}
}

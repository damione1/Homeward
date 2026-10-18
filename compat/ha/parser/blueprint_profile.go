package parser

// These are closed syntax grammars, not HA action or device admission.
func blueprintInstanceRule() *rule {
	return mapping(map[string]*rule{
		"id": scalar(), "alias": scalar(), "description": scalar(), "category": scalar(),
		"mode":          blueprintModeRule(),
		"use_blueprint": mapping(map[string]*rule{"path": scalar(), "input": {kind: KindMapping, dynamic: &rule{open: true}}}),
	})
}

func blueprintModeRule() *rule {
	return &rule{kind: KindScalar, values: map[string]bool{"single": true, "restart": true, "queued": true}}
}

func scalarOrList() *rule {
	return &rule{alternatives: []*rule{scalar(), sequence(scalar())}}
}

func blueprintBodyRule(placeholders bool) *rule {
	open := &rule{open: true}
	trigger := mapping(map[string]*rule{
		"trigger":    {kind: KindScalar, values: map[string]bool{"event": true, "device": true}},
		"event_type": scalar(), "event_data": open, "id": scalar(),
		"domain": scalar(), "device_id": scalar(), "type": scalar(), "subtype": scalar(),
	})
	conditions := sequence(mapping(map[string]*rule{
		"condition": {kind: KindScalar, values: map[string]bool{"trigger": true, "template": true}},
		"id":        scalarOrList(), "value_template": scalar(),
	}))
	action := mapping(nil)
	actions := sequence(action)
	action.fields = map[string]*rule{
		"variables": {kind: KindMapping, dynamic: open},
		"choose":    sequence(mapping(map[string]*rule{"conditions": conditions, "sequence": actions})),
		"default":   actions, "if": conditions, "then": actions, "else": actions,
		"repeat":   mapping(map[string]*rule{"count": scalar(), "sequence": actions}),
		"parallel": actions, "sequence": actions,
		"wait_for_trigger":    sequence(trigger),
		"timeout":             mapping(map[string]*rule{"milliseconds": scalar(), "seconds": scalar(), "minutes": scalar()}),
		"continue_on_timeout": scalar(), "stop": scalar(),
		"action": scalar(), "metadata": open, "data": open,
		"target": mapping(map[string]*rule{"entity_id": scalarOrList(), "device_id": scalarOrList(), "area_id": scalarOrList()}),
	}
	body := mapping(map[string]*rule{
		"id": scalar(), "alias": scalar(), "description": scalar(), "category": scalar(),
		"mode": blueprintModeRule(), "max": scalar(),
		"max_exceeded": {kind: KindScalar, values: map[string]bool{"silent": true, "warning": true, "error": true}},
		"variables":    {kind: KindMapping, dynamic: open}, "triggers": sequence(trigger), "conditions": conditions, "actions": actions,
	})
	if placeholders {
		allowBlueprintInputs(body, make(map[*rule]bool))
	}
	return body
}

func allowBlueprintInputs(r *rule, seen map[*rule]bool) {
	if r == nil || seen[r] {
		return
	}
	seen[r] = true
	r.input = true
	for _, child := range r.fields {
		allowBlueprintInputs(child, seen)
	}
	for _, child := range r.alternatives {
		allowBlueprintInputs(child, seen)
	}
	allowBlueprintInputs(r.item, seen)
	allowBlueprintInputs(r.dynamic, seen)
}

func blueprintDefinitionRule() *rule {
	body := blueprintBodyRule(true)
	filter := mapping(map[string]*rule{"integration": scalar(), "manufacturer": scalar(), "model": scalar()})
	selector := mapping(map[string]*rule{
		SelectorDevice.String(): mapping(map[string]*rule{"multiple": scalar(), "filter": sequence(filter)}),
		SelectorAction.String(): mapping(map[string]*rule{}), SelectorBoolean.String(): mapping(map[string]*rule{}),
		SelectorNumber.String(): mapping(map[string]*rule{
			"min": scalar(), "max": scalar(), "step": scalar(), "unit_of_measurement": scalar(), "mode": scalar(),
		}),
	})
	leaf := mapping(map[string]*rule{
		"name": scalar(), "description": scalar(), "default": {open: true}, "selector": selector,
	})
	entry := mapping(map[string]*rule{
		"name": scalar(), "description": scalar(), "default": {open: true}, "selector": selector,
		"icon": scalar(), "collapsed": scalar(), "input": {kind: KindMapping, dynamic: leaf},
	})
	body.fields["blueprint"] = mapping(map[string]*rule{
		"name": scalar(), "description": scalar(), "domain": {kind: KindScalar, values: map[string]bool{"automation": true}},
		"author": scalar(), "source_url": scalar(),
		"homeassistant": mapping(map[string]*rule{"min_version": scalar()}),
		"input":         {kind: KindMapping, dynamic: entry},
	})
	return body
}

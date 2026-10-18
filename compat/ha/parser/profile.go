package parser

// The syntax profile is intentionally smaller than Home Assistant's schema.
// Unknown fields at every closed grammar level are errors. Service data is
// open syntax: every entry is retained for the later semantic compiler.
type rule struct {
	kind    Kind
	fields  map[string]*rule
	dynamic *rule
	item    *rule
	open    bool
	values  map[string]bool
}

func scalar() *rule                         { return &rule{kind: KindScalar} }
func mapping(fields map[string]*rule) *rule { return &rule{kind: KindMapping, fields: fields} }
func sequence(item *rule) *rule             { return &rule{kind: KindSequence, item: item} }

func profileRule() *rule {
	openSyntax := &rule{open: true}
	mode := &rule{kind: KindScalar, values: map[string]bool{"single": true, "restart": true, "queued": true}}
	target := mapping(map[string]*rule{"entity_id": scalar(), "device_id": scalar(), "area_id": scalar()})
	trigger := mapping(map[string]*rule{
		"trigger":    {kind: KindScalar, values: map[string]bool{"event": true, "mqtt": true, "state": true}},
		"event_type": scalar(), "topic": scalar(), "payload": scalar(),
		"entity_id": scalar(), "to": scalar(), "from": scalar(), "attribute": scalar(), "for": scalar(),
	})
	action := mapping(map[string]*rule{
		"action": scalar(), "delay": scalar(), "wait_for_trigger": sequence(trigger),
		"timeout": scalar(), "continue_on_timeout": scalar(),
		"target": target, "data": openSyntax, "event": scalar(),
	})
	automation := mapping(map[string]*rule{
		"id": scalar(), "alias": scalar(), "description": scalar(),
		"triggers": sequence(trigger), "actions": sequence(action),
		"mode": mode,
		"max":  scalar(),
	})
	script := mapping(map[string]*rule{
		"alias": scalar(), "description": scalar(), "sequence": sequence(action),
		"mode": mode,
		"max":  scalar(),
	})
	boolHelper := mapping(map[string]*rule{"initial": scalar(), "name": scalar(), "icon": scalar()})
	numberHelper := mapping(map[string]*rule{"initial": scalar(), "min": scalar(), "max": scalar(), "step": scalar(), "name": scalar(), "icon": scalar(), "unit_of_measurement": scalar()})
	counterHelper := mapping(map[string]*rule{"initial": scalar(), "minimum": scalar(), "maximum": scalar(), "step": scalar(), "name": scalar(), "icon": scalar()})
	return mapping(map[string]*rule{
		"automation":    sequence(automation),
		"script":        {kind: KindMapping, dynamic: script},
		"input_boolean": {kind: KindMapping, dynamic: boolHelper},
		"input_number":  {kind: KindMapping, dynamic: numberHelper},
		"counter":       {kind: KindMapping, dynamic: counterHelper},
	})
}

func validateProfile(root Node) *Diagnostic { return validate(root, profileRule()) }

func validate(n Node, r *rule) *Diagnostic {
	if r.open {
		return nil // preserved in full; no field was consumed or dropped
	}
	if n.kind != r.kind && !(r.kind == KindScalar && n.kind == KindSecretRef) {
		return diagnostic(CodeUnsupportedForm, n.source, n.path, "YAML shape outside profile")
	}
	switch r.kind {
	case KindMapping:
		for _, entry := range n.entries {
			child := r.fields[entry.Key]
			if child == nil {
				child = r.dynamic
			}
			if child == nil {
				return diagnostic(CodeUnknownField, entry.KeySource, entry.Value.path, "field outside profile")
			}
			if d := validate(entry.Value, child); d != nil {
				return d
			}
		}
	case KindSequence:
		for _, item := range n.items {
			if d := validate(item, r.item); d != nil {
				return d
			}
		}
	case KindScalar:
		if n.kind == KindSecretRef && len(r.values) != 0 {
			return diagnostic(CodeUnsupportedForm, n.source, n.path, "secret reference cannot select a grammar variant")
		}
		if len(r.values) != 0 && n.scalar.kind != ScalarString {
			return diagnostic(CodeUnsupportedForm, n.source, n.path, "grammar variant must be a string")
		}
		if len(r.values) != 0 && !r.values[n.scalar.text] {
			return diagnostic(CodeUnsupportedForm, n.source, n.path, "scalar value outside profile")
		}
	}
	return nil
}

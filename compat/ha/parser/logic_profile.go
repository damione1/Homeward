package parser

// logicRule extends only the NHA-039 admission surface. Core and declarative
// keep their historical grammar; blueprint definition and expansion keep theirs.
// The semantic compiler still checks every action, target, and open data value.
func logicRule() *rule {
	body := blueprintBodyRule(false)
	// Keep the syntax shape open here so the IR owns the closed vocabularies
	// and can issue field-specific semantic refusals for unknown values.
	body.fields["mode"] = scalar()
	body.fields["max_exceeded"] = scalar()
	trigger := mapping(map[string]*rule{
		"trigger": {kind: KindScalar, values: map[string]bool{
			"time": true, "sun": true, "event": true, "mqtt": true, "device": true,
		}},
		"at": scalar(), "event": scalar(), "offset": scalar(),
		"event_type": scalar(), "event_data": &rule{open: true}, "id": scalar(),
		"topic": scalar(), "payload": scalar(),
		"domain": scalar(), "device_id": scalar(), "type": scalar(), "subtype": scalar(),
	})
	condition := mapping(map[string]*rule{
		"condition": {kind: KindScalar, values: map[string]bool{
			"state": true, "trigger": true, "template": true,
		}},
		"entity_id": scalar(), "state": scalar(),
		"id": scalarOrList(), "value_template": scalar(),
	})
	body.fields["triggers"] = sequence(trigger)
	body.fields["conditions"] = sequence(condition)
	logicActions := body.fields["actions"].item
	logicActions.fields["wait_for_trigger"] = sequence(trigger)
	logicActions.fields["if"] = sequence(condition)
	logicActions.fields["choose"].item.fields["conditions"] = sequence(condition)
	// A source in scope/logic_mvp_sources is a single automation mapping. The
	// wrapped automation list is also admitted for include-based bundles.
	body.fields["automation"] = sequence(bodyWithoutAutomation(body))
	return body
}

func bodyWithoutAutomation(body *rule) *rule {
	fields := make(map[string]*rule, len(body.fields)-1)
	for key, value := range body.fields {
		if key != "automation" {
			fields[key] = value
		}
	}
	return mapping(fields)
}

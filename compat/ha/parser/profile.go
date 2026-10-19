package parser

// The syntax profile is intentionally smaller than Home Assistant's schema.
// Unknown fields at every closed grammar level are errors. Service data is
// open syntax: every entry is retained for the later semantic compiler.
type rule struct {
	kind         Kind
	fields       map[string]*rule
	dynamic      *rule
	item         *rule
	open         bool
	values       map[string]bool
	input        bool
	alternatives []*rule
}

func scalar() *rule                         { return &rule{kind: KindScalar} }
func mapping(fields map[string]*rule) *rule { return &rule{kind: KindMapping, fields: fields} }
func sequence(item *rule) *rule             { return &rule{kind: KindSequence, item: item} }

func profileRule(profile SyntaxProfile) *rule {
	if profile == SyntaxProfileBlueprintInstance {
		return blueprintInstanceRule()
	}
	if profile == SyntaxProfileBlueprintDefinition {
		return blueprintDefinitionRule()
	}
	if profile == SyntaxProfileLogic {
		return logicRule()
	}
	if profile == SyntaxProfileScene {
		return sceneRule()
	}
	if profile == SyntaxProfileLovelace {
		return lovelaceRule()
	}
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
	boolHelper := mapping(map[string]*rule{FieldInitial.Key(): scalar(), FieldName.Key(): scalar(), FieldIcon.Key(): scalar()})
	numberHelper := mapping(map[string]*rule{FieldInitial.Key(): scalar(), FieldMin.Key(): scalar(), FieldMax.Key(): scalar(), FieldStep.Key(): scalar(), FieldName.Key(): scalar(), FieldIcon.Key(): scalar(), FieldUnitOfMeasurement.Key(): scalar()})
	counterHelper := mapping(map[string]*rule{FieldInitial.Key(): scalar(), FieldMinimum.Key(): scalar(), FieldMaximum.Key(): scalar(), FieldStep.Key(): scalar(), FieldName.Key(): scalar(), FieldIcon.Key(): scalar()})
	root := mapping(map[string]*rule{
		FamilyAutomation.Key():   sequence(automation),
		FamilyScript.Key():       {kind: KindMapping, dynamic: script},
		FamilyInputBoolean.Key(): {kind: KindMapping, dynamic: boolHelper},
		FamilyInputNumber.Key():  {kind: KindMapping, dynamic: numberHelper},
		FamilyCounter.Key():      {kind: KindMapping, dynamic: counterHelper},
	})
	if profile == SyntaxProfileDeclarative || profile == SyntaxProfileDeclarative2 {
		root.fields[FamilyInputNumber.Key()].dynamic.fields[FieldMode.Key()] = scalar()
		group := mapping(map[string]*rule{
			FieldName.Key(): scalar(), FieldEntities.Key(): sequence(scalar()),
			FieldAll.Key(): scalar(), FieldIcon.Key(): scalar(),
		})
		root.fields[FamilyGroup.Key()] = &rule{kind: KindMapping, dynamic: group}
	}
	if profile == SyntaxProfileDeclarative2 {
		root.fields[FamilyCounter.Key()].dynamic.fields[FieldRestore.Key()] = scalar()
		root.fields[FamilyInputButton.Key()] = &rule{kind: KindMapping, dynamic: mapping(map[string]*rule{
			FieldName.Key(): scalar(), FieldIcon.Key(): scalar(),
		})}
		root.fields[FamilyInputDatetime.Key()] = &rule{kind: KindMapping, dynamic: mapping(map[string]*rule{
			FieldName.Key(): scalar(), FieldIcon.Key(): scalar(), FieldInitial.Key(): scalar(),
			FieldHasDate.Key(): scalar(), FieldHasTime.Key(): scalar(),
		})}
		root.fields[FamilyInputSelect.Key()] = &rule{kind: KindMapping, dynamic: mapping(map[string]*rule{
			FieldName.Key(): scalar(), FieldIcon.Key(): scalar(), FieldInitial.Key(): scalar(),
			FieldOptions.Key(): sequence(scalar()),
		})}
		root.fields[FamilyInputText.Key()] = &rule{kind: KindMapping, dynamic: mapping(map[string]*rule{
			FieldName.Key(): scalar(), FieldIcon.Key(): scalar(), FieldInitial.Key(): scalar(),
			FieldMin.Key(): scalar(), FieldMax.Key(): scalar(), FieldMode.Key(): scalar(),
		})}
	}
	return root
}

// Scene syntax is closed at its structural levels. Entity attributes are
// retained like service data so the scene compiler can refuse one member at a
// time with its original path and position, without discarding other members.
func sceneRule() *rule {
	member := &rule{alternatives: []*rule{{open: true, kind: KindMapping}, {kind: KindScalar}}}
	entities := &rule{kind: KindMapping, dynamic: member}
	metadata := &rule{kind: KindMapping, dynamic: mapping(map[string]*rule{"entity_only": scalar()})}
	scene := mapping(map[string]*rule{
		"id": scalar(), "name": scalar(), "icon": scalar(),
		"entities": entities, "metadata": metadata,
	})
	// Both accepted forms have a mapping root. The semantic compiler requires
	// exactly one form; a union here only admits its known structural keys.
	scene.fields["scene"] = sequence(mapping(map[string]*rule{
		"id": scalar(), "name": scalar(), "icon": scalar(),
		"entities": entities, "metadata": metadata,
	}))
	return scene
}

func validateProfile(root Node, profile SyntaxProfile) *Diagnostic {
	return validate(root, profileRule(profile))
}

func validate(n Node, r *rule) *Diagnostic {
	if n.kind == KindInputRef && r.input {
		return nil
	}
	if len(r.alternatives) != 0 {
		for _, variant := range r.alternatives {
			if n.kind == variant.kind || (n.kind == KindSecretRef && variant.kind == KindScalar) {
				return validate(n, variant)
			}
		}
		return diagnostic(CodeUnsupportedForm, n.source, n.path, "YAML shape outside profile")
	}
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

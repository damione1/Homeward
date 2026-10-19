package parser

// Family is the closed set of top-level categories in the supported syntax.
// The spelling table is shared by grammar validation and business admission.
type Family uint8

const (
	FamilyInvalid Family = iota
	FamilyAutomation
	FamilyScript
	FamilyInputBoolean
	FamilyInputNumber
	FamilyCounter
	FamilyGroup
	FamilyInputButton
	FamilyInputDatetime
	FamilyInputSelect
	FamilyInputText
)

var familyKeys = [...]string{
	FamilyInvalid:       "",
	FamilyAutomation:    "automation",
	FamilyScript:        "script",
	FamilyInputBoolean:  "input_boolean",
	FamilyInputNumber:   "input_number",
	FamilyCounter:       "counter",
	FamilyGroup:         "group",
	FamilyInputButton:   "input_button",
	FamilyInputDatetime: "input_datetime",
	FamilyInputSelect:   "input_select",
	FamilyInputText:     "input_text",
}

func (f Family) Key() string {
	if int(f) >= len(familyKeys) {
		return ""
	}
	return familyKeys[f]
}

func FamilyFromKey(key string) Family {
	for f := FamilyAutomation; int(f) < len(familyKeys); f++ {
		if familyKeys[f] == key {
			return f
		}
	}
	return FamilyInvalid
}

// DeclarativeField covers fields whose meaning NHA-028 admits. Program fields
// remain syntax only until NHA-039.
type DeclarativeField uint8

const (
	FieldInvalid DeclarativeField = iota
	FieldInitial
	FieldName
	FieldIcon
	FieldMin
	FieldMax
	FieldStep
	FieldMinimum
	FieldMaximum
	FieldUnitOfMeasurement
	FieldMode
	FieldEntities
	FieldAll
	FieldHasDate
	FieldHasTime
	FieldOptions
	FieldRestore
)

var declarativeFieldKeys = [...]string{
	FieldInvalid:           "",
	FieldInitial:           "initial",
	FieldName:              "name",
	FieldIcon:              "icon",
	FieldMin:               "min",
	FieldMax:               "max",
	FieldStep:              "step",
	FieldMinimum:           "minimum",
	FieldMaximum:           "maximum",
	FieldUnitOfMeasurement: "unit_of_measurement",
	FieldMode:              "mode",
	FieldEntities:          "entities",
	FieldAll:               "all",
	FieldHasDate:           "has_date",
	FieldHasTime:           "has_time",
	FieldOptions:           "options",
	FieldRestore:           "restore",
}

func (f DeclarativeField) Key() string {
	if int(f) >= len(declarativeFieldKeys) {
		return ""
	}
	return declarativeFieldKeys[f]
}

func DeclarativeFieldFromKey(key string) DeclarativeField {
	for f := FieldInitial; int(f) < len(declarativeFieldKeys); f++ {
		if declarativeFieldKeys[f] == key {
			return f
		}
	}
	return FieldInvalid
}

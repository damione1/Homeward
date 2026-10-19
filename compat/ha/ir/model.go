// Package ir compiles a deliberately small Home Assistant logic profile into
// immutable, non-executable Go values. It neither authorizes nor dispatches a
// command. A parser syntax tree never escapes in an admitted value.
package ir

import (
	"time"
	"unsafe"

	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/mqtt"
)

type Code uint8

const (
	CodeInvalid Code = iota
	CodeInvalidInput
	CodeUnknownField
	CodeUnsupportedForm
	CodeInvalidValue
	CodeUnknownReference
	CodeExcludedReference
	CodeMissingCapability
	CodeUnknownService
	CodeUnsupportedTemplate
)

func (c Code) String() string {
	switch c {
	case CodeInvalidInput:
		return "HA_IR_INVALID_INPUT"
	case CodeUnknownField:
		return "HA_IR_UNKNOWN_FIELD"
	case CodeUnsupportedForm:
		return "HA_IR_UNSUPPORTED_FORM"
	case CodeInvalidValue:
		return "HA_IR_INVALID_VALUE"
	case CodeUnknownReference:
		return "HA_IR_UNKNOWN_REFERENCE"
	case CodeExcludedReference:
		return "HA_IR_EXCLUDED_REFERENCE"
	case CodeMissingCapability:
		return "HA_IR_MISSING_CAPABILITY"
	case CodeUnknownService:
		return "HA_IR_UNKNOWN_SERVICE"
	case CodeUnsupportedTemplate:
		return "HA_IR_UNSUPPORTED_TEMPLATE"
	default:
		return "HA_IR_INVALID"
	}
}

type Position struct {
	File         string
	Line, Column int
}

type ReferenceKind uint8

const (
	ReferenceInvalid ReferenceKind = iota
	ReferenceInclude
	ReferenceAlias
)

type Reference struct {
	Kind ReferenceKind
	At   Position
}

type InputOrigin uint8

const (
	InputOriginInvalid InputOrigin = iota
	InputOriginInvocation
	InputOriginDefault
)

type InputUse struct {
	name        string
	at          Position
	declaration Position
	origin      InputOrigin
	valueSource Position
	references  []Reference
}

func (u InputUse) Name() string            { return u.name }
func (u InputUse) At() Position            { return u.at }
func (u InputUse) Declaration() Position   { return u.declaration }
func (u InputUse) Origin() InputOrigin     { return u.origin }
func (u InputUse) ValueSource() Position   { return u.valueSource }
func (u InputUse) References() []Reference { return append([]Reference(nil), u.references...) }
func (u InputUse) clone() InputUse         { u.references = u.References(); return u }

type Source struct {
	object     string
	at         Position
	path       string
	references []Reference
	inputs     []InputUse
}

func (s Source) Object() string          { return s.object }
func (s Source) At() Position            { return s.at }
func (s Source) Path() string            { return s.path }
func (s Source) References() []Reference { return append([]Reference(nil), s.references...) }
func (s Source) Inputs() []InputUse {
	out := make([]InputUse, len(s.inputs))
	for i, item := range s.inputs {
		out[i] = item.clone()
	}
	return out
}
func (s Source) clone() Source { s.references = s.References(); s.inputs = s.Inputs(); return s }
func (s Source) planBytes() int {
	bytes := 2*(len(s.object)+len(s.at.File)+len(s.path)) +
		len(s.references)*int(unsafe.Sizeof(Reference{})) + len(s.inputs)*int(unsafe.Sizeof(InputUse{}))
	for _, reference := range s.references {
		bytes += 2 * len(reference.At.File)
	}
	for _, input := range s.inputs {
		bytes += 2 * (len(input.name) + len(input.at.File) + len(input.declaration.File) + len(input.valueSource.File))
		bytes += len(input.references) * int(unsafe.Sizeof(Reference{}))
		for _, reference := range input.references {
			bytes += 2 * len(reference.At.File)
		}
	}
	return bytes
}

type Diagnostic struct {
	code       Code
	object     string
	field      string
	source     Source
	reason     string
	syntaxCode string
}

func (d Diagnostic) Code() Code         { return d.code }
func (d Diagnostic) Object() string     { return d.object }
func (d Diagnostic) Field() string      { return d.field }
func (d Diagnostic) Source() Source     { return d.source.clone() }
func (d Diagnostic) Reason() string     { return d.reason }
func (d Diagnostic) SyntaxCode() string { return d.syntaxCode }
func (d Diagnostic) Valid() bool        { return d.code != CodeInvalid && d.object != "" }
func (d Diagnostic) clone() Diagnostic  { d.source = d.source.clone(); return d }

type Result struct {
	accepted []Automation
	rejected []Diagnostic
}

func (r Result) Accepted() []Automation {
	out := make([]Automation, len(r.accepted))
	for i, item := range r.accepted {
		out[i] = item.clone()
	}
	return out
}
func (r Result) Rejected() []Diagnostic {
	out := make([]Diagnostic, len(r.rejected))
	for i, item := range r.rejected {
		out[i] = item.clone()
	}
	return out
}

type Requirement uint8

const (
	RequirementInvalid Requirement = iota
	RequirementNHA022Authorization
	RequirementNHA046Scene
	RequirementNHA046Transition
	RequirementNHA048EventOrMQTT
	RequirementNHA105Sun
	RequirementNHA115Execution
	RequirementObservedBrightnessAndRevision
)

type AutomationMode uint8

const (
	AutomationModeInvalid AutomationMode = iota
	AutomationSingle
	AutomationRestart
	AutomationQueued
)

func (m AutomationMode) String() string {
	switch m {
	case AutomationSingle:
		return "single"
	case AutomationRestart:
		return "restart"
	case AutomationQueued:
		return "queued"
	default:
		return ""
	}
}
func parseAutomationMode(value string) AutomationMode {
	switch value {
	case "single":
		return AutomationSingle
	case "restart":
		return AutomationRestart
	case "queued":
		return AutomationQueued
	default:
		return AutomationModeInvalid
	}
}

type MaxExceededMode uint8

const (
	MaxExceededInvalid MaxExceededMode = iota
	MaxExceededUnspecified
	MaxExceededSilent
	MaxExceededWarning
	MaxExceededError
)

func (m MaxExceededMode) String() string {
	switch m {
	case MaxExceededSilent:
		return "silent"
	case MaxExceededWarning:
		return "warning"
	case MaxExceededError:
		return "error"
	default:
		return ""
	}
}
func parseMaxExceededMode(value string) MaxExceededMode {
	switch value {
	case "":
		return MaxExceededUnspecified
	case "silent":
		return MaxExceededSilent
	case "warning":
		return MaxExceededWarning
	case "error":
		return MaxExceededError
	default:
		return MaxExceededInvalid
	}
}

type Automation struct {
	id, alias, description, category string
	mode                             AutomationMode
	maxExceeded                      MaxExceededMode
	max                              int
	hasMax                           bool
	source                           Source
	triggers                         []Trigger
	conditions                       []Condition
	variables                        []Variable
	actions                          []Action
	requirements                     []Requirement
}

func (a Automation) Valid() bool {
	return a.id != "" && a.mode != AutomationModeInvalid && a.maxExceeded != MaxExceededInvalid && len(a.triggers) > 0 && len(a.actions) > 0
}
func (a Automation) ID() string                   { return a.id }
func (a Automation) Alias() string                { return a.alias }
func (a Automation) Description() string          { return a.description }
func (a Automation) Category() string             { return a.category }
func (a Automation) Mode() AutomationMode         { return a.mode }
func (a Automation) Max() (int, bool)             { return a.max, a.hasMax }
func (a Automation) MaxExceeded() MaxExceededMode { return a.maxExceeded }
func (a Automation) Source() Source               { return a.source.clone() }
func (a Automation) Triggers() []Trigger          { return cloneTriggers(a.triggers) }

// TriggerPlanBytes charges the selected trigger projection before the runtime
// clones it. It includes owned source arrays and dynamic strings, not actions
// or conditions, which the trigger service never retains.
func (a Automation) TriggerPlanBytes() (int, int) {
	bytes := 256
	for _, trigger := range a.triggers {
		alias, _ := trigger.entityAlias.Value()
		device, _ := trigger.deviceID.Value()
		eventType, _ := trigger.eventType.Value()
		topic, _ := trigger.topic.Value()
		dataDevice, _ := trigger.eventData.deviceID.Value()
		bytes += int(unsafe.Sizeof(Trigger{})) + 256 + 2*(len(a.id)+len(trigger.id)+len(trigger.key)+
			len(trigger.from)+len(trigger.to)+len(trigger.payload)+len(trigger.subtype)+len(trigger.eventData.command)+
			len(alias)+len(device)+len(eventType)+len(topic)+len(dataDevice))
		bytes += len(trigger.eventData.args) * int(unsafe.Sizeof(int64(0)))
		bytes += trigger.source.planBytes()
	}
	return len(a.triggers), bytes
}
func (a Automation) Conditions() []Condition { return cloneConditions(a.conditions) }
func (a Automation) Variables() []Variable {
	out := make([]Variable, len(a.variables))
	for i, v := range a.variables {
		out[i] = v.clone()
	}
	return out
}
func (a Automation) Actions() []Action { return cloneActions(a.actions) }
func (a Automation) Requirements() []Requirement {
	return append([]Requirement(nil), a.requirements...)
}
func (a Automation) clone() Automation {
	a.source = a.source.clone()
	a.triggers = a.Triggers()
	a.conditions = a.Conditions()
	a.variables = a.Variables()
	a.actions = a.Actions()
	a.requirements = a.Requirements()
	return a
}

type TriggerKind uint8

const (
	TriggerInvalid TriggerKind = iota
	TriggerTime
	TriggerSunset
	TriggerEvent
	TriggerMQTT
	TriggerDeviceMQTT
	TriggerState
	TriggerNumericState
	TriggerTimePattern
	TriggerCalendar
)

// TriggerAttribute is the typed subset of HA state attributes represented by
// Homeward's observed values. AttributeUnspecified selects the primary state.
type TriggerAttribute uint8

const (
	AttributeUnspecified TriggerAttribute = iota
	AttributeBrightness
	AttributeColorTemp
)

type PatternPartKind uint8

const (
	PatternPartInvalid PatternPartKind = iota
	PatternPartAny
	PatternPartExact
	PatternPartEvery
)

type PatternPart struct {
	kind  PatternPartKind
	value uint8
}

func (p PatternPart) Kind() PatternPartKind { return p.kind }
func (p PatternPart) Value() int            { return int(p.value) }

type CalendarEdge uint8

const (
	CalendarEdgeInvalid CalendarEdge = iota
	CalendarEdgeStart
	CalendarEdgeEnd
)

type EventData struct {
	deviceID                HADeviceRef
	command                 string
	hasCommand              bool
	clusterID, endpointID   int64
	hasCluster, hasEndpoint bool
	args                    []int64
	hasArgs                 bool
}

func (e EventData) DeviceRef() HADeviceRef    { return e.deviceID }
func (e EventData) Command() (string, bool)   { return e.command, e.hasCommand }
func (e EventData) ClusterID() (int64, bool)  { return e.clusterID, e.hasCluster }
func (e EventData) EndpointID() (int64, bool) { return e.endpointID, e.hasEndpoint }
func (e EventData) Args() ([]int64, bool)     { return append([]int64(nil), e.args...), e.hasArgs }
func (e EventData) clone() EventData          { e.args, _ = e.Args(); return e }

type Trigger struct {
	kind                                         TriggerKind
	id                                           string
	key                                          string
	source                                       Source
	entityAlias                                  domain.HAAlias
	attribute                                    TriggerAttribute
	from, to                                     string
	hasFrom, hasTo                               bool
	above, below                                 float64
	hasAbove, hasBelow                           bool
	forDuration                                  time.Duration
	hasFor                                       bool
	patternHours, patternMinutes, patternSeconds PatternPart
	calendarEdge                                 CalendarEdge
	secondsOfDay                                 int
	offsetSeconds                                int
	eventType                                    EventType
	deviceID                                     HADeviceRef
	topic                                        mqtt.TopicFilter
	payload, subtype                             string
	hasPayload                                   bool
	eventData                                    EventData
}

func (t Trigger) Kind() TriggerKind           { return t.kind }
func (t Trigger) ID() string                  { return t.id }
func (t Trigger) Key() string                 { return t.key }
func (t Trigger) Source() Source              { return t.source.clone() }
func (t Trigger) EntityAlias() domain.HAAlias { return t.entityAlias }
func (t Trigger) Attribute() TriggerAttribute { return t.attribute }
func (t Trigger) From() (string, bool)        { return t.from, t.hasFrom }
func (t Trigger) To() (string, bool)          { return t.to, t.hasTo }
func (t Trigger) Above() (float64, bool)      { return t.above, t.hasAbove }
func (t Trigger) Below() (float64, bool)      { return t.below, t.hasBelow }
func (t Trigger) For() (time.Duration, bool)  { return t.forDuration, t.hasFor }
func (t Trigger) Pattern() (hours, minutes, seconds PatternPart) {
	return t.patternHours, t.patternMinutes, t.patternSeconds
}
func (t Trigger) CalendarEdge() CalendarEdge { return t.calendarEdge }
func (t Trigger) SecondsOfDay() int          { return t.secondsOfDay }
func (t Trigger) OffsetSeconds() int         { return t.offsetSeconds }
func (t Trigger) EventType() EventType       { return t.eventType }
func (t Trigger) Topic() mqtt.TopicFilter    { return t.topic }
func (t Trigger) Payload() (string, bool)    { return t.payload, t.hasPayload }
func (t Trigger) DeviceRef() HADeviceRef     { return t.deviceID }
func (t Trigger) Subtype() string            { return t.subtype }
func (t Trigger) Data() EventData            { return t.eventData.clone() }
func (t Trigger) clone() Trigger {
	t.source = t.source.clone()
	t.eventData = t.eventData.clone()
	return t
}
func cloneTriggers(in []Trigger) []Trigger {
	out := make([]Trigger, len(in))
	for i, item := range in {
		out[i] = item.clone()
	}
	return out
}

type ConditionKind uint8

const (
	ConditionInvalid ConditionKind = iota
	ConditionState
	ConditionTrigger
	ConditionTemplate
)

type Condition struct {
	kind           ConditionKind
	source         Source
	entityID       domain.HAAlias
	state          string
	triggerIDs     []string
	templateSource string
	program        template.Program
}

func (c Condition) Kind() ConditionKind         { return c.kind }
func (c Condition) Source() Source              { return c.source.clone() }
func (c Condition) EntityAlias() domain.HAAlias { return c.entityID }
func (c Condition) State() string               { return c.state }
func (c Condition) TriggerIDs() []string        { return append([]string(nil), c.triggerIDs...) }
func (c Condition) TemplateSource() string      { return c.templateSource }
func (c Condition) Program() template.Program   { return c.program }
func (c Condition) clone() Condition {
	c.source = c.source.clone()
	c.triggerIDs = c.TriggerIDs()
	return c
}
func cloneConditions(in []Condition) []Condition {
	out := make([]Condition, len(in))
	for i, item := range in {
		out[i] = item.clone()
	}
	return out
}

type ValueKind uint8

const (
	ValueInvalid ValueKind = iota
	ValueString
	ValueBool
	ValueInteger
	ValueDecimal
	ValueNull
)

type Value struct {
	kind    ValueKind
	source  Source
	text    string
	boolean bool
	integer int64
	decimal float64
}

func (v Value) Kind() ValueKind  { return v.kind }
func (v Value) Source() Source   { return v.source.clone() }
func (v Value) Text() string     { return v.text }
func (v Value) Bool() bool       { return v.boolean }
func (v Value) Integer() int64   { return v.integer }
func (v Value) Decimal() float64 { return v.decimal }
func (v Value) clone() Value     { v.source = v.source.clone(); return v }

type Variable struct {
	name   string
	value  Value
	source Source
}

func (v Variable) Name() string    { return v.name }
func (v Variable) Value() Value    { return v.value.clone() }
func (v Variable) Source() Source  { return v.source.clone() }
func (v Variable) clone() Variable { v.value = v.value.clone(); v.source = v.source.clone(); return v }

type ActionKind uint8

const (
	ActionInvalid ActionKind = iota
	ActionVariables
	ActionSequence
	ActionChoose
	ActionIf
	ActionRepeat
	ActionParallel
	ActionWait
	ActionStop
	ActionCall
)

type Branch struct {
	conditions []Condition
	actions    []Action
	source     Source
}

func (b Branch) Conditions() []Condition { return cloneConditions(b.conditions) }
func (b Branch) Actions() []Action       { return cloneActions(b.actions) }
func (b Branch) Source() Source          { return b.source.clone() }
func (b Branch) clone() Branch {
	b.conditions = b.Conditions()
	b.actions = b.Actions()
	b.source = b.source.clone()
	return b
}

type Action struct {
	kind                                                ActionKind
	source                                              Source
	variables                                           []Variable
	conditions                                          []Condition
	branches                                            []Branch
	children                                            []Action
	otherwise                                           []Action
	triggers                                            []Trigger
	count                                               int
	timeoutMillis                                       int64
	hasTimeout, continueOnTimeout, hasContinueOnTimeout bool
	stop                                                string
	call                                                Call
}

func (a Action) Kind() ActionKind { return a.kind }
func (a Action) Source() Source   { return a.source.clone() }
func (a Action) Variables() []Variable {
	out := make([]Variable, len(a.variables))
	for i, v := range a.variables {
		out[i] = v.clone()
	}
	return out
}
func (a Action) Conditions() []Condition { return cloneConditions(a.conditions) }
func (a Action) Branches() []Branch {
	out := make([]Branch, len(a.branches))
	for i, v := range a.branches {
		out[i] = v.clone()
	}
	return out
}
func (a Action) Children() []Action              { return cloneActions(a.children) }
func (a Action) Otherwise() []Action             { return cloneActions(a.otherwise) }
func (a Action) Triggers() []Trigger             { return cloneTriggers(a.triggers) }
func (a Action) Count() int                      { return a.count }
func (a Action) TimeoutMillis() (int64, bool)    { return a.timeoutMillis, a.hasTimeout }
func (a Action) ContinueOnTimeout() (bool, bool) { return a.continueOnTimeout, a.hasContinueOnTimeout }
func (a Action) Stop() string                    { return a.stop }
func (a Action) Call() Call                      { return a.call.clone() }
func (a Action) clone() Action {
	a.source = a.source.clone()
	a.variables = a.Variables()
	a.conditions = a.Conditions()
	a.branches = a.Branches()
	a.children = a.Children()
	a.otherwise = a.Otherwise()
	a.triggers = a.Triggers()
	a.call = a.Call()
	return a
}
func cloneActions(in []Action) []Action {
	out := make([]Action, len(in))
	for i, v := range in {
		out[i] = v.clone()
	}
	return out
}

type CallKind uint8

const (
	CallInvalid CallKind = iota
	CallClimateSetTemperature
	CallSwitchTurnOn
	CallLightTurnOn
	CallLightTurnOff
	CallSceneTurnOn
)

type Number struct {
	present bool
	value   float64
	source  Source
}

func (n Number) Present() bool  { return n.present }
func (n Number) Value() float64 { return n.value }
func (n Number) Source() Source { return n.source.clone() }
func (n Number) clone() Number  { n.source = n.source.clone(); return n }

type BrightnessKind uint8

const (
	BrightnessInvalid BrightnessKind = iota
	BrightnessUnchanged
	BrightnessAbsolutePercent
	BrightnessRelativePercent
)

type Brightness struct {
	kind    BrightnessKind
	percent Number
}

func (b Brightness) Kind() BrightnessKind { return b.kind }
func (b Brightness) Percent() Number      { return b.percent.clone() }
func (b Brightness) clone() Brightness    { b.percent = b.percent.clone(); return b }

type Target struct {
	alias  domain.HAAlias
	source Source
}

func (t Target) Alias() domain.HAAlias { return t.alias }
func (t Target) Source() Source        { return t.source.clone() }
func (t Target) clone() Target         { t.source = t.source.clone(); return t }

type Call struct {
	kind              CallKind
	targets           []Target
	temperature       Number
	brightness        Brightness
	transitionSeconds Number
}

func (c Call) Kind() CallKind { return c.kind }
func (c Call) Targets() []Target {
	out := make([]Target, len(c.targets))
	for i, v := range c.targets {
		out[i] = v.clone()
	}
	return out
}
func (c Call) Temperature() Number    { return c.temperature.clone() }
func (c Call) Brightness() Brightness { return c.brightness.clone() }
func (c Call) BrightnessPercent() Number {
	if c.brightness.kind == BrightnessAbsolutePercent {
		return c.brightness.percent.clone()
	}
	return Number{}
}
func (c Call) BrightnessStepPercent() Number {
	if c.brightness.kind == BrightnessRelativePercent {
		return c.brightness.percent.clone()
	}
	return Number{}
}
func (c Call) TransitionSeconds() Number { return c.transitionSeconds.clone() }
func (c Call) clone() Call {
	c.targets = c.Targets()
	c.temperature = c.temperature.clone()
	c.brightness = c.brightness.clone()
	c.transitionSeconds = c.transitionSeconds.clone()
	return c
}

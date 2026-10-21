package calendars

import "strings"

// TextField is closed so a caller cannot silently choose a new size limit.
type TextField uint8

const (
	TextFieldInvalid TextField = iota
	SummaryField
	DescriptionField
	LocationField
)

func (f TextField) limit() int {
	switch f {
	case SummaryField, LocationField:
		return 512
	case DescriptionField:
		return 4096
	default:
		return 0
	}
}

// TextValue distinguishes an absent property from a present empty property.
// Its zero is Absent; admission owns a copy of the supplied bytes.
type TextValue struct {
	present bool
	value   string
	field   TextField
}

func AbsentText() TextValue { return TextValue{} }
func PresentText(raw string, field TextField) (TextValue, error) {
	if field.limit() == 0 || !textValid(raw, field.limit(), false) {
		return TextValue{}, failure(InvalidInput)
	}
	return TextValue{present: true, value: strings.Clone(raw), field: field}, nil
}
func (v TextValue) Present() bool         { return v.present }
func (v TextValue) Value() (string, bool) { return v.value, v.present }
func (v TextValue) ValueOrEmpty() string  { return v.value }
func (v TextValue) validFor(field TextField) bool {
	if !v.present {
		return v.value == "" && v.field == TextFieldInvalid
	}
	return v.field == field && textValid(v.value, field.limit(), false)
}

// Unspecified is intentional absence; it is not an unknown wire token.
type EventStatus uint8

const (
	EventUnspecified EventStatus = iota
	EventTentative
	EventConfirmed
	EventCancelled
)

func (s EventStatus) valid() bool {
	switch s {
	case EventUnspecified, EventTentative, EventConfirmed, EventCancelled:
		return true
	}
	return false
}

type EventContent struct {
	summary, description, location TextValue
	status                         EventStatus
	admitted                       bool
}

func NewEventContent(summary, description, location TextValue) (EventContent, error) {
	return contentWithStatus(summary, description, location, EventUnspecified)
}
func contentWithStatus(summary, description, location TextValue, status EventStatus) (EventContent, error) {
	if !summary.validFor(SummaryField) || !description.validFor(DescriptionField) || !location.validFor(LocationField) || !status.valid() {
		return EventContent{}, failure(InvalidInput)
	}
	return EventContent{summary: summary, description: description, location: location, status: status, admitted: true}, nil
}
func (c EventContent) Valid() bool {
	return c.admitted && c.status.valid() && c.summary.validFor(SummaryField) && c.description.validFor(DescriptionField) && c.location.validFor(LocationField)
}
func (c EventContent) Summary() TextValue     { return c.summary }
func (c EventContent) Description() TextValue { return c.description }
func (c EventContent) Location() TextValue    { return c.location }
func (c EventContent) Status() EventStatus    { return c.status }
func (c EventContent) byteLength() int {
	return len(c.summary.value) + len(c.description.value) + len(c.location.value)
}

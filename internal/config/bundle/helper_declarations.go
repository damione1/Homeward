package bundle

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/domain"
)

type ButtonHelper struct {
	valid      bool
	id         domain.HAAlias
	name, icon Text
}

func (h ButtonHelper) Valid() bool        { _, err := h.id.Value(); return h.valid && err == nil }
func (h ButtonHelper) ID() domain.HAAlias { return h.id }
func (h ButtonHelper) Name() (Text, bool) { return h.name, h.name.kind != TextInvalid }
func (h ButtonHelper) Icon() (Text, bool) { return h.icon, h.icon.kind != TextInvalid }

type DatetimeHelper struct {
	valid      bool
	id         domain.HAAlias
	name, icon Text
	mode       domain.HelperKind
	initial    domain.HelperValue
	hasInitial bool
}

func (h DatetimeHelper) Valid() bool {
	if !h.valid || (h.mode != domain.HelperDate && h.mode != domain.HelperTime && h.mode != domain.HelperDateTime) {
		return false
	}
	if _, err := h.id.Value(); err != nil {
		return false
	}
	if h.hasInitial {
		kind, err := h.initial.Kind()
		return err == nil && kind == h.mode
	}
	return !h.initial.Valid()
}
func (h DatetimeHelper) ID() domain.HAAlias                  { return h.id }
func (h DatetimeHelper) Name() (Text, bool)                  { return h.name, h.name.kind != TextInvalid }
func (h DatetimeHelper) Icon() (Text, bool)                  { return h.icon, h.icon.kind != TextInvalid }
func (h DatetimeHelper) Mode() domain.HelperKind             { return h.mode }
func (h DatetimeHelper) Initial() (domain.HelperValue, bool) { return h.initial, h.hasInitial }

type SelectHelper struct {
	valid           bool
	id              domain.HAAlias
	name, icon      Text
	options         []string
	initial         string
	explicitInitial bool
}

func (h SelectHelper) copy() SelectHelper { h.options = append([]string(nil), h.options...); return h }
func (h SelectHelper) Valid() bool {
	if !h.valid || len(h.options) == 0 || len(h.options) > 256 {
		return false
	}
	if _, err := h.id.Value(); err != nil {
		return false
	}
	seen := make(map[string]bool, len(h.options))
	member := false
	for _, option := range h.options {
		if _, err := domain.NewHelperSelect(option); err != nil || strings.TrimSpace(option) != option || seen[option] {
			return false
		}
		seen[option] = true
		member = member || option == h.initial
	}
	return member
}
func (h SelectHelper) ID() domain.HAAlias      { return h.id }
func (h SelectHelper) Name() (Text, bool)      { return h.name, h.name.kind != TextInvalid }
func (h SelectHelper) Icon() (Text, bool)      { return h.icon, h.icon.kind != TextInvalid }
func (h SelectHelper) Options() []string       { return append([]string(nil), h.options...) }
func (h SelectHelper) Initial() (string, bool) { return h.initial, h.Valid() }
func (h SelectHelper) ExplicitInitial() bool   { return h.explicitInitial }

type TextMode uint8

const (
	TextModeInvalid TextMode = iota
	TextModeText
	TextModePassword
)

type TextHelper struct {
	valid            bool
	id               domain.HAAlias
	name, icon       Text
	minimum, maximum int
	mode             TextMode
	initial          string
	hasInitial       bool
}

func (h TextHelper) Valid() bool {
	if !h.valid || (h.mode != TextModeText && h.mode != TextModePassword) || h.minimum < 0 || h.maximum < h.minimum || h.maximum > 255 {
		return false
	}
	if _, err := h.id.Value(); err != nil {
		return false
	}
	if h.hasInitial {
		_, err := domain.NewHelperText(h.initial)
		return err == nil && utf8.RuneCountInString(h.initial) >= h.minimum && utf8.RuneCountInString(h.initial) <= h.maximum
	}
	return h.initial == ""
}
func (h TextHelper) ID() domain.HAAlias      { return h.id }
func (h TextHelper) Name() (Text, bool)      { return h.name, h.name.kind != TextInvalid }
func (h TextHelper) Icon() (Text, bool)      { return h.icon, h.icon.kind != TextInvalid }
func (h TextHelper) Bounds() (int, int)      { return h.minimum, h.maximum }
func (h TextHelper) Mode() TextMode          { return h.mode }
func (h TextHelper) Initial() (string, bool) { return h.initial, h.hasInitial }

func stringScalar(node parser.Node) (string, error) {
	s, ok := node.Scalar()
	if !ok || s.Kind() != parser.ScalarString || !utf8.ValidString(s.Text()) {
		return "", diagnostic(CodeInvalidValue, node, "expected literal string")
	}
	return s.Text(), nil
}

func admitButton(item parser.Entry) (ButtonHelper, error) {
	id, err := alias(parser.FamilyInputButton.Key(), item.Key, item.Value)
	if err != nil {
		return ButtonHelper{}, err
	}
	h := ButtonHelper{id: id}
	for _, field := range item.Value.Entries() {
		switch parser.DeclarativeFieldFromKey(field.Key) {
		case parser.FieldName:
			h.name, err = textValue(field.Value)
		case parser.FieldIcon:
			h.icon, err = textValue(field.Value)
		default:
			return ButtonHelper{}, diagnostic(CodeInvalidValue, field.Value, "unsupported button field")
		}
		if err != nil {
			return ButtonHelper{}, err
		}
	}
	h.valid = true
	return h, nil
}

func parseCivil(raw string, mode domain.HelperKind) (domain.HelperValue, error) {
	parts := func(value string, widths ...int) ([]int, error) {
		separators := map[int]bool{}
		offset := 0
		values := make([]int, len(widths))
		for i, width := range widths {
			if len(value) < offset+width {
				return nil, domain.ErrInvalidHelperValue
			}
			segment := value[offset : offset+width]
			for _, digit := range segment {
				if digit < '0' || digit > '9' {
					return nil, domain.ErrInvalidHelperValue
				}
			}
			n, err := strconv.Atoi(segment)
			if err != nil {
				return nil, err
			}
			values[i] = n
			offset += width
			if i < len(widths)-1 {
				separators[offset] = true
				offset++
			}
		}
		if offset != len(value) {
			return nil, domain.ErrInvalidHelperValue
		}
		for at := range separators {
			if value[at] != '-' && value[at] != ':' {
				return nil, domain.ErrInvalidHelperValue
			}
		}
		return values, nil
	}
	parseDate := func(text string) (domain.CivilDate, error) {
		if len(text) != 10 || text[4] != '-' || text[7] != '-' {
			return domain.CivilDate{}, domain.ErrInvalidHelperValue
		}
		v, err := parts(text, 4, 2, 2)
		if err != nil {
			return domain.CivilDate{}, err
		}
		return domain.NewCivilDate(v[0], v[1], v[2])
	}
	parseTime := func(text string) (domain.CivilTime, error) {
		if len(text) != 8 || text[2] != ':' || text[5] != ':' {
			return domain.CivilTime{}, domain.ErrInvalidHelperValue
		}
		v, err := parts(text, 2, 2, 2)
		if err != nil {
			return domain.CivilTime{}, err
		}
		return domain.NewCivilTime(v[0], v[1], v[2])
	}
	switch mode {
	case domain.HelperDate:
		v, err := parseDate(raw)
		if err != nil {
			return domain.HelperValue{}, err
		}
		return domain.NewHelperDate(v)
	case domain.HelperTime:
		v, err := parseTime(raw)
		if err != nil {
			return domain.HelperValue{}, err
		}
		return domain.NewHelperTime(v)
	case domain.HelperDateTime:
		if len(raw) != 19 || raw[10] != ' ' {
			return domain.HelperValue{}, domain.ErrInvalidHelperValue
		}
		d, err := parseDate(raw[:10])
		if err != nil {
			return domain.HelperValue{}, err
		}
		t, err := parseTime(raw[11:])
		if err != nil {
			return domain.HelperValue{}, err
		}
		v, err := domain.NewCivilDateTime(d, t)
		if err != nil {
			return domain.HelperValue{}, err
		}
		return domain.NewHelperDateTime(v)
	default:
		return domain.HelperValue{}, domain.ErrInvalidHelperValue
	}
}

func admitDatetime(item parser.Entry) (DatetimeHelper, error) {
	id, err := alias(parser.FamilyInputDatetime.Key(), item.Key, item.Value)
	if err != nil {
		return DatetimeHelper{}, err
	}
	h := DatetimeHelper{id: id}
	var hasDate, hasTime, seenDate, seenTime bool
	var raw string
	for _, field := range item.Value.Entries() {
		switch parser.DeclarativeFieldFromKey(field.Key) {
		case parser.FieldName:
			h.name, err = textValue(field.Value)
		case parser.FieldIcon:
			h.icon, err = textValue(field.Value)
		case parser.FieldHasDate:
			hasDate, err = boolValue(field.Value)
			seenDate = true
		case parser.FieldHasTime:
			hasTime, err = boolValue(field.Value)
			seenTime = true
		case parser.FieldInitial:
			raw, err = stringScalar(field.Value)
			h.hasInitial = err == nil
		default:
			return DatetimeHelper{}, diagnostic(CodeInvalidValue, field.Value, "unsupported datetime field")
		}
		if err != nil {
			return DatetimeHelper{}, err
		}
	}
	if !seenDate || !seenTime || (!hasDate && !hasTime) {
		return DatetimeHelper{}, diagnostic(CodeInvalidValue, item.Value, "datetime requires has_date or has_time")
	}
	switch {
	case hasDate && hasTime:
		h.mode = domain.HelperDateTime
	case hasDate:
		h.mode = domain.HelperDate
	default:
		h.mode = domain.HelperTime
	}
	if h.hasInitial {
		h.initial, err = parseCivil(raw, h.mode)
		if err != nil {
			return DatetimeHelper{}, diagnostic(CodeInvalidValue, item.Value, "initial does not match datetime mode")
		}
	}
	h.valid = true
	return h, nil
}

func admitSelect(item parser.Entry) (SelectHelper, error) {
	id, err := alias(parser.FamilyInputSelect.Key(), item.Key, item.Value)
	if err != nil {
		return SelectHelper{}, err
	}
	h := SelectHelper{id: id}
	var found bool
	for _, field := range item.Value.Entries() {
		switch parser.DeclarativeFieldFromKey(field.Key) {
		case parser.FieldName:
			h.name, err = textValue(field.Value)
		case parser.FieldIcon:
			h.icon, err = textValue(field.Value)
		case parser.FieldInitial:
			h.initial, err = stringScalar(field.Value)
			h.explicitInitial = err == nil
		case parser.FieldOptions:
			found = true
			if len(field.Value.Items()) == 0 || len(field.Value.Items()) > 256 {
				return SelectHelper{}, diagnostic(CodeInvalidValue, field.Value, "select requires 1..256 options")
			}
			seen := make(map[string]bool)
			for _, node := range field.Value.Items() {
				value, e := stringScalar(node)
				_, valueErr := domain.NewHelperSelect(value)
				if e != nil || valueErr != nil || strings.TrimSpace(value) != value || seen[value] {
					return SelectHelper{}, diagnostic(CodeInvalidValue, node, "invalid or duplicate select option")
				}
				seen[value] = true
				h.options = append(h.options, value)
			}
		default:
			return SelectHelper{}, diagnostic(CodeInvalidValue, field.Value, "unsupported select field")
		}
		if err != nil {
			return SelectHelper{}, err
		}
	}
	if !found {
		return SelectHelper{}, diagnostic(CodeMissingField, item.Value, "select options are required")
	}
	if !h.explicitInitial {
		h.initial = h.options[0]
	}
	member := false
	for _, option := range h.options {
		member = member || h.initial == option
	}
	if !member {
		return SelectHelper{}, diagnostic(CodeInvalidValue, item.Value, "select initial is outside options")
	}
	h.valid = true
	return h, nil
}

func admitText(item parser.Entry) (TextHelper, error) {
	id, err := alias(parser.FamilyInputText.Key(), item.Key, item.Value)
	if err != nil {
		return TextHelper{}, err
	}
	h := TextHelper{id: id, minimum: 0, maximum: 100, mode: TextModeText}
	for _, field := range item.Value.Entries() {
		switch parser.DeclarativeFieldFromKey(field.Key) {
		case parser.FieldName:
			h.name, err = textValue(field.Value)
		case parser.FieldIcon:
			h.icon, err = textValue(field.Value)
		case parser.FieldMin:
			var n int64
			n, err = intValue(field.Value)
			h.minimum = int(n)
		case parser.FieldMax:
			var n int64
			n, err = intValue(field.Value)
			h.maximum = int(n)
		case parser.FieldInitial:
			h.initial, err = stringScalar(field.Value)
			h.hasInitial = err == nil
		case parser.FieldMode:
			var mode string
			mode, err = stringScalar(field.Value)
			switch mode {
			case "text":
				h.mode = TextModeText
			case "password":
				h.mode = TextModePassword
			default:
				return TextHelper{}, diagnostic(CodeInvalidValue, field.Value, "unknown text mode")
			}
		default:
			return TextHelper{}, diagnostic(CodeInvalidValue, field.Value, "unsupported text field")
		}
		if err != nil {
			return TextHelper{}, err
		}
	}
	if h.minimum < 0 || h.maximum < h.minimum || h.maximum > 255 || (h.hasInitial && (utf8.RuneCountInString(h.initial) < h.minimum || utf8.RuneCountInString(h.initial) > h.maximum)) {
		return TextHelper{}, diagnostic(CodeInvalidValue, item.Value, "text bounds or initial are invalid")
	}
	h.valid = true
	if !h.Valid() {
		return TextHelper{}, diagnostic(CodeInvalidValue, item.Value, "text initial is not representable in storage")
	}
	return h, nil
}

package calendars

import (
	"fmt"
	"slices"
	"strings"
)

const RecurrenceProfile = "homeward.calendar-recurrence/1"

type Frequency uint8

const (
	FrequencyInvalid Frequency = iota
	Daily
	Weekly
	Monthly
	Yearly
)

func (f Frequency) name() string {
	switch f {
	case Daily:
		return "DAILY"
	case Weekly:
		return "WEEKLY"
	case Monthly:
		return "MONTHLY"
	case Yearly:
		return "YEARLY"
	default:
		return ""
	}
}

type ruleState uint8

const (
	ruleInvalid ruleState = iota
	ruleNone
	rulePresent
)

type ruleEnd uint8

const (
	ruleInfinite ruleEnd = iota
	ruleCount
	ruleUntil
)

// Rule is private admitted data, not rrule.ROption. Its own zero is invalid;
// NoRecurrence is an explicit valid absence. Its anchor is bound to its span.
type Rule struct {
	state        ruleState
	frequency    Frequency
	anchor       Point
	interval     int
	weekdays     uint8
	monthDays    []int
	end          ruleEnd
	count        int
	untilDate    CivilDate
	untilInstant Instant
	canonical    string
}

func NoRecurrence() Rule            { return Rule{state: ruleNone} }
func (r Rule) Present() bool        { return r.state == rulePresent }
func (r Rule) Text() (string, bool) { return r.canonical, r.state == rulePresent }
func (r Rule) Frequency() Frequency {
	if r.state != rulePresent {
		return FrequencyInvalid
	}
	return r.frequency
}
func (r Rule) validFor(span Span) bool {
	if !span.Valid() {
		return false
	}
	if r.state == ruleNone {
		return r.frequency == FrequencyInvalid && r.anchor == (Point{}) && r.interval == 0 && r.weekdays == 0 && len(r.monthDays) == 0 && r.end == ruleInfinite && r.count == 0 && r.untilDate == (CivilDate{}) && r.untilInstant == (Instant{}) && r.canonical == ""
	}
	if r.state != rulePresent || r.frequency.name() == "" || r.anchor != span.point() || r.interval < 1 || r.interval > 12 || len(r.canonical) == 0 || len(r.canonical) > 1024 {
		return false
	}
	switch r.end {
	case ruleInfinite:
		if r.count != 0 || r.untilDate.Valid() || r.untilInstant.Valid() {
			return false
		}
	case ruleCount:
		if r.count < 1 || r.count > 100000 || r.untilDate.Valid() || r.untilInstant.Valid() {
			return false
		}
	case ruleUntil:
		if r.count != 0 || span.kind == AllDay && (!r.untilDate.Valid() || r.untilInstant.Valid()) || span.kind == Timed && (!r.untilInstant.Valid() || r.untilDate.Valid()) {
			return false
		}
	default:
		return false
	}
	switch r.frequency {
	case Daily:
		return r.weekdays == 0 && len(r.monthDays) == 0
	case Weekly:
		return r.weekdays > 0 && r.weekdays < 128 && r.weekdays&(1<<uint(r.anchor.datePart().ordinal()%7)) != 0 && len(r.monthDays) == 0
	case Monthly:
		if r.weekdays != 0 || len(r.monthDays) < 1 || len(r.monthDays) > 31 {
			return false
		}
	case Yearly:
		return r.weekdays == 0 && len(r.monthDays) == 0
	default:
		return false
	}
	previous := 0
	containsAnchor := false
	for _, day := range r.monthDays {
		if day < 1 || day > 31 || day <= previous {
			return false
		}
		previous = day
		containsAnchor = containsAnchor || day == int(r.anchor.datePart().day)
	}
	return containsAnchor
}
func cloneRule(r Rule) Rule { r.monthDays = slices.Clone(r.monthDays); return r }

func ParseRule(raw string, span Span) (Rule, error) {
	if !span.Valid() || len(raw) == 0 || len(raw) > 1024 {
		return Rule{}, ruleFailure(RuleSyntax)
	}
	anchor := span.point()
	// Upstream otherwise substitutes time.Now for this exact Go zero.
	if anchor.wall().wallUTC().IsZero() {
		return Rule{}, ruleFailure(RuleZeroAnchor)
	}
	properties := make(map[string]string)
	for _, part := range strings.Split(raw, ";") {
		key, value, ok := strings.Cut(part, "=")
		if !ok || value == "" {
			return Rule{}, ruleFailure(RuleSyntax)
		}
		if _, duplicate := properties[key]; duplicate {
			return Rule{}, ruleFailure(RuleDuplicate)
		}
		properties[key] = value
	}
	if properties["COUNT"] != "" && properties["UNTIL"] != "" {
		return Rule{}, ruleFailure(RuleCountUntil)
	}
	r := Rule{state: rulePresent, anchor: anchor, interval: 1}
	for key, value := range properties {
		switch key {
		case "FREQ":
			switch value {
			case "DAILY":
				r.frequency = Daily
			case "WEEKLY":
				r.frequency = Weekly
			case "MONTHLY":
				r.frequency = Monthly
			case "YEARLY":
				r.frequency = Yearly
			default:
				return Rule{}, ruleFailure(RuleFrequency)
			}
		case "INTERVAL":
			n, err := digits(value)
			if err != nil || n < 1 || n > 12 {
				return Rule{}, ruleFailure(RuleInterval)
			}
			r.interval = n
		case "COUNT":
			n, err := digits(value)
			if err != nil || n < 1 || n > 100000 {
				return Rule{}, ruleFailure(RuleCount)
			}
			r.end = ruleCount
			r.count = n
		case "UNTIL":
			r.end = ruleUntil
			if span.kind == AllDay {
				if len(value) != 8 {
					return Rule{}, ruleFailure(RuleUntilType)
				}
				d, err := ParseDate(value[:4] + "-" + value[4:6] + "-" + value[6:])
				if err != nil {
					return Rule{}, ruleFailure(RuleUntilType)
				}
				r.untilDate = d
				if d.ordinal() < span.startDate.ordinal() {
					return Rule{}, ruleFailure(RuleAnchor)
				}
			} else {
				if len(value) != 16 || value[8] != 'T' || value[15] != 'Z' {
					return Rule{}, ruleFailure(RuleUntilType)
				}
				i, err := ParseInstant(value[:4] + "-" + value[4:6] + "-" + value[6:8] + "T" + value[9:11] + ":" + value[11:13] + ":" + value[13:15] + "Z")
				if err != nil {
					return Rule{}, ruleFailure(RuleUntilType)
				}
				r.untilInstant = i
				if i.value.Before(span.start.value) {
					return Rule{}, ruleFailure(RuleAnchor)
				}
			}
		case "BYDAY", "BYMONTHDAY", "BYMONTH", "WKST":
		default:
			return Rule{}, ruleFailure(RuleProperty)
		}
	}
	if r.frequency == FrequencyInvalid {
		return Rule{}, ruleFailure(RuleFrequency)
	}
	if value, present := properties["BYDAY"]; present {
		if r.frequency != Weekly {
			return Rule{}, ruleFailure(RuleWeekday)
		}
		for _, text := range strings.Split(value, ",") {
			index := -1
			switch text {
			case "MO":
				index = 0
			case "TU":
				index = 1
			case "WE":
				index = 2
			case "TH":
				index = 3
			case "FR":
				index = 4
			case "SA":
				index = 5
			case "SU":
				index = 6
			}
			if index < 0 || r.weekdays&(1<<uint(index)) != 0 {
				return Rule{}, ruleFailure(RuleWeekday)
			}
			r.weekdays |= 1 << uint(index)
		}
		if r.weekdays&(1<<uint(anchor.datePart().ordinal()%7)) == 0 {
			return Rule{}, ruleFailure(RuleAnchor)
		}
	} else if r.frequency == Weekly {
		r.weekdays = 1 << uint(anchor.datePart().ordinal()%7)
	}
	if value, present := properties["WKST"]; present && (r.frequency != Weekly || value != "MO") {
		return Rule{}, ruleFailure(RuleWeekStart)
	}
	if value, present := properties["BYMONTHDAY"]; present {
		if r.frequency != Monthly && r.frequency != Yearly {
			return Rule{}, ruleFailure(RuleMonthDay)
		}
		var mask uint32
		for _, text := range strings.Split(value, ",") {
			n, err := digits(text)
			if err != nil || n < 1 || n > 31 || mask&(1<<uint(n-1)) != 0 {
				return Rule{}, ruleFailure(RuleMonthDay)
			}
			mask |= 1 << uint(n-1)
			r.monthDays = append(r.monthDays, n)
		}
		if mask&(1<<uint(anchor.datePart().day-1)) == 0 || r.frequency == Yearly && len(r.monthDays) != 1 {
			return Rule{}, ruleFailure(RuleAnchor)
		}
	}
	if value, present := properties["BYMONTH"]; present {
		n, err := digits(value)
		if err != nil || r.frequency != Yearly || n != int(anchor.datePart().month) {
			return Rule{}, ruleFailure(RuleMonth)
		}
	}
	if r.frequency == Monthly && len(r.monthDays) == 0 {
		r.monthDays = []int{int(anchor.datePart().day)}
	}
	if r.frequency == Yearly {
		r.monthDays = nil
	}
	slices.Sort(r.monthDays)
	r.canonical = r.encode()
	if !r.validFor(span) {
		return Rule{}, ruleFailure(RuleSyntax)
	}
	return r, nil
}

func (r Rule) encode() string {
	parts := []string{"FREQ=" + r.frequency.name(), fmt.Sprintf("INTERVAL=%d", r.interval)}
	if r.frequency == Weekly {
		names := [7]string{"MO", "TU", "WE", "TH", "FR", "SA", "SU"}
		days := make([]string, 0, 7)
		for i, name := range names {
			if r.weekdays&(1<<uint(i)) != 0 {
				days = append(days, name)
			}
		}
		parts = append(parts, "BYDAY="+strings.Join(days, ","), "WKST=MO")
	}
	if r.frequency == Monthly {
		days := make([]string, len(r.monthDays))
		for i, day := range r.monthDays {
			days[i] = fmt.Sprint(day)
		}
		parts = append(parts, "BYMONTHDAY="+strings.Join(days, ","))
	}
	switch r.end {
	case ruleCount:
		parts = append(parts, fmt.Sprintf("COUNT=%d", r.count))
	case ruleUntil:
		if r.untilDate.Valid() {
			parts = append(parts, "UNTIL="+strings.ReplaceAll(r.untilDate.String(), "-", ""))
		} else {
			parts = append(parts, "UNTIL="+r.untilInstant.value.Format("20060102T150405Z"))
		}
	}
	return strings.Join(parts, ";")
}

// These weights come from the pinned generate/rebuild loops qualified in P0.
// They are structural work units, separate from the monotone time/candidate caps.
func (r Rule) workCharge() int64 {
	switch r.frequency {
	case Daily:
		return 72
	case Weekly:
		return 145
	case Monthly:
		return int64(8*(31*(len(r.monthDays)+4)+32) + 36)
	case Yearly:
		return 17860
	default:
		return 0
	}
}

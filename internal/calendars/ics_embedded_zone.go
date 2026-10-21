package calendars

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	rrule "github.com/teambition/rrule-go"
)

// EmbeddedObservanceInput is untrusted wire material. The admitted form below
// contains typed offsets and a deliberately narrow annual recurrence profile.
type EmbeddedObservanceInput struct {
	Anchor               LocalDateTime
	OffsetFrom, OffsetTo int
	Rule                 string
	RDates               []LocalDateTime
}
type EmbeddedZoneInput struct {
	Name        string
	Observances []EmbeddedObservanceInput
}
type observanceRuleState uint8

const (
	observanceAbsent observanceRuleState = iota
	observancePresent
)

type observanceEnd uint8

const (
	observanceInfinite observanceEnd = iota
	observanceCount
	observanceUntil
)

type observanceRule struct {
	state       observanceRuleState
	certificate annualCertificate
	end         observanceEnd
	count       int
	until       Instant
}
type embeddedObservance struct {
	anchor   LocalDateTime
	from, to int
	rule     observanceRule
	rdates   []LocalDateTime
}
type zoneTransition struct {
	at       time.Time
	from, to int
}
type embeddedZone struct {
	dataID      string
	sourceBytes int
	source      EmbeddedZoneInput // immutable original admitted definition, duplicates retained
	observances []embeddedObservance
	mu          sync.Mutex
	throughYear int
	transitions []zoneTransition
	initial     int
}

type budgetKey struct{}

func withCalendarBudget(ctx context.Context, b *calculationBudget) context.Context {
	return context.WithValue(ctx, budgetKey{}, b)
}
func zoneBudget(ctx context.Context) *calculationBudget {
	if b, ok := ctx.Value(budgetKey{}).(*calculationBudget); ok && b != nil {
		return b
	}
	return newBudget(ctx)
}
func validOffset(v int) bool { return v > -24*3600 && v < 24*3600 }
func parseObservanceRule(raw string, anchor LocalDateTime) (observanceRule, error) {
	if raw == "" {
		return observanceRule{}, nil
	}
	if !anchor.Valid() {
		return observanceRule{}, failure(InconsistentObservance)
	}
	if len(raw) > 1024 {
		return observanceRule{}, failure(UnsupportedObservanceRule)
	}
	p := make(map[string]string)
	for _, part := range strings.Split(raw, ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok || k == "" || v == "" {
			return observanceRule{}, failure(InconsistentObservance)
		}
		if _, dup := p[k]; dup {
			return observanceRule{}, failure(InconsistentObservance)
		}
		p[k] = v
	}
	if p["FREQ"] != "YEARLY" || p["INTERVAL"] != "" && p["INTERVAL"] != "1" {
		return observanceRule{}, failure(UnsupportedObservanceRule)
	}
	if p["COUNT"] != "" && p["UNTIL"] != "" {
		return observanceRule{}, failure(InconsistentObservance)
	}
	shape := annualShape{month: anchor.wallUTC().Month(), selector: annualMonthDay, day: int(anchor.date.day)}
	selectorSeen := false
	rule := observanceRule{state: observancePresent}
	for k, v := range p {
		switch k {
		case "FREQ", "INTERVAL":
		case "BYMONTH":
			n, e := strconv.Atoi(v)
			if e != nil || n < 1 || n > 12 {
				return observanceRule{}, failure(UnsupportedObservanceRule)
			}
			shape.month = time.Month(n)
		case "BYMONTHDAY":
			if selectorSeen {
				return observanceRule{}, failure(UnsupportedObservanceRule)
			}
			selectorSeen = true
			n, e := strconv.Atoi(v)
			if e != nil || n == 0 || n < -31 || n > 31 {
				return observanceRule{}, failure(UnsupportedObservanceRule)
			}
			shape.selector = annualMonthDay
			shape.day = n
		case "BYDAY":
			if selectorSeen || len(v) < 3 || len(v) > 4 {
				return observanceRule{}, failure(UnsupportedObservanceRule)
			}
			selectorSeen = true
			n, e := strconv.Atoi(v[:len(v)-2])
			if e != nil || n == 0 || n < -5 || n > 5 {
				return observanceRule{}, failure(UnsupportedObservanceRule)
			}
			weekday := time.Sunday
			switch v[len(v)-2:] {
			case "SU":
				weekday = time.Sunday
			case "MO":
				weekday = time.Monday
			case "TU":
				weekday = time.Tuesday
			case "WE":
				weekday = time.Wednesday
			case "TH":
				weekday = time.Thursday
			case "FR":
				weekday = time.Friday
			case "SA":
				weekday = time.Saturday
			default:
				return observanceRule{}, failure(UnsupportedObservanceRule)
			}
			shape.selector = annualOrdinalWeekday
			shape.day = 0
			shape.ordinal = n
			shape.weekday = weekday
		case "COUNT":
			n, e := strconv.Atoi(v)
			if e != nil || n < 1 || n > 10000 {
				return observanceRule{}, failure(UnsupportedObservanceRule)
			}
			rule.end = observanceCount
			rule.count = n
		case "UNTIL":
			value, e := parseICalUTC(v)
			if e != nil {
				return observanceRule{}, failure(InconsistentObservance)
			}
			rule.end = observanceUntil
			rule.until = value
		default:
			return observanceRule{}, failure(UnsupportedObservanceRule)
		}
	}
	if selectorSeen && p["BYMONTH"] == "" {
		return observanceRule{}, failure(UnsupportedObservanceRule)
	}
	cert, e := certifyAnnualShape(shape)
	if e != nil {
		return observanceRule{}, failure(InconsistentObservance)
	}
	if e = cert.checkAnchor(anchor.wallUTC()); e != nil {
		return observanceRule{}, failure(InconsistentObservance)
	}
	rule.certificate = cert
	return rule, nil
}
func sameObservance(a, b embeddedObservance) bool {
	return a.anchor == b.anchor && a.from == b.from && a.to == b.to && a.rule == b.rule && slices.Equal(a.rdates, b.rdates)
}
func observanceCeiling(o embeddedObservance, year int) time.Time {
	end := time.Date(year, 12, 31, 23, 59, 59, 0, time.UTC)
	if o.rule.end == observanceUntil {
		until := o.rule.until.value.Add(time.Duration(o.from) * time.Second)
		if until.Before(end) {
			end = until
		}
	}
	hard := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	if end.After(hard) {
		end = hard
	}
	return end
}

// Each iterator owns a monotone allocation credit, not an occurrence index.
// The annual certificate proves a candidate within400years; generate may
// rebuild the following year before returning it. Only an actual upstream
// result advances lastYear. No date is predicted or produced here.
const observanceIteratorFixedCredit uint64 = 12288 + 4096
const observanceOrdinalYearCredit uint64 = 4096 // mask3072 + ranges24 + margin1000
const zoneCompilationCopyCredit uint64 = 1 << 20

type observanceMaskCredit struct {
	selector    annualSelector
	throughYear int
}

func newObservanceMaskCredit(a *admissionSession, selector annualSelector, anchorYear int) (observanceMaskCredit, error) {
	if anchorYear < 1 || anchorYear > 9999 {
		return observanceMaskCredit{}, failure(InvalidDate)
	}
	switch selector {
	case annualMonthDay, annualOrdinalWeekday:
	case annualInvalid:
		return observanceMaskCredit{}, failure(InconsistentObservance)
	default:
		return observanceMaskCredit{}, failure(InconsistentObservance)
	}
	credit := observanceIteratorFixedCredit
	if selector == annualOrdinalWeekday {
		credit += observanceOrdinalYearCredit
	}
	if err := a.reserveMasks(credit); err != nil {
		return observanceMaskCredit{}, err
	}
	return observanceMaskCredit{selector: selector, throughYear: anchorYear}, nil
}
func (c *observanceMaskCredit) beforeNext(a *admissionSession, lastYear int) error {
	if a == nil || a.state != leaseOwned || c == nil || c.throughYear < 1 || c.throughYear > 9999 || lastYear < 1 || lastYear > 9999 {
		return failure(InvalidInput)
	}
	switch c.selector {
	case annualMonthDay:
		return nil
	case annualOrdinalWeekday:
		target := lastYear + 401
		if target > 9999 {
			target = 9999
		}
		if target <= c.throughYear {
			return nil
		}
		if err := a.reserveMasks(uint64(target-c.throughYear) * observanceOrdinalYearCredit); err != nil {
			return err
		}
		c.throughYear = target
		return nil
	case annualInvalid:
		return failure(InconsistentObservance)
	}
	return failure(InconsistentObservance)
}

// Called only after charging the first Next. Fixed credit and the first
// rebuild precede options/NewRRule/Iterator allocations in the original module.
func newObservanceIterator(ctx context.Context, o embeddedObservance, year int) (*pinnedNext, observanceMaskCredit, error) {
	shape := o.rule.certificate.shape
	credit, err := newObservanceMaskCredit(admissionFrom(ctx), shape.selector, int(o.anchor.date.year))
	if err != nil {
		return nil, observanceMaskCredit{}, err
	}
	options := rrule.ROption{Freq: rrule.YEARLY, Dtstart: o.anchor.wallUTC(), Interval: 1, Wkst: rrule.MO, Bymonth: []int{int(shape.month)}}
	switch shape.selector {
	case annualMonthDay:
		options.Bymonthday = []int{shape.day}
	case annualOrdinalWeekday:
		weekdays := [7]rrule.Weekday{rrule.SU, rrule.MO, rrule.TU, rrule.WE, rrule.TH, rrule.FR, rrule.SA}
		options.Byweekday = []rrule.Weekday{weekdays[int(shape.weekday)].Nth(shape.ordinal)}
	case annualInvalid:
		return nil, observanceMaskCredit{}, failure(InconsistentObservance)
	default:
		return nil, observanceMaskCredit{}, failure(InconsistentObservance)
	}
	if o.rule.end == observanceCount {
		options.Count = o.rule.count
	}
	next, err := newPinnedNext(ctx, options, observanceCeiling(o, year))
	return next, credit, err
}
func AdmitEmbeddedZone(ctx context.Context, input EmbeddedZoneInput) (Zone, error) {
	owned, release, scopeErr := admissionScope(ctx)
	if scopeErr != nil {
		return Zone{}, scopeErr
	}
	defer release()
	ctx = owned
	if err := contextValid(ctx); err != nil {
		return Zone{}, err
	}
	if input.Name == "" || len(input.Name) > 128 || input.Name == "Local" || strings.ContainsAny(input.Name, "\x00\r\n\\") || len(input.Observances) < 1 || len(input.Observances) > 16 {
		return Zone{}, failure(InvalidZone)
	}
	sourceCharge := int64(16384 + 3*len(input.Name))
	for _, raw := range input.Observances {
		if !raw.Anchor.Valid() || !validOffset(raw.OffsetFrom) || !validOffset(raw.OffsetTo) || len(raw.Rule) > 1024 || len(raw.RDates) > 256 {
			return Zone{}, failure(InvalidZone)
		}
		sourceCharge += 2048 + 3*int64(len(raw.Rule)) + int64(len(raw.RDates))*pointCharge
	}
	if owner := admissionFrom(ctx); owner != nil && owner.image != nil {
		if err := owner.image.grow(sourceCharge); err != nil {
			return Zone{}, err
		}
	}
	original := EmbeddedZoneInput{Name: strings.Clone(input.Name), Observances: make([]EmbeddedObservanceInput, 0, len(input.Observances))}
	var canon strings.Builder
	canon.WriteString(input.Name)
	observances := make([]embeddedObservance, 0, len(input.Observances))
	for _, raw := range input.Observances {
		if !raw.Anchor.Valid() || !validOffset(raw.OffsetFrom) || !validOffset(raw.OffsetTo) || len(raw.RDates) > 256 {
			return Zone{}, failure(InvalidZone)
		}
		rule, err := parseObservanceRule(raw.Rule, raw.Anchor)
		if err != nil {
			return Zone{}, err
		}
		rdates := slices.Clone(raw.RDates)
		for _, date := range rdates {
			if !date.Valid() {
				return Zone{}, failure(InvalidDate)
			}
		}
		slices.SortFunc(rdates, func(a, b LocalDateTime) int { return a.compare(b) })
		rdates = slices.Compact(rdates)
		original.Observances = append(original.Observances, EmbeddedObservanceInput{Anchor: raw.Anchor, OffsetFrom: raw.OffsetFrom, OffsetTo: raw.OffsetTo, Rule: strings.Clone(raw.Rule), RDates: rdates})
		candidate := embeddedObservance{anchor: raw.Anchor, from: raw.OffsetFrom, to: raw.OffsetTo, rule: rule, rdates: rdates}
		duplicate := false
		for _, existing := range observances {
			if sameObservance(existing, candidate) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			observances = append(observances, candidate)
		}
		if a := admissionFrom(ctx); a != nil {
			a.observation.RawObservances++
			if !duplicate {
				a.observation.UniqueObservances++
			}
		}
		fmt.Fprintf(&canon, "|%s|%d|%d|%s", raw.Anchor.String(), raw.OffsetFrom, raw.OffsetTo, raw.Rule)
		for _, d := range rdates {
			canon.WriteString("|")
			canon.WriteString(d.String())
		}
	}
	if canon.Len() > 64*1024 {
		return Zone{}, failure(CapacityExceeded)
	}
	hash := sha256.Sum256([]byte(canon.String()))
	z := &embeddedZone{dataID: "embedded-vtimezone/sha256:" + hex.EncodeToString(hash[:]), sourceBytes: canon.Len(), source: original, observances: observances}
	zone := Zone{name: strings.Clone(input.Name), embedded: z}
	// Establish the first offset and reject contradictory definitions before
	// admitting the zone, while keeping the transition table operation sized.
	firstYear := 9999
	for _, o := range observances {
		if y := int(o.anchor.date.year); y < firstYear {
			firstYear = y
		}
	}
	if err := z.ensure(ctx, firstYear); err != nil {
		return Zone{}, err
	}
	return zone, nil
}
func (z *embeddedZone) ensure(ctx context.Context, year int) error {
	owned, release, scopeErr := admissionScope(ctx)
	if scopeErr != nil {
		return scopeErr
	}
	defer release()
	ctx = owned
	if year < 1 || year > 9999 {
		return failure(InvalidDate)
	}
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.throughYear >= year {
		return nil
	}
	b := zoneBudget(ctx)
	if a := admissionFrom(ctx); a != nil {
		if err := b.charge(0, false); err != nil {
			return err
		}
		if err := a.reserveCopies(zoneCompilationCopyCredit); err != nil {
			return err
		}
		a.observation.ZoneCompilations++
	}
	transitions := make([]zoneTransition, 0, 64)
	add := func(o embeddedObservance, local LocalDateTime) error {
		if err := b.charge(8, true); err != nil {
			return err
		}
		at := local.wallUTC().Add(-time.Duration(o.from) * time.Second)
		transitions = append(transitions, zoneTransition{at: at, from: o.from, to: o.to})
		if a := admissionFrom(ctx); a != nil {
			a.observation.TransitionAppends++
		}
		if len(transitions) > 4096 {
			return failure(CapacityExceeded)
		}
		return nil
	}
	for _, o := range z.observances {
		if err := canceled(ctx); err != nil {
			return err
		}
		if int(o.anchor.date.year) > year {
			continue
		}
		if o.rule.state == observanceAbsent {
			if err := add(o, o.anchor); err != nil {
				return err
			}
		} else {
			var next *pinnedNext
			var credit observanceMaskCredit
			lastYear := int(o.anchor.date.year)
			first := true
			for {
				// Refusal precedes all new iterator/mask credit and Next.
				if err := b.charge(146400, false); err != nil {
					return err
				}
				if next == nil {
					var err error
					next, credit, err = newObservanceIterator(ctx, o, year)
					if err != nil {
						return err
					}
				}
				if err := credit.beforeNext(admissionFrom(ctx), lastYear); err != nil {
					return err
				}
				candidate, present, err := next.take(ctx)
				if err != nil {
					return err
				}
				if !present {
					break
				}
				lastYear = candidate.Year()
				local := localFromTime(candidate)
				if first && local != o.anchor {
					return failure(InconsistentObservance)
				}
				first = false
				at := candidate.Add(-time.Duration(o.from) * time.Second)
				if o.rule.end == observanceUntil && at.After(o.rule.until.value) {
					break
				}
				if err := add(o, local); err != nil {
					return err
				}
			}
		}
		for _, date := range o.rdates {
			if int(date.date.year) <= year {
				if err := add(o, date); err != nil {
					return err
				}
			}
		}
	}
	slices.SortFunc(transitions, func(a, b zoneTransition) int { return a.at.Compare(b.at) })
	compact := transitions[:0]
	for _, t := range transitions {
		if len(compact) > 0 && compact[len(compact)-1].at.Equal(t.at) {
			last := compact[len(compact)-1]
			if last.from != t.from || last.to != t.to {
				return failure(InvalidZone)
			}
			continue
		}
		if len(compact) > 0 && compact[len(compact)-1].to != t.from {
			return failure(InvalidZone)
		}
		compact = append(compact, t)
	}
	if len(compact) == 0 {
		return failure(ZoneCoverageUnknown)
	}
	if len(compact)*96 > 1024*1024 {
		return failure(CapacityExceeded)
	}
	z.transitions = slices.Clone(compact)
	z.initial = compact[0].from
	z.throughYear = year
	return nil
}
func (z *embeddedZone) offsetAt(ctx context.Context, at time.Time) (int, error) {
	year := at.UTC().Year() + 1
	if year > 9999 {
		year = 9999
	}
	if err := z.ensure(ctx, year); err != nil {
		return 0, err
	}
	z.mu.Lock()
	defer z.mu.Unlock()
	i, _ := slices.BinarySearchFunc(z.transitions, at, func(t zoneTransition, at time.Time) int { return t.at.Compare(at) })
	if i < len(z.transitions) && z.transitions[i].at.Equal(at) {
		return z.transitions[i].to, nil
	}
	if i == 0 {
		return z.initial, nil
	}
	return z.transitions[i-1].to, nil
}
func (z *embeddedZone) nextTransition(ctx context.Context, at time.Time) (zoneBoundary, error) {
	year := at.UTC().Year() + 2
	if year > 9999 {
		year = 9999
	}
	if err := z.ensure(ctx, year); err != nil {
		return zoneBoundary{}, err
	}
	z.mu.Lock()
	defer z.mu.Unlock()
	i, _ := slices.BinarySearchFunc(z.transitions, at, func(t zoneTransition, at time.Time) int { return t.at.Compare(at) })
	for i < len(z.transitions) && !z.transitions[i].at.After(at) {
		i++
	}
	if i < len(z.transitions) {
		return zoneBoundary{at: z.transitions[i].at, present: true}, nil
	}
	return zoneBoundary{}, nil
}

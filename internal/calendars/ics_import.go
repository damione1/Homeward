package calendars

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	ics "github.com/arran4/golang-ical"
)

// admitRemoteICS is private to the Service writer. The writer must verify the
// authoritative binding before invoking this admission and again at commit.
// Its image and working reservation stay owned through encoding and commit.
func admitRemoteICS(ctx context.Context, id CalendarID, raw []byte, projection *Zone) (*remoteImage, error) {
	image, _, err := admitRemoteICSObserved(ctx, id, raw, projection)
	return image, err
}
func admitRemoteICSObserved(ctx context.Context, id CalendarID, raw []byte, configuredProjection *Zone) (result *remoteImage, observation icsAdmissionObservation, resultErr error) {
	image, err := newRemoteImage(ctx, id)
	if err != nil {
		return nil, observation, err
	}
	success := false
	defer func() {
		observation = image.parse.observation
		if !success {
			image.close()
		}
	}()
	ctx = image.ctx
	normalized, stats, err := guard(ctx, raw)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			err = &Error{code: Canceled, cause: err}
		}
		return nil, observation, err
	}
	charge := estimateNormalized(normalized, stats)
	if err = image.parse.reserveTo(charge.Reserved); err != nil {
		return nil, observation, err
	}
	image.parse.observation.ParserCalls++
	parsed, err := ics.ParseCalendar(bytes.NewReader(normalized))
	if err != nil {
		return nil, observation, failure(InvalidInput)
	}
	configuredProjection, err = detachRemoteProjection(image, configuredProjection)
	if err != nil {
		return nil, observation, err
	}
	if err = admitParsedICS(ctx, image, parsed, configuredProjection); err != nil {
		return nil, observation, err
	}
	success = true
	return image, observation, nil
}

// Configuration definitions may outlive this operation; their lazy index may
// not. Reuse the same reserved result-copy owner, without another parser/scope.
func detachRemoteProjection(image *remoteImage, configured *Zone) (*Zone, error) {
	if configured == nil || configured.embedded == nil {
		return configured, nil
	}
	copy, err := newRemoteResultCopies(&calendarImage{remote: image, lease: image.lease}).zone(*configured)
	if err != nil {
		return nil, err
	}
	return &copy, nil
}
func scalar(properties []ics.IANAProperty, name string) (*ics.IANAProperty, error) {
	var found *ics.IANAProperty
	for i := range properties {
		if strings.EqualFold(properties[i].IANAToken, name) {
			if found != nil {
				return nil, failure(InvalidInput)
			}
			found = &properties[i]
		}
	}
	return found, nil
}
func requiredScalar(properties []ics.IANAProperty, name string) (*ics.IANAProperty, error) {
	p, err := scalar(properties, name)
	if err != nil {
		return nil, err
	}
	if p == nil || p.Value == "" {
		return nil, failure(InvalidInput)
	}
	return p, nil
}
func calendarScalar(cal *ics.Calendar, name string) (string, bool, error) {
	var value string
	found := false
	for _, p := range cal.CalendarProperties {
		if strings.EqualFold(p.IANAToken, name) {
			if found {
				return "", false, failure(InvalidInput)
			}
			value = p.Value
			found = true
		}
	}
	return value, found, nil
}
func parameter(p *ics.IANAProperty, name string) (string, bool, error) {
	var value string
	found := false
	for key, values := range p.ICalParameters {
		if strings.EqualFold(key, name) {
			if found || len(values) != 1 || values[0] == "" {
				return "", false, failure(InvalidInput)
			}
			value = values[0]
			found = true
		}
	}
	return value, found, nil
}
func parseICalDate(raw string) (CivilDate, error) {
	if len(raw) != 8 {
		return CivilDate{}, failure(InvalidDate)
	}
	return ParseDate(raw[:4] + "-" + raw[4:6] + "-" + raw[6:])
}
func parseICalLocal(raw string) (LocalDateTime, error) {
	if len(raw) != 15 || raw[8] != 'T' {
		return LocalDateTime{}, failure(InvalidDate)
	}
	return ParseLocalDateTime(raw[:4] + "-" + raw[4:6] + "-" + raw[6:8] + "T" + raw[9:11] + ":" + raw[11:13] + ":" + raw[13:15])
}
func parseICalUTC(raw string) (Instant, error) {
	if len(raw) != 16 || raw[8] != 'T' || raw[15] != 'Z' {
		return Instant{}, failure(InvalidDate)
	}
	return ParseInstant(raw[:4] + "-" + raw[4:6] + "-" + raw[6:8] + "T" + raw[9:11] + ":" + raw[11:13] + ":" + raw[13:15] + "Z")
}
func parseOffset(raw string) (int, error) {
	if (len(raw) != 5 && len(raw) != 7) || (raw[0] != '+' && raw[0] != '-') || raw == "-0000" || raw == "-000000" {
		return 0, failure(InvalidZone)
	}
	for _, c := range raw[1:] {
		if c < '0' || c > '9' {
			return 0, failure(InvalidZone)
		}
	}
	h, _ := strconv.Atoi(raw[1:3])
	m, _ := strconv.Atoi(raw[3:5])
	s := 0
	if len(raw) == 7 {
		s, _ = strconv.Atoi(raw[5:7])
	}
	if h >= 24 || m >= 60 || s >= 60 {
		return 0, failure(InvalidZone)
	}
	offset := h*3600 + m*60 + s
	if raw[0] == '-' {
		offset = -offset
	}
	if !validOffset(offset) {
		return 0, failure(InvalidZone)
	}
	return offset, nil
}
func parseObservance(c ics.Component) (EmbeddedObservanceInput, error) {
	var props []ics.IANAProperty
	switch v := c.(type) {
	case *ics.Standard:
		props = v.Properties
	case *ics.Daylight:
		props = v.Properties
	default:
		return EmbeddedObservanceInput{}, failure(InvalidZone)
	}
	start, err := requiredScalar(props, "DTSTART")
	if err != nil {
		return EmbeddedObservanceInput{}, err
	}
	anchor, err := parseICalLocal(start.Value)
	if err != nil {
		return EmbeddedObservanceInput{}, err
	}
	fromP, err := requiredScalar(props, "TZOFFSETFROM")
	if err != nil {
		return EmbeddedObservanceInput{}, err
	}
	toP, err := requiredScalar(props, "TZOFFSETTO")
	if err != nil {
		return EmbeddedObservanceInput{}, err
	}
	from, err := parseOffset(fromP.Value)
	if err != nil {
		return EmbeddedObservanceInput{}, err
	}
	to, err := parseOffset(toP.Value)
	if err != nil {
		return EmbeddedObservanceInput{}, err
	}
	ruleP, err := scalar(props, "RRULE")
	if err != nil {
		return EmbeddedObservanceInput{}, err
	}
	out := EmbeddedObservanceInput{Anchor: anchor, OffsetFrom: from, OffsetTo: to}
	if ruleP != nil {
		out.Rule = ruleP.Value
	}
	for _, p := range props {
		if !strings.EqualFold(p.IANAToken, "RDATE") {
			continue
		}
		for _, part := range strings.Split(p.Value, ",") {
			date, e := parseICalLocal(part)
			if e != nil {
				return EmbeddedObservanceInput{}, e
			}
			out.RDates = append(out.RDates, date)
			if len(out.RDates) > 256 {
				return EmbeddedObservanceInput{}, failure(CapacityExceeded)
			}
		}
	}
	return out, nil
}
func admitZones(ctx context.Context, cal *ics.Calendar) (map[string]Zone, error) {
	raw := cal.Timezones()
	if len(raw) > 8 {
		return nil, failure(CapacityExceeded)
	}
	zones := make(map[string]Zone, len(raw))
	sourceBytes := 0
	for _, v := range raw {
		p, err := requiredScalar(v.Properties, "TZID")
		if err != nil {
			return nil, err
		}
		if _, dup := zones[p.Value]; dup {
			return nil, failure(InvalidZone)
		}
		input := EmbeddedZoneInput{Name: p.Value}
		for _, sub := range v.SubComponents() {
			o, e := parseObservance(sub)
			if e != nil {
				return nil, e
			}
			input.Observances = append(input.Observances, o)
		}
		z, err := AdmitEmbeddedZone(ctx, input)
		if err != nil {
			return nil, err
		}
		sourceBytes += z.embedded.sourceBytes
		if sourceBytes > 512*1024 {
			return nil, failure(CapacityExceeded)
		}
		zones[p.Value] = z
	}
	return zones, nil
}

type wireForm uint8

const (
	wireInvalid wireForm = iota
	wireDate
	wireUTC
	wireTZID
	wireFloating
)

type wireTemporal struct {
	form    wireForm
	date    CivilDate
	local   LocalDateTime
	instant Instant
	zone    Zone
}

func readTemporal(ctx context.Context, p *ics.IANAProperty, zones map[string]Zone, projection Zone) (wireTemporal, error) {
	if p == nil {
		return wireTemporal{}, failure(InvalidDate)
	}
	value, hasValue, err := parameter(p, "VALUE")
	if err != nil {
		return wireTemporal{}, err
	}
	tzid, hasTZID, err := parameter(p, "TZID")
	if err != nil {
		return wireTemporal{}, err
	}
	if hasValue && value != "DATE" && value != "DATE-TIME" {
		return wireTemporal{}, failure(EndType)
	}
	if value == "DATE" || len(p.Value) == 8 && !hasTZID && !hasValue {
		if hasTZID {
			return wireTemporal{}, failure(EndType)
		}
		d, e := parseICalDate(p.Value)
		return wireTemporal{form: wireDate, date: d}, e
	}
	if strings.HasSuffix(p.Value, "Z") {
		if hasTZID {
			return wireTemporal{}, failure(EndType)
		}
		i, e := parseICalUTC(p.Value)
		if e != nil {
			return wireTemporal{}, e
		}
		z, e := ParseZone("UTC")
		return wireTemporal{form: wireUTC, instant: i, zone: z}, e
	}
	local, e := parseICalLocal(p.Value)
	if e != nil {
		return wireTemporal{}, e
	}
	if hasTZID {
		zone, ok := zones[tzid]
		if !ok {
			zone, e = ParseZone(tzid)
			if e != nil {
				return wireTemporal{}, e
			}
		}
		return wireTemporal{form: wireTZID, local: local, zone: zone}, nil
	}
	if !projection.Valid() {
		return wireTemporal{}, failure(MissingFloatingZone)
	}
	return wireTemporal{form: wireFloating, local: local, zone: projection}, nil
}
func (w wireTemporal) at(ctx context.Context) (Instant, error) {
	if w.form == wireUTC {
		return w.instant, nil
	}
	if w.form != wireTZID && w.form != wireFloating {
		return Instant{}, failure(EndType)
	}
	at, present, err := resolveWall(ctx, w.local, w.zone, true)
	if err != nil {
		return Instant{}, err
	}
	if !present {
		return Instant{}, failure(InvalidDate)
	}
	return at, nil
}
func boundedDurationDigits(raw string, limit int) (int, error) {
	if raw == "" {
		return 0, failure(InvalidDuration)
	}
	n := 0
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, failure(InvalidDuration)
		}
		digit := int(c - '0')
		if digit > limit || n > (limit-digit)/10 {
			return 0, failure(DurationLimit)
		}
		n = n*10 + digit
	}
	return n, nil
}
func duration(raw string) (NominalLength, error) {
	if strings.HasPrefix(raw, "+") {
		raw = raw[1:]
	}
	if len(raw) < 2 || raw[0] != 'P' {
		return NominalLength{}, failure(InvalidDuration)
	}
	text := raw[1:]
	if strings.HasSuffix(text, "W") {
		weeks, err := boundedDurationDigits(text[:len(text)-1], 366/7)
		if err != nil {
			return NominalLength{}, err
		}
		return NewNominalLength(weeks*7, 0)
	}
	before, after, hasTime := strings.Cut(text, "T")
	days, seconds := 0, 0
	if before != "" {
		if !strings.HasSuffix(before, "D") {
			return NominalLength{}, failure(InvalidDuration)
		}
		n, err := boundedDurationDigits(before[:len(before)-1], 366)
		if err != nil {
			return NominalLength{}, err
		}
		days = n
	}
	if before == "" && !hasTime {
		return NominalLength{}, failure(InvalidDuration)
	}
	if hasTime {
		if after == "" {
			return NominalLength{}, failure(InvalidDuration)
		}
		previous := 0
		for after != "" {
			i := strings.IndexAny(after, "HMS")
			if i < 1 {
				return NominalLength{}, failure(InvalidDuration)
			}
			order, unit := 0, 0
			switch after[i] {
			case 'H':
				order = 1
				unit = 3600
			case 'M':
				order = 2
				unit = 60
			case 'S':
				order = 3
				unit = 1
			}
			if order <= previous {
				return NominalLength{}, failure(InvalidDuration)
			}
			previous = order
			remaining := 366*86400 - days*86400 - seconds
			n, err := boundedDurationDigits(after[:i], remaining/unit)
			if err != nil {
				return NominalLength{}, err
			}
			seconds += n * unit
			after = after[i+1:]
		}
	}
	if days == 0 && seconds == 0 {
		return NominalLength{}, failure(InvalidDuration)
	}
	return NewNominalLength(days, seconds)
}
func sourceForWire(form wireForm) temporalSource {
	switch form {
	case wireUTC:
		return sourceUTC
	case wireTZID:
		return sourceTZID
	case wireFloating:
		return sourceFloating
	case wireInvalid, wireDate:
		return sourceInvalid
	}
	return sourceInvalid
}
func admitSpan(ctx context.Context, props []ics.IANAProperty, zones map[string]Zone, projection Zone) (Span, error) {
	startP, err := requiredScalar(props, "DTSTART")
	if err != nil {
		return Span{}, err
	}
	endP, err := scalar(props, "DTEND")
	if err != nil {
		return Span{}, err
	}
	durationP, err := scalar(props, "DURATION")
	if err != nil {
		return Span{}, err
	}
	if endP != nil && durationP != nil {
		return Span{}, failure(ConflictingEnd)
	}
	start, err := readTemporal(ctx, startP, zones, projection)
	if err != nil {
		return Span{}, err
	}
	if start.form == wireDate {
		end := CivilDate{}
		if endP != nil {
			value, e := readTemporal(ctx, endP, zones, projection)
			if e != nil {
				return Span{}, e
			}
			if value.form != wireDate {
				return Span{}, failure(EndType)
			}
			end = value.date
		} else if durationP != nil {
			n, e := duration(durationP.Value)
			if e != nil {
				return Span{}, e
			}
			if n.seconds != 0 {
				return Span{}, failure(EndType)
			}
			end, e = start.date.add(n.days)
			if e != nil {
				return Span{}, e
			}
		} else {
			end, err = start.date.add(1)
			if err != nil {
				return Span{}, err
			}
		}
		return AllDaySpan(start.date, end)
	}
	startAt, err := start.at(ctx)
	if err != nil {
		return Span{}, err
	}
	anchor := start.local
	if start.form == wireUTC {
		anchor = localFromTime(startAt.value)
	}
	if durationP != nil {
		length, e := duration(durationP.Value)
		if e != nil {
			return Span{}, e
		}
		span, e := TimedNominalSpan(ctx, anchor, length, start.zone)
		if e != nil {
			return Span{}, e
		}
		span.source = sourceForWire(start.form)
		return span, nil
	}
	if endP == nil {
		span, e := TimedPoint(ctx, anchor, start.zone)
		if e != nil {
			return Span{}, e
		}
		span.source = sourceForWire(start.form)
		return span, nil
	}
	end, err := readTemporal(ctx, endP, zones, projection)
	if err != nil {
		return Span{}, err
	}
	if end.form == wireDate {
		return Span{}, failure(EndType)
	}
	endAt, err := end.at(ctx)
	if err != nil {
		return Span{}, err
	}
	span, err := TimedSpan(ctx, anchor, endAt, start.zone)
	if err != nil {
		return Span{}, err
	}
	if end.zone.DataID() != start.zone.DataID() || end.zone.Name() != start.zone.Name() {
		span.endZone = end.zone
	}
	span.source = sourceForWire(start.form)
	return span, nil
}
func admitText(props []ics.IANAProperty) (EventContent, error) {
	names := []string{"SUMMARY", "DESCRIPTION", "LOCATION"}
	fields := []TextField{SummaryField, DescriptionField, LocationField}
	values := make([]TextValue, 3)
	for i, name := range names {
		p, err := scalar(props, name)
		if err != nil {
			return EventContent{}, err
		}
		if p == nil {
			values[i] = AbsentText()
			continue
		}
		values[i], err = PresentText(ics.FromText(p.Value), fields[i])
		if err != nil {
			return EventContent{}, err
		}
	}
	status := EventUnspecified
	p, err := scalar(props, "STATUS")
	if err != nil {
		return EventContent{}, err
	}
	if p != nil {
		switch strings.ToUpper(p.Value) {
		case "TENTATIVE":
			status = EventTentative
		case "CONFIRMED":
			status = EventConfirmed
		case "CANCELLED":
			status = EventCancelled
		default:
			return EventContent{}, failure(InvalidStatus)
		}
	}
	return contentWithStatus(values[0], values[1], values[2], status)
}
func admitPoint(ctx context.Context, p *ics.IANAProperty, base Span, known []Point, zones map[string]Zone, projection Zone) (Point, error) {
	if p == nil {
		return Point{}, failure(InvalidOccurrence)
	}
	if _, hasRange, err := parameter(p, "RANGE"); err != nil || hasRange {
		return Point{}, failure(InvalidOccurrence)
	}
	value, err := readTemporal(ctx, p, zones, projection)
	if err != nil {
		return Point{}, err
	}
	if base.kind == AllDay {
		if value.form != wireDate {
			return Point{}, failure(EndType)
		}
		return DatePoint(value.date)
	}
	if value.form == wireDate {
		return Point{}, failure(EndType)
	}
	instant, err := value.at(ctx)
	if err != nil {
		return Point{}, err
	}
	// Explicit origins can have a civil gap spelling different from localAt.
	// Match those existing origins by their already admitted effective instant.
	if instant.value.Equal(base.start.value) {
		return base.point(), nil
	}
	for _, point := range known {
		span, present, e := occurrenceSpan(ctx, base, point, true, zoneBudget(ctx))
		if e != nil {
			return Point{}, e
		}
		if present && span.start.value.Equal(instant.value) {
			return point, nil
		}
	}
	// Keep an unmatched same-context civil spelling exactly, including a gap.
	// Matching the canonical explicit origins above applies in every context.
	if (value.form == wireTZID || value.form == wireFloating) && value.zone.Name() == base.zone.Name() && value.zone.DataID() == base.zone.DataID() {
		return TimePoint(value.local)
	}
	local, err := base.zone.localAt(ctx, instant.value)
	if err != nil {
		return Point{}, err
	}
	candidate, err := TimePoint(local)
	if err != nil {
		return Point{}, err
	}
	resolved, present, err := resolveWall(ctx, local, base.zone, true)
	if err != nil {
		return Point{}, err
	}
	if present && resolved.value.Equal(instant.value) {
		return candidate, nil
	}
	return Point{kind: Timed, origin: originResolved, local: local, resolved: instant}, nil
}
func admitPoints(ctx context.Context, props []ics.IANAProperty, name string, base Span, known []Point, zones map[string]Zone, projection Zone) ([]Point, error) {
	var out []Point
	for i := range props {
		if !strings.EqualFold(props[i].IANAToken, name) {
			continue
		}
		for _, raw := range strings.Split(props[i].Value, ",") {
			p := props[i]
			p.Value = raw
			if strings.Contains(raw, "/") {
				return nil, failure(InvalidOccurrence)
			}
			point, err := admitPoint(ctx, &p, base, known, zones, projection)
			if err != nil {
				return nil, err
			}
			out = append(out, point)
			if len(out) > MaxDates {
				return nil, failure(CapacityExceeded)
			}
		}
	}
	return canonicalExplicitOrigins(ctx, out, base)
}

// Normalize the finite explicit union, never recurrence expansion. Sorting
// existing admitted instants groups aliases without inventing an ID/index or
// enumerating any new date. Anchor wins; otherwise the civil Point spelling
// precedes a resolved Point, then Point's canonical order decides. Gap spelling
// precedes its projected later wall time; first/second folds have different at.
func canonicalExplicitOrigins(ctx context.Context, input []Point, base Span) ([]Point, error) {
	if len(input) > MaxDates {
		return nil, failure(CapacityExceeded)
	}
	for _, point := range input {
		if !point.Valid() || point.kind != base.kind {
			return nil, failure(InvalidOccurrence)
		}
	}
	if base.kind == AllDay {
		return canonicalPoints(input, base.kind)
	}
	type explicitOrigin struct {
		point Point
		at    Instant
	}
	admitted := make([]explicitOrigin, 0, len(input))
	budget := zoneBudget(ctx)
	for _, point := range input {
		span, present, e := occurrenceSpan(ctx, base, point, true, budget)
		if e != nil {
			return nil, e
		}
		if !present {
			return nil, failure(InvalidOccurrence)
		}
		if span.start.value.Equal(base.start.value) {
			point = base.point()
		}
		admitted = append(admitted, explicitOrigin{point: point, at: span.start})
	}
	slices.SortFunc(admitted, func(a, b explicitOrigin) int {
		if order := a.at.value.Compare(b.at.value); order != 0 {
			return order
		}
		if a.point == base.point() && b.point != base.point() {
			return -1
		}
		if b.point == base.point() && a.point != base.point() {
			return 1
		}
		if a.point.origin != b.point.origin {
			if a.point.origin == originCivil {
				return -1
			}
			return 1
		}
		return pointLess(a.point, b.point)
	})
	out := make([]Point, 0, len(admitted))
	for i, origin := range admitted {
		if i == 0 || !origin.at.value.Equal(admitted[i-1].at.value) {
			out = append(out, origin.point)
		}
	}
	return canonicalPoints(out, base.kind)
}
func admitParsedICS(ctx context.Context, result *remoteImage, cal *ics.Calendar, configuredProjection *Zone) error {
	if cal == nil {
		return failure(InvalidInput)
	}
	ctx = withCalendarBudget(ctx, zoneBudget(ctx))
	zones, err := admitZones(ctx, cal)
	if err != nil {
		return err
	}
	projection := Zone{}
	if configuredProjection != nil {
		if !configuredProjection.Valid() {
			return failure(InvalidZone)
		}
		projection = *configuredProjection
	} else if raw, ok, e := calendarScalar(cal, "X-WR-TIMEZONE"); e != nil {
		return e
	} else if ok {
		if z, embedded := zones[raw]; embedded {
			projection = z
		} else {
			projection, e = ParseZone(raw)
			if e != nil {
				return e
			}
		}
	}
	type group struct {
		master    *ics.VEvent
		overrides []*ics.VEvent
	}
	groups := make(map[string]*group)
	order := make([]string, 0)
	for _, v := range cal.Events() {
		uidP, e := requiredScalar(v.Properties, "UID")
		if e != nil {
			return e
		}
		if _, e = ParseUID(uidP.Value); e != nil {
			return e
		}
		g := groups[uidP.Value]
		if g == nil {
			g = &group{}
			groups[uidP.Value] = g
			order = append(order, uidP.Value)
		}
		recurrence, e := scalar(v.Properties, "RECURRENCE-ID")
		if e != nil {
			return e
		}
		if recurrence == nil {
			if g.master != nil {
				return failure(ContradictoryIdentity)
			}
			g.master = v
		} else {
			g.overrides = append(g.overrides, v)
		}
	}
	if len(groups) > MaxEvents {
		return failure(CapacityExceeded)
	}
	slices.Sort(order)
	result.zones = zones
	result.events = make([]EventSnapshot, 0, len(order))
	for _, uidRaw := range order {
		g := groups[uidRaw]
		if g.master == nil {
			return failure(InvalidOccurrence)
		}
		if len(g.overrides) > MaxExceptions {
			return failure(CapacityExceeded)
		}
		if err := result.lease.grow(remoteWireEventCharge(g.master, g.overrides)); err != nil {
			return err
		}
		span, e := admitSpan(ctx, g.master.Properties, zones, projection)
		if e != nil {
			return e
		}
		content, e := admitText(g.master.Properties)
		if e != nil {
			return e
		}
		rule := NoRecurrence()
		if rp, e := scalar(g.master.Properties, "RRULE"); e != nil {
			return e
		} else if rp != nil {
			rule, e = ParseRule(rp.Value, span)
			if e != nil {
				return e
			}
		}
		additions, e := admitPoints(ctx, g.master.Properties, "RDATE", span, nil, zones, projection)
		if e != nil {
			return e
		}
		exclusions, e := admitPoints(ctx, g.master.Properties, "EXDATE", span, additions, zones, projection)
		if e != nil {
			return e
		}
		exceptions := make([]Exception, 0, len(g.overrides))
		seenOrigins := make(map[Point]bool, len(g.overrides))
		for _, override := range g.overrides {
			recurrence, e := requiredScalar(override.Properties, "RECURRENCE-ID")
			if e != nil {
				return e
			}
			point, e := admitPoint(ctx, recurrence, span, additions, zones, projection)
			if e != nil {
				return e
			}
			if seenOrigins[point] {
				return failure(ContradictoryIdentity)
			}
			seenOrigins[point] = true
			status, e := scalar(override.Properties, "STATUS")
			if e != nil {
				return e
			}
			if status != nil && strings.EqualFold(status.Value, "CANCELLED") {
				exception, e := Cancel(point)
				if e != nil {
					return e
				}
				exceptions = append(exceptions, exception)
				continue
			}
			replacement := Span{}
			if start, e := scalar(override.Properties, "DTSTART"); e != nil {
				return e
			} else if start != nil {
				replacement, e = admitSpan(ctx, override.Properties, zones, projection)
				if e != nil {
					return e
				}
			} else {
				var present bool
				replacement, present, e = occurrenceSpan(ctx, span, point, true, zoneBudget(ctx))
				if e != nil {
					return e
				}
				if !present {
					return failure(InvalidOccurrence)
				}
			}
			replacementContent, e := admitText(override.Properties)
			if e != nil {
				return e
			}
			exception, e := Replace(point, replacement, replacementContent)
			if e != nil {
				return e
			}
			exceptions = append(exceptions, exception)
		}
		event, e := admitEventContent(EventInput{Span: span, Rule: rule, Additions: additions, Exclusions: exclusions, Exceptions: exceptions}, contentImported, content)
		if e != nil {
			return e
		}
		if e = validateEvent(ctx, event, zoneBudget(ctx)); e != nil {
			return e
		}
		uid, _ := ParseUID(uidRaw)
		key := EventKey{calendar: result.binding, uid: uid}
		result.events = append(result.events, EventSnapshot{key: key, definition: event})
	}
	return nil
}

// Charges every wire point before conversion, including duplicate spellings.
// Unescaping text cannot expand its byte length. Scratch and owned copies are
// included so admission never grows a retained collection without credit.
func remoteWireEventCharge(master *ics.VEvent, overrides []*ics.VEvent) int64 {
	charge := eventBaseCharge + int64(len(overrides))*exceptionCharge
	add := func(properties []ics.IANAProperty) {
		for _, p := range properties {
			charge += 3*int64(len(p.Value)+len(p.IANAToken)) + 256
			if strings.EqualFold(p.IANAToken, "RDATE") || strings.EqualFold(p.IANAToken, "EXDATE") {
				charge += int64(strings.Count(p.Value, ",")+1) * pointCharge
			}
		}
	}
	add(master.Properties)
	for _, v := range overrides {
		add(v.Properties)
	}
	return charge
}

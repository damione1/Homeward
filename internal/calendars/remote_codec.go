package calendars

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"github.com/damione1/Homeward/internal/storage"
	"slices"
	"time"
)

// remote-v1 is independent of the legacy local SQL representation. Wire tags
// are mapped explicitly here; Go discriminants are never cast onto the wire.
const remoteMagic = "HWRCICS\x00"
const remoteHeaderBytes = 20
const remoteMaxZones = 72 // eight embedded definitions plus 64 admitted IANA zones

type remotePayload struct {
	data   []byte
	digest [32]byte
}

func remoteImageCounts(image *remoteImage) (storage.RemoteCalendarCounts, error) {
	if !image.owned() {
		return storage.RemoteCalendarCounts{}, failure(InvalidInput)
	}
	c := storage.RemoteCalendarCounts{Events: int64(len(image.events))}
	var zones [remoteMaxZones]Zone
	n := 0
	add := func(z Zone) error {
		if z == (Zone{}) {
			return nil
		}
		if !z.Valid() {
			return failure(Corrupt)
		}
		for _, old := range zones[:n] {
			if compareRemoteZone(old, z) == 0 {
				return nil
			}
		}
		if n == len(zones) {
			return failure(CapacityExceeded)
		}
		zones[n] = z
		n++
		if z.embedded != nil {
			c.SourceBytes += int64(z.embedded.sourceBytes)
			c.Observances += int64(len(z.embedded.source.Observances))
			for _, o := range z.embedded.source.Observances {
				c.RDates += int64(len(o.RDates))
			}
		}
		return nil
	}
	for _, z := range image.zones {
		if err := add(z); err != nil {
			return c, err
		}
	}
	for _, s := range image.events {
		e := s.definition
		c.Points += int64(len(e.additions) + len(e.exclusions))
		c.Exceptions += int64(len(e.exceptions))
		c.TextBytes += int64(len(s.key.uid.value) + len(e.rule.canonical) + e.content.byteLength())
		if err := add(e.span.zone); err != nil {
			return c, err
		}
		if err := add(e.span.endZone); err != nil {
			return c, err
		}
		for _, x := range e.exceptions {
			if x.kind == ReplaceOccurrence {
				c.TextBytes += int64(x.content.byteLength())
			}
			if x.replacement.Valid() {
				if err := add(x.replacement.zone); err != nil {
					return c, err
				}
				if err := add(x.replacement.endZone); err != nil {
					return c, err
				}
			}
		}
	}
	c.Zones = int64(n)
	if !c.Valid() {
		return c, failure(CapacityExceeded)
	}
	return c, nil
}

func (remotePayload) String() string               { return "calendars: remote payload [redacted]" }
func (p remotePayload) GoString() string           { return p.String() }
func (remotePayload) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

type remoteZoneKey struct {
	embedded   bool
	name, data string
}

func remoteZoneIdentity(z Zone) remoteZoneKey {
	return remoteZoneKey{z.embedded != nil, z.Name(), z.DataID()}
}
func compareRemoteZone(a, b Zone) int {
	x, y := remoteZoneIdentity(a), remoteZoneIdentity(b)
	if x.embedded != y.embedded {
		if !x.embedded {
			return -1
		}
		return 1
	}
	if x.name < y.name {
		return -1
	}
	if x.name > y.name {
		return 1
	}
	if x.data < y.data {
		return -1
	}
	if x.data > y.data {
		return 1
	}
	return 0
}

type remoteEncoder struct {
	data  []byte
	n     int
	err   error
	zones []Zone
}

func (w *remoteEncoder) put(raw []byte) {
	if w.err != nil {
		return
	}
	if len(raw) > int(MaxImageBytes)-w.n {
		w.err = failure(CapacityExceeded)
		return
	}
	if w.data != nil {
		if len(raw) > len(w.data)-w.n {
			w.err = failure(Corrupt)
			return
		}
		copy(w.data[w.n:], raw)
	}
	w.n += len(raw)
}
func (w *remoteEncoder) u8(v uint8) { w.put([]byte{v}) }
func (w *remoteEncoder) u16(v uint16) {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	w.put(b[:])
}
func (w *remoteEncoder) u32(v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	w.put(b[:])
}
func (w *remoteEncoder) text(v string) {
	if w.err != nil {
		return
	}
	if len(v) > 65535 || len(v) > int(MaxImageBytes)-w.n-2 {
		w.err = failure(CapacityExceeded)
		return
	}
	w.u16(uint16(len(v)))
	if w.data == nil {
		w.n += len(v)
	} else {
		w.put([]byte(v))
	}
}
func (w *remoteEncoder) frame(fn func()) {
	start := w.n
	w.u32(0)
	fn()
	if w.err == nil && w.data != nil {
		binary.BigEndian.PutUint32(w.data[start:start+4], uint32(w.n-start-4))
	}
}
func (w *remoteEncoder) date(v CivilDate) { w.u16(v.year); w.u8(v.month); w.u8(v.day) }
func (w *remoteEncoder) local(v LocalDateTime) {
	w.date(v.date)
	w.u8(v.hour)
	w.u8(v.minute)
	w.u8(v.second)
}
func (w *remoteEncoder) instant(v Instant) {
	w.local(localFromTime(v.value.UTC()))
	w.u32(uint32(v.value.Nanosecond()))
}
func (w *remoteEncoder) point(p Point) {
	switch p.kind {
	case AllDay:
		w.u8(1)
		w.date(p.date)
	case Timed:
		switch p.origin {
		case originCivil:
			w.u8(2)
			w.local(p.local)
		case originResolved:
			w.u8(3)
			w.local(p.local)
			w.instant(p.resolved)
		default:
			w.err = failure(Corrupt)
		}
	default:
		w.err = failure(Corrupt)
	}
}
func (w *remoteEncoder) content(c EventContent) {
	switch c.status {
	case EventUnspecified:
		w.u8(1)
	case EventTentative:
		w.u8(2)
	case EventConfirmed:
		w.u8(3)
	case EventCancelled:
		w.u8(4)
	default:
		w.err = failure(Corrupt)
	}
	for _, v := range []TextValue{c.summary, c.description, c.location} {
		w.textValue(v)
	}
}
func (w *remoteEncoder) textValue(v TextValue) {
	if !v.present {
		w.u8(0)
	} else {
		w.u8(1)
		w.text(v.value)
	}
}
func (w *remoteEncoder) zoneRef(z Zone) {
	if z == (Zone{}) {
		w.u16(0)
		return
	}
	index, ok := slices.BinarySearchFunc(w.zones, z, compareRemoteZone)
	if !ok {
		w.err = failure(Corrupt)
		return
	}
	w.u16(uint16(index + 1))
}
func (w *remoteEncoder) span(s Span) {
	switch s.mode {
	case spanAllDay:
		w.u8(1)
		w.date(s.startDate)
		w.date(s.endDate)
		return
	case spanPoint:
		w.u8(2)
	case spanExact:
		w.u8(3)
	case spanNominal:
		w.u8(4)
	default:
		w.err = failure(Corrupt)
		return
	}
	switch s.source {
	case sourceZoned:
		w.u8(1)
	case sourceUTC:
		w.u8(2)
	case sourceTZID:
		w.u8(3)
	case sourceFloating:
		w.u8(4)
	default:
		w.err = failure(Corrupt)
	}
	w.local(s.anchor)
	w.instant(s.start)
	w.zoneRef(s.zone)
	switch s.mode {
	case spanPoint:
	case spanExact:
		w.instant(s.end)
		w.zoneRef(s.endZone)
	case spanNominal:
		w.u16(uint16(s.nominal.days))
		w.u32(uint32(s.nominal.seconds))
		w.instant(s.end)
	}
}
func (w *remoteEncoder) zone(z Zone) {
	w.frame(func() {
		if z.embedded == nil {
			w.u8(1)
			w.text(z.name)
			w.text(z.DataID())
			return
		}
		w.u8(2)
		w.text(z.name)
		w.text(z.DataID())
		source := z.embedded.source
		if source.Name != z.name || len(source.Observances) < 1 || len(source.Observances) > 16 {
			w.err = failure(Corrupt)
			return
		}
		w.u8(uint8(len(source.Observances)))
		for _, o := range source.Observances {
			w.local(o.Anchor)
			w.u32(uint32(int32(o.OffsetFrom)))
			w.u32(uint32(int32(o.OffsetTo)))
			w.text(o.Rule)
			w.u16(uint16(len(o.RDates)))
			for _, d := range o.RDates {
				w.local(d)
			}
		}
	})
}
func (w *remoteEncoder) event(s EventSnapshot) {
	w.frame(func() {
		e := s.definition
		if !s.key.Valid() || !e.Valid() || e.contentKind != contentImported {
			w.err = failure(Corrupt)
			return
		}
		w.text(s.key.uid.value)
		w.content(e.content)
		w.span(e.span)
		switch e.rule.state {
		case ruleNone:
			w.u8(0)
		case rulePresent:
			w.u8(1)
			w.text(e.rule.canonical)
		default:
			w.err = failure(Corrupt)
		}
		for _, points := range [][]Point{e.additions, e.exclusions} {
			w.u16(uint16(len(points)))
			for _, p := range points {
				w.point(p)
			}
		}
		w.u16(uint16(len(e.exceptions)))
		for _, x := range e.exceptions {
			w.frame(func() {
				w.point(x.original)
				switch x.kind {
				case CancelOccurrence:
					w.u8(1)
				case MoveOccurrence:
					w.u8(2)
					w.span(x.replacement)
				case ReplaceOccurrence:
					w.u8(3)
					w.span(x.replacement)
					w.content(x.content)
				default:
					w.err = failure(Corrupt)
				}
			})
		}
	})
}

func encodeRemoteImage(ctx context.Context, image *remoteImage) (remotePayload, error) {
	if err := contextValid(ctx); err != nil {
		return remotePayload{}, err
	}
	if !image.owned() {
		return remotePayload{}, failure(InvalidInput)
	}
	if err := contextValid(image.ctx); err != nil {
		return remotePayload{}, err
	}
	if len(image.events) > MaxEvents || len(image.zones) > 8 {
		return remotePayload{}, failure(CapacityExceeded)
	}
	// The collection is bounded before its first backing allocation.
	if err := image.lease.grow(remoteMaxZones * 256); err != nil {
		return remotePayload{}, err
	}
	zones := make([]Zone, 0, remoteMaxZones)
	addZone := func(z Zone) error {
		if z == (Zone{}) {
			return nil
		}
		if !z.Valid() {
			return failure(Corrupt)
		}
		for _, old := range zones {
			if compareRemoteZone(old, z) == 0 {
				return nil
			}
		}
		if len(zones) == remoteMaxZones {
			return failure(CapacityExceeded)
		}
		zones = append(zones, z)
		return nil
	}
	for _, z := range image.zones {
		if err := addZone(z); err != nil {
			return remotePayload{}, err
		}
	}
	for _, s := range image.events {
		if s.key.calendar != image.binding {
			return remotePayload{}, failure(BindingMismatch)
		}
		if err := addZone(s.definition.span.zone); err != nil {
			return remotePayload{}, err
		}
		if err := addZone(s.definition.span.endZone); err != nil {
			return remotePayload{}, err
		}
		for _, x := range s.definition.exceptions {
			if err := addZone(x.replacement.zone); err != nil {
				return remotePayload{}, err
			}
			if err := addZone(x.replacement.endZone); err != nil {
				return remotePayload{}, err
			}
		}
	}
	slices.SortFunc(zones, compareRemoteZone)
	write := func(w *remoteEncoder) {
		w.put([]byte(remoteMagic))
		w.u16(1)
		w.u16(1)
		w.u16(uint16(len(zones)))
		w.u16(uint16(len(image.events)))
		w.u32(0)
		for _, z := range zones {
			w.zone(z)
		}
		for i, s := range image.events {
			if i > 0 && image.events[i-1].key.uid.value >= s.key.uid.value {
				w.err = failure(Corrupt)
				return
			}
			w.event(s)
		}
		if w.err == nil && w.data != nil {
			binary.BigEndian.PutUint32(w.data[16:20], uint32(w.n-remoteHeaderBytes))
		}
	}
	sizer := remoteEncoder{zones: zones}
	write(&sizer)
	if sizer.err != nil {
		return remotePayload{}, sizer.err
	}
	if sizer.n > int(MaxImageBytes) {
		return remotePayload{}, failure(CapacityExceeded)
	}
	if err := image.lease.grow(3 * int64(sizer.n)); err != nil {
		return remotePayload{}, err
	}
	if err := image.parse.reserveData(uint64(3 * sizer.n)); err != nil {
		return remotePayload{}, err
	}
	w := remoteEncoder{data: make([]byte, sizer.n), zones: zones}
	write(&w)
	if w.err != nil {
		return remotePayload{}, w.err
	}
	if err := zoneBudget(image.ctx).charge(int64(w.n), false); err != nil {
		return remotePayload{}, err
	}
	return remotePayload{data: w.data, digest: sha256.Sum256(w.data)}, nil
}

type remoteDecoder struct {
	data  []byte
	n     int
	err   error
	image *remoteImage
	zones []Zone
}

func (r *remoteDecoder) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}
func (r *remoteDecoder) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || n > len(r.data)-r.n {
		r.fail(failure(Corrupt))
		return nil
	}
	b := r.data[r.n : r.n+n]
	r.n += n
	return b
}
func (r *remoteDecoder) u8() uint8 {
	b := r.take(1)
	if b == nil {
		return 0
	}
	return b[0]
}
func (r *remoteDecoder) u16() uint16 {
	b := r.take(2)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint16(b)
}
func (r *remoteDecoder) u32() uint32 {
	b := r.take(4)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}
func (r *remoteDecoder) text(limit int) string {
	n := int(r.u16())
	if r.err != nil {
		return ""
	}
	if n > limit {
		r.fail(failure(CapacityExceeded))
		return ""
	}
	b := r.take(n)
	if r.err != nil {
		return ""
	}
	s := string(b)
	if !textValid(s, limit, false) {
		r.fail(failure(Corrupt))
	}
	return s
}
func (r *remoteDecoder) count(limit int, charge int64) int {
	n := int(r.u16())
	if r.err != nil {
		return 0
	}
	if n > limit {
		r.fail(failure(CapacityExceeded))
		return 0
	}
	if err := r.image.lease.grow(int64(n) * charge); err != nil {
		r.fail(err)
		return 0
	}
	return n
}

// The untrusted EmbeddedZoneInput is temporary. Admission clones its source
// into the image under the existing source charge; these input allocations
// keep separate cumulative work credit until the decode operation returns.
func (r *remoteDecoder) reserveStaging(bytes uint64) bool {
	if r.err != nil {
		return false
	}
	if !r.image.owned() {
		r.fail(failure(InvalidInput))
		return false
	}
	if err := contextValid(r.image.ctx); err != nil {
		r.fail(err)
		return false
	}
	if err := r.image.parse.reserveData(bytes); err != nil {
		r.fail(err)
		return false
	}
	r.image.parse.observation.DecodeStagingReservedBytes += bytes
	return true
}
func (r *remoteDecoder) stagingPointCount(limit int) int {
	n := int(r.u16())
	if r.err != nil {
		return 0
	}
	if n > limit {
		r.fail(failure(CapacityExceeded))
		return 0
	}
	// n comes from u16, so this multiplication cannot overflow uint64.
	if !r.reserveStaging(uint64(n) * uint64(pointCharge)) {
		return 0
	}
	return n
}
func (r *remoteDecoder) frame(fn func(*remoteDecoder)) {
	n := r.u32()
	if r.err != nil {
		return
	}
	if uint64(n) > uint64(len(r.data)-r.n) {
		r.fail(failure(Corrupt))
		return
	}
	b := r.take(int(n))
	sub := remoteDecoder{data: b, image: r.image, zones: r.zones}
	fn(&sub)
	if sub.err != nil {
		r.fail(sub.err)
	} else if sub.n != len(sub.data) {
		r.fail(failure(Corrupt))
	}
}
func (r *remoteDecoder) date() CivilDate {
	d := CivilDate{r.u16(), r.u8(), r.u8()}
	if r.err == nil && !d.Valid() {
		r.fail(failure(Corrupt))
	}
	return d
}
func (r *remoteDecoder) local() LocalDateTime {
	v := LocalDateTime{r.date(), r.u8(), r.u8(), r.u8()}
	if r.err == nil && !v.Valid() {
		r.fail(failure(Corrupt))
	}
	return v
}
func (r *remoteDecoder) instant() Instant {
	v := r.local()
	nano := r.u32()
	if r.err != nil {
		return Instant{}
	}
	if nano >= 1000000000 {
		r.fail(failure(Corrupt))
		return Instant{}
	}
	i, err := NewInstant(time.Date(int(v.date.year), time.Month(v.date.month), int(v.date.day), int(v.hour), int(v.minute), int(v.second), int(nano), time.UTC))
	if err != nil {
		r.fail(failure(Corrupt))
	}
	return i
}
func (r *remoteDecoder) point() Point {
	var p Point
	switch r.u8() {
	case 1:
		p = Point{kind: AllDay, date: r.date()}
	case 2:
		p = Point{kind: Timed, local: r.local()}
	case 3:
		p = Point{kind: Timed, origin: originResolved, local: r.local(), resolved: r.instant()}
	default:
		r.fail(failure(Incompatible))
	}
	if r.err == nil && !p.Valid() {
		r.fail(failure(Corrupt))
	}
	return p
}
func (r *remoteDecoder) content() EventContent {
	var status EventStatus
	switch r.u8() {
	case 1:
		status = EventUnspecified
	case 2:
		status = EventTentative
	case 3:
		status = EventConfirmed
	case 4:
		status = EventCancelled
	default:
		r.fail(failure(Incompatible))
	}
	values := [3]TextValue{}
	for i, field := range []TextField{SummaryField, DescriptionField, LocationField} {
		switch r.u8() {
		case 0:
			values[i] = AbsentText()
		case 1:
			raw := r.text(field.limit())
			if r.err == nil {
				v, err := PresentText(raw, field)
				if err != nil {
					r.fail(decodeFailure(err))
				}
				values[i] = v
			}
		default:
			r.fail(failure(Incompatible))
		}
	}
	if r.err != nil {
		return EventContent{}
	}
	c, err := contentWithStatus(values[0], values[1], values[2], status)
	if err != nil {
		r.fail(decodeFailure(err))
	}
	return c
}
func (r *remoteDecoder) zoneRef(required bool) Zone {
	index := int(r.u16())
	if r.err != nil {
		return Zone{}
	}
	if index == 0 && !required {
		return Zone{}
	}
	if index < 1 || index > len(r.zones) {
		r.fail(failure(Corrupt))
		return Zone{}
	}
	return r.zones[index-1]
}
func (r *remoteDecoder) span() Span {
	tag := r.u8()
	if tag == 1 {
		a, b := r.date(), r.date()
		if r.err != nil {
			return Span{}
		}
		s, err := AllDaySpan(a, b)
		if err != nil {
			r.fail(decodeFailure(err))
		}
		return s
	}
	var mode spanMode
	switch tag {
	case 2:
		mode = spanPoint
	case 3:
		mode = spanExact
	case 4:
		mode = spanNominal
	default:
		r.fail(failure(Incompatible))
		return Span{}
	}
	var source temporalSource
	switch r.u8() {
	case 1:
		source = sourceZoned
	case 2:
		source = sourceUTC
	case 3:
		source = sourceTZID
	case 4:
		source = sourceFloating
	default:
		r.fail(failure(Incompatible))
	}
	anchor, start, z := r.local(), r.instant(), r.zoneRef(true)
	end := start
	endZone := Zone{}
	nominal := NominalLength{}
	switch mode {
	case spanPoint:
	case spanExact:
		end = r.instant()
		endZone = r.zoneRef(false)
	case spanNominal:
		days, seconds := int(r.u16()), r.u32()
		end = r.instant()
		if r.err == nil {
			v, err := NewNominalLength(days, int(seconds))
			if err != nil {
				r.fail(decodeFailure(err))
			}
			nominal = v
		}
	}
	if r.err != nil {
		return Span{}
	}
	ctx := r.image.ctx
	var s Span
	var err error
	switch mode {
	case spanPoint:
		s, err = TimedPoint(ctx, anchor, z)
	case spanExact:
		s, err = TimedSpan(ctx, anchor, end, z)
	case spanNominal:
		s, err = TimedNominalSpan(ctx, anchor, nominal, z)
	}
	if err != nil {
		r.fail(decodeFailure(err))
		return Span{}
	}
	if s.start != start || s.end != end {
		r.fail(failure(Corrupt))
		return Span{}
	}
	if source == sourceUTC && (z.Name() != "UTC" || z.embedded != nil || anchor != localFromTime(start.value)) {
		r.fail(failure(Corrupt))
		return Span{}
	}
	if endZone.Valid() {
		if _, err = endZone.localAt(ctx, end.value); err != nil {
			r.fail(decodeFailure(err))
			return Span{}
		}
	}
	s.source = source
	s.endZone = endZone
	if !s.Valid() {
		r.fail(failure(Corrupt))
	}
	return s
}
func (r *remoteDecoder) zone() Zone {
	var result Zone
	r.frame(func(p *remoteDecoder) {
		tag := p.u8()
		name, data := p.text(128), p.text(128)
		if p.err != nil {
			return
		}
		switch tag {
		case 1:
			if data != TimezoneDataID {
				p.fail(failure(Incompatible))
				return
			}
			z, err := ParseZone(name)
			if err != nil {
				p.fail(decodeFailure(err))
				return
			}
			result = z
		case 2:
			n := int(p.u8())
			if n < 1 || n > 16 {
				p.fail(failure(CapacityExceeded))
				return
			}
			if !p.reserveStaging(uint64(n) * 2048) {
				return
			}
			input := EmbeddedZoneInput{Name: name, Observances: make([]EmbeddedObservanceInput, 0, n)}
			for i := 0; i < n && p.err == nil; i++ {
				o := EmbeddedObservanceInput{Anchor: p.local(), OffsetFrom: int(int32(p.u32())), OffsetTo: int(int32(p.u32())), Rule: p.text(1024)}
				count := p.stagingPointCount(256)
				if p.err != nil {
					return
				}
				o.RDates = make([]LocalDateTime, 0, count)
				for j := 0; j < count; j++ {
					v := p.local()
					if j > 0 && o.RDates[j-1].compare(v) >= 0 {
						p.fail(failure(Corrupt))
						return
					}
					o.RDates = append(o.RDates, v)
				}
				input.Observances = append(input.Observances, o)
			}
			if p.err != nil {
				return
			}
			z, err := AdmitEmbeddedZone(p.image.ctx, input)
			if err != nil {
				p.fail(decodeFailure(err))
				return
			}
			if z.DataID() != data {
				p.fail(failure(Corrupt))
				return
			}
			result = z
		default:
			p.fail(failure(Incompatible))
		}
	})
	return result
}
func (r *remoteDecoder) points(kind TemporalKind) []Point {
	n := r.count(MaxDates, pointCharge)
	if r.err != nil {
		return nil
	}
	out := make([]Point, 0, n)
	for i := 0; i < n && r.err == nil; i++ {
		p := r.point()
		if p.kind != kind || i > 0 && pointLess(out[i-1], p) >= 0 {
			r.fail(failure(Corrupt))
			return nil
		}
		out = append(out, p)
	}
	return out
}
func (r *remoteDecoder) event() EventSnapshot {
	var result EventSnapshot
	r.frame(func(p *remoteDecoder) {
		if err := p.image.lease.grow(eventBaseCharge); err != nil {
			p.fail(err)
			return
		}
		uid, err := ParseUID(p.text(255))
		if err != nil {
			p.fail(failure(Corrupt))
			return
		}
		content, span := p.content(), p.span()
		rule := NoRecurrence()
		switch p.u8() {
		case 0:
		case 1:
			raw := p.text(1024)
			if p.err == nil {
				rule, err = ParseRule(raw, span)
				if err != nil {
					p.fail(decodeFailure(err))
					return
				}
				if rule.canonical != raw {
					p.fail(failure(Corrupt))
					return
				}
			}
		default:
			p.fail(failure(Incompatible))
		}
		adds, excludes := p.points(span.kind), p.points(span.kind)
		n := p.count(MaxExceptions, exceptionCharge)
		if p.err != nil {
			return
		}
		exceptions := make([]Exception, 0, n)
		for i := 0; i < n && p.err == nil; i++ {
			var x Exception
			p.frame(func(q *remoteDecoder) {
				point := q.point()
				var e error
				switch q.u8() {
				case 1:
					x, e = Cancel(point)
				case 2:
					x, e = Move(point, q.span())
				case 3:
					replacement, c := q.span(), q.content()
					x, e = Replace(point, replacement, c)
				default:
					q.fail(failure(Incompatible))
				}
				if e != nil {
					q.fail(decodeFailure(e))
				}
			})
			if i > 0 && pointLess(exceptions[i-1].original, x.original) >= 0 {
				p.fail(failure(Corrupt))
				return
			}
			exceptions = append(exceptions, x)
		}
		if p.err != nil {
			return
		}
		event, err := admitEventContent(EventInput{Span: span, Rule: rule, Additions: adds, Exclusions: excludes, Exceptions: exceptions}, contentImported, content)
		if err != nil {
			p.fail(decodeFailure(err))
			return
		}
		if err = validateEvent(p.image.ctx, event, zoneBudget(p.image.ctx)); err != nil {
			p.fail(decodeFailure(err))
			return
		}
		result = EventSnapshot{key: EventKey{calendar: p.image.binding, uid: uid}, definition: event}
	})
	return result
}

type remoteDecodeObservation struct {
	Admission icsAdmissionObservation
	Image     imageCreditObservation
}

func decodeRemoteImage(ctx context.Context, id CalendarID, payload remotePayload) (*remoteImage, error) {
	image, _, err := decodeRemoteImageObserved(ctx, id, payload)
	return image, err
}

// Observe the operation before its failure cleanup clears the owned image
// credit. The returned evidence contains numbers only, never the source graph.
func decodeRemoteImageObserved(ctx context.Context, id CalendarID, payload remotePayload) (result *remoteImage, observation remoteDecodeObservation, resultErr error) {
	return decodeRemoteImageOwned(ctx, id, payload, nil)
}

// A SQL reader supplies its already acquired owner. The standalone codec path
// keeps its historical opening and observations; neither path nests admission.
func decodeRemoteImageOwned(ctx context.Context, id CalendarID, payload remotePayload, existing *remoteImage) (result *remoteImage, observation remoteDecodeObservation, resultErr error) {
	if err := contextValid(ctx); err != nil {
		return nil, observation, err
	}
	if len(payload.data) < remoteHeaderBytes {
		return nil, observation, failure(Corrupt)
	}
	if len(payload.data) > int(MaxImageBytes) {
		return nil, observation, failure(CapacityExceeded)
	}
	if !bytes.Equal(payload.data[:8], []byte(remoteMagic)) {
		return nil, observation, failure(Corrupt)
	}
	if binary.BigEndian.Uint16(payload.data[8:10]) != 1 || binary.BigEndian.Uint16(payload.data[10:12]) != 1 {
		return nil, observation, failure(Incompatible)
	}
	nz, ne := int(binary.BigEndian.Uint16(payload.data[12:14])), int(binary.BigEndian.Uint16(payload.data[14:16]))
	if nz > remoteMaxZones || ne > MaxEvents {
		return nil, observation, failure(CapacityExceeded)
	}
	if uint64(binary.BigEndian.Uint32(payload.data[16:20])) != uint64(len(payload.data)-remoteHeaderBytes) || sha256.Sum256(payload.data) != payload.digest {
		return nil, observation, failure(Corrupt)
	}
	image := existing
	if image != nil && (!image.owned() || image.binding != id || !image.sqlPreflight) {
		return nil, observation, failure(InvalidInput)
	}
	if image == nil {
		var initialObservation remoteDecodeObservation
		var err error
		image, initialObservation, err = newRemoteImageObserved(ctx, id)
		if err != nil {
			return nil, initialObservation, err
		}
	}
	var err error
	success := false
	defer func() {
		observation.Admission = image.parse.observation
		observation.Image = image.lease.creditObservation()
		if !success {
			image.close()
		}
	}()
	if err = image.lease.grow(3*int64(len(payload.data)) + int64(nz)*256 + int64(ne)*512); err != nil {
		return nil, observation, err
	}
	if !image.sqlPreflight {
		err = image.parse.reserveData(uint64(3 * len(payload.data)))
	}
	if err != nil {
		return nil, observation, err
	}
	if err = zoneBudget(image.ctx).charge(int64(len(payload.data)), false); err != nil {
		return nil, observation, err
	}
	r := remoteDecoder{data: payload.data, n: remoteHeaderBytes, image: image, zones: make([]Zone, 0, nz)}
	image.zones = make(map[string]Zone)
	embeddedCount, sourceBytes := 0, 0
	for i := 0; i < nz; i++ {
		z := r.zone()
		if r.err != nil {
			return nil, observation, r.err
		}
		if i > 0 && compareRemoteZone(r.zones[i-1], z) >= 0 {
			return nil, observation, failure(Corrupt)
		}
		if z.embedded != nil {
			embeddedCount++
			sourceBytes += z.embedded.sourceBytes
			if embeddedCount > 8 || sourceBytes > 512*1024 {
				return nil, observation, failure(CapacityExceeded)
			}
			if _, exists := image.zones[z.name]; exists {
				return nil, observation, failure(Corrupt)
			}
			image.zones[z.name] = z
		}
		r.zones = append(r.zones, z)
	}
	image.events = make([]EventSnapshot, 0, ne)
	for i := 0; i < ne; i++ {
		event := r.event()
		if r.err != nil {
			return nil, observation, r.err
		}
		if i > 0 && image.events[i-1].key.uid.value >= event.key.uid.value {
			return nil, observation, failure(Corrupt)
		}
		image.events = append(image.events, event)
	}
	if r.n != len(r.data) {
		return nil, observation, failure(Corrupt)
	}
	success = true
	return image, observation, nil
}

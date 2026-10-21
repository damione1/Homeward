package calendars

// Result copies detach the mutable lazy transition index from an operation.
// One fresh index per distinct embedded zone is shared by the values of one
// returned page/list. Its private admitted definition is immutable and can be
// shared without retaining the old index, image, lease or operation context.
// Future calculations acquire their own scope.
type remoteResultCopies struct {
	image *calendarImage
	zones map[*embeddedZone]*embeddedZone
}

func reserveResultBytes(image *calendarImage, n int64) error {
	if image.remote == nil {
		return nil
	}
	if n < 0 {
		return failure(CapacityExceeded)
	}
	if err := image.remote.lease.grow(n); err != nil {
		return err
	}
	return image.remote.parse.reserveData(uint64(n))
}
func newRemoteResultCopies(image *calendarImage) *remoteResultCopies {
	return &remoteResultCopies{image: image}
}
func (c *remoteResultCopies) zone(z Zone) (Zone, error) {
	if z.embedded == nil || c.image.remote == nil {
		return z, nil
	}
	if old := c.zones[z.embedded]; old != nil {
		z.embedded = old
		return z, nil
	}
	source := z.embedded
	// AdmitEmbeddedZone already owns clones of the public input. Its source,
	// canonical observances and nested RDates are never mutated or exposed;
	// ensure only reads them and produces a private transition index. Sharing
	// those admitted values allocates no new definition or producer storage.
	// Keep the existing conservative fixed object/map charge, before allocation.
	charge := int64(16384)
	if c.zones == nil {
		charge += remoteMaxZones * 256
	}
	if err := reserveResultBytes(c.image, charge); err != nil {
		return Zone{}, err
	}
	if c.zones == nil {
		c.zones = make(map[*embeddedZone]*embeddedZone)
	}
	// Copy immutable fields explicitly: copying the entire embeddedZone would
	// retain a mutex and the old mutable transitions/throughYear/initial state.
	detached := &embeddedZone{dataID: source.dataID, sourceBytes: source.sourceBytes, source: source.source, observances: source.observances}
	c.zones[source] = detached
	z.embedded = detached
	return z, nil
}
func (c *remoteResultCopies) span(s Span) (Span, error) {
	var err error
	s.zone, err = c.zone(s.zone)
	if err != nil {
		return Span{}, err
	}
	s.endZone, err = c.zone(s.endZone)
	return s, err
}
func (c *remoteResultCopies) event(e Event) (Event, error) {
	if c.image.remote == nil {
		return cloneEvent(e), nil
	}
	if !e.Valid() {
		return e, nil
	}
	if err := reserveResultBytes(c.image, e.ramCharge()); err != nil {
		return Event{}, err
	}
	copy := cloneEvent(e)
	var err error
	copy.span, err = c.span(copy.span)
	if err != nil {
		return Event{}, err
	}
	for i := range copy.exceptions {
		if copy.exceptions[i].replacement.Valid() {
			copy.exceptions[i].replacement, err = c.span(copy.exceptions[i].replacement)
			if err != nil {
				return Event{}, err
			}
		}
	}
	return copy, nil
}
func copyResultEvent(image *calendarImage, e Event) (Event, error) {
	return newRemoteResultCopies(image).event(e)
}

// Grow only when an admitted occurrence needs another slot. Charge every new
// backing array in full, not just its capacity delta: earlier arrays may remain
// live until GC. The same cumulative parse and image owners bound all copies.
func growRemoteOccurrenceHeap(image *calendarImage, found occurrenceHeap, maximum int) (occurrenceHeap, error) {
	if image == nil || image.remote == nil || !image.remote.owned() || maximum < 1 || maximum > MaxPageSize+1 || cap(found) > maximum {
		return nil, failure(InvalidInput)
	}
	if len(found) < cap(found) {
		return found, nil
	}
	if cap(found) == maximum {
		return nil, failure(CapacityExceeded)
	}
	capacity := max(1, 2*cap(found))
	if capacity > maximum {
		capacity = maximum
	}
	if err := reserveResultBytes(image, int64(capacity)*automationOccurrenceCharge); err != nil {
		return nil, err
	}
	grown := make(occurrenceHeap, len(found), capacity)
	copy(grown, found)
	return grown, nil
}

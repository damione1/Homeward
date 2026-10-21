package calendars

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func nha101ResultZone(t *testing.T) (*remoteImage, Zone, EmbeddedZoneInput) {
	t.Helper()
	f := nha100Setup(t)
	id := f.bind(t, "calendar.famille")
	image, err := newRemoteImage(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(image.close)
	input := EmbeddedZoneInput{Name: "Synthetic/Private", Observances: []EmbeddedObservanceInput{{Anchor: nha100Local(t, "2020-01-01T00:00:00"), RDates: []LocalDateTime{nha100Local(t, "2021-01-01T00:00:00")}}}}
	zone, err := AdmitEmbeddedZone(image.ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	return image, zone, input
}

func TestNHA101ResultZonePrivateDefinitionAndIndependentIndexes(t *testing.T) {
	image, original, input := nha101ResultZone(t)
	want := EmbeddedZoneInput{Name: "Synthetic/Private", Observances: []EmbeddedObservanceInput{{Anchor: nha100Local(t, "2020-01-01T00:00:00"), RDates: []LocalDateTime{nha100Local(t, "2021-01-01T00:00:00")}}}}
	// Public input remains caller owned. Changing both slice levels after
	// admission must not reach either returned definition or its producers.
	input.Name = "Modified/Public"
	input.Observances[0].Anchor = nha100Local(t, "2030-01-01T00:00:00")
	input.Observances[0].OffsetTo = 3600
	input.Observances[0].RDates[0] = nha100Local(t, "2031-01-01T00:00:00")
	first, err := newRemoteResultCopies(&calendarImage{remote: image, lease: image.lease}).zone(original)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newRemoteResultCopies(&calendarImage{remote: image, lease: image.lease}).zone(original)
	if err != nil {
		t.Fatal(err)
	}
	for _, z := range []Zone{first, second} {
		if z.embedded == original.embedded || z.DataID() != original.DataID() || !reflect.DeepEqual(z.embedded.source, want) {
			t.Fatal("result borrowed its operation index or caller input")
		}
		if z.embedded.throughYear != 0 || len(z.embedded.transitions) != 0 || z.embedded.initial != 0 {
			t.Fatal("result inherited an operation transition table")
		}
	}
	if first.embedded == second.embedded {
		t.Fatal("independent results shared a mutable index")
	}
	originalYear := original.embedded.throughYear
	image.close()
	if image.parse.state != leaseClosed || image.lease.creditObservation().ReservedBytes != 0 {
		t.Fatal("returned definitions retained the operation leases")
	}
	// Rebuild indexes after the creating operation is really closed. The
	// public-input edits above must not affect offsets, RDates or provenance.
	for i, entry := range []struct {
		zone Zone
		year int
	}{{first, 2040}, {second, 2030}} {
		if err := entry.zone.embedded.ensure(context.Background(), entry.year); err != nil {
			t.Fatal(err)
		}
		if len(entry.zone.embedded.transitions) != 2 || entry.zone.embedded.throughYear != entry.year || !reflect.DeepEqual(entry.zone.embedded.source, want) {
			t.Fatal("closed operation lost its private immutable definition")
		}
		if offset, err := entry.zone.offsetAt(context.Background(), time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil || offset != 0 {
			t.Fatal("public input edits changed an admitted offset", i, offset, err)
		}
	}
	if original.embedded.throughYear != originalYear || first.embedded.throughYear != 2040 || second.embedded.throughYear != 2030 {
		t.Fatal("compiling one returned index mutated another")
	}
	if err := original.embedded.ensure(context.Background(), 2050); err != nil {
		t.Fatal(err)
	}
	if first.embedded.throughYear != 2040 || second.embedded.throughYear != 2030 || !reflect.DeepEqual(original.embedded.source, want) {
		t.Fatal("original index extension mutated returned definitions or indexes")
	}
}

func TestNHA101ResultZoneRefusalPreservesSourceAndCredit(t *testing.T) {
	image, original, _ := nha101ResultZone(t)
	beforeYear, beforeCount := original.embedded.throughYear, len(original.embedded.transitions)
	beforeSource := original.DataID()
	reserved := image.lease.creditObservation().ReservedBytes
	if err := image.lease.grow(MaxImageBytes - int64(reserved)); err != nil {
		t.Fatal(err)
	}
	beforeParse := image.parse.observation.ReservedBytes
	copies := newRemoteResultCopies(&calendarImage{remote: image, lease: image.lease})
	_, err := copies.zone(original)
	nha100Code(t, err, CapacityExceeded)
	credit := image.lease.creditObservation()
	if copies.zones != nil || credit.ReservedBytes != uint64(MaxImageBytes) || credit.RequestedBytes <= uint64(MaxImageBytes) || image.parse.observation.ReservedBytes != beforeParse {
		t.Fatal("refused result allocated a map or acquired unowned credit")
	}
	if original.embedded.throughYear != beforeYear || len(original.embedded.transitions) != beforeCount || original.DataID() != beforeSource {
		t.Fatal("capacity refusal mutated the source")
	}
}

func TestNHA101RemoteOccurrenceHeapCumulativeCreditAndRefusal(t *testing.T) {
	image, _, _ := nha101ResultZone(t)
	view := &calendarImage{remote: image, lease: image.lease}
	beforeImage := image.lease.creditObservation().ReservedBytes
	beforeParse := image.parse.observation.ReservedBytes
	found, err := growRemoteOccurrenceHeap(view, nil, MaxPageSize+1)
	if err != nil {
		t.Fatal(err)
	}
	// A sparse query pays for its one real slot, rather than the public
	// maximum page size. Reusing an already reserved slot pays nothing twice.
	if cap(found) != 1 || image.lease.creditObservation().ReservedBytes-beforeImage != automationOccurrenceCharge || image.parse.observation.ReservedBytes-beforeParse != automationOccurrenceCharge {
		t.Fatal("sparse page prepaid its maximum instead of its actual allocation")
	}
	if _, err := growRemoteOccurrenceHeap(view, found, MaxPageSize+1); err != nil {
		t.Fatal(err)
	}
	if image.parse.observation.ReservedBytes-beforeParse != automationOccurrenceCharge {
		t.Fatal("existing heap capacity charged twice")
	}
	found = append(found, Occurrence{})
	grown, err := growRemoteOccurrenceHeap(view, found, MaxPageSize+1)
	if err != nil {
		t.Fatal(err)
	}
	// Both the first1-slot and replacement2-slot arrays are charged, since
	// retaining only the capacity delta would underpay transient live memory.
	if len(grown) != 1 || cap(grown) != 2 || image.lease.creditObservation().ReservedBytes-beforeImage != 3*automationOccurrenceCharge || image.parse.observation.ReservedBytes-beforeParse != 3*automationOccurrenceCharge {
		t.Fatal("heap growth did not pay every new backing array in full")
	}
	grown = append(grown, Occurrence{})
	reserved := image.lease.creditObservation().ReservedBytes
	if err := image.lease.grow(MaxImageBytes - int64(reserved)); err != nil {
		t.Fatal(err)
	}
	parseAtLimit := image.parse.observation.ReservedBytes
	refused, err := growRemoteOccurrenceHeap(view, grown, MaxPageSize+1)
	nha100Code(t, err, CapacityExceeded)
	if refused != nil || len(grown) != 2 || cap(grown) != 2 || image.parse.observation.ReservedBytes != parseAtLimit || image.lease.creditObservation().ReservedBytes != uint64(MaxImageBytes) {
		t.Fatal("heap refusal published storage or acquired unowned capacity")
	}
}

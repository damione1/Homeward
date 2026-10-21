package calendars

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"
)

func nha101StagingImage(t *testing.T, ctx context.Context, id CalendarID) *remoteImage {
	t.Helper()
	image, err := newRemoteImage(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(image.close)
	return image
}

func TestNHA101CodecStagingCreditsBeforeInputCopies(t *testing.T) {
	f := nha100Setup(t)
	id := f.bind(t, "calendar.famille")
	t.Run("CumulativeWorkingCredit", func(t *testing.T) {
		image := nha101StagingImage(t, context.Background(), id)
		before := image.lease.creditObservation()
		reserved := image.parse.reserved
		r := remoteDecoder{data: []byte{0, 1, 0, 1}, image: image}
		for i := 0; i < 2; i++ {
			if n := r.stagingPointCount(MaxDates); r.err != nil || n != 1 {
				t.Fatal("valid input count refused", r.err)
			}
		}
		if image.lease.creditObservation() != before {
			t.Fatal("temporary input was charged as retained image data")
		}
		credit := uint64(2) * uint64(pointCharge)
		if image.parse.reserved != reserved+credit || image.parse.observation.DecodeStagingReservedBytes != credit {
			t.Fatal("temporary input did not retain its cumulative working credit")
		}
	})
	t.Run("CountBoundBeforeData", func(t *testing.T) {
		image := nha101StagingImage(t, context.Background(), id)
		beforeImage, beforeParse := image.lease.creditObservation(), image.parse.observation
		r := remoteDecoder{data: []byte{1, 1}, image: image} // 257, with no point body
		if n := r.stagingPointCount(MaxDates); n != 0 {
			t.Fatal("oversize input count admitted")
		}
		nha100Code(t, r.err, CapacityExceeded)
		if r.n != 2 || image.lease.creditObservation() != beforeImage || image.parse.observation != beforeParse {
			t.Fatal("oversize count traversed or acquired its point body")
		}
	})
	t.Run("WorkingRefusalBeforeData", func(t *testing.T) {
		image := nha101StagingImage(t, context.Background(), id)
		if err := image.parse.reserveTo(parseReservationLimit); err != nil {
			t.Fatal(err)
		}
		before := image.lease.creditObservation()
		r := remoteDecoder{data: []byte{0, 2}, image: image}
		if n := r.stagingPointCount(MaxDates); n != 0 {
			t.Fatal("unowned input count admitted")
		}
		nha100Code(t, r.err, CapacityExceeded)
		if image.lease.creditObservation() != before || image.parse.reserved != parseReservationLimit || image.parse.observation.DecodeStagingReservedBytes != 0 || image.parse.observation.RequestedBytes <= parseReservationLimit {
			t.Fatal("working refusal changed retained credit or hid the denied request")
		}
	})
	t.Run("CanceledBeforeData", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		image := nha101StagingImage(t, ctx, id)
		beforeImage, beforeParse := image.lease.creditObservation(), image.parse.observation
		cancel()
		r := remoteDecoder{data: []byte{0, 1}, image: image}
		if n := r.stagingPointCount(MaxDates); n != 0 {
			t.Fatal("canceled input count admitted")
		}
		nha100Code(t, r.err, Canceled)
		if image.lease.creditObservation() != beforeImage || image.parse.observation != beforeParse {
			t.Fatal("canceled input acquired credit")
		}
	})
	images.mu.Lock()
	active, used := images.active, images.used
	images.mu.Unlock()
	if active != 0 || used != 0 {
		t.Fatal("staging refusal/cancel leaked an image lease")
	}
	available := nha101StagingImage(t, context.Background(), id)
	available.close()
}

func TestNHA101CodecZoneInputBoundsBeforeAdmission(t *testing.T) {
	f := nha100Setup(t)
	id := f.bind(t, "calendar.famille")
	for _, entry := range []struct {
		name        string
		observances uint8
		dates       uint16
		full        bool
	}{
		{"ObservancePlusOne", 17, 0, false},
		{"RDatePlusOne", 1, MaxDates + 1, false},
		{"WorkingCreditExhausted", 1, 0, true},
	} {
		t.Run(entry.name, func(t *testing.T) {
			image := nha101StagingImage(t, context.Background(), id)
			if entry.full {
				if err := image.parse.reserveTo(parseReservationLimit); err != nil {
					t.Fatal(err)
				}
			}
			before := image.lease.creditObservation()
			w := remoteEncoder{data: make([]byte, 256)}
			w.frame(func() {
				w.u8(2)
				w.text("Codec/InputBound")
				w.text("unreachable-data-id")
				w.u8(entry.observances)
				if entry.observances == 1 && !entry.full {
					w.local(nha100Local(t, "2026-01-01T00:00:00"))
					w.u32(0)
					w.u32(0)
					w.text("")
					w.u16(entry.dates)
				}
			})
			if w.err != nil {
				t.Fatal(w.err)
			}
			r := remoteDecoder{data: w.data[:w.n], image: image}
			_ = r.zone()
			nha100Code(t, r.err, CapacityExceeded)
			if image.lease.creditObservation() != before || image.parse.observation.RawObservances != 0 || image.parse.observation.ZoneCompilations != 0 {
				t.Fatal("invalid or unowned zone input reached retained-source admission")
			}
			if entry.observances > 16 || entry.full {
				if image.parse.observation.DecodeStagingReservedBytes != 0 {
					t.Fatal("denied observance array was credited")
				}
			}
		})
	}
}

func TestNHA101CodecDecodeObservationBeforeCleanup(t *testing.T) {
	f := nha100Setup(t)
	id := f.bind(t, "calendar.famille")
	payload := nha101EncodeClose(t, nha101Admit(t, id, nha101Raw(t, "07-overrides.ics")))
	bad := bytes.Clone(payload.data)
	eventStart := remoteHeaderBytes + 4
	uidLen := int(bad[eventStart])<<8 | int(bad[eventStart+1])
	bad[eventStart+2+uidLen] = 0xff
	decoded, observation, err := decodeRemoteImageObserved(context.Background(), id, remotePayload{data: bad, digest: sha256.Sum256(bad)})
	if decoded != nil {
		decoded.close()
		t.Fatal("unknown status admitted")
	}
	nha100Code(t, err, Incompatible)
	if observation.Admission.ReservedBytes < parseFixedCredit || observation.Admission.ParserCalls != 0 || observation.Image.ReservedBytes <= uint64(imageFixedCharge) || observation.Image.RequestedBytes < observation.Image.ReservedBytes {
		t.Fatalf("decode refusal lost its own pre-cleanup observation: %+v", observation)
	}
	images.mu.Lock()
	active, used := images.active, images.used
	images.mu.Unlock()
	if active != 0 || used != 0 {
		t.Fatal("observed decode refusal leaked an image")
	}
	first, err := images.reserve(imageFixedCharge)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.release)
	second, err := images.reserve(imageFixedCharge)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.release)
	decoded, observation, err = decodeRemoteImageObserved(context.Background(), id, payload)
	nha100Code(t, err, Busy)
	if decoded != nil || observation.Admission.ReservedBytes != parseFixedCredit || observation.Image.ReservedBytes != 0 || observation.Image.RequestedBytes != uint64(imageFixedCharge) {
		t.Fatal("initial image refusal lost its actually acquired parse scope or denied image request")
	}
	first.release()
	second.release()
	decoded, observation, err = decodeRemoteImageObserved(context.Background(), id, payload)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(decoded.close)
	if observation.Admission != decoded.parse.observation || observation.Image != decoded.lease.creditObservation() {
		t.Fatal("successful decode observation differs from the live owner")
	}
	decoded.close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	decoded, observation, err = decodeRemoteImageObserved(ctx, id, payload)
	nha100Code(t, err, Canceled)
	if decoded != nil || observation != (remoteDecodeObservation{}) {
		t.Fatal("a decode canceled before admission reported stale credit")
	}
}

func TestNHA101CodecImageObservationKeepsRefusedRequest(t *testing.T) {
	lease, err := images.reserve(imageFixedCharge)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.release)
	if err = lease.grow(MaxImageBytes - imageFixedCharge); err != nil {
		t.Fatal(err)
	}
	err = lease.grow(1)
	nha100Code(t, err, CapacityExceeded)
	observation := lease.creditObservation()
	if observation.ReservedBytes != uint64(MaxImageBytes) || observation.RequestedBytes != uint64(MaxImageBytes)+1 {
		t.Fatal("refused image extension lost the actual owned/requested distinction")
	}
	lease.release()
	if observation.ReservedBytes != uint64(MaxImageBytes) {
		t.Fatal("cleanup mutated captured evidence")
	}
}

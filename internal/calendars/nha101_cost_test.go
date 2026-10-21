//go:build !race

package calendars

import (
	"context"
	"runtime"
	"runtime/debug"
	"testing"
)

func nha101Measure(fn func()) uint64 {
	runtime.GC()
	previous := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(previous)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func TestNHA101CostAdmissionAndCodecComposition(t *testing.T) {
	f := nha100Setup(t)
	id := f.bind(t, "calendar.famille")
	for _, name := range []string{"20-eight-zones-conversion.ics", "21-override-content-conversion.ics", "22-sparse-observance-allocation.ics"} {
		t.Run(name, func(t *testing.T) {
			raw := costRaw(t, name)
			var image *remoteImage
			var err error
			var payload remotePayload
			var observation icsAdmissionObservation
			delta := nha101Measure(func() {
				image, observation, err = admitRemoteICSObserved(context.Background(), id, raw, nil)
				if err == nil {
					payload, err = encodeRemoteImage(context.Background(), image)
					observation = image.parse.observation
				}
			})
			t.Logf("admission+encode bytes=%d TotalAlloc=%d reserved=%d requested=%d error=%v observation=%+v", len(raw), delta, observation.ReservedBytes, observation.RequestedBytes, err, observation)
			if image != nil {
				image.close()
			}
			if err != nil {
				t.Fatal(err)
			}
			if delta > observation.ReservedBytes || observation.ReservedBytes > parseReservationLimit {
				t.Fatal("new codec composition exceeded owned cumulative parse credit")
			}
			var decoded *remoteImage
			var decodeObservation remoteDecodeObservation
			delta = nha101Measure(func() { decoded, decodeObservation, err = decodeRemoteImageObserved(context.Background(), id, payload) })
			observation = decodeObservation.Admission
			if decoded != nil {
				decoded.close()
			}
			t.Logf("decode TotalAlloc=%d reserved=%d error=%v observation=%+v", delta, observation.ReservedBytes, err, decodeObservation)
			if err != nil {
				t.Fatal(err)
			}
			if delta > observation.ReservedBytes || observation.ReservedBytes > parseReservationLimit {
				t.Fatal("remote reopen codec exceeded owned cumulative parse credit")
			}
		})
	}
}

func TestNHA101CostFarRefusalDoesNotPublishPartialIndex(t *testing.T) {
	f := nha100Setup(t)
	id := f.bind(t, "calendar.famille")
	image := nha101Admit(t, id, nha101Raw(t, "10-vtimezone-dst.ics"))
	payload, err := encodeRemoteImage(context.Background(), image)
	if err != nil {
		image.close()
		t.Fatal(err)
	}
	zone := image.zones["Custom/NY"].embedded
	oldYear := zone.throughYear
	oldTransitions := append([]zoneTransition(nil), zone.transitions...)
	delta := nha101Measure(func() { err = zone.ensure(image.ctx, 9999) })
	observation := image.parse.observation
	t.Logf("far9999 TotalAlloc=%d reserved=%d error=%v observation=%+v oldYear=%d afterYear=%d", delta, observation.ReservedBytes, err, observation, oldYear, zone.throughYear)
	if zone.throughYear != oldYear || len(zone.transitions) != len(oldTransitions) {
		image.close()
		t.Fatal("refused far compilation published coverage")
	}
	for i, tr := range oldTransitions {
		if tr != zone.transitions[i] {
			image.close()
			t.Fatal("refused compilation mutated the old index")
		}
	}
	image.close()
	nha100Code(t, err, BudgetExceeded)
	if delta > observation.ReservedBytes || observation.ReservedBytes > parseReservationLimit {
		t.Fatal("far calculation exceeded owned reservation")
	}
	// Reopen is a distinct codec operation. This unit proof does not claim SQL
	// cache availability or reset the exhausted budget inside the old operation.
	reopened, err := decodeRemoteImage(context.Background(), id, payload)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	if values := queryTime(t, reopened, "2026"); len(values) != 5 {
		t.Fatal("previously admitted2026 representation lost after far refusal")
	}
}

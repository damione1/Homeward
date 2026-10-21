package calendars

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNHA101DomainGuardCancellationAndAvailability(t *testing.T) {
	f := nha100Setup(t)
	id := f.bind(t, "calendar.famille")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := admitRemoteICSObserved(ctx, id, nha101Raw(t, "02-utc.ics"), nil)
	nha100Code(t, err, Canceled)
	var typedNil *nha101CancelAtGuardContext
	_, err = admitRemoteICS(typedNil, id, nha101Raw(t, "02-utc.ics"), nil)
	nha100Code(t, err, InvalidInput)
	ctxAtGuard := &nha101CancelAtGuardContext{Context: context.Background(), done: make(chan struct{})}
	image, observation, err := admitRemoteICSObserved(ctxAtGuard, id, nha101Raw(t, "02-utc.ics"), nil)
	if image != nil || observation.ParserCalls != 0 || !errors.Is(err, context.Canceled) {
		t.Fatal("guard cancellation entered parser or lost cancellation cause")
	}
	nha100Code(t, err, Canceled)
	image = nha101Admit(t, id, nha101Raw(t, "02-utc.ics"))
	_, err = admitRemoteICS(context.Background(), id, nha101Raw(t, "02-utc.ics"), nil)
	nha100Code(t, err, Busy)
	image.close()
	_, err = encodeRemoteImage(context.Background(), image)
	nha100Code(t, err, InvalidInput)
	image = nha101Admit(t, id, nha101Raw(t, "02-utc.ics"))
	image.close()
	images.mu.Lock()
	active, used := images.active, images.used
	images.mu.Unlock()
	if active != 0 || used != 0 {
		t.Fatalf("remote leases leaked: images=%d bytes=%d", active, used)
	}
}

type nha101CancelAtGuardContext struct {
	context.Context
	calls int
	done  chan struct{}
}

func (c *nha101CancelAtGuardContext) Done() <-chan struct{} { return c.done }
func (c *nha101CancelAtGuardContext) Err() error {
	c.calls++
	if c.calls >= 2 {
		if c.calls == 2 {
			close(c.done)
		}
		return context.Canceled
	}
	return nil
}

func TestNHA101DomainSynchronousNextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	value := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	iterator := &pinnedNext{ceiling: value, next: func() (time.Time, bool) { cancel(); return value, true }}
	_, present, err := iterator.take(ctx)
	nha100Code(t, err, Canceled)
	if present || !errors.Is(err, context.Canceled) {
		t.Fatal("Next emitted after synchronous cancellation")
	}
}

func TestNHA101DomainGapOverridesCollideInBothOrders(t *testing.T) {
	f := nha100Setup(t)
	id := f.bind(t, "calendar.famille")
	const master = "BEGIN:VEVENT\r\nUID:gap-two-overrides\r\nDTSTART;TZID=America/New_York:20260307T023000\r\nDURATION:PT1H\r\nRDATE;TZID=America/New_York:20260308T023000\r\nRDATE:20260308T073000Z\r\nEND:VEVENT\r\n"
	const first = "BEGIN:VEVENT\r\nUID:gap-two-overrides\r\nRECURRENCE-ID;TZID=America/New_York:20260308T023000\r\nDTSTART:20260310T140000Z\r\nDURATION:PT1H\r\nEND:VEVENT\r\n"
	second := strings.Replace(strings.Replace(first, "20260308T023000", "20260308T033000", 1), "20260310T140000Z", "20260311T140000Z", 1)
	for _, body := range []string{master + first + second, second + master + first} {
		_, err := admitRemoteICS(context.Background(), id, []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\n"+body+"END:VCALENDAR\r\n"), nil)
		nha100Code(t, err, ContradictoryIdentity)
	}
}

func TestNHA101DomainImageSlotAndPreParserCapacity(t *testing.T) {
	f := nha100Setup(t)
	id := f.bind(t, "calendar.famille")
	first, err := images.reserve(imageFixedCharge)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.release)
	second, err := images.reserve(imageFixedCharge)
	if err != nil {
		first.release()
		t.Fatal(err)
	}
	t.Cleanup(second.release)
	_, err = admitRemoteICS(context.Background(), id, nha101Raw(t, "02-utc.ics"), nil)
	nha100Code(t, err, Busy)
	first.release()
	second.release()
	for _, name := range []string{"03-parameter-amplification.ics", "04-parameter-key-storm.ics"} {
		image, observation, err := admitRemoteICSObserved(context.Background(), id, costRaw(t, name), nil)
		nha100Code(t, err, CapacityExceeded)
		if image != nil || observation.ParserCalls != 0 || observation.ReservedBytes > parseReservationLimit || observation.RequestedBytes <= parseReservationLimit {
			t.Fatal("over-budget AST entered before reservation")
		}
	}
	available := nha101Admit(t, id, nha101Raw(t, "02-utc.ics"))
	available.close()
}

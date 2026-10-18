package sequencer_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/sequencer"
)

type nha019Publisher struct {
	session *integrations.Session
	binding integrations.Binding
	entity  domain.EntityID
}

func nha019Fixture(t *testing.T, limits sequencer.QueueLimits) (*sequencer.Sequencer, *integrations.Manager, *fake.Fixture) {
	t.Helper()
	sequence, err := sequencer.NewWithLimits(sequencer.RealClock{}, 1024, limits)
	if err != nil {
		t.Fatal(err)
	}
	_, identity, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(identity, sequence, catalog)
	if err != nil {
		t.Fatal(err)
	}
	return sequence, host, identity
}

func nha019Start(t *testing.T, host *integrations.Manager, name string) nha019Publisher {
	t.Helper()
	instance, err := domain.ParseIntegrationInstanceID(name)
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := fake.NewDriver(time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	binding, err := driver.Binding()
	if err != nil {
		t.Fatal(err)
	}
	_, entity, err := binding.IDs()
	if err != nil {
		t.Fatal(err)
	}
	return nha019Publisher{session: session, binding: binding, entity: entity}
}

func nha019Publish(t *testing.T, p nha019Publisher, on bool) {
	t.Helper()
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	value := domain.NewSwitchValue(on)
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	state, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.session.Publish(context.Background(), p.binding, state); err != nil {
		t.Fatal(err)
	}
}

func nha019Emit(t *testing.T, p nha019Publisher, n int) {
	t.Helper()
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	seq := uint64(n)
	event, err := integrations.NewDeviceEvent("fake.button.press", at, &seq)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.session.Emit(context.Background(), p.binding, event); err != nil {
		t.Fatal(err)
	}
}

func nha019Next(t *testing.T, sub *sequencer.Subscription) (sequencer.Delivery, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return sub.Next(ctx)
}

func TestNHA019UICoalescingPreservesOrderAndEdges(t *testing.T) {
	limits, _ := sequencer.NewQueueLimits(4, 2, 2, 2)
	sequence, host, _ := nha019Fixture(t, limits)
	a := nha019Start(t, host, "nha019-a")
	b := nha019Start(t, host, "nha019-b")
	ui, start, err := sequence.SubscribeUI()
	if err != nil {
		t.Fatal(err)
	}
	defer ui.Close()
	nha019Publish(t, a, true)  // A(seq1)
	nha019Publish(t, b, true)  // B(seq2)
	nha019Publish(t, a, false) // A(seq3) moves after B
	nha019Emit(t, a, 1)
	nha019Emit(t, a, 2)
	status, _ := ui.Status()
	if status.Capacity != 4 || status.Queued != 4 || status.Coalesced != 1 || status.Lost != 1 || status.ReplayValid {
		t.Fatalf("UI bounds/coalescing: %+v", status)
	}
	gap, err := nha019Next(t, ui)
	if !errors.Is(err, sequencer.ErrCursorInvalidated) || gap.Kind() != sequencer.DeliveryLoss {
		t.Fatalf("coalescing did not invalidate cursor: %v %+v", err, gap)
	}
	loss, _ := gap.Loss()
	_, base, _ := start.Components()
	_, lost, _ := loss.From.Components()
	if loss.Count != 1 || lost != base+1 || loss.Reason != sequencer.LossUIResync {
		t.Fatalf("wrong UI loss: %+v", loss)
	}
	var got []sequencer.Entry
	for range 4 {
		delivery, err := nha019Next(t, ui)
		if err != nil || delivery.ReplayValid() {
			t.Fatalf("UI entry improperly marked replayable: %v %+v", err, delivery)
		}
		entry, ok := delivery.Entry()
		if !ok {
			t.Fatal("expected UI entry")
		}
		got = append(got, entry)
	}
	for i, entry := range got {
		_, seq, _ := entry.Cursor().Components()
		want := base + uint64(i) + 2
		if seq != want {
			t.Fatalf("UI order went backward or skipped wrong item: got %d want %d", seq, want)
		}
	}
	if got[0].Kind() != sequencer.KindState || got[1].Kind() != sequencer.KindState || got[2].Kind() != sequencer.KindEvent || got[3].Kind() != sequencer.KindEvent {
		t.Fatal("an edge was coalesced with a state or another edge")
	}
	for i, entry := range got[2:] {
		_, event, ok := entry.Event()
		_, _, sourceSeq, err := event.Components()
		if !ok || err != nil || sourceSeq == nil || *sourceSeq != uint64(i+1) {
			t.Fatalf("edge %d vanished or changed", i)
		}
	}
}

func TestNHA019FiveEdgesOverflowWithoutBlockingOtherSubscriber(t *testing.T) {
	limits, _ := sequencer.NewQueueLimits(2, 2, 2, 2)
	sequence, host, _ := nha019Fixture(t, limits)
	p := nha019Start(t, host, "nha019-overflow")
	slow, start, _ := sequence.SubscribeEdge()
	defer slow.Close()
	fast, _, _ := sequence.SubscribeEdge()
	defer fast.Close()
	recorder, _, _ := sequence.SubscribeRecorder()
	defer recorder.Close()
	for n := 1; n <= 5; n++ {
		nha019Emit(t, p, n)
		delivery, err := nha019Next(t, fast)
		if err != nil || !delivery.ReplayValid() {
			t.Fatalf("healthy subscriber blocked at edge %d: %v", n, err)
		}
		entry, ok := delivery.Entry()
		if !ok || entry.Kind() != sequencer.KindEvent {
			t.Fatalf("healthy subscriber missed edge %d", n)
		}
		_, event, _ := entry.Event()
		_, _, number, _ := event.Components()
		if number == nil || *number != uint64(n) {
			t.Fatalf("healthy subscriber reordered edge %d", n)
		}
	}
	snapshot, _ := sequence.Capture()
	_, before, _ := start.Components()
	_, after, _ := snapshot.Cursor.Components()
	if after != before+5 {
		t.Fatalf("slow subscriber blocked sequencer: %d to %d", before, after)
	}
	status, _ := slow.Status()
	if status.Queued != 0 || status.Lost != 5 || !status.Degraded || status.ReplayValid {
		t.Fatalf("edge overflow was not bounded and visible: %+v", status)
	}
	delivery, err := nha019Next(t, slow)
	if !errors.Is(err, sequencer.ErrEdgeOverflow) {
		t.Fatalf("overflow error missing: %v", err)
	}
	loss, ok := delivery.Loss()
	if !ok || loss.Count != 5 || loss.Reason != sequencer.LossEdgeOverflow {
		t.Fatalf("edge loss range missing: %+v", loss)
	}
	_, from, _ := loss.From.Components()
	_, to, _ := loss.To.Components()
	if from != before+1 || to != after {
		t.Fatalf("wrong edge loss range %d..%d", from, to)
	}
	if _, err := nha019Next(t, slow); !errors.Is(err, sequencer.ErrEdgeOverflow) {
		t.Fatalf("degraded edge stream resumed silently: %v", err)
	}
	recorderStatus, _ := recorder.Status()
	if recorderStatus.Queued != 2 || recorderStatus.Lost != 3 || recorderStatus.ReplayValid {
		t.Fatalf("recorder did not expose bounded loss: %+v", recorderStatus)
	}
	recorderGap, err := nha019Next(t, recorder)
	if !errors.Is(err, sequencer.ErrRecorderGap) {
		t.Fatalf("recorder loss error missing: %v", err)
	}
	recorderLoss, _ := recorderGap.Loss()
	_, recorderFrom, _ := recorderLoss.From.Components()
	_, recorderTo, _ := recorderLoss.To.Components()
	if recorderLoss.Count != 3 || recorderFrom != before+1 || recorderTo != before+3 {
		t.Fatalf("recorder lost range: %+v", recorderLoss)
	}
	for n := 4; n <= 5; n++ {
		remaining, err := nha019Next(t, recorder)
		entry, ok := remaining.Entry()
		_, seq, _ := entry.Cursor().Components()
		if err != nil || !ok || seq != before+uint64(n) || remaining.ReplayValid() {
			t.Fatalf("recorder remaining %d: %v %+v", n, err, remaining)
		}
	}
	entries, cursor, err := sequence.ReadAfter(start, 5)
	if err != nil || len(entries) != 5 || cursor != snapshot.Cursor {
		t.Fatalf("admission journal differs from subscriptions: %d %v", len(entries), err)
	}
}

func TestNHA019SubscribeCursorMatchesConcurrentHostJournalSuffix(t *testing.T) {
	limits, _ := sequencer.NewQueueLimits(2, 1024, 2, 2)
	sequence, host, _ := nha019Fixture(t, limits)
	p := nha019Start(t, host, "nha019-concurrent")
	before, _ := sequence.Capture()
	started := make(chan struct{})
	resume := make(chan struct{})
	subscribed := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
		for n := 1; n <= 500; n++ {
			if n == 101 {
				close(started)
				<-resume
			}
			if n == 401 {
				<-subscribed
			}
			number := uint64(n)
			event, err := integrations.NewDeviceEvent("fake.button.press", at, &number)
			if err == nil {
				err = p.session.Emit(context.Background(), p.binding, event)
			}
			if err != nil {
				finished <- err
				return
			}
		}
		finished <- nil
	}()
	<-started
	type result struct {
		sub    *sequencer.Subscription
		cursor sequencer.Cursor
		err    error
	}
	created := make(chan result, 1)
	go func() {
		sub, cursor, err := sequence.SubscribeEdge()
		created <- result{sub: sub, cursor: cursor, err: err}
		close(subscribed)
	}()
	close(resume)
	got := <-created
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	defer got.sub.Close()
	after, _ := sequence.Capture()
	_, first, _ := before.Cursor.Components()
	_, boundary, _ := got.cursor.Components()
	_, last, _ := after.Cursor.Components()
	if boundary < first+100 || boundary > first+400 || last != first+500 {
		t.Fatalf("subscription boundary outside publication: %d < %d < %d", first, boundary, last)
	}
	want, end, err := sequence.ReadAfter(got.cursor, 500)
	if err != nil || end != after.Cursor || len(want) == 0 {
		t.Fatalf("journal suffix unreadable: %d %v", len(want), err)
	}
	status, _ := got.sub.Status()
	if status.Queued != len(want) || status.Lost != 0 || !status.ReplayValid {
		t.Fatalf("subscription lost concurrent suffix: %+v want=%d", status, len(want))
	}
	for i, expected := range want {
		delivery, err := nha019Next(t, got.sub)
		entry, ok := delivery.Entry()
		if err != nil || !ok || !delivery.ReplayValid() || entry.Cursor() != expected.Cursor() {
			t.Fatalf("suffix entry %d differs: %v got=%+v want=%+v", i, err, entry.Cursor(), expected.Cursor())
		}
		_, event, ok := entry.Event()
		_, _, number, eventErr := event.Components()
		_, seq, _ := entry.Cursor().Components()
		if !ok || eventErr != nil || number == nil || *number != seq-first {
			t.Fatalf("suffix event %d changed or reordered", i)
		}
	}
}

func TestNHA019CommandQueueRejectionAndLifecycle(t *testing.T) {
	limits, _ := sequencer.NewQueueLimits(2, 2, 2, 2)
	queue, err := sequencer.NewCommandQueue[string](limits)
	if err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 2; n++ {
		if err := queue.TryOffer(fmt.Sprintf("intent-%d", n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := queue.TryOffer("intent-3"); !errors.Is(err, sequencer.ErrResourceExhausted) {
		t.Fatalf("full command queue did not reject: %v", err)
	}
	status, _ := queue.Status()
	if status.Capacity != 2 || status.Queued != 2 || status.Rejected != 1 {
		t.Fatalf("command metric/bounds: %+v", status)
	}
	if err := queue.Close(); err != nil {
		t.Fatal(err)
	}
	if err := queue.TryOffer("late"); !errors.Is(err, sequencer.ErrCommandQueueClosed) {
		t.Fatalf("closed command queue accepted work: %v", err)
	}
	for n := 1; n <= 2; n++ {
		item, err := queue.Take(context.Background())
		if err != nil || item != fmt.Sprintf("intent-%d", n) {
			t.Fatalf("accepted command lost on close: %q %v", item, err)
		}
	}
	if _, err := queue.Take(context.Background()); !errors.Is(err, sequencer.ErrCommandQueueClosed) {
		t.Fatalf("closed drained queue did not terminate: %v", err)
	}
	if _, err := sequencer.NewCommandQueue[int](sequencer.QueueLimits{}); !errors.Is(err, sequencer.ErrInvalidInput) {
		t.Fatalf("zero limits admitted: %v", err)
	}
}

func TestNHA019CommandQueueConcurrentOfferTakeStatusClose(t *testing.T) {
	limits, _ := sequencer.NewQueueLimits(2, 2, 2, 2)
	queue, _ := sequencer.NewCommandQueue[int](limits)
	type producerResult struct {
		accepted, rejected int
		err                error
	}
	produced := make(chan producerResult, 1)
	consumed := make(chan int, 1)
	statusErr := make(chan error, 1)
	halfway := make(chan struct{})
	stopStatus := make(chan struct{})
	go func() {
		result := producerResult{}
		for n := 1; n <= 1000; n++ {
			if n == 500 {
				close(halfway)
			}
			switch err := queue.TryOffer(n); {
			case err == nil:
				result.accepted++
			case errors.Is(err, sequencer.ErrResourceExhausted):
				result.rejected++
			case errors.Is(err, sequencer.ErrCommandQueueClosed):
				produced <- result
				return
			default:
				result.err = err
				produced <- result
				return
			}
		}
		produced <- result
	}()
	go func() {
		count := 0
		for {
			_, err := queue.Take(context.Background())
			if errors.Is(err, sequencer.ErrCommandQueueClosed) {
				consumed <- count
				return
			}
			if err != nil {
				consumed <- -1
				return
			}
			count++
		}
	}()
	go func() {
		for {
			select {
			case <-stopStatus:
				statusErr <- nil
				return
			default:
			}
			if _, err := queue.Status(); err != nil {
				statusErr <- err
				return
			}
		}
	}()
	<-halfway
	if err := queue.Close(); err != nil {
		t.Fatal(err)
	}
	result := <-produced
	count := <-consumed
	close(stopStatus)
	if err := <-statusErr; err != nil {
		t.Fatal(err)
	}
	if result.err != nil || count != result.accepted || count < 0 {
		t.Fatalf("accepted intents lost during close: %+v consumed=%d", result, count)
	}
	status, _ := queue.Status()
	if status.Queued != 0 || status.Rejected != uint64(result.rejected) || !status.Closed {
		t.Fatalf("concurrent queue status: %+v producer=%+v", status, result)
	}
}

func TestNHA019SubscriptionCloseWakesReaderAndRejectsZero(t *testing.T) {
	limits, _ := sequencer.NewQueueLimits(2, 2, 2, 2)
	sequence, _, _ := nha019Fixture(t, limits)
	var active []*sequencer.Subscription
	for range sequencer.MaxSubscriptions {
		item, _, err := sequence.SubscribeUI()
		if err != nil {
			t.Fatalf("subscription limit reached early: %v", err)
		}
		active = append(active, item)
	}
	if _, _, err := sequence.SubscribeUI(); !errors.Is(err, sequencer.ErrSubscriptionFull) {
		t.Fatalf("subscriber count unbounded: %v", err)
	}
	for _, item := range active {
		if err := item.Close(); err != nil {
			t.Fatal(err)
		}
	}
	sub, _, err := sequence.SubscribeEdge()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := sub.Next(context.Background())
		done <- err
	}()
	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, sequencer.ErrSubscriptionClosed) {
			t.Fatalf("closed reader returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("subscription close left an orphaned reader")
	}
	if _, err := sub.Next(nil); !errors.Is(err, sequencer.ErrInvalidInput) {
		t.Fatalf("nil context admitted: %v", err)
	}
	if _, err := sequencer.NewQueueLimits(0, 2, 2, 2); !errors.Is(err, sequencer.ErrInvalidInput) {
		t.Fatalf("zero UI capacity admitted: %v", err)
	}
	if _, err := sequencer.NewWithLimits(sequencer.RealClock{}, 2, sequencer.QueueLimits{}); !errors.Is(err, sequencer.ErrInvalidInput) {
		t.Fatalf("zero limit set admitted: %v", err)
	}
}

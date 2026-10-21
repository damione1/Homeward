package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/internal/workday"
)

func nha102Config(t *testing.T, port int, enabled bool) nha029RunFixture {
	t.Helper()
	section := ""
	if enabled {
		section = "workday: {timezone: America/Toronto}\n"
	}
	raw := fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\n%s", shortPrivateDataDir(t), port, section)
	return nha029RunFixtureFromRaw(t, []byte(raw))
}

func nha102Day(t *testing.T, text string) calendars.CivilDate {
	t.Helper()
	date, err := calendars.ParseDate(text)
	if err != nil {
		t.Fatal(err)
	}
	return date
}

func nha102Snapshot(t *testing.T, sequence *sequencer.Sequencer, wantStates int) sequencer.Snapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot, err := sequence.CaptureBounded(16 << 20)
		if err == nil && len(snapshot.States) == wantStates {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatalf("workday states: got %d, want %d, err %v", len(snapshot.States), wantStates, err)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestNHA102ConfiguredRunBindsReaderAndPublishesLocalDate(t *testing.T) {
	cluster := testpg.Start(t)
	cfg := nha102Config(t, cluster.Port, true)
	clock := &nha105RunClock{wall: time.Date(2026, 12, 25, 12, 0, 0, 0, time.UTC)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	ready := make(chan struct{})
	readers := make(chan workday.CalendarReader, 2)
	sequences := make(chan *sequencer.Sequencer, 1)
	deps := runDependencies{
		sequenceClock: clock, workdayClock: clock,
		configureWorkdayCalendar: func(_ context.Context, reader workday.CalendarReader) error { readers <- reader; return nil },
		observe:                  func(_ context.Context, sequence *sequencer.Sequencer) error { sequences <- sequence; return nil },
	}
	go func() {
		done <- runWithDependencies(ctx, cfg.startup, (&net.ListenConfig{}).Listen, func(net.Addr) { close(ready) }, nil, deps)
	}()
	joined := false
	t.Cleanup(func() {
		cancel()
		if !joined {
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(10 * time.Second):
				t.Error("workday Run did not join")
			}
		}
	})
	select {
	case <-ready:
	case err := <-done:
		joined = true
		t.Fatalf("workday Run exited before ready: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("workday Run did not start")
	}
	reader := <-readers
	select {
	case <-readers:
		t.Fatal("calendar callback invoked twice")
	default:
	}
	sequence := <-sequences
	id, err := reader.CalendarID()
	if err != nil || id.Origin() != calendars.Calculated {
		t.Fatalf("reader identity origin: %v, %v", id.Origin(), err)
	}
	alias, _ := id.Alias()
	aliasText, _ := alias.Value()
	if aliasText != "calendar.workday_sensor_calendar" {
		t.Fatal("reader bound to wrong alias")
	}
	if _, mutable := reader.(interface {
		CreateCalendar(context.Context, calendars.CalendarID) error
	}); mutable {
		t.Fatal("reader exposed write capability")
	}
	from, to := nha102Day(t, "2026-12-21"), nha102Day(t, "2026-12-28")
	spans, err := reader.Dates(ctx, from, to)
	if err != nil || len(spans) != 5 {
		t.Fatalf("Christmas week: %d dates, err %v", len(spans), err)
	}
	for i, span := range spans {
		start, end, ok := span.Dates()
		wantStart := nha102Day(t, fmt.Sprintf("2026-12-%02d", 21+i))
		wantEnd, _ := wantStart.AddDays(1)
		if !ok || start != wantStart || end != wantEnd {
			t.Fatalf("event %d is not one all-day workday", i)
		}
	}
	for _, window := range []struct{ from, to, first, second string }{
		{"2026-03-06", "2026-03-10", "2026-03-06", "2026-03-09"},
		{"2026-10-30", "2026-11-03", "2026-10-30", "2026-11-02"},
	} {
		projected, err := reader.Dates(ctx, nha102Day(t, window.from), nha102Day(t, window.to))
		if err != nil || len(projected) != 2 {
			t.Fatalf("DST civil window %s: got %d dates, err %v", window.from, len(projected), err)
		}
		for index, want := range []string{window.first, window.second} {
			start, end, allDay := projected[index].Dates()
			next, _ := nha102Day(t, want).AddDays(1)
			if !allDay || start != nha102Day(t, want) || end != next {
				t.Fatalf("DST event %d for %s lost civil one-day interval", index, window.from)
			}
		}
	}
	if _, err := reader.Dates(ctx, from, from); err == nil {
		t.Fatal("empty window admitted")
	}
	if dates, err := reader.Dates(nil, from, to); err == nil || dates != nil {
		t.Fatal("nil context admitted or allocated calendar dates")
	}
	if _, err := reader.Dates(ctx, nha102Day(t, "2026-01-01"), nha102Day(t, "2027-01-02")); err != nil {
		t.Fatalf("366-day window refused: %v", err)
	}
	if dates, err := reader.Dates(ctx, nha102Day(t, "2026-01-01"), nha102Day(t, "2027-01-03")); err == nil || dates != nil {
		t.Fatal("367-day window was allocated or admitted")
	}
	terminal, err := reader.Dates(ctx, nha102Day(t, "9999-12-30"), nha102Day(t, "9999-12-31"))
	if err != nil || len(terminal) > 1 {
		t.Fatalf("terminal admitted window: %d dates, err %v", len(terminal), err)
	}
	snapshot := nha102Snapshot(t, sequence, 1)
	if len(snapshot.Registry) != 2 {
		t.Fatalf("workday expected two bound aliases, got %d", len(snapshot.Registry))
	}
	var binary domain.EntityID
	for id, record := range snapshot.Registry {
		_, alias, _, err := record.Aliases()
		if err != nil {
			t.Fatal(err)
		}
		name, _ := alias.Value()
		if name == "binary_sensor.workday_sensor" {
			binary = id
		}
	}
	state, found := snapshot.States[binary]
	if !found {
		t.Fatal("binary alias has no state")
	}
	value, present := state.CurrentValue()
	on, switchValue := value.Switch()
	if !present || !switchValue || !on {
		t.Fatal("weekday holiday did not publish present true")
	}
	deadline := time.Now().Add(time.Second)
	for {
		clock.mu.Lock()
		armed := len(clock.timers) > 0
		clock.mu.Unlock()
		if armed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("workday timer not armed")
		}
		time.Sleep(time.Millisecond)
	}
	clock.advance(17 * time.Hour)
	deadline = time.Now().Add(5 * time.Second)
	for {
		snapshot, err = sequence.CaptureBounded(16 << 20)
		if err == nil {
			state = snapshot.States[binary]
			value, present = state.CurrentValue()
			on, switchValue = value.Switch()
			if present && switchValue && !on {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("Saturday local midnight did not publish present false")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		joined = true
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("workday Run did not stop")
	}
	// A new Run keeps the durable calendar identity and republishes the
	// current false value after restart, without replaying the prior day.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	done2 := make(chan error, 1)
	ready2 := make(chan struct{})
	go func() {
		done2 <- runWithDependencies(ctx2, cfg.startup, (&net.ListenConfig{}).Listen, func(net.Addr) { close(ready2) }, nil, deps)
	}()
	select {
	case <-ready2:
	case err := <-done2:
		t.Fatalf("restarted Run exited: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("restarted Run did not start")
	}
	secondReader := <-readers
	secondID, err := secondReader.CalendarID()
	if err != nil || secondID != id {
		t.Fatalf("calendar identity changed after restart: %v", err)
	}
	secondSequence := <-sequences
	secondImage := nha102Snapshot(t, secondSequence, 1)
	secondState, found := secondImage.States[binary]
	if !found {
		t.Fatal("binary identity changed after restart")
	}
	secondValue, present := secondState.CurrentValue()
	secondOn, isSwitch := secondValue.Switch()
	if !present || !isSwitch || secondOn {
		t.Fatal("restart did not publish current present false")
	}
	cancel2()
	select {
	case err := <-done2:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("restarted Run did not join")
	}
}

func TestNHA102UnconfiguredRunDoesNotCallCalendarConsumer(t *testing.T) {
	cluster := testpg.Start(t)
	cfg := nha102Config(t, cluster.Port, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := make(chan struct{}, 1)
	sequences := make(chan *sequencer.Sequencer, 1)
	done := make(chan error, 1)
	ready := make(chan struct{})
	deps := runDependencies{
		configureWorkdayCalendar: func(context.Context, workday.CalendarReader) error { called <- struct{}{}; return nil },
		observe:                  func(_ context.Context, sequence *sequencer.Sequencer) error { sequences <- sequence; return nil },
	}
	go func() {
		done <- runWithDependencies(ctx, cfg.startup, (&net.ListenConfig{}).Listen, func(net.Addr) { close(ready) }, nil, deps)
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("unconfigured Run exited: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("unconfigured Run did not start")
	}
	select {
	case <-called:
		t.Fatal("unconfigured workday callback invoked")
	default:
	}
	image, err := (<-sequences).CaptureBounded(16 << 20)
	if err != nil || len(image.Registry) != 0 || len(image.States) != 0 {
		t.Fatalf("unconfigured Run produced workday bindings/states: registry=%d states=%d err=%v", len(image.Registry), len(image.States), err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("unconfigured Run did not join")
	}
}

func TestNHA102CalendarConsumerFailureStopsBeforeServing(t *testing.T) {
	cluster := testpg.Start(t)
	cfg := nha102Config(t, cluster.Port, true)
	want := errors.New("synthetic calendar consumer failure")
	var calls, listened, observed atomic.Int32
	deps := runDependencies{
		configureWorkdayCalendar: func(_ context.Context, reader workday.CalendarReader) error {
			calls.Add(1)
			if err := workday.ValidateReader(reader); err != nil {
				return err
			}
			return want
		},
		observe: func(context.Context, *sequencer.Sequencer) error { observed.Add(1); return nil },
	}
	listen := func(context.Context, string, string) (net.Listener, error) {
		listened.Add(1)
		return nil, errors.New("should not listen")
	}
	err := runWithDependencies(context.Background(), cfg.startup, listen, nil, nil, deps)
	if !errors.Is(err, want) || calls.Load() != 1 || listened.Load() != 0 || observed.Load() != 0 {
		t.Fatalf("calendar failure escaped startup fence: err=%v callbacks=%d listen=%d observe=%d", err, calls.Load(), listened.Load(), observed.Load())
	}
}

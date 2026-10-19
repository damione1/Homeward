package solar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"google.golang.org/protobuf/proto"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type nha105PilotClock struct {
	mu         sync.Mutex
	wall       time.Time
	tick       time.Duration
	timers     []*nha105PilotTimer
	armed      chan time.Duration
	budgetJump bool
}
type nha105PilotTimer struct {
	clock   *nha105PilotClock
	due     time.Duration
	ch      chan time.Time
	stopped bool
}

func (c *nha105PilotClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.wall }
func (c *nha105PilotClock) MonoNow() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	value := c.tick
	if c.budgetJump {
		c.tick += time.Second
	}
	return value
}
func (c *nha105PilotClock) NewTimer(d time.Duration) automationtriggers.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &nha105PilotTimer{clock: c, due: c.tick + d, ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, timer)
	if d <= 0 {
		timer.ch <- c.wall
	}
	if c.armed != nil {
		select {
		case c.armed <- d:
		default:
		}
	}
	return timer
}
func (t *nha105PilotTimer) C() <-chan time.Time { return t.ch }
func (t *nha105PilotTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	return was
}
func (c *nha105PilotClock) advance(wall, elapsed time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(wall)
	c.tick += elapsed
	for _, timer := range c.timers {
		if !timer.stopped && timer.due <= c.tick {
			select {
			case timer.ch <- c.wall:
			default:
			}
		}
	}
}

type nha105PilotTasks struct {
	ctx     context.Context
	done    chan error
	started bool
	joined  bool
	result  error
}

func (tasks *nha105PilotTasks) Go(task integrations.Task) error {
	tasks.started = true
	go func() { tasks.done <- task(tasks.ctx) }()
	return nil
}
func (tasks *nha105PilotTasks) join(t *testing.T) error {
	t.Helper()
	if !tasks.started || tasks.joined {
		return tasks.result
	}
	select {
	case tasks.result = <-tasks.done:
		tasks.joined = true
		return tasks.result
	case <-time.After(time.Second):
		t.Error("solar task failed to join")
		return context.DeadlineExceeded
	}
}
func nha105PilotCleanup(t *testing.T, pilot integrations.Pilot, cancel context.CancelFunc, tasks *nha105PilotTasks) {
	t.Helper()
	t.Cleanup(func() {
		_ = pilot.Stop(context.Background())
		cancel()
		if err := tasks.join(t); err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
}

type nha105PublishGate struct {
	integrations.SessionHost
	count     int
	before    int
	entered   chan struct{}
	release   chan struct{}
	published chan int
	bindings  []integrations.Binding
}

func (g *nha105PublishGate) Discover(ctx context.Context, candidates []integrations.Candidate) ([]integrations.Binding, error) {
	bindings, err := g.SessionHost.Discover(ctx, candidates)
	g.bindings = append([]integrations.Binding(nil), bindings...)
	return bindings, err
}
func (g *nha105PublishGate) Publish(ctx context.Context, binding integrations.Binding, observation integrations.StateObservation) error {
	g.count++
	if g.count == g.before {
		close(g.entered)
		select {
		case <-g.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := g.SessionHost.Publish(ctx, binding, observation); err != nil {
		return err
	}
	if g.published != nil {
		g.published <- g.count
	}
	return nil
}
func nha105PilotWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("solar barrier timed out")
	}
}

func TestNHA105PublicationPrefixAndSnapshotConsistency(t *testing.T) {
	clock := &nha105PilotClock{wall: time.Date(2024, 3, 10, 12, 0, 0, 0, time.UTC), armed: make(chan time.Duration, 8)}
	host, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := InstanceID()
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	gate := &nha105PublishGate{SessionHost: session, before: 3, entered: make(chan struct{}), release: make(chan struct{}), published: make(chan int, 32)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tasks := &nha105PilotTasks{ctx: ctx, done: make(chan error, 1)}
	factory, err := NewPilotFactory(nha105Settings(t, 0, 0, 0, "UTC"), clock)
	if err != nil {
		t.Fatal(err)
	}
	pilot := factory()
	nha105PilotCleanup(t, pilot, cancel, tasks)
	if _, commands := pilot.(integrations.SupervisedCommandPilot); commands {
		t.Fatal("solar acquired a command driver")
	}
	if err := pilot.Start(ctx, gate, tasks); err != nil {
		t.Fatal(err)
	}
	nha105PilotWait(t, gate.entered)
	prefix, err := fixture.Sequencer().Capture()
	if err != nil || len(prefix.States) != 2 {
		t.Fatal("prefix claimed atomic publication")
	}
	close(gate.release)
	select {
	case <-clock.armed:
	case <-time.After(3 * time.Second):
		t.Fatal("pilot did not arm one timer")
	}
	complete, err := fixture.Sequencer().Capture()
	if err != nil || len(complete.States) != observationCount {
		t.Fatal("seven states not published")
	}
	_, sunEntity, _ := gate.bindings[6].IDs()
	sun, _ := complete.States[sunEntity].CurrentValue()
	solar, _ := sun.Solar()
	input, _ := solar.Components()
	for i, binding := range gate.bindings[:6] {
		_, entity, _ := binding.IDs()
		state := complete.States[entity]
		native, ok := state.CurrentValue()
		if !ok {
			t.Fatal("event absent")
		}
		event, ok := native.SolarEvent()
		if !ok {
			t.Fatal("event became another state kind")
		}
		kind, occurrence, err := event.Components()
		if err != nil || kind != domain.SolarEventKinds()[i] || occurrence != input.Next[i] {
			t.Fatal("aggregate and event observations disagree")
		}
		_, _, times, source, _, _ := state.Components()
		_, seq, _ := source.Components()
		if times.ObservedAt == nil || *times.ObservedAt != clock.Now().UTC() || seq == nil || *seq != uint64(i+1) {
			t.Fatal("capture or source order differs")
		}
	}
	if len(prefix.States) != 2 {
		t.Fatal("old prefix capture mutated")
	}
	clock.advance(3*time.Minute, 3*time.Minute)
	_ = pilot.Stop(context.Background())
	cancel()
	if err := tasks.join(t); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	clock.mu.Lock()
	defer clock.mu.Unlock()
	for _, timer := range clock.timers {
		if !timer.stopped {
			t.Fatal("timer orphaned")
		}
	}
}

func TestNHA105PilotCancellationDuringBlockedPublication(t *testing.T) {
	clock := &nha105PilotClock{wall: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
	host, fixture, _ := fake.NewFixtureWithClock(clock)
	instance, _ := InstanceID()
	session, _ := host.OpenSession(instance)
	gate := &nha105PublishGate{SessionHost: session, before: 3, entered: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tasks := &nha105PilotTasks{ctx: ctx, done: make(chan error, 1)}
	factory, _ := NewPilotFactory(nha105Settings(t, 89.9, 0, 0, "UTC"), clock)
	pilot := factory()
	nha105PilotCleanup(t, pilot, cancel, tasks)
	if err := pilot.Start(ctx, gate, tasks); err != nil {
		t.Fatal(err)
	}
	nha105PilotWait(t, gate.entered)
	if err := pilot.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := tasks.join(t); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	snapshot, _ := fixture.Sequencer().Capture()
	if len(snapshot.States) != 2 {
		t.Fatal("cancellation published a late tail")
	}
}

func TestNHA105PilotCadenceCancellationAndBudget(t *testing.T) {
	clock := &nha105PilotClock{wall: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), budgetJump: true}
	calculator := nha105Calculator(t, 0, 0, 0, "UTC")
	pilot := &Pilot{settings: &calculator.settings, clock: clock}
	if _, err := pilot.computation(context.Background(), calculator); err == nil {
		t.Fatal("cooperative publication budget ignored")
	} else {
		var solarError *Error
		if !errors.As(err, &solarError) || solarError.Code() != BudgetExceeded {
			t.Fatal("budget became an astronomical absence")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pilot.computation(ctx, calculator); err == nil {
		t.Fatal("canceled calculation admitted")
	}
}

type nha105PilotReferenceOccurrence struct {
	Kind    string `json:"kind"`
	Instant string `json:"instant_utc"`
	Reason  string `json:"reason"`
}

func nha105PublishedOccurrence(t *testing.T, got domain.SolarOccurrence, want nha105PilotReferenceOccurrence) {
	t.Helper()
	at, reason, kind, err := got.Components()
	if err != nil {
		t.Fatal(err)
	}
	switch want.Kind {
	case "present":
		expected, err := time.Parse(time.RFC3339Nano, want.Instant)
		if err != nil {
			t.Fatal(err)
		}
		delta := at.Sub(expected)
		if kind != domain.SolarOccurrencePresent || delta < -time.Second || delta > time.Second {
			t.Fatal("published occurrence differs from qualified HA reference")
		}
	case "absent":
		if kind != domain.SolarOccurrenceAbsent || reason != domain.SolarNoEventWithinSearchHorizon || want.Reason != "no_event_within_search_horizon" || !at.IsZero() {
			t.Fatal("published absence differs from qualified HA reference")
		}
	default:
		t.Fatal("unknown archived occurrence")
	}
}

// This exercises the new publication/wire path using the already qualified
// archive. It does not invoke the P0 probe or start another HA/Astral worker.
func TestNHA105QualifiedReferencesThroughPilotAndWire(t *testing.T) {
	root := filepath.Join("..", "..", "compat", "ha", "nha105")
	inputs, err := os.ReadFile(filepath.Join(root, "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	archiveBytes, err := os.ReadFile(filepath.Join(root, "observed.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases struct {
		Cases []struct {
			ID         string   `json:"id"`
			Latitude   *float64 `json:"latitude"`
			Longitude  *float64 `json:"longitude"`
			Elevation  *float64 `json:"elevation_m"`
			Zone       string   `json:"timezone"`
			Now        string   `json:"now_utc"`
			AfterEvent string   `json:"after_event"`
			Shift      int64    `json:"after_shift_us"`
		} `json:"cases"`
	}
	var archive struct {
		Inputs struct {
			Hash string `json:"cases_sha256"`
		} `json:"inputs"`
		HA struct {
			Cases []struct {
				ID     string `json:"id"`
				Events []struct {
					Event string                         `json:"event"`
					Next  nha105PilotReferenceOccurrence `json:"next"`
				} `json:"events"`
				Position struct {
					State     string  `json:"state"`
					Elevation float64 `json:"elevation"`
					Azimuth   float64 `json:"azimuth"`
					Rising    bool    `json:"rising"`
				} `json:"position"`
				Sun struct {
					Error    string                           `json:"error"`
					Change   string                           `json:"next_change"`
					Position string                           `json:"next_position"`
					Next     []nha105PilotReferenceOccurrence `json:"next"`
				} `json:"sun"`
			} `json:"cases"`
		} `json:"ha"`
	}
	if json.Unmarshal(inputs, &cases) != nil || json.Unmarshal(archiveBytes, &archive) != nil {
		t.Fatal("invalid preserved qualification inputs")
	}
	hash := sha256.Sum256(inputs)
	if archive.Inputs.Hash != hex.EncodeToString(hash[:]) || len(cases.Cases) != 26 || len(archive.HA.Cases) != 26 {
		t.Fatal("preserved qualified corpus changed")
	}
	exceptions := 0
	for index, input := range cases.Cases {
		want := archive.HA.Cases[index]
		if input.ID != want.ID {
			t.Fatal("qualified corpus order changed")
		}
		if want.Sun.Error != "" {
			exceptions++
			if want.Sun.Error != "no_event_within_search_horizon" {
				t.Fatal("unknown preserved HA entity exception")
			}
		}
		t.Run(input.ID, func(t *testing.T) {
			settings, err := ParseSettings(Input{Latitude: input.Latitude, Longitude: input.Longitude, ElevationMeters: input.Elevation, Timezone: input.Zone})
			if err != nil {
				t.Fatal(err)
			}
			now, err := time.Parse(time.RFC3339Nano, input.Now)
			if err != nil {
				t.Fatal(err)
			}
			if input.AfterEvent != "" {
				calculator, err := NewCalculator(settings)
				if err != nil {
					t.Fatal(err)
				}
				kind, err := domain.ParseSolarEventKind(input.AfterEvent)
				if err != nil {
					t.Fatal(err)
				}
				anchor, err := calculator.NextEvent(context.Background(), kind, now)
				if err != nil {
					t.Fatal(err)
				}
				at, _, presence, err := anchor.Components()
				if err != nil || presence != domain.SolarOccurrencePresent {
					t.Fatal("qualified equality anchor missing")
				}
				now = at.Add(time.Duration(input.Shift) * time.Microsecond)
			}
			clock := &nha105PilotClock{wall: now.UTC(), armed: make(chan time.Duration, 8)}
			host, fixture, err := fake.NewFixtureWithClock(clock)
			if err != nil {
				t.Fatal(err)
			}
			instance, _ := InstanceID()
			session, err := host.OpenSession(instance)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			tasks := &nha105PilotTasks{ctx: ctx, done: make(chan error, 1)}
			factory, err := NewPilotFactory(settings, clock)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			pilot := factory()
			nha105PilotCleanup(t, pilot, cancel, tasks)
			if err := pilot.Start(ctx, session, tasks); err != nil {
				t.Fatal(err)
			}
			var delay time.Duration
			select {
			case delay = <-clock.armed:
			case <-time.After(3 * time.Second):
				t.Fatal("qualified pilot did not publish and schedule")
			}
			snapshot, err := fixture.Sequencer().CaptureBounded(4 << 20)
			if err != nil || len(snapshot.States) != 7 {
				t.Fatal("qualified pilot lost one of seven observations")
			}
			if len(want.Events) != 6 {
				t.Fatal("qualified reference lost events")
			}
			for _, state := range snapshot.States {
				wire, err := contracts.EncodeObservedState(state)
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := proto.Marshal(wire)
				if err != nil {
					t.Fatal(err)
				}
				decoded := wire.ProtoReflect().Type().New().Interface()
				if err := proto.Unmarshal(encoded, decoded); err != nil || !proto.Equal(wire, decoded) {
					t.Fatal("published wire roundtrip changed state")
				}
				native, present := state.CurrentValue()
				if !present {
					t.Fatal("qualified astronomical absence became unavailable")
				}
				if event, ok := native.SolarEvent(); ok {
					kind, occurrence, err := event.Components()
					if err != nil {
						t.Fatal(err)
					}
					i, _ := kind.Index()
					name, _ := kind.Name()
					if name != want.Events[i].Event {
						t.Fatal("event branch order changed")
					}
					nha105PublishedOccurrence(t, occurrence, want.Events[i].Next)
					if admitted, err := contracts.AdmitWireSolarEventValue(wire.GetSolarEvent()); err != nil || admitted != event {
						t.Fatal("event wire admission changed published value")
					}
				} else if sun, ok := native.Solar(); ok {
					parts, _ := sun.Components()
					horizon := "below_horizon"
					if parts.Horizon == domain.SolarAboveHorizon {
						horizon = "above_horizon"
					}
					if horizon != want.Position.State || parts.Rising != want.Position.Rising || math.Abs(parts.ElevationDegrees-want.Position.Elevation) > 0.010000001 || math.Abs(parts.AzimuthDegrees-want.Position.Azimuth) > 0.010000001 {
						t.Fatal("published solar position differs from qualified reference")
					}
					for i, occurrence := range parts.Next {
						nha105PublishedOccurrence(t, occurrence, want.Events[i].Next)
					}
					if admitted, err := contracts.AdmitWireSolarValue(wire.GetSolar()); err != nil || admitted != sun {
						t.Fatal("solar wire admission changed published value")
					}
				} else {
					t.Fatal("qualified pilot published another state kind")
				}
			}
			if want.Sun.Error == "" {
				change, err := time.Parse(time.RFC3339Nano, want.Sun.Change)
				if err != nil {
					t.Fatal(err)
				}
				if want.Sun.Position != "" {
					position, err := time.Parse(time.RFC3339Nano, want.Sun.Position)
					if err != nil {
						t.Fatal(err)
					}
					if position.Before(change) {
						change = position
					}
				}
				difference := delay - change.Sub(now)
				if difference < -time.Second || difference > time.Second {
					t.Fatal("pilot timer differs from qualified HA cadence")
				}
			}
		})
	}
	if exceptions != 4 {
		t.Fatal("preserved HA entity exceptions changed")
	}
}

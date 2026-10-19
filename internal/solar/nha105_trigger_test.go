package solar

import (
	"archive/zip"
	"context"
	"encoding/binary"
	"encoding/json"
	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type nha105TriggerResolver struct{}

// Hold the first registered timer until the test advances its clock. The
// initial040 wake can then cancel it only after the jump, so any replacement
// computes its delay at the new wall/monotonic instant.
type nha105SunStartClock struct {
	*nha105PilotClock
	first       sync.Once
	releaseOnce sync.Once
	armed       chan time.Duration
	release     chan struct{}
}

func (c *nha105SunStartClock) NewTimer(delay time.Duration) automationtriggers.Timer {
	timer := c.nha105PilotClock.NewTimer(delay)
	c.first.Do(func() {
		c.armed <- delay
		<-c.release
	})
	return timer
}

func (c *nha105SunStartClock) Release() { c.releaseOnce.Do(func() { close(c.release) }) }

func (nha105TriggerResolver) ResolveEntity(domain.HAAlias) (domain.EntityID, error) {
	return domain.ParseEntityID("nha105-unused")
}
func (nha105TriggerResolver) ResolveDevice(ir.HADeviceRef) (domain.DeviceID, error) {
	return domain.DeviceID{}, automationtriggers.ErrInvalidPlan
}
func (nha105TriggerResolver) ResolveCalendar(domain.HAAlias) (automationtriggers.CalendarSourceID, error) {
	return automationtriggers.CalendarSourceID{}, automationtriggers.ErrInvalidPlan
}

func nha105SunAutomation(t *testing.T) ir.Automation {
	t.Helper()
	entity, err := ir.NewEntity("switch.fixture", ir.ReferencePresent, ir.SupportObserveState, ir.SupportSwitchTurnOn)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := ir.NewCatalogue([]ir.Entity{entity}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err := parser.NewSourceFile("nha105.yaml", []byte("id: solar_fixture\ntriggers:\n- trigger: sun\n  event: sunset\n  offset: '-01:00:00'\nactions:\n- action: switch.turn_on\n  target: {entity_id: switch.fixture}\n  data: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := ir.CompileSources("automation.solar_fixture", "nha105.yaml", []parser.SourceFile{source}, catalog)
	if err != nil || len(compiled.Accepted()) != 1 {
		t.Fatal("synthetic sun trigger failed admission")
	}
	return compiled.Accepted()[0]
}

func nha105RunSunTrigger(t *testing.T, settings Settings, start time.Time) (time.Time, time.Time) {
	t.Helper()
	clock := &nha105SunStartClock{nha105PilotClock: &nha105PilotClock{wall: start}, armed: make(chan time.Duration, 1), release: make(chan struct{})}
	_, fixture, err := fake.NewFixtureWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	input, err := ConfigureTriggerInput(settings, automationtriggers.PlanInput{Automations: []ir.Automation{nha105SunAutomation(t)}, Resolver: nha105TriggerResolver{}})
	if err != nil {
		t.Fatal(err)
	}
	sunset, err := input.SunSource.NextSunset(context.Background(), start, input.Zone, input.SolarPosition)
	if err != nil {
		t.Fatal(err)
	}
	at, availability, err := sunset.Components()
	if err != nil || availability != automationtriggers.SunPresent {
		t.Fatal("expected synthetic sunset absent")
	}
	plan, err := automationtriggers.NewPlan(input)
	if err != nil {
		t.Fatal(err)
	}
	service, err := automationtriggers.NewService(plan, fixture.Sequencer(), clock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		clock.Release()
		_ = service.Close()
	}()
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	target := at.Add(-time.Hour)
	if !target.After(start) {
		t.Fatal("invalid synthetic trigger anchor")
	}
	select {
	case delay := <-clock.armed:
		if delay != target.Sub(start) {
			t.Fatalf("source040 armed a different solar offset: delay=%v want=%v", delay, target.Sub(start))
		}
	case <-time.After(time.Second):
		t.Fatal("trigger engine did not arm")
	}
	clock.advance(target.Sub(start), target.Sub(start))
	clock.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := service.NextResult(ctx)
	if err != nil || result.Kind() != automationtriggers.ResultFired || !result.At().Equal(target) {
		t.Fatalf("source040 lost or double-applied solar offset: err=%v kind=%v at=%v want=%v", err, result.Kind(), result.At(), target)
	}
	short, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	if duplicate, err := service.NextResult(short); err == nil && duplicate.Kind() == automationtriggers.ResultFired {
		t.Fatal("duplicate solar occurrence")
	}
	return at, result.At()
}

func TestNHA105SunsetMinusHourThroughTriggerEngine(t *testing.T) {
	for _, day := range []string{"2024-03-10T00:00:00Z", "2024-11-03T00:00:00Z"} {
		start, _ := time.Parse(time.RFC3339, day)
		settings := nha105Settings(t, 0, 0, 1000, "America/New_York")
		nha105RunSunTrigger(t, settings, start)
		source, _ := NewTriggerSource(settings)
		wrong, _ := automationtriggers.NewSolarPosition(1, 0)
		if _, err := source.NextSunset(context.Background(), start, "America/New_York", wrong); err == nil {
			t.Fatal("private observer mismatch admitted")
		}
		correct, _ := automationtriggers.NewSolarPosition(0, 0)
		if _, err := source.NextSunset(context.Background(), start, "UTC", correct); err == nil {
			t.Fatal("private timezone mismatch admitted")
		}
	}
}

func TestNHA105PinnedSunSourceWithContradictoryAmbientZone(t *testing.T) {
	type observed struct {
		Offset int
		Date   string
		Sunset time.Time
		Fired  time.Time
	}
	if output := os.Getenv("HOMEWARD_NHA105_ZONE_RESULT"); output != "" {
		start := time.Date(2024, 3, 10, 0, 0, 0, 0, time.UTC)
		location, err := time.LoadLocation("America/New_York")
		if err != nil {
			t.Fatal(err)
		}
		_, offset := start.In(location).Zone()
		at, fired := nha105RunSunTrigger(t, nha105Settings(t, 0, 0, 0, "America/New_York"), start)
		data, _ := json.Marshal(observed{offset, start.In(location).Format("2006-01-02"), at, fired})
		if err := os.WriteFile(output, data, 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	root := t.TempDir()
	archive, err := os.Create(filepath.Join(root, "zoneinfo.zip"))
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(archive)
	member, err := writer.CreateHeader(&zip.FileHeader{Name: "America/New_York", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	tzif := make([]byte, 44)
	copy(tzif, "TZif")
	binary.BigEndian.PutUint32(tzif[36:40], 1)
	binary.BigEndian.PutUint32(tzif[40:44], 4)
	var localType [6]byte
	binary.BigEndian.PutUint32(localType[:4], 9*3600)
	tzif = append(tzif, localType[:]...)
	tzif = append(tzif, []byte("XST\x00")...)
	if _, err := member.Write(tzif); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	var observations [2]observed
	for i := range observations {
		path := filepath.Join(root, []string{"normal.json", "contradictory.json"}[i])
		command := exec.Command(os.Args[0], "-test.run=^TestNHA105PinnedSunSourceWithContradictoryAmbientZone$", "-test.count=1")
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "ZONEINFO=") && !strings.HasPrefix(entry, "HOMEWARD_NHA105_ZONE_RESULT=") {
				command.Env = append(command.Env, entry)
			}
		}
		command.Env = append(command.Env, "HOMEWARD_NHA105_ZONE_RESULT="+path)
		if i == 1 {
			command.Env = append(command.Env, "ZONEINFO="+filepath.Join(root, "zoneinfo.zip"))
		}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("isolated timezone oracle failed: %v %s", err, output)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &observations[i]); err != nil {
			t.Fatal(err)
		}
	}
	if observations[0].Offset == observations[1].Offset || observations[0].Date == observations[1].Date {
		t.Fatal("ambient timezone control was not discriminating")
	}
	if !observations[0].Sunset.Equal(observations[1].Sunset) || !observations[0].Fired.Equal(observations[1].Fired) {
		t.Fatal("ambient archive changed pinned solar result or040 occurrence")
	}
}

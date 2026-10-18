package sequencer_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/sequencer"
)

type nha021Clock struct {
	mu   sync.Mutex
	tick time.Duration
}

func (c *nha021Clock) Now() time.Time          { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) }
func (c *nha021Clock) MonoNow() time.Duration  { c.mu.Lock(); defer c.mu.Unlock(); return c.tick }
func (c *nha021Clock) Advance(d time.Duration) { c.mu.Lock(); c.tick += d; c.mu.Unlock() }

func TestNHA021JournalTimeAndByteRetention(t *testing.T) {
	clock := &nha021Clock{}
	seq, err := sequencer.New(clock, 8)
	if err != nil {
		t.Fatal(err)
	}
	start, _ := seq.Start()
	if err := seq.PublishRegistry(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	clock.Advance(sequencer.JournalWindow + time.Nanosecond)
	if _, _, err := seq.ReadAfter(start, 1); !errors.Is(err, sequencer.ErrCursorExpired) {
		t.Fatalf("time retention: %v", err)
	}
	current, _ := seq.Capture()
	sub, _, _, err := seq.SubscribeViewAfter(current.Cursor, sequencer.MaxJournalCapacity)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	device, _ := domain.ParseDeviceID("device")
	entity, _ := domain.ParseEntityID(strings.Repeat("x", sequencer.MaxJournalBytes+1))
	instance, _ := domain.ParseIntegrationInstanceID("fixture")
	key, _ := integrations.NewSourceKey("device", "relay")
	alias, _ := domain.ParseHAAlias("switch.fixture")
	record, err := integrations.NewRegistryRecord(device, entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := seq.PublishRegistry(context.Background(), []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	status, err := sub.Status()
	if err != nil || status.QueuedBytes != 0 || !status.Degraded {
		t.Fatalf("view byte queue was not bounded: %+v %v", status, err)
	}
	if _, _, err := sub.Poll(); !errors.Is(err, sequencer.ErrViewOverflow) {
		t.Fatalf("oversized live entry: %v", err)
	}
	if _, _, err := seq.ReadAfter(current.Cursor, 1); !errors.Is(err, sequencer.ErrCursorExpired) {
		t.Fatalf("byte retention: %v", err)
	}
	snapshot, err := seq.Capture()
	if err != nil || len(snapshot.Registry) != 1 {
		t.Fatalf("projection lost with journal eviction: %v", err)
	}
}

package mqtt

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
	"github.com/eclipse/paho.golang/paho"
)

const nha166RetainedQoS0Count = 146 // Passive SRV2 packet metadata, 23 October 2026.

func nha166RetainedReady(broker *testbroker.Broker, producerID string) bool {
	if broker.PublishCount(producerID) != nha166RetainedQoS0Count {
		return false
	}
	// Mochi also retains its own $SYS topics, so an aggregate Len cannot
	// attest the synthetic fixture population.
	for i := range nha166RetainedQoS0Count {
		topic := fmt.Sprintf("nha068/state/retained/%03d", i)
		packet, ok := broker.Server.Topics.Retained.Get(topic)
		if !ok || packet.FixedHeader.Qos != 0 || !packet.FixedHeader.Retain || len(packet.Payload) != 1 || packet.Payload[0] != byte(i) {
			return false
		}
	}
	return true
}

type nha166SlowObservation struct {
	first   chan struct{}
	release <-chan struct{}
	seen    chan string
	once    sync.Once
}

func (h *nha166SlowObservation) Observe(ctx context.Context, _ integrations.SessionHost, observation Observation) error {
	if observation.class != ObservationRestored {
		return ErrInvalidSettings
	}
	first := false
	h.once.Do(func() { first = true; close(h.first) })
	if first {
		select {
		case <-h.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// Each later delivery represents a short SQL/mapper step. The initial
	// retained burst must be paced through the fixed 32-slot queue.
	timer := time.NewTimer(time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return ctx.Err()
	}
	topic, err := observation.topic.Value()
	if err != nil {
		return err
	}
	select {
	case h.seen <- topic:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestNHA166RetainedQoS0BurstBackpressuresWithoutLoss(t *testing.T) {
	broker := testbroker.Start(t)
	producerID := "nha166-retained-producer"
	producer := broker.Connect(t, testbroker.ProducerUser, testbroker.ProducerPassword, producerID, true, 0, nil)
	for i := range nha166RetainedQoS0Count {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, err := producer.Client.Publish(ctx, &paho.Publish{
			Topic: fmt.Sprintf("nha068/state/retained/%03d", i), Payload: []byte{byte(i)}, QoS: 0, Retain: true,
		})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for !nha166RetainedReady(broker, producerID) {
		if time.Now().After(deadline) {
			t.Fatal("synthetic retained population incomplete")
		}
		time.Sleep(time.Millisecond)
	}

	settings := nha068Settings(t, broker.Address, ModePassive)
	broker.WatchSentTopics(settings.clientID)
	release := make(chan struct{})
	handler := &nha166SlowObservation{first: make(chan struct{}), release: release, seen: make(chan string, nha166RetainedQoS0Count)}
	pilot, err := NewPilot(settings, handler)
	if err != nil {
		t.Fatal(err)
	}
	journal, _ := nha056Journal(t)
	cells, err := journal.Cells()
	if err != nil || pilot.AttachObservation(cells, journal) != nil {
		t.Fatal("observation attachment failed", err)
	}
	tasks := newNHA068Tasks()
	host := &nha068Host{provider: nha068Provider{instance: settings.instance, password: testbroker.PilotPassword}, health: make(chan integrations.HealthReport, 4)}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	err = pilot.Start(ctx, host, tasks)
	cancel()
	if err != nil {
		tasks.cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { tasks.close(t, pilot) })
	select {
	case <-handler.first:
	case <-time.After(3 * time.Second):
		t.Fatal("retained burst did not enter the consumer")
	}
	deadline = time.Now().Add(time.Second)
	for len(pilot.observationQueue) != queueCapacity {
		if time.Now().After(deadline) {
			t.Fatal("retained burst did not fill the bounded queue")
		}
		time.Sleep(time.Millisecond)
	}
	// A real stalled mapper cannot hold the callback indefinitely. This short
	// synthetic stall separates backpressure from immediate overflow.
	time.Sleep(20 * time.Millisecond)
	close(release)
	seen := make([]string, 0, nha166RetainedQoS0Count)
	complete := time.NewTimer(5 * time.Second)
	defer complete.Stop()
	for len(seen) < nha166RetainedQoS0Count {
		select {
		case topic := <-handler.seen:
			seen = append(seen, topic)
		case err := <-tasks.errors:
			t.Fatalf("retained burst failed: %v", err)
		case <-complete.C:
			t.Fatalf("retained burst incomplete: %d/%d", len(seen), nha166RetainedQoS0Count)
		}
	}
	sent := broker.SentTopics(settings.clientID)
	if !reflect.DeepEqual(seen, sent) {
		t.Fatalf("retained delivery order/count differs from broker: got=%d sent=%d", len(seen), len(sent))
	}
	queue, err := cells.RuntimeQueue(obs.QueueClass_QUEUE_CLASS_MQTT_INBOUND)
	if err != nil || queue.GetInstalled() == nil {
		t.Fatal("MQTT queue unavailable after retained burst", err)
	}
	installed := queue.GetInstalled()
	if installed.CapacityEntries.GetValue() != queueCapacity || installed.RejectedTotal.GetExact() != 0 || installed.LostDeliveriesTotal.GetExact() != 0 {
		t.Fatal("backpressure changed queue bound or counted an avoidable loss")
	}
	if broker.PublishCount(settings.clientID) != 0 {
		t.Fatal("passive retained observation sent an application publication")
	}
}

func TestNHA166StopUnblocksAdmissionWaitingForConsumer(t *testing.T) {
	broker := testbroker.Start(t)
	settings := nha068Settings(t, broker.Address, ModePassive)
	handler := nha056BlockedObservation{entered: make(chan struct{}, 1)}
	pilot, err := NewPilot(settings, handler)
	if err != nil {
		t.Fatal(err)
	}
	journal, _ := nha056Journal(t)
	cells, err := journal.Cells()
	if err != nil || pilot.AttachObservation(cells, journal) != nil {
		t.Fatal("observation attachment failed", err)
	}
	tasks := newNHA068Tasks()
	host := &nha068Host{provider: nha068Provider{instance: settings.instance, password: testbroker.PilotPassword}, health: make(chan integrations.HealthReport, 4)}
	startCtx, startCancel := context.WithTimeout(context.Background(), 4*time.Second)
	err = pilot.Start(startCtx, host, tasks)
	startCancel()
	if err != nil {
		tasks.cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { tasks.close(t, pilot) })
	broker.Publish(t, "nha068/state/cancel", []byte("first"), false)
	select {
	case <-handler.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("consumer did not block")
	}
	producer := broker.Connect(t, testbroker.ProducerUser, testbroker.ProducerPassword, "nha166-cancel-producer", true, 0, nil)
	for i := range queueCapacity + 1 {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, err := producer.Client.Publish(ctx, &paho.Publish{Topic: "nha068/state/cancel", Payload: []byte{byte(i)}, QoS: 0})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(150 * time.Millisecond)
	for len(pilot.observationQueue) != queueCapacity {
		if time.Now().After(deadline) {
			t.Fatal("callback did not reach a full queue before cancellation")
		}
		time.Sleep(time.Millisecond)
	}
	tasks.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := pilot.Stop(ctx); err != nil {
		t.Fatalf("Stop did not join the waiting callback: %v", err)
	}
	done := make(chan struct{})
	go func() { tasks.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("admission callback survived Stop")
	}
	counters, err := cells.QueueCounters(obs.QueueClass_QUEUE_CLASS_MQTT_INBOUND)
	if err != nil || counters.Lost().GetExact() != 0 {
		t.Fatal("orderly cancellation mislabeled as queue loss", err)
	}
}

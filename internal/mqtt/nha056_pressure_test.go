package mqtt

import (
	"context"
	"testing"
	"time"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
	"github.com/eclipse/paho.golang/paho"
)

type nha056BlockedObservation struct{ entered chan struct{} }

func (h nha056BlockedObservation) Observe(ctx context.Context, _ integrations.SessionHost, _ Observation) error {
	select {
	case h.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestNHA056RealMQTTPressureCountsLossAndJoinsQueueWithoutPublication(t *testing.T) {
	broker := testbroker.Start(t)
	settings := nha068Settings(t, broker.Address, ModePassive)
	handler := nha056BlockedObservation{entered: make(chan struct{}, 1)}
	pilot, err := NewPilot(settings, handler)
	if err != nil {
		t.Fatal(err)
	}
	journal, _ := nha056Journal(t)
	cells, err := journal.Cells()
	if err != nil {
		t.Fatal(err)
	}
	if err := pilot.AttachObservation(cells, journal); err != nil {
		t.Fatal(err)
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
	broker.Publish(t, "nha068/state/device", []byte("first"), false)
	select {
	case <-handler.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("actual MQTT consumer did not enter handler")
	}
	// QoS0 bypasses MQTT ReceiveMaximum; a blocked handler otherwise leaves
	// QoS1 packets in the broker rather than filling the installed application queue.
	producer := broker.Connect(t, testbroker.ProducerUser, testbroker.ProducerPassword, "nha056-qos0-pressure", true, 0, nil)
	for range queueCapacity + 1 {
		publishCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		_, err := producer.Client.Publish(publishCtx, &paho.Publish{Topic: "nha068/state/device", Payload: []byte("NHA056_MQTT_PAYLOAD_CANARY"), QoS: 0})
		stop()
		if err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		queue, err := cells.RuntimeQueue(obs.QueueClass_QUEUE_CLASS_MQTT_INBOUND)
		if err != nil {
			t.Fatal(err)
		}
		installed := queue.GetInstalled()
		if installed != nil && installed.RejectedTotal.GetExact() == 1 {
			if installed.QueuedEntries.GetValue() != queueCapacity || installed.CapacityEntries.GetValue() != queueCapacity || installed.LostDeliveriesTotal.GetExact() != 1 || installed.PendingEntries.GetNotMeasured() == nil {
				t.Fatal("actual MQTT pressure populations incorrect")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("actual MQTT overflow not observed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	tasks.close(t, pilot)
	queue, err := cells.RuntimeQueue(obs.QueueClass_QUEUE_CLASS_MQTT_INBOUND)
	if err != nil || queue.GetNotInstalled() == nil {
		t.Fatal("joined MQTT queue remained installed", err)
	}
	counters, err := cells.QueueCounters(obs.QueueClass_QUEUE_CLASS_MQTT_INBOUND)
	if err != nil || counters.Lost().GetExact() != 1 {
		t.Fatal("teardown lost cumulative MQTT loss", err)
	}
	if broker.PublishCount(settings.clientID) != 0 {
		t.Fatal("observing pressure emitted application publication")
	}
}

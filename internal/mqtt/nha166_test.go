package mqtt

import (
	"context"
	"errors"
	"testing"

	"github.com/damione1/Homeward/internal/mqtt/testbroker"
)

func TestNHA166ReconnectKeepsPassiveSettingsWithoutWillOrApplicationPublish(t *testing.T) {
	broker := testbroker.Start(t)
	settings := nha068Settings(t, broker.Address, ModePassive)
	if !settings.StrictlyPassive() {
		t.Fatal("admitted passive profile was not strict")
	}
	if nha068Settings(t, broker.Address, ModeActive).StrictlyPassive() {
		t.Fatal("active profile passed read-only admission")
	}
	for incarnation := 0; incarnation < 2; incarnation++ {
		capture := nha068Capture{observations: make(chan Observation, 8)}
		pilot, tasks := nha068Start(t, settings, capture)
		connected, ok := broker.Server.Clients.Get(settings.clientID)
		if !ok || connected.Properties.Will.Flag != 0 || connected.Properties.Will.TopicName != "" || len(connected.Properties.Will.Payload) != 0 {
			t.Fatal("CONNECT installed a will")
		}
		topic, _ := ParseTopic("nha068/command/relay")
		command, _ := NewCommandPublication(topic, []byte("synthetic-negative"))
		if err := pilot.WriteCommand(context.Background(), command); !errors.Is(err, ErrPassiveMode) {
			t.Fatal("passive command was not rejected before transport", err)
		}
		broker.Publish(t, "nha068/state/device", []byte("synthetic-read"), false)
		_ = nha068Observation(t, capture.observations)
		tasks.close(t, pilot)
		if broker.PublishCount(settings.clientID) != 0 {
			t.Fatal("passive startup/negative/disconnect published application data")
		}
	}
}

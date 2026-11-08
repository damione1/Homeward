package mqtt

import (
	"context"
	"testing"
	"time"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

func TestNHA211MQTTUncorrelatedSendEndsDurablyWithoutBlockingNextIntention(t *testing.T) {
	r := newNHA180FeedbackRig(t, false, false)
	cluster := testpg.Start(t)
	settings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	service, err := integrations.NewDurableCommandService(context.Background(), r.manager, nha069Generation{}, store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := service.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	// This public test peer identifier is independent of the broker's fixture credentials.
	const statePeerClientID = "nha211-state-peer"
	peer := r.broker.Connect(t, testbroker.ProducerUser, testbroker.ProducerPassword, statePeerClientID, true, 0, nil)
	on := true
	request := r.request("nha211-mqtt-first", &actions.SetLight{On: &on})
	for i := 0; i < 2; i++ {
		response, err := service.Dispatch(context.Background(), r.actor, request)
		if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_SENT_UNVERIFIED {
			t.Fatal("send not terminal", response, err)
		}
		select {
		case packet := <-r.packets:
			if packet.Retain || packet.QoS != 0 || packet.Properties != nil && (packet.Properties.ResponseTopic != "" || len(packet.Properties.CorrelationData) != 0) {
				t.Fatal("uncorrelated command acquired speculative response metadata")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("missing isolated publication")
		}
		cause, err := r.driver.last.Causality()
		if err != nil {
			t.Fatal(err)
		}
		metadata, err := encodeCorrelation(cause, nha114CurrentScope(r))
		if err != nil {
			t.Fatal(err)
		}
		// Even a valid unsolicited echo cannot promote a policy without correlation.
		nha114PublishAndWait(t, r, peer, "zigbee2mqtt/kitchen/state", []byte(`{"state":"ON"}`), []byte(metadata.wire), false)
		id, _ := domain.ParseCommandID(request.CommandId)
		status, found, err := service.Status(context.Background(), id)
		if err != nil || !found || status.Response == nil || status.Response.Status != actions.ActionStatus_ACTION_STATUS_SENT_UNVERIFIED {
			t.Fatal("observation changed durable outcome", status, err)
		}
		r.status(t, request.CommandId, actions.ActionStatus_ACTION_STATUS_SENT_UNVERIFIED)
		if _, err := service.Dispatch(context.Background(), r.actor, request); err != nil {
			t.Fatal(err)
		}
		if count := r.broker.PublishCount(r.settings.clientID); count != i+1 {
			t.Fatal("duplicate replayed", count)
		}
		request = r.request("nha211-mqtt-next", &actions.SetLight{On: &on})
	}
}

func TestNHA211MQTTFeedbackPolicyAdmission(t *testing.T) {
	good := CommandFeedbackInput{SourceDevice: "synthetic-controller", SourceEntity: "relay", Mode: "mqtt5_correlated_state"}
	for _, input := range [][]CommandFeedbackInput{
		{good, good}, {{SourceDevice: good.SourceDevice, SourceEntity: good.SourceEntity, Mode: "mqtt5_correlated_states"}},
		{{SourceDevice: "", SourceEntity: good.SourceEntity, Mode: good.Mode}}, make([]CommandFeedbackInput, 129),
	} {
		s := Settings{mode: ModeActive}
		if s.admitCommandFeedback(input) == nil {
			t.Fatal("invalid feedback admitted")
		}
	}
	s := Settings{mode: ModePassive}
	if s.admitCommandFeedback([]CommandFeedbackInput{good}) == nil {
		t.Fatal("passive correlation admitted")
	}
	s.mode = ModeActive
	if err := s.admitCommandFeedback([]CommandFeedbackInput{good}); err != nil {
		t.Fatal(err)
	}
	key, _ := integrations.NewSourceKey(good.SourceDevice, good.SourceEntity)
	if s.commandFeedback[key] != feedbackCorrelatedState {
		t.Fatal("exact peer not configured")
	}
	other, _ := integrations.NewSourceKey(good.SourceDevice, "another-relay")
	if s.commandFeedback[other] != feedbackSentUnverified {
		t.Fatal("policy widened to another entity")
	}
}

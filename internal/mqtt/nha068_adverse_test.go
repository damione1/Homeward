package mqtt

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho"
)

func nha068RawBroker(t *testing.T, stage string) (string, <-chan struct{}) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	done := make(chan struct{})
	blocked := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if stage == "tls" {
			buf := make([]byte, 512)
			if _, err := conn.Read(buf); err == nil {
				close(entered)
			}
			_, _ = io.Copy(io.Discard, conn)
			return
		}
		packet, err := packets.ReadPacket(conn)
		if err != nil || packet.FixedHeader.Type != packets.CONNECT {
			t.Logf("raw CONNECT read: packet=%#v err=%v", packet, err)
			return
		}
		if stage == "connect" {
			close(entered)
			_, _ = io.Copy(io.Discard, conn)
			return
		}
		if stage == "sessionpresent" {
			_, err = (&packets.Connack{Properties: &packets.Properties{}, SessionPresent: true}).WriteTo(conn)
			if err == nil {
				close(entered)
				_, _ = io.Copy(io.Discard, conn)
			}
			return
		}
		_, err = (&packets.Connack{Properties: &packets.Properties{}}).WriteTo(conn)
		if err != nil {
			t.Logf("raw CONNACK write: %v", err)
			return
		}
		for {
			packet, err = packets.ReadPacket(conn)
			if err != nil {
				t.Logf("raw post-CONNACK read: %v", err)
				return
			}
			if packet.FixedHeader.Type == packets.PINGREQ {
				_, _ = (&packets.Pingresp{}).WriteTo(conn)
				continue
			}
			if packet.FixedHeader.Type != packets.SUBSCRIBE {
				t.Logf("raw expected SUBSCRIBE, got type %d", packet.FixedHeader.Type)
				return
			}
			break
		}
		close(entered)
		if stage == "subscribe" {
			_, _ = io.Copy(io.Discard, conn)
			return
		}
		sub := packet.Content.(*packets.Subscribe)
		_, err = (&packets.Suback{Properties: &packets.Properties{}, PacketID: sub.PacketID, Reasons: []byte{1}}).WriteTo(conn)
		if err != nil {
			return
		}
		if stage == "blockedwrite" {
			<-blocked
			return
		}
		_, _ = io.Copy(io.Discard, conn)
	}()
	t.Cleanup(func() {
		close(blocked)
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("raw broker handler did not join")
		}
	})
	return listener.Addr().String(), entered
}

func TestNHA068RejectsIncoherentSessionPresentAndJoinsPaho(t *testing.T) {
	address, entered := nha068RawBroker(t, "sessionpresent")
	settings := nha068Settings(t, address, ModePassive)
	p, err := NewPilot(settings, nha068Capture{observations: make(chan Observation, 1)})
	if err != nil {
		t.Fatal(err)
	}
	host := &nha068Host{provider: nha068Provider{instance: settings.instance, password: testbroker.PilotPassword}, health: make(chan integrations.HealthReport, 1)}
	tasks := newNHA068Tasks()
	startDone := make(chan error, 1)
	go func() { startDone <- p.Start(context.Background(), host, tasks) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("incoherent CONNACK not sent")
	}
	select {
	case err := <-startDone:
		if !errors.Is(err, ErrBrokerConnection) {
			t.Fatalf("incoherent CONNACK accepted: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("incoherent CONNACK did not terminate")
	}
	if p.client == nil {
		t.Fatal("successful Paho CONNECT was not tracked for cleanup")
	}
	select {
	case <-p.client.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Paho workers survived CONNACK rejection")
	}
	tasks.close(t, p)
}

func TestNHA068BlockedPhysicalWriteHasDeadlineAndJoins(t *testing.T) {
	address, _ := nha068RawBroker(t, "blockedwrite")
	settings := nha068Settings(t, address, ModeActive)
	p, err := NewPilot(settings, nha068Capture{observations: make(chan Observation, 1)})
	if err != nil {
		t.Fatal(err)
	}
	tasks := newNHA068Tasks()
	host := &nha068Host{provider: nha068Provider{instance: settings.instance, password: testbroker.PilotPassword}, health: make(chan integrations.HealthReport, 1)}
	startCtx, startCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer startCancel()
	if err := p.Start(startCtx, host, tasks); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tasks.close(t, p) })
	if tcp, ok := p.conn.(*net.TCPConn); ok {
		if err := tcp.SetWriteBuffer(1024); err != nil {
			t.Fatal(err)
		}
	}
	topic, _ := ParseTopic("nha068/command/relay")
	command, _ := NewCommandPublication(topic, make([]byte, maxPayload))
	deadline := time.Now().Add(5 * time.Second)
	uncertain := false
	for i := 0; i < 256 && time.Now().Before(deadline); i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := p.WriteCommand(ctx, command)
		cancel()
		if errors.Is(err, ErrCommandWriteUncertain) {
			uncertain = true
			break
		}
		if err != nil {
			t.Fatalf("blocked write returned unexpected result: %v", err)
		}
	}
	if !uncertain {
		t.Fatal("nonreading broker never exhausted bounded command write")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tasks.cancel()
	if err := p.Stop(stopCtx); err != nil {
		t.Fatalf("Stop did not join physical writer: %v", err)
	}
	tasks.wg.Wait()
}

func TestNHA068StopDuringTLSConnectAndSubscribe(t *testing.T) {
	for _, stage := range []string{"tls", "connect", "subscribe"} {
		t.Run(stage, func(t *testing.T) {
			address, entered := nha068RawBroker(t, stage)
			settings := nha068Settings(t, address, ModePassive)
			if stage == "tls" {
				input := Input{Profile: "5", Broker: "mqtts://" + address, Instance: "nha068-test-instance", Mode: "passive", UsernameSecret: "mqtt-username", PasswordSecret: "mqtt-password", Subscriptions: []string{"nha068/state/#"}}
				var err error
				settings, err = ParseSettings(input)
				if err != nil {
					t.Fatal(err)
				}
			}
			p, err := NewPilot(settings, nha068Capture{observations: make(chan Observation, 1)})
			if err != nil {
				t.Fatal(err)
			}
			host := &nha068Host{provider: nha068Provider{instance: settings.instance, password: testbroker.PilotPassword}, health: make(chan integrations.HealthReport, 1)}
			tasks := newNHA068Tasks()
			startDone := make(chan error, 1)
			go func() { startDone <- p.Start(context.Background(), host, tasks) }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("pilot never reached " + stage)
			}
			tasks.cancel()
			stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := p.Stop(stopCtx); err != nil {
				t.Fatalf("stop at %s: %v", stage, err)
			}
			select {
			case err := <-startDone:
				if err == nil {
					t.Fatal("Start succeeded after stop")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Start did not join at " + stage)
			}
			tasks.wg.Wait()
		})
	}
}

func TestNHA068CanceledCommandSendsNoPublishAndPilotIsSingleUse(t *testing.T) {
	broker := testbroker.Start(t)
	settings := nha068Settings(t, broker.Address, ModeActive)
	observer := broker.Connect(t, testbroker.ObserverUser, testbroker.ObserverPassword, "nha068-cancel-observer", true, 0, nil)
	messages := observer.SubscribeRAP(t, true, "nha068/command/#")
	p, _ := nha068Start(t, settings, nha068Capture{observations: make(chan Observation, 1)})
	topic, _ := ParseTopic("nha068/command/relay")
	command, _ := NewCommandPublication(topic, []byte("should-never-publish"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.WriteCommand(ctx, command); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled command: %v", err)
	}
	select {
	case packet := <-messages:
		t.Fatalf("canceled command published: %#v", packet)
	case <-time.After(50 * time.Millisecond):
	}
	if err := p.Start(context.Background(), &nha068Host{}, newNHA068Tasks()); !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("second Start admitted: %v", err)
	}
	var nilHost *nha068Host
	newPilot, _ := NewPilot(settings, nha068Capture{})
	if err := newPilot.Start(context.Background(), nilHost, newNHA068Tasks()); !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("typed nil host admitted: %v", err)
	}
	var nilTasks *nha068Tasks
	newPilot, _ = NewPilot(settings, nha068Capture{})
	if err := newPilot.Start(context.Background(), &nha068Host{}, nilTasks); !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("typed nil tasks admitted: %v", err)
	}
	newPilot, _ = NewPilot(settings, nha068Capture{})
	if err := newPilot.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := newPilot.Start(context.Background(), &nha068Host{}, newNHA068Tasks()); !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("Start after Stop admitted: %v", err)
	}
	if err := (&Pilot{}).Stop(context.Background()); !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("zero Pilot Stop admitted: %v", err)
	}
}

func TestNHA068SlowHandlerQueueOverflowClosesTransport(t *testing.T) {
	broker := testbroker.Start(t)
	settings := nha068Settings(t, broker.Address, ModePassive)
	// This handler cannot consume its first observation until cleanup cancels
	// the task. QoS0 floods bypass ReceiveMaximum, exercising callback bounds.
	p, tasks := nha068Start(t, settings, nha068Capture{observations: make(chan Observation)})
	producer := broker.Connect(t, testbroker.ProducerUser, testbroker.ProducerPassword, "nha068-qos0-flood", true, 0, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for i := 0; i < 100; i++ {
		if _, err := producer.Client.Publish(ctx, &paho.Publish{Topic: "nha068/state/flood", Payload: []byte{byte(i)}, QoS: 0}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-tasks.errors:
		if !errors.Is(err, ErrObservationGap) {
			t.Fatalf("overflow failure: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("slow handler did not fence overflowing queue")
	}
	select {
	case <-p.client.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("overflow left Paho client open")
	}
}

func TestNHA068RepeatedRejectedConnectsCloseBeforeCredentialRenewal(t *testing.T) {
	broker := testbroker.Start(t)
	settings := nha068Settings(t, broker.Address, ModePassive)
	for attempt := 0; attempt < 12; attempt++ {
		p, err := NewPilot(settings, nha068Capture{observations: make(chan Observation, 1)})
		if err != nil {
			t.Fatal(err)
		}
		tasks := newNHA068Tasks()
		host := &nha068Host{provider: nha068Provider{instance: settings.instance, password: "expired-canary"}, health: make(chan integrations.HealthReport, 1)}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err = p.Start(ctx, host, tasks)
		cancel()
		if !errors.Is(err, ErrBrokerConnection) {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		tasks.cancel()
		if err := p.Stop(stopCtx); err != nil {
			t.Fatalf("attempt %d stop: %v", attempt, err)
		}
		stopCancel()
		tasks.wg.Wait()
	}
	// A fresh supervisor attempt reads the provider again. The same client ID
	// succeeds after credential rotation; failed attempts carry no Paho state.
	_, _ = nha068Start(t, settings, nha068Capture{observations: make(chan Observation, 1)})
}

func TestNHA068CredentialLengthsAndNULRejectedBeforeDial(t *testing.T) {
	broker := testbroker.Start(t)
	settings := nha068Settings(t, broker.Address, ModePassive)
	for _, tc := range []struct{ name, username, password string }{
		{name: "username NUL", username: "canary\x00invalid", password: testbroker.PilotPassword},
		{name: "username too long", username: strings.Repeat("a", 129), password: testbroker.PilotPassword},
		{name: "password exceeds MQTT binary length", password: strings.Repeat("p", 65536)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewPilot(settings, nha068Capture{observations: make(chan Observation, 1)})
			if err != nil {
				t.Fatal(err)
			}
			host := &nha068Host{provider: nha068Provider{instance: settings.instance, password: tc.password, username: tc.username}, health: make(chan integrations.HealthReport, 1)}
			tasks := newNHA068Tasks()
			if err := p.Start(context.Background(), host, tasks); !errors.Is(err, ErrInvalidSettings) {
				t.Fatalf("invalid credential reached dial: %v", err)
			}
			tasks.close(t, p)
			if client, ok := broker.Server.Clients.Get(settings.clientID); ok && !client.Closed() {
				t.Fatal("invalid credential reached broker")
			}
		})
	}
}

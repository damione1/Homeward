// Package testbroker runs an isolated MQTT 5 broker for NHA-068 integration
// tests. It listens only on an ephemeral loopback port and has canary users.
package testbroker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho"
	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
	mochipackets "github.com/mochi-mqtt/server/v2/packets"
)

const (
	PilotUser        = "nha068-pilot"
	PilotPassword    = "canary-pilot-068"
	ProducerUser     = "nha068-producer"
	ProducerPassword = "canary-producer-068"
	ObserverUser     = "nha068-observer"
	ObserverPassword = "canary-observer-068"
)

type Broker struct {
	Server    *mochi.Server
	Address   string
	sequence  atomic.Uint64
	publishes *publishCounter
}

// publishCounter counts inbound application PUBLISH packets by MQTT client ID.
// Protocol ACKs and subscriptions never enter this count; payloads are ignored.
type publishCounter struct {
	mochi.HookBase
	mu       sync.Mutex
	byClient map[string]int
}

func (*publishCounter) ID() string               { return "nha068-publish-counter" }
func (*publishCounter) Provides(event byte) bool { return event == mochi.OnPacketRead }
func (h *publishCounter) OnPacketRead(client *mochi.Client, packet mochipackets.Packet) (mochipackets.Packet, error) {
	if packet.FixedHeader.Type == mochipackets.Publish {
		h.mu.Lock()
		h.byClient[client.ID]++
		h.mu.Unlock()
	}
	return packet, nil
}
func (b *Broker) PublishCount(clientID string) int {
	b.publishes.mu.Lock()
	defer b.publishes.mu.Unlock()
	return b.publishes.byClient[clientID]
}

func Start(t *testing.T) *Broker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := mochi.New(&mochi.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ledger := &auth.Ledger{Users: auth.Users{
		PilotUser:    {Password: PilotPassword, ACL: auth.Filters{"nha068/state/#": auth.ReadOnly, "nha068/presence/pilot": auth.WriteOnly, "nha068/command/#": auth.WriteOnly}},
		ProducerUser: {Password: ProducerPassword, ACL: auth.Filters{"nha068/state/#": auth.WriteOnly}},
		ObserverUser: {Password: ObserverPassword, ACL: auth.Filters{"nha068/presence/#": auth.ReadOnly, "nha068/command/#": auth.ReadOnly}},
	}, ACL: auth.ACLRules{{Filters: auth.Filters{"#": auth.Deny}}}}
	if err := s.AddHook(new(auth.Hook), &auth.Options{Ledger: ledger}); err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	counter := &publishCounter{byClient: make(map[string]int)}
	if err := s.AddHook(counter, nil); err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	if err := s.AddListener(listeners.NewNet("nha068-test", ln)); err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	if err := s.Serve(); err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Mochi v2.7.9's Server.Close calls Clients.GetByListener, whose
		// nested RLock can deadlock against a concurrent Clients.Delete writer.
		// Drain this disposable listener and its client goroutines first, with
		// a single-lock snapshot. The subsequent Close has no live clients.
		s.Listeners.CloseAll(func(id string) {
			for _, client := range s.Clients.GetAll() {
				if client.Net.Listener == id && !client.Closed() {
					client.Stop(nil)
				}
			}
		})
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return &Broker{Server: s, Address: ln.Addr().String(), publishes: counter}
}

type Peer struct {
	Client *paho.Client
	Conn   net.Conn
	Ack    *paho.Connack
}

func (b *Broker) Connect(t *testing.T, user, password, clientID string, clean bool, expiry uint32, will *paho.WillMessage) *Peer {
	t.Helper()
	conn, err := net.DialTimeout("tcp", b.Address, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client := paho.NewClient(paho.ClientConfig{Conn: packets.NewThreadSafeConn(conn), PacketTimeout: 3 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ack, err := client.Connect(ctx, &paho.Connect{ClientID: clientID, Username: user, Password: []byte(password), UsernameFlag: true, PasswordFlag: true,
		CleanStart: clean, KeepAlive: 10, Properties: &paho.ConnectProperties{SessionExpiryInterval: &expiry}, WillMessage: will})
	if err != nil {
		_ = conn.Close()
		t.Fatalf("broker connect %s: %v", clientID, err)
	}
	t.Cleanup(func() { _ = conn.Close(); <-client.Done() })
	return &Peer{Client: client, Conn: conn, Ack: ack}
}

func (b *Broker) Publish(t *testing.T, topic string, payload []byte, retained bool) {
	t.Helper()
	peer := b.Connect(t, ProducerUser, ProducerPassword, fmt.Sprintf("nha068-producer-%d", b.sequence.Add(1)), true, 0, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response, err := peer.Client.Publish(ctx, &paho.Publish{Topic: topic, Payload: payload, QoS: 1, Retain: retained})
	if err != nil || response == nil || response.ReasonCode >= 0x80 {
		t.Fatalf("broker publish: %#v, %v", response, err)
	}
	_ = peer.Client.Disconnect(&paho.Disconnect{ReasonCode: 0})
}

func (p *Peer) Subscribe(t *testing.T, topics ...string) <-chan *paho.Publish {
	return p.SubscribeRAP(t, false, topics...)
}

func (p *Peer) SubscribeRAP(t *testing.T, rap bool, topics ...string) <-chan *paho.Publish {
	t.Helper()
	observed := make(chan *paho.Publish, 256)
	remove := p.Client.AddOnPublishReceived(func(received paho.PublishReceived) (bool, error) {
		if received.Packet != nil {
			packet := *received.Packet
			packet.Payload = append([]byte(nil), received.Packet.Payload...)
			select {
			case observed <- &packet:
			default:
			}
		}
		return true, nil
	})
	t.Cleanup(remove)
	subs := make([]paho.SubscribeOptions, len(topics))
	for i, topic := range topics {
		subs[i] = paho.SubscribeOptions{Topic: topic, QoS: 1, RetainAsPublished: rap}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ack, err := p.Client.Subscribe(ctx, &paho.Subscribe{Subscriptions: subs})
	if err != nil || ack == nil || len(ack.Reasons) != len(subs) {
		t.Fatalf("broker subscribe: %#v, %v", ack, err)
	}
	for _, reason := range ack.Reasons {
		if reason >= 0x80 {
			t.Fatalf("broker SUBACK reason %d", reason)
		}
	}
	return observed
}

// Package testbroker runs an isolated MQTT 5 broker for NHA-068 integration
// tests. It listens only on an ephemeral loopback port and has canary users.
package testbroker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"reflect"
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
	mu         sync.Mutex
	byClient   map[string]int
	sentTopics map[string][]string
	pending    map[publicationID]*publication
}

func (*publishCounter) ID() string { return "nha068-publish-counter" }
func (*publishCounter) Provides(event byte) bool {
	return event == mochi.OnPacketRead || event == mochi.OnPacketSent || event == mochi.OnPublished
}
func (h *publishCounter) OnPacketRead(client *mochi.Client, packet mochipackets.Packet) (mochipackets.Packet, error) {
	if packet.FixedHeader.Type == mochipackets.Publish {
		h.mu.Lock()
		h.byClient[client.ID]++
		h.mu.Unlock()
	}
	return packet, nil
}

// Sent topics are a synthetic broker witness of the order written to one
// subscriber. They let the transport test compare delivery order without
// assuming Mochi's retained-message map iteration order.
func (h *publishCounter) OnPacketSent(client *mochi.Client, packet mochipackets.Packet, _ []byte) {
	if packet.FixedHeader.Type != mochipackets.Publish {
		return
	}
	h.mu.Lock()
	if topics, watched := h.sentTopics[client.ID]; watched {
		h.sentTopics[client.ID] = append(topics, packet.TopicName)
	}
	h.mu.Unlock()
}

// Mochi v2.7.9 server.go:952,965-966 writes PUBACK before fanout. This hook
// witnesses broker fanout only; it does not witness subscriber/Host processing.
func (h *publishCounter) OnPublished(client *mochi.Client, _ mochipackets.Packet) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := publicationID{client: client}
	if publication, ok := h.pending[id]; ok && publication.state == publicationSending {
		delete(h.pending, id)
		close(publication.fanout)
	}
}

const maxPendingPublications = 32

// A broker client is one TCP connection, even when a successor reuses its MQTT
// client ID. The old hook retains the old pointer while its fanout is running.
type publicationID struct{ client *mochi.Client }

type publicationPermitState uint8

const (
	publicationPermitUnspecified publicationPermitState = iota
	publicationPermitAvailable
	publicationPermitConsumed
)

// Connect fixes this identity after CONNACK. Peer copies share the permit;
// only its state changes, under owner.mu, and release never makes it available.
type publicationPermit struct {
	owner  *publishCounter
	client *paho.Client
	id     publicationID
	state  publicationPermitState
}

type publicationStage uint8

const (
	publicationUnspecified publicationStage = iota
	publicationAdmission
	publicationPUBACK
	publicationFanout
)

type publicationError struct {
	stage publicationStage
	cause error
}

func (e publicationError) Error() string {
	if e.cause == nil {
		return "test broker: invalid publication failure"
	}
	switch e.stage {
	case publicationAdmission:
		return "test broker: publication admission failed: " + e.cause.Error()
	case publicationPUBACK:
		return "test broker: publication PUBACK failed: " + e.cause.Error()
	case publicationFanout:
		return "test broker: publication fanout not witnessed: " + e.cause.Error()
	case publicationUnspecified:
		return "test broker: invalid publication failure"
	default:
		return "test broker: invalid publication failure"
	}
}

func (e publicationError) Unwrap() error { return e.cause }

var (
	errInvalidPublication  = errors.New("invalid publication")
	errPublicationFull     = errors.New("publication witness capacity reached")
	errPublicationRejected = errors.New("broker rejected publication")
	errPublicationPeerUsed = errors.New("peer publication permit already consumed")
)

type publicationState uint8

const (
	publicationStateUnspecified publicationState = iota
	publicationPrepared
	publicationSending
	publicationReleased
)

// One freshly connected producer owns one publication. These channels witness
// independent receipts: the client accepted PUBACK, and the broker finished
// fanout. Fanout may finish before the client receives PUBACK.
type publication struct {
	owner        *publishCounter
	client       *paho.Client
	id           publicationID
	acknowledged chan struct{}
	fanout       chan struct{}
	state        publicationState // guarded by owner.mu; the receipt is single-use
}

func (b *Broker) preparePublication(peer *Peer) (*publication, error) {
	if b == nil || b.publishes == nil || peer == nil || peer.Client == nil || peer.publicationPermit == nil {
		return nil, publicationError{stage: publicationAdmission, cause: errInvalidPublication}
	}
	h := b.publishes
	permit := peer.publicationPermit
	if permit.owner != h || permit.client != peer.Client || permit.id.client == nil {
		return nil, publicationError{stage: publicationAdmission, cause: errInvalidPublication}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pending == nil {
		return nil, publicationError{stage: publicationAdmission, cause: errInvalidPublication}
	}
	switch permit.state {
	case publicationPermitAvailable:
	case publicationPermitConsumed:
		return nil, publicationError{stage: publicationAdmission, cause: errPublicationPeerUsed}
	case publicationPermitUnspecified:
		return nil, publicationError{stage: publicationAdmission, cause: errInvalidPublication}
	default:
		return nil, publicationError{stage: publicationAdmission, cause: errInvalidPublication}
	}
	if _, exists := h.pending[permit.id]; exists {
		return nil, publicationError{stage: publicationAdmission, cause: errInvalidPublication}
	}
	if len(h.pending) >= maxPendingPublications {
		return nil, publicationError{stage: publicationAdmission, cause: errPublicationFull}
	}
	p := &publication{owner: h, client: permit.client, id: permit.id, acknowledged: make(chan struct{}), fanout: make(chan struct{}), state: publicationPrepared}
	permit.state = publicationPermitConsumed
	h.pending[p.id] = p
	return p, nil
}

func (p *publication) release() {
	if p == nil || p.owner == nil {
		return
	}
	p.owner.mu.Lock()
	defer p.owner.mu.Unlock()
	p.state = publicationReleased
	if p.owner.pending[p.id] == p {
		delete(p.owner.pending, p.id)
	}
}

func (p *publication) send(ctx context.Context, topic string, payload []byte, retained bool) error {
	if p == nil || p.owner == nil || p.client == nil || missingContext(ctx) {
		return publicationError{stage: publicationAdmission, cause: errInvalidPublication}
	}
	if err := ctx.Err(); err != nil {
		return publicationError{stage: publicationAdmission, cause: err}
	}
	p.owner.mu.Lock()
	if p.state != publicationPrepared || p.owner.pending[p.id] != p {
		p.owner.mu.Unlock()
		return publicationError{stage: publicationAdmission, cause: errInvalidPublication}
	}
	p.state = publicationSending
	p.owner.mu.Unlock()
	defer p.release()
	response, err := p.client.Publish(ctx, &paho.Publish{Topic: topic, Payload: payload, QoS: 1, Retain: retained})
	if response != nil && response.ReasonCode >= 0x80 {
		// Paho v0.23.0 returns both the rejected PUBACK and its error. Keep
		// that transport cause while admitting the known broker rejection.
		return publicationError{stage: publicationPUBACK, cause: errors.Join(errPublicationRejected, err)}
	}
	if err != nil {
		return publicationError{stage: publicationPUBACK, cause: err}
	}
	if response == nil {
		return publicationError{stage: publicationPUBACK, cause: errPublicationRejected}
	}
	close(p.acknowledged)
	select {
	case <-p.fanout:
		return nil
	case <-ctx.Done():
		return publicationError{stage: publicationFanout, cause: ctx.Err()}
	}
}

func missingContext(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	value := reflect.ValueOf(ctx)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
func (b *Broker) PublishCount(clientID string) int {
	b.publishes.mu.Lock()
	defer b.publishes.mu.Unlock()
	return b.publishes.byClient[clientID]
}

func (b *Broker) SentTopics(clientID string) []string {
	b.publishes.mu.Lock()
	defer b.publishes.mu.Unlock()
	return append([]string(nil), b.publishes.sentTopics[clientID]...)
}

func (b *Broker) WatchSentTopics(clientID string) {
	b.publishes.mu.Lock()
	defer b.publishes.mu.Unlock()
	b.publishes.sentTopics[clientID] = nil
}

func Start(t *testing.T) *Broker {
	t.Helper()
	return start(t)
}

func start(t *testing.T, hooks ...mochi.Hook) *Broker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := mochi.New(&mochi.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ledger := &auth.Ledger{Users: auth.Users{
		PilotUser:    {Password: PilotPassword, ACL: auth.Filters{"nha068/state/#": auth.ReadOnly, "nha068/presence/pilot": auth.WriteOnly, "nha068/command/#": auth.WriteOnly, "homeassistant/#": auth.ReadOnly, "zigbee2mqtt/#": auth.ReadWrite}},
		ProducerUser: {Password: ProducerPassword, ACL: auth.Filters{"nha068/state/#": auth.WriteOnly, "homeassistant/#": auth.WriteOnly, "zigbee2mqtt/#": auth.WriteOnly}},
		ObserverUser: {Password: ObserverPassword, ACL: auth.Filters{"nha068/presence/#": auth.ReadOnly, "nha068/command/#": auth.ReadOnly, "zigbee2mqtt/#": auth.ReadOnly}},
	}, ACL: auth.ACLRules{{Filters: auth.Filters{"#": auth.Deny}}}}
	if err := s.AddHook(new(auth.Hook), &auth.Options{Ledger: ledger}); err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	counter := &publishCounter{byClient: make(map[string]int), sentTopics: make(map[string][]string), pending: make(map[publicationID]*publication)}
	if err := s.AddHook(counter, nil); err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	for _, hook := range hooks {
		if err := s.AddHook(hook, nil); err != nil {
			_ = ln.Close()
			t.Fatal(err)
		}
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
	Client            *paho.Client
	Conn              net.Conn
	Ack               *paho.Connack
	publicationPermit *publicationPermit
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
	// Mochi v2.7.9 server.go:458-460 registers the exact broker client before
	// CONNACK. Match the TCP endpoint too: a concurrent takeover may have
	// replaced the textual client ID between CONNACK and this lookup.
	brokerClient, exists := b.Server.Clients.Get(clientID)
	if !exists || brokerClient == nil {
		t.Fatalf("broker connect %s: connected broker client missing during identity capture", clientID)
	}
	if brokerClient.Net.Conn == nil || brokerClient.Net.Remote != conn.LocalAddr().String() || brokerClient.Closed() {
		t.Fatalf("broker connect %s: connection identity changed during capture (possible concurrent takeover): expected_remote=%s actual_remote=%s closed=%t", clientID, conn.LocalAddr(), brokerClient.Net.Remote, brokerClient.Closed())
	}
	permit := &publicationPermit{owner: b.publishes, client: client, id: publicationID{client: brokerClient}, state: publicationPermitAvailable}
	return &Peer{Client: client, Conn: conn, Ack: ack, publicationPermit: permit}
}

func (b *Broker) Publish(t *testing.T, topic string, payload []byte, retained bool) {
	t.Helper()
	peer := b.Connect(t, ProducerUser, ProducerPassword, fmt.Sprintf("nha068-producer-%d", b.sequence.Add(1)), true, 0, nil)
	defer func() { _ = peer.Client.Disconnect(&paho.Disconnect{ReasonCode: 0}) }()
	publication, err := b.preparePublication(peer)
	if err != nil {
		t.Fatal(err)
	}
	defer publication.release()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := publication.send(ctx, topic, payload, retained); err != nil {
		t.Fatal(err)
	}
}

type sessionBoundary uint8

const (
	sessionBoundaryUnspecified sessionBoundary = iota
	sessionOffline
	sessionRemoved
)

// WaitOfflineSession witnesses that the previously configured persistent
// session is closed before an offline publication. Local Done alone cannot.
func (b *Broker) WaitOfflineSession(t *testing.T, clientID string) {
	t.Helper()
	b.waitSession(t, clientID, sessionOffline)
}

// WaitSessionRemoved runs before a successor connects. In Mochi v2.7.9
// server.go:485-491, OnDisconnect precedes unsubscribe and Clients.Delete.
func (b *Broker) WaitSessionRemoved(t *testing.T, clientID string) {
	t.Helper()
	b.waitSession(t, clientID, sessionRemoved)
}

func (b *Broker) waitSession(t *testing.T, clientID string, boundary sessionBoundary) {
	t.Helper()
	if b == nil || b.Server == nil || clientID == "" {
		t.Fatal("invalid broker session boundary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		client, exists := b.Server.Clients.Get(clientID)
		closed := exists && client.Closed()
		switch boundary {
		case sessionOffline:
			if closed {
				return
			}
		case sessionRemoved:
			if !exists {
				return
			}
		case sessionBoundaryUnspecified:
			t.Fatal("unspecified broker session boundary")
		default:
			t.Fatal("invalid broker session boundary")
		}
		select {
		case <-poll.C:
		case <-ctx.Done():
			t.Fatalf("broker session boundary %d not reached: exists=%t closed=%t", boundary, exists, closed)
		}
	}
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

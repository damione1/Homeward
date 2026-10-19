package testbroker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/paho"
	mochi "github.com/mochi-mqtt/server/v2"
	mochipackets "github.com/mochi-mqtt/server/v2/packets"
)

// Mochi v2.7.9 clients.go:642 invokes OnPacketSent after writing the PUBACK;
// server.go:952-966 cannot reach fanout until this hook returns. Nothing here
// replaces processPublish, a provider method, or the real TCP connection.
type nha182FanoutGate struct {
	mochi.HookBase
	clientID      string
	sent          chan struct{}
	release       chan struct{}
	published     chan struct{}
	sentOnce      sync.Once
	releaseOnce   sync.Once
	publishedOnce sync.Once
}

func newNHA182FanoutGate(t *testing.T, clientID string) *nha182FanoutGate {
	t.Helper()
	g := &nha182FanoutGate{clientID: clientID, sent: make(chan struct{}), release: make(chan struct{}), published: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

func (*nha182FanoutGate) ID() string { return "nha182-fanout-gate" }
func (*nha182FanoutGate) Provides(event byte) bool {
	return event == mochi.OnPacketSent || event == mochi.OnPublished
}
func (g *nha182FanoutGate) OnPacketSent(client *mochi.Client, packet mochipackets.Packet, _ []byte) {
	if client.ID == g.clientID && packet.FixedHeader.Type == mochipackets.Puback {
		g.sentOnce.Do(func() { close(g.sent) })
		<-g.release
	}
}
func (g *nha182FanoutGate) OnPublished(client *mochi.Client, _ mochipackets.Packet) {
	if client.ID == g.clientID {
		g.publishedOnce.Do(func() { close(g.published) })
	}
}
func (g *nha182FanoutGate) open() { g.releaseOnce.Do(func() { close(g.release) }) }

// Two fixed slots hold different TCP connections with the same MQTT client ID.
// Selecting by the broker pointer lets the old fanout finish independently of
// the still-suspended successor, without a client-ID-wide release or history.
type nha182ConnectionFanoutGate struct {
	mochi.HookBase
	mu      sync.Mutex
	clients [2]*mochi.Client
	gates   [2]*nha182FanoutGate
}

func (*nha182ConnectionFanoutGate) ID() string { return "nha182-connection-fanout-gate" }
func (*nha182ConnectionFanoutGate) Provides(event byte) bool {
	return event == mochi.OnPacketSent || event == mochi.OnPublished
}
func (g *nha182ConnectionFanoutGate) bind(t *testing.T, slot int, peer *Peer) {
	t.Helper()
	if slot < 0 || slot >= len(g.clients) || peer == nil || peer.publicationPermit == nil || peer.publicationPermit.id.client == nil {
		t.Fatal("invalid connection fanout gate binding")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.clients[slot] != nil || g.gates[slot] == nil {
		t.Fatal("connection fanout gate slot is unavailable")
	}
	g.clients[slot] = peer.publicationPermit.id.client
}
func (g *nha182ConnectionFanoutGate) gate(client *mochi.Client) *nha182FanoutGate {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, bound := range g.clients {
		if bound != nil && bound == client {
			return g.gates[i]
		}
	}
	return nil
}
func (g *nha182ConnectionFanoutGate) OnPacketSent(client *mochi.Client, packet mochipackets.Packet, encoded []byte) {
	if gate := g.gate(client); gate != nil {
		gate.OnPacketSent(client, packet, encoded)
	}
}
func (g *nha182ConnectionFanoutGate) OnPublished(client *mochi.Client, packet mochipackets.Packet) {
	if gate := g.gate(client); gate != nil {
		gate.OnPublished(client, packet)
	}
}

func nha182Wait(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatalf("missing witness: %s", name)
	}
}

func nha182Result(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("publication did not join")
		return nil
	}
}

func nha182Packet(t *testing.T, packets <-chan *paho.Publish, payload string) {
	t.Helper()
	select {
	case packet := <-packets:
		if packet == nil || string(packet.Payload) != payload || packet.Retain {
			t.Fatalf("live fanout packet = %#v, want payload %q and RETAIN=false", packet, payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("broker fanout did not reach isolated subscriber")
	}
}

func nha182Pending(b *Broker) int {
	b.publishes.mu.Lock()
	defer b.publishes.mu.Unlock()
	return len(b.publishes.pending)
}

func nha182RejectPeerReuse(t *testing.T, broker *Broker, peers ...*Peer) {
	t.Helper()
	for _, peer := range peers {
		unexpected, err := broker.preparePublication(peer)
		if unexpected != nil {
			unexpected.release()
		}
		var failure publicationError
		if !errors.Is(err, errPublicationPeerUsed) || !errors.As(err, &failure) || failure.stage != publicationAdmission {
			t.Fatalf("used peer admitted another publication witness: %v", err)
		}
	}
}

func nha182Launch(t *testing.T, gate *nha182FanoutGate, cancel context.CancelFunc, run func() error) <-chan error {
	t.Helper()
	returned := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		returned <- run()
	}()
	t.Cleanup(func() {
		cancel()
		gate.open()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("publication goroutine survived cleanup")
		}
	})
	return returned
}

func TestNHA182PUBACKOnlyReturnsBeforeFanout(t *testing.T) {
	for _, retained := range []bool{false, true} {
		t.Run(fmt.Sprintf("retained=%t", retained), func(t *testing.T) {
			const clientID = "nha182-old-producer"
			gate := newNHA182FanoutGate(t, clientID)
			broker := start(t, gate)
			// Register after broker cleanup so a failing assertion always releases
			// the hook before Server.Close joins the broker's client goroutine.
			t.Cleanup(gate.open)
			observer := broker.Connect(t, ObserverUser, ObserverPassword, "nha182-observer", true, 0, nil)
			packets := observer.Subscribe(t, "zigbee2mqtt/nha182/state")
			producer := broker.Connect(t, ProducerUser, ProducerPassword, clientID, true, 0, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			returned := nha182Launch(t, gate, cancel, func() error {
				response, err := producer.Client.Publish(ctx, &paho.Publish{Topic: "zigbee2mqtt/nha182/state", Payload: []byte("old"), QoS: 1, Retain: retained})
				if err == nil && (response == nil || response.ReasonCode >= 0x80) {
					err = errPublicationRejected
				}
				// This is precisely the old helper's completion boundary.
				_ = producer.Client.Disconnect(&paho.Disconnect{ReasonCode: 0})
				return err
			})
			nha182Wait(t, gate.sent, "PUBACK written, fanout suspended")
			if err := nha182Result(t, returned); err != nil {
				t.Fatal(err)
			}
			select {
			case <-gate.published:
				t.Fatal("fanout finished while its broker hook was suspended")
			default:
			}
			select {
			case packet := <-packets:
				t.Fatalf("packet arrived before fanout: %#v", packet)
			default:
			}
			t.Logf("old helper returned after PUBACK while real TCP fanout remained suspended (retained=%t)", retained)
			gate.open()
			nha182Wait(t, gate.published, "broker fanout finished")
			nha182Packet(t, packets, "old")
		})
	}
}

func TestNHA182PublicationWaitsForItsOwnFanout(t *testing.T) {
	for _, retained := range []bool{false, true} {
		t.Run(fmt.Sprintf("retained=%t", retained), func(t *testing.T) {
			const clientID = "nha182-tracked-producer"
			gate := newNHA182FanoutGate(t, clientID)
			broker := start(t, gate)
			t.Cleanup(gate.open)
			observer := broker.Connect(t, ObserverUser, ObserverPassword, "nha182-observer", true, 0, nil)
			packets := observer.Subscribe(t, "zigbee2mqtt/nha182/state")
			producer := broker.Connect(t, ProducerUser, ProducerPassword, clientID, true, 0, nil)
			publication, err := broker.preparePublication(producer)
			if err != nil {
				t.Fatal(err)
			}
			defer publication.release()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			returned := nha182Launch(t, gate, cancel, func() error { return publication.send(ctx, "zigbee2mqtt/nha182/state", []byte("tracked"), retained) })
			nha182Wait(t, gate.sent, "PUBACK written")
			nha182Wait(t, publication.acknowledged, "PUBACK accepted by the producer")
			// A second real producer must complete without releasing this one.
			broker.Publish(t, "zigbee2mqtt/nha182/other", []byte("other"), retained)
			if pending := nha182Pending(broker); pending != 1 {
				t.Fatalf("pending witnesses = %d, want only suspended producer", pending)
			}
			select {
			case err := <-returned:
				t.Fatalf("publication returned before its fanout: %v", err)
			default:
			}
			gate.open()
			if err := nha182Result(t, returned); err != nil {
				t.Fatal(err)
			}
			nha182Wait(t, gate.published, "matching fanout finished")
			nha182Packet(t, packets, "tracked")
			if pending := nha182Pending(broker); pending != 0 {
				t.Fatalf("completed publication retained %d witnesses", pending)
			}
			if err := publication.send(ctx, "zigbee2mqtt/nha182/state", nil, false); !errors.Is(err, errInvalidPublication) {
				t.Fatalf("single-use publication accepted reuse: %v", err)
			}
		})
	}
}

func TestNHA182CancelledFanoutReleasesWitness(t *testing.T) {
	const clientID = "nha182-cancelled-producer"
	gate := newNHA182FanoutGate(t, clientID)
	broker := start(t, gate)
	t.Cleanup(gate.open)
	producer := broker.Connect(t, ProducerUser, ProducerPassword, clientID, true, 0, nil)
	producerCopy := *producer
	publication, err := broker.preparePublication(producer)
	if err != nil {
		t.Fatal(err)
	}
	defer publication.release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := nha182Launch(t, gate, cancel, func() error { return publication.send(ctx, "zigbee2mqtt/nha182/state", []byte("cancelled"), false) })
	nha182Wait(t, publication.acknowledged, "accepted PUBACK before cancellation")
	cancel()
	err = nha182Result(t, returned)
	var failure publicationError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &failure) || failure.stage != publicationFanout {
		t.Fatalf("cancelled fanout failure = %v", err)
	}
	if pending := nha182Pending(broker); pending != 0 {
		t.Fatalf("cancelled publication retained %d witnesses", pending)
	}
	nha182RejectPeerReuse(t, broker, producer, &producerCopy)
	gate.open()
	nha182Wait(t, gate.published, "cancelled publication broker goroutine joined")
}

func TestNHA182ReusedClientIDCannotReleaseSuccessorFanout(t *testing.T) {
	for _, retained := range []bool{false, true} {
		t.Run(fmt.Sprintf("retained=%t", retained), func(t *testing.T) {
			const clientID = "nha182-reused-producer"
			oldGate := newNHA182FanoutGate(t, clientID)
			successorGate := newNHA182FanoutGate(t, clientID)
			gate := &nha182ConnectionFanoutGate{gates: [2]*nha182FanoutGate{oldGate, successorGate}}
			broker := start(t, gate)
			t.Cleanup(func() { oldGate.open(); successorGate.open() })
			observer := broker.Connect(t, ObserverUser, ObserverPassword, "nha182-reuse-observer", true, 0, nil)
			packets := observer.Subscribe(t, "zigbee2mqtt/nha182/state")
			old := broker.Connect(t, ProducerUser, ProducerPassword, clientID, true, 0, nil)
			gate.bind(t, 0, old)
			oldPublication, err := broker.preparePublication(old)
			if err != nil {
				t.Fatal(err)
			}
			defer oldPublication.release()
			oldContext, cancelOld := context.WithCancel(context.Background())
			defer cancelOld()
			oldReturned := nha182Launch(t, oldGate, cancelOld, func() error {
				return oldPublication.send(oldContext, "zigbee2mqtt/nha182/state", []byte("old"), retained)
			})
			nha182Wait(t, oldGate.sent, "old connection PUBACK written before fanout")
			nha182Wait(t, oldPublication.acknowledged, "old connection PUBACK accepted")
			cancelOld()
			var failure publicationError
			if err := nha182Result(t, oldReturned); !errors.Is(err, context.Canceled) || !errors.As(err, &failure) || failure.stage != publicationFanout {
				t.Fatalf("old connection cancellation = %v", err)
			}
			if pending := nha182Pending(broker); pending != 0 {
				t.Fatalf("old connection retained %d witnesses", pending)
			}
			successor := broker.Connect(t, ProducerUser, ProducerPassword, clientID, true, 0, nil)
			gate.bind(t, 1, successor)
			oldClient, successorClient := old.publicationPermit.id.client, successor.publicationPermit.id.client
			if oldClient == successorClient || oldClient.ID != successorClient.ID {
				t.Fatal("test requires different broker connections with the same MQTT client ID")
			}
			successorPublication, err := broker.preparePublication(successor)
			if err != nil {
				t.Fatal(err)
			}
			defer successorPublication.release()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			returned := nha182Launch(t, successorGate, cancel, func() error {
				return successorPublication.send(ctx, "zigbee2mqtt/nha182/state", []byte("successor"), retained)
			})
			nha182Wait(t, successorGate.sent, "successor PUBACK written, its fanout suspended")
			nha182Wait(t, successorPublication.acknowledged, "successor PUBACK accepted")
			select {
			case <-oldGate.published:
				t.Fatal("old connection fanout escaped its gate")
			default:
			}
			oldGate.open()
			nha182Wait(t, oldGate.published, "old connection fanout completed while successor is suspended")
			nha182Packet(t, packets, "old")
			select {
			case <-successorPublication.fanout:
				t.Fatal("old connection released the successor's publication witness")
			default:
			}
			if pending := nha182Pending(broker); pending != 1 {
				t.Fatalf("successor pending witnesses = %d, want 1", pending)
			}
			select {
			case err := <-returned:
				t.Fatalf("successor returned before its own fanout: %v", err)
			default:
			}
			successorGate.open()
			if err := nha182Result(t, returned); err != nil {
				t.Fatal(err)
			}
			nha182Wait(t, successorGate.published, "successor's own fanout completed")
			nha182Packet(t, packets, "successor")
			if pending := nha182Pending(broker); pending != 0 {
				t.Fatalf("successor completion retained %d witnesses", pending)
			}
		})
	}
}

func TestNHA182RejectedPUBACKReleasesWitness(t *testing.T) {
	broker := Start(t)
	producer := broker.Connect(t, ObserverUser, ObserverPassword, "nha182-forbidden-producer", true, 0, nil)
	producerCopy := *producer
	publication, err := broker.preparePublication(producer)
	if err != nil {
		t.Fatal(err)
	}
	defer publication.release()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = publication.send(ctx, "zigbee2mqtt/nha182/state", []byte("forbidden"), false)
	var failure publicationError
	if !errors.Is(err, errPublicationRejected) || !errors.As(err, &failure) || failure.stage != publicationPUBACK {
		t.Fatalf("rejected publication failure = %v", err)
	}
	joined, ok := failure.cause.(interface{ Unwrap() []error })
	if !ok || len(joined.Unwrap()) != 2 || joined.Unwrap()[0] != errPublicationRejected || joined.Unwrap()[1] == nil {
		t.Fatalf("rejected PUBACK did not retain its Paho cause: %v", err)
	}
	t.Logf("rejected PUBACK retained Paho cause: %v", joined.Unwrap()[1])
	if pending := nha182Pending(broker); pending != 0 {
		t.Fatalf("rejected publication retained %d witnesses", pending)
	}
	nha182RejectPeerReuse(t, broker, producer, &producerCopy)
}

type nha182NilContext struct{}

func (*nha182NilContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (*nha182NilContext) Done() <-chan struct{}       { return nil }
func (*nha182NilContext) Err() error                  { return nil }
func (*nha182NilContext) Value(any) any               { return nil }

func TestNHA182PublicationAdmissionAndCapacity(t *testing.T) {
	var zero publication
	var typedNil context.Context = (*nha182NilContext)(nil)
	for _, ctx := range []context.Context{nil, typedNil, context.Background()} {
		if err := zero.send(ctx, "zigbee2mqtt/nha182/state", nil, false); !errors.Is(err, errInvalidPublication) {
			t.Fatalf("zero publication admitted: %v", err)
		}
	}
	if !missingContext(typedNil) || missingContext(context.Background()) {
		t.Fatal("context admission confused typed nil and a valid context")
	}
	broker := Start(t)
	for _, peer := range []*Peer{nil, {}, {Client: new(paho.Client)}} {
		if _, err := broker.preparePublication(peer); !errors.Is(err, errInvalidPublication) {
			t.Fatalf("peer without a connected publication permit admitted: %v", err)
		}
	}
	var pending []*publication
	defer func() {
		for _, publication := range pending {
			publication.release()
		}
	}()
	for i := 0; i < maxPendingPublications; i++ {
		peer := broker.Connect(t, ProducerUser, ProducerPassword, fmt.Sprintf("nha182-capacity-%d", i), true, 0, nil)
		peerCopy := *peer
		publication, err := broker.preparePublication(peer)
		if err != nil {
			t.Fatal(err)
		}
		pending = append(pending, publication)
		nha182RejectPeerReuse(t, broker, peer, &peerCopy)
	}
	overflow := broker.Connect(t, ProducerUser, ProducerPassword, "nha182-overflow", true, 0, nil)
	overflowCopy := *overflow
	if _, err := broker.preparePublication(overflow); !errors.Is(err, errPublicationFull) {
		t.Fatalf("witness bound not enforced: %v", err)
	}
	if count := nha182Pending(broker); count != maxPendingPublications {
		t.Fatalf("overflow changed witness count: %d", count)
	}
	pending[0].release()
	retried, err := broker.preparePublication(&overflowCopy)
	if err != nil {
		t.Fatalf("capacity refusal consumed the unused peer permit: %v", err)
	}
	pending = append(pending, retried)
	nha182RejectPeerReuse(t, broker, overflow, &overflowCopy)
	for _, publication := range pending {
		publication.release()
	}
	nha182RejectPeerReuse(t, broker, overflow, &overflowCopy)
	if nha182Pending(broker) != 0 {
		t.Fatal("released witness map is not empty")
	}
}

func TestNHA182PeerPermitRejectsForeignBrokerAndMutatedClient(t *testing.T) {
	broker := Start(t)
	foreign := Start(t)
	peer := broker.Connect(t, ProducerUser, ProducerPassword, "nha182-permit-owner", true, 0, nil)
	if _, err := foreign.preparePublication(peer); !errors.Is(err, errInvalidPublication) {
		t.Fatalf("foreign broker admitted a peer publication permit: %v", err)
	}
	changed := *peer
	changed.Client = new(paho.Client)
	if _, err := broker.preparePublication(&changed); !errors.Is(err, errInvalidPublication) {
		t.Fatalf("mutated peer client admitted: %v", err)
	}
	publication, err := broker.preparePublication(peer)
	if err != nil {
		t.Fatalf("invalid admission consumed the legitimate permit: %v", err)
	}
	publication.release()
	nha182RejectPeerReuse(t, broker, peer)
}

type nha182DisconnectGate struct {
	mochi.HookBase
	clientID string
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
	openOnce sync.Once
}

func (*nha182DisconnectGate) ID() string               { return "nha182-disconnect-gate" }
func (*nha182DisconnectGate) Provides(event byte) bool { return event == mochi.OnDisconnect }
func (g *nha182DisconnectGate) OnDisconnect(client *mochi.Client, _ error, expire bool) {
	if client.ID == g.clientID && expire {
		g.once.Do(func() { close(g.entered) })
		<-g.release
	}
}
func (g *nha182DisconnectGate) open() { g.openOnce.Do(func() { close(g.release) }) }

func TestNHA182SessionRemovalFollowsBrokerCleanup(t *testing.T) {
	const clientID = "nha182-expiring-session"
	gate := &nha182DisconnectGate{clientID: clientID, entered: make(chan struct{}), release: make(chan struct{})}
	broker := start(t, gate)
	t.Cleanup(gate.open)
	peer := broker.Connect(t, ObserverUser, ObserverPassword, clientID, true, 0, nil)
	_ = peer.Subscribe(t, "zigbee2mqtt/nha182/state")
	if err := peer.Client.Disconnect(&paho.Disconnect{ReasonCode: 0}); err != nil {
		t.Fatal(err)
	}
	nha182Wait(t, peer.Client.Done(), "local client done")
	nha182Wait(t, gate.entered, "broker OnDisconnect before cleanup")
	if client, exists := broker.Server.Clients.Get(clientID); !exists || !client.Closed() {
		t.Fatal("OnDisconnect must not be confused with deleted broker session")
	}
	if _, exists := broker.Server.Topics.Subscribers("zigbee2mqtt/nha182/state").Subscriptions[clientID]; !exists {
		t.Fatal("test did not hold the old broker subscription before cleanup")
	}
	gate.open()
	broker.WaitSessionRemoved(t, clientID)
	if _, exists := broker.Server.Topics.Subscribers("zigbee2mqtt/nha182/state").Subscriptions[clientID]; exists {
		t.Fatal("deleted broker session retained its subscription")
	}
}

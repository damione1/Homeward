package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	planwire "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Only the disposable pilot's actual loopback TCP transport crosses this
// gate. Closing every admitted socket and refusing reconnects creates a stable
// observable outage, while the independent broker/producer remain running.
type nha114TCPFault struct {
	listener    net.Listener
	backend     string
	blocked     atomic.Bool
	mu          sync.Mutex
	connections map[net.Conn]struct{}
	workers     sync.WaitGroup
	accepted    chan struct{}
}

func nha114StartTCPFault(t *testing.T, backend string) *nha114TCPFault {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &nha114TCPFault{listener: listener, backend: backend, connections: make(map[net.Conn]struct{}), accepted: make(chan struct{})}
	go func() {
		defer close(p.accepted)
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			if p.blocked.Load() {
				client.Close()
				continue
			}
			server, err := net.DialTimeout("tcp", backend, time.Second)
			if err != nil {
				client.Close()
				continue
			}
			p.mu.Lock()
			if p.blocked.Load() {
				p.mu.Unlock()
				client.Close()
				server.Close()
				continue
			}
			p.connections[client] = struct{}{}
			p.connections[server] = struct{}{}
			p.workers.Add(1)
			p.mu.Unlock()
			go func() {
				defer p.workers.Done()
				done := make(chan struct{})
				go func() { _, _ = io.Copy(server, client); _ = server.Close(); close(done) }()
				_, _ = io.Copy(client, server)
				_ = client.Close()
				_ = server.Close()
				<-done
				p.mu.Lock()
				delete(p.connections, client)
				delete(p.connections, server)
				p.mu.Unlock()
			}()
		}
	}()
	t.Cleanup(func() { p.blocked.Store(true); _ = listener.Close(); <-p.accepted; p.cut(); p.workers.Wait() })
	return p
}
func (p *nha114TCPFault) address() string { return p.listener.Addr().String() }
func (p *nha114TCPFault) cut() {
	p.blocked.Store(true)
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.connections {
		_ = c.Close()
	}
}
func (p *nha114TCPFault) resume() { p.blocked.Store(false) }

func (f *nha114Fixture) reconnectMQTT(t *testing.T) {
	t.Helper()
	previousSession := f.waitNativeBindingSession(t, "")
	old, ok := f.broker.Server.Clients.Get(f.clientID)
	if !ok {
		t.Fatal("actual pilot connection absent")
	}
	f.mqttWire.cut()
	nha114Eventually(t, "actual network loss was not publicly visible", func() bool {
		r, err := f.observation.GetIntegrationHealth(f.ctx, nha056SessionRequest(&obs.GetIntegrationHealthRequest{IntegrationInstanceId: nha114Instance}, f.token))
		if err != nil {
			t.Fatal("authenticated integration health", err)
		}
		phase := r.Msg.Supervision.GetSupervised().GetPhase()
		return phase == obs.PilotPhase_PILOT_PHASE_RETRYING || phase == obs.PilotPhase_PILOT_PHASE_DEGRADED
	})
	f.mqttWire.resume()
	nha114Eventually(t, "actual MQTT transport did not reconnect cleanly", func() bool {
		next, ok := f.broker.Server.Clients.Get(f.clientID)
		return ok && next != old && !next.Closed() && next.State.Subscriptions.Len() > 0 && next.Properties.Clean && next.Properties.Props.SessionExpiryInterval == 0
	})
	// Broker readiness precedes retained discovery/state replay. Require all
	// ten native publications from the NEW actual session before resnapshot;
	// old-session state or a partially rebuilt grant cannot satisfy this cut.
	f.waitNativeBindingSession(t, previousSession)
	f.waitBindings(t)
}

func TestNHA114NetworkAndClientWatchRecoveryNeverRedispatch(t *testing.T) {
	f := nha114Start(t)
	// Cancellation before the request is sent is a separate boundary from
	// disconnecting a reader after a command packet has reached the broker.
	unsent := f.command(t, "nha114-client-cancel-before-send", nha114Aliases[2], true)
	unsentCtx, stopUnsent := context.WithCancel(f.ctx)
	stopUnsent()
	if _, err := f.capabilities.ExecuteCapability(unsentCtx, nha056SessionRequest(unsent, f.token)); connect.CodeOf(err) != connect.CodeCanceled {
		t.Fatal("already cancelled client sent an action request", err)
	}
	f.noCommands(t, 0)
	command := f.command(t, "nha114-client-network-request", nha114Aliases[2], true)
	command.ExpireTime = timestamppb.New(time.Now().Add(5 * time.Second))
	f.submit(t, command)
	packet := f.nextCommand(t)
	f.noCommands(t, 1)
	// Operations are reconciled by unary reads. Disconnect a genuine entity
	// stream while the intention is pending, then read that same operation again.
	if nha114Metadata(t, f.progress(t, command.RequestId)).Phase != api.CommandPhase_COMMAND_PHASE_DISPATCHED {
		t.Fatal("pending intention absent")
	}
	projection := f.view(t)
	watchCtx, stopWatch := context.WithCancel(f.ctx)
	opened := make(chan error, 1)
	go func() {
		watch, err := f.views.WatchEntityView(watchCtx, nha056SessionRequest(&api.WatchEntityViewRequest{Name: projection.Name, ResumeToken: projection.Resume}, f.token))
		if err != nil {
			opened <- err
			return
		}
		defer watch.Close()
		if !watch.Receive() {
			opened <- watch.Err()
			return
		}
		opened <- nil
		for watch.Receive() {
		}
		opened <- watch.Err()
	}()
	// A real observation sends headers and proves the authenticated stream opened.
	f.publish(t, nha114StateTopic(0), []byte("OFF"), nil)
	select {
	case err := <-opened:
		if err != nil {
			t.Fatal("client entity stream", err)
		}
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	stopWatch()
	select {
	case err := <-opened:
		if connect.CodeOf(err) != connect.CodeCanceled {
			t.Fatal("lost client stream did not expose cancellation", err)
		}
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	if nha114Metadata(t, f.progress(t, command.RequestId)).Phase != api.CommandPhase_COMMAND_PHASE_DISPATCHED {
		t.Fatal("reconnected reader lost same intention")
	}
	f.noCommands(t, 1)
	durableBefore := f.durableResponse(t, command.RequestId)
	f.reconnectMQTT(t)
	progress := f.progress(t, command.RequestId)
	if !bytes.Equal(f.durableResponse(t, command.RequestId), durableBefore) || nha114Metadata(t, progress).Phase != api.CommandPhase_COMMAND_PHASE_DISPATCHED {
		t.Fatal("lost binding did not preserve exact durable result and unconfirmed dispatched progress")
	}
	f.noCommands(t, 1)
	// A delayed old-session echo cannot confirm the retired action and does
	// not submit anything. A subsequent new explicit request remains usable.
	if packet.Properties == nil {
		t.Fatal("native command metadata absent")
	}
	f.publish(t, nha114StateTopic(2), []byte("ON"), packet.Properties.CorrelationData)
	f.waitSwitch(t, 2, true)
	if after := f.progress(t, command.RequestId); !bytes.Equal(f.durableResponse(t, command.RequestId), durableBefore) || nha114Metadata(t, after).Phase != api.CommandPhase_COMMAND_PHASE_DISPATCHED {
		t.Fatal("old session echo confirmed retired action")
	}
	// Session loss preserves pending progress. The real admitted deadline,
	// observed through GetOperation, is what makes this dispatched outcome
	// indeterminate; there is no arbitrary sleep or synthetic clock mutation.
	nha114Eventually(t, "lost-session action did not become indeterminate at its admitted deadline", func() bool {
		after := f.progress(t, command.RequestId)
		if !bytes.Equal(f.durableResponse(t, command.RequestId), durableBefore) {
			t.Fatal("deadline changed persisted dispatch result")
		}
		return after.Done && nha114Metadata(t, after).Phase == api.CommandPhase_COMMAND_PHASE_INDETERMINATE
	})
	duplicate, err := f.capabilities.ExecuteCapability(f.ctx, nha056SessionRequest(command, f.token))
	if err != nil || duplicate.Msg.Name != f.operationNames[command.RequestId] {
		t.Fatal("same-request durable reconciliation unavailable", err)
	}
	f.noCommands(t, 1)
	fresh := f.command(t, "nha114-explicit-after-network", nha114Aliases[2], false)
	f.submit(t, fresh)
	reply := f.nextCommand(t)
	f.publish(t, reply.Properties.ResponseTopic, []byte("OFF"), reply.Properties.CorrelationData)
	nha114Eventually(t, "new explicit command not confirmed after recovery", func() bool {
		return nha114Metadata(t, f.progress(t, fresh.RequestId)).Phase == api.CommandPhase_COMMAND_PHASE_CONFIRMED
	})
	f.noCommands(t, 2)
	f.drainAndCount(t, 2)
}

func TestNHA114PostStartupPostgresStopAndSameCatalogRecovery(t *testing.T) {
	f := nha114Start(t)
	// A live MQTT occurrence reaches the actual internal automation command
	// port while SQL is stopped, bypassing no human authentication boundary.
	// This creates a real durable I/O failure, without a synthetic SQL error.
	raw := []byte("id: nha114_database_fault\ntriggers:\n - trigger: state\n   entity_id: " + nha114Aliases[0] + "\n   from: 'off'\n   to: 'on'\nactions:\n - action: switch.turn_on\n   target: {entity_id: " + nha114Aliases[1] + "}\n   data: {}\n")
	f.applySource(t, planwire.SourceFamily_SOURCE_FAMILY_AUTOMATION, "automation.turn_on_lights_at_sunset", "nha114.db-automation", "automation.yaml", raw)
	command := f.command(t, "nha114-before-pg-stop", nha114Aliases[2], true)
	f.submit(t, command)
	f.nextCommand(t)
	f.noCommands(t, 1)
	before := f.durableResponse(t, command.RequestId)
	if err := f.cluster.Stop(); err != nil {
		t.Fatal("actual isolated PG stop", err)
	}
	f.publish(t, nha114StateTopic(0), []byte("ON"), nil)
	nha114Eventually(t, "real post-start durable failure not visible on same-UID health", func() bool { _, degraded := f.operatorHealth(t); return degraded })
	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	defer cancel()
	if _, err := f.observation.GetHealth(ctx, nha056SessionRequest(&obs.GetHealthRequest{}, f.token)); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatal("public authenticated health did not expose exact SQL unavailability", err)
	}
	f.noCommands(t, 1)
	if err := f.cluster.Restart(); err != nil {
		t.Fatal("same PG catalog restart", err)
	}
	// The PG session holding the singleton lock was actually lost. Recovery
	// intentionally drains Run and reacquires that fence; it is not automatic
	// reconnection of a poisoned singleton session or a fresh empty catalog.
	if err := f.run.join(t); err != nil {
		var release *storage.RuntimeReleaseError
		if !errors.As(err, &release) {
			t.Fatal("unexpected Run terminal failure", err)
		}
		t.Log("actual Run drained with lost singleton release confirmation")
	}
	f.startRun(t)
	f.login(t)
	f.refreshClients(t)
	f.waitBindings(t)
	after := f.progress(t, command.RequestId)
	if !bytes.Equal(before, f.durableResponse(t, command.RequestId)) || nha114Metadata(t, after).LiveProgressAvailable {
		t.Fatal("same catalog did not preserve the original durable request/result without live proof")
	}
	f.noCommands(t, 1)
	ready, degraded := f.operatorHealth(t)
	if !ready || degraded {
		t.Fatal("restarted actual Run did not reacquire durable readiness")
	}
	// Replay consults the stored record. It must never emit the command again.
	if _, err := f.capabilities.ExecuteCapability(f.ctx, nha056SessionRequest(command, f.token)); err != nil {
		t.Fatal("stored command not readable/replayable after restart", err)
	}
	f.noCommands(t, 1)
	f.drainAndCount(t, 1)
}

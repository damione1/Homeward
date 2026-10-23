package mqtt

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"reflect"
	"sync"
	"time"
	"unicode/utf8"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/observability"
	"github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.golang/paho"
)

const (
	maxPayload    = 64 * 1024
	queueCapacity = 32
	// A retained QoS0 burst is not limited by MQTT Receive Maximum. Wait for
	// consumer progress without growing the queue, then fail closed on a stall.
	observationAdmissionTimeout = 200 * time.Millisecond
	connectTimeout              = 5 * time.Second
	commandTimeout              = 2 * time.Second
)

var ErrBrokerConnection = errors.New("mqtt: broker connection failed")
var ErrSubscription = errors.New("mqtt: broker rejected subscription")
var ErrCommandWriteUncertain = errors.New("mqtt: command transport write failed; delivery is unknown and must not be retried automatically")

type ObservationClass uint8

const (
	ObservationUnspecified ObservationClass = iota
	ObservationLive
	ObservationRestored
)

// Observation carries raw payload and optional bounded packet correlation.
// A retained publication is restoration, never an edge event or command proof.
// NHA-069 decides entity mapping and value decoding.
type Observation struct {
	topic       Topic
	payload     []byte
	class       ObservationClass
	correlation correlationMetadata
}

func (o Observation) Record() (Topic, []byte, domain.Quality, error) {
	if _, err := o.topic.Value(); err != nil {
		return Topic{}, nil, domain.Quality{}, err
	}
	switch o.class {
	case ObservationLive:
		q, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		return o.topic, append([]byte(nil), o.payload...), q, nil
	case ObservationRestored:
		q, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessStale, domain.ProvenanceRestored)
		return o.topic, append([]byte(nil), o.payload...), q, nil
	case ObservationUnspecified:
		return Topic{}, nil, domain.Quality{}, ErrInvalidSettings
	default:
		return Topic{}, nil, domain.Quality{}, ErrInvalidSettings
	}
}

func (o Observation) Event() (Topic, []byte, error) {
	if _, err := o.topic.Value(); err != nil {
		return Topic{}, nil, err
	}
	switch o.class {
	case ObservationLive:
		return o.topic, append([]byte(nil), o.payload...), nil
	case ObservationRestored:
		return Topic{}, nil, ErrNotAnEvent
	case ObservationUnspecified:
		return Topic{}, nil, ErrInvalidSettings
	default:
		return Topic{}, nil, ErrInvalidSettings
	}
}

type ObservationHandler interface {
	Observe(context.Context, integrations.SessionHost, Observation) error
}

// UnmappedObservations is the explicit transport-only sink until NHA-069 adds
// discovery. It makes no Host binding and emits no automation event.
type UnmappedObservations struct{}

func (UnmappedObservations) Observe(context.Context, integrations.SessionHost, Observation) error {
	return nil
}

type CommandPublication struct {
	topic         Topic
	payload       []byte
	responseTopic Topic
	correlation   correlationMetadata
}

func NewCommandPublication(topic Topic, payload []byte) (CommandPublication, error) {
	if _, err := topic.Value(); err != nil || len(payload) == 0 || len(payload) > maxPayload {
		return CommandPublication{}, ErrInvalidTopic
	}
	return CommandPublication{topic: topic, payload: append([]byte(nil), payload...)}, nil
}

func (c CommandPublication) withResponseCausality(topic Topic, causality domain.Causality, scope correlationScope) (CommandPublication, error) {
	if _, err := topic.Value(); err != nil {
		return CommandPublication{}, err
	}
	metadata, err := encodeCorrelation(causality, scope)
	if err != nil {
		return CommandPublication{}, err
	}
	c.responseTopic, c.correlation = topic, metadata
	return c, nil
}

type delivery struct {
	packet      *paho.Publish
	observation Observation
}

type pilotPhase uint8

const (
	pilotUnspecified pilotPhase = iota
	pilotCreated
	pilotStarting
	pilotConnected
	pilotStopping
	pilotStopped
)

type Pilot struct {
	observation             *observability.Cells
	observationQueue        chan delivery
	observationConsumerDone chan struct{}
	observationInstalled    bool
	settings                Settings
	handler                 ObservationHandler
	mu                      sync.Mutex
	conn                    net.Conn
	client                  *paho.Client
	phase                   pilotPhase
	startDone               chan struct{}
	startCancel             context.CancelFunc
	admissionStop           chan struct{}
	admissionStopOnce       sync.Once
	shutdownOnce            sync.Once
	fault                   chan error
	commandGate             chan struct{}
}

func NewPilot(settings Settings, handler ObservationHandler) (*Pilot, error) {
	if _, err := settings.Instance(); err != nil || missingInterface(handler) {
		return nil, ErrInvalidSettings
	}
	return &Pilot{settings: settings, handler: handler, phase: pilotCreated, fault: make(chan error, 1), startDone: make(chan struct{}), admissionStop: make(chan struct{}), commandGate: make(chan struct{}, 1)}, nil
}

func missingInterface(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func NewPilotFactory(settings Settings, handler ObservationHandler) (integrations.PilotFactory, error) {
	if _, err := NewPilot(settings, handler); err != nil {
		return nil, err
	}
	return func() integrations.Pilot {
		return &Pilot{settings: settings, handler: handler, phase: pilotCreated, fault: make(chan error, 1), startDone: make(chan struct{}), admissionStop: make(chan struct{}), commandGate: make(chan struct{}, 1)}
	}, nil
}

func (p *Pilot) Start(ctx context.Context, host integrations.SessionHost, tasks integrations.Tasks) (retErr error) {
	if p == nil || ctx == nil || missingInterface(host) || missingInterface(tasks) {
		return ErrInvalidSettings
	}
	startCtx, startCancel := context.WithCancel(ctx)
	defer startCancel()
	p.mu.Lock()
	if p.phase != pilotCreated {
		p.mu.Unlock()
		return ErrInvalidSettings
	}
	p.phase = pilotStarting
	p.startCancel = startCancel
	p.mu.Unlock()
	defer close(p.startDone)
	if _, err := p.settings.Instance(); err != nil {
		return err
	}
	if err := startCtx.Err(); err != nil {
		return err
	}
	provider, err := host.Secrets()
	if err != nil {
		return err
	}
	if missingInterface(provider) {
		return ErrInvalidSettings
	}
	usernameValue, err := provider.Read(startCtx, p.settings.username)
	if err != nil {
		return err
	}
	passwordValue, err := provider.Read(startCtx, p.settings.password)
	if err != nil {
		return err
	}
	username, err := usernameValue.Bytes()
	if err != nil {
		return err
	}
	defer clear(username)
	password, err := passwordValue.Bytes()
	if err != nil {
		return err
	}
	defer clear(password)
	if len(username) == 0 || len(username) > 128 || !utf8.Valid(username) || hasNUL(username) || len(password) == 0 || len(password) > 65535 {
		return ErrInvalidSettings
	}
	connectCtx, cancel := context.WithTimeout(startCtx, connectTimeout)
	defer cancel()
	var conn net.Conn
	if p.settings.tls {
		conn, err = (&tls.Dialer{NetDialer: &net.Dialer{}, Config: &tls.Config{MinVersion: tls.VersionTLS12}}).DialContext(connectCtx, "tcp", p.settings.address)
	} else {
		conn, err = (&net.Dialer{}).DialContext(connectCtx, "tcp", p.settings.address)
	}
	if err != nil {
		return ErrBrokerConnection
	}
	p.mu.Lock()
	if p.phase != pilotStarting || startCtx.Err() != nil {
		p.mu.Unlock()
		_ = conn.Close()
		return context.Canceled
	}
	p.conn = conn
	p.mu.Unlock()
	// Connect's context only covers its handshake. Closing the socket is what
	// interrupts a blocked MQTT packet read or write.
	defer func() {
		if retErr != nil {
			p.stopAdmissions()
			_ = conn.Close()
			p.mu.Lock()
			joined := p.client != nil
			p.mu.Unlock()
			if joined {
				<-clientDone(p)
			}
		}
	}()
	queue := make(chan delivery, queueCapacity)
	space := make(chan struct{}, queueCapacity)
	p.mu.Lock()
	p.observationQueue = queue
	p.observationInstalled = true
	p.observationConsumerDone = make(chan struct{})
	p.mu.Unlock()
	_ = p.observation.ChangeRuntimeQueue(obs.QueueClass_QUEUE_CLASS_MQTT_INBOUND, 1, 0, queueCapacity, 0, 0)
	consumerStarted := false
	defer func() {
		if !consumerStarted {
			close(p.observationConsumerDone)
		}
	}()
	client := paho.NewClient(paho.ClientConfig{
		Conn:                       packets.NewThreadSafeConn(&packetConn{Conn: conn}),
		ClientID:                   p.settings.clientID,
		PacketTimeout:              connectTimeout,
		EnableManualAcknowledgment: true,
		OnPublishReceived: []func(paho.PublishReceived) (bool, error){func(received paho.PublishReceived) (bool, error) {
			if received.Packet == nil {
				p.fail(ErrInvalidTopic)
				return true, ErrInvalidTopic
			}
			topic, err := ParseTopic(received.Packet.Topic)
			if err != nil || len(received.Packet.Payload) > maxPayload || !p.allowedObservation(topic) {
				p.fail(ErrInvalidTopic)
				return true, ErrInvalidTopic
			}
			class := ObservationLive
			if received.Packet.Retain {
				class = ObservationRestored
			}
			var correlation correlationMetadata
			if class == ObservationLive && received.Packet.Properties != nil {
				// Invalid metadata cannot confer proof; the ordinary state is
				// still delivered. No incoming ResponseTopic is acted upon.
				correlation, _ = parseCorrelation(received.Packet.Properties.CorrelationData)
			}
			item := delivery{packet: received.Packet, observation: Observation{topic: topic, payload: append([]byte(nil), received.Packet.Payload...), class: class, correlation: correlation}}
			if err := p.admitObservation(queue, space, item); err != nil {
				return true, err
			}
			return true, nil
		}},
	})
	if err := conn.SetWriteDeadline(time.Now().Add(connectTimeout)); err != nil {
		return ErrBrokerConnection
	}
	connectWatchStop := make(chan struct{})
	connectWatchDone := make(chan struct{})
	go func() {
		defer close(connectWatchDone)
		select {
		case <-connectCtx.Done():
			_ = conn.Close()
		case <-connectWatchStop:
		}
	}()
	max := uint32(maxWirePacket)
	receive := uint16(queueCapacity)
	noOfflineSession := uint32(0)
	packet := &paho.Connect{ClientID: p.settings.clientID, Username: string(username), Password: password,
		UsernameFlag: true, PasswordFlag: true, KeepAlive: 20, CleanStart: true,
		Properties: &paho.ConnectProperties{SessionExpiryInterval: &noOfflineSession, ReceiveMaximum: &receive, MaximumPacketSize: &max},
	}
	if p.settings.mode == ModeActive {
		packet.WillMessage = &paho.WillMessage{Topic: p.settings.willTopic.value, Payload: append([]byte(nil), p.settings.willPayload...), QoS: 1, Retain: false}
	}
	ack, err := client.Connect(connectCtx, packet)
	close(connectWatchStop)
	<-connectWatchDone
	if err != nil {
		<-client.Done()
		return ErrBrokerConnection
	}
	p.mu.Lock()
	p.client = client // Only a successful CONNECT grants a live Paho client.
	if p.phase == pilotStarting {
		p.phase = pilotConnected
		_ = conn.SetWriteDeadline(time.Time{})
	}
	phase := p.phase
	p.mu.Unlock()
	if ack == nil || ack.ReasonCode != 0 || ack.SessionPresent || phase != pilotConnected {
		return ErrBrokerConnection
	}
	consumerStarted = true
	if err := tasks.Go(func(ctx context.Context) error {
		defer close(p.observationConsumerDone)
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-client.Done():
				return nil
			case item := <-queue:
				p.mu.Lock()
				_ = p.observation.ChangeRuntimeQueue(obs.QueueClass_QUEUE_CLASS_MQTT_INBOUND, 0, -1, 0, 0, 0)
				p.mu.Unlock()
				select {
				case space <- struct{}{}:
				default:
				}
				if err := p.handler.Observe(ctx, host, item.observation); err != nil {
					p.fail(err)
					return err
				}
				if ctx.Err() != nil {
					return nil
				}
				if err := client.Ack(item.packet); err != nil {
					p.fail(err)
					return err
				}
			}
		}
	}); err != nil {
		consumerStarted = false
		return err
	}
	if err := tasks.Go(func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			p.shutdown(true)
			<-client.Done()
			return nil
		case <-client.Done():
			if ctx.Err() != nil {
				return nil
			}
			select {
			case fault := <-p.fault:
				return fault
			default:
				return ErrBrokerConnection
			}
		}
	}); err != nil {
		return err
	}
	subs := make([]paho.SubscribeOptions, 0, len(p.settings.subscriptions))
	for _, filter := range p.settings.subscriptions {
		// MQTT 5 RAP=false: retained snapshot arrives with RETAIN=1, while
		// a new live publication arrives with RETAIN=0 even if stored by broker.
		subs = append(subs, paho.SubscribeOptions{Topic: filter.value, QoS: 1, RetainAsPublished: false})
	}
	subscribeCtx, subscribeCancel := context.WithTimeout(startCtx, connectTimeout)
	defer subscribeCancel()
	subscribeDeadline, _ := subscribeCtx.Deadline()
	if err := conn.SetWriteDeadline(subscribeDeadline); err != nil {
		return ErrSubscription
	}
	subscribeWatchStop := make(chan struct{})
	subscribeWatchDone := make(chan struct{})
	go func() {
		defer close(subscribeWatchDone)
		select {
		case <-subscribeCtx.Done():
			_ = conn.Close()
		case <-subscribeWatchStop:
		}
	}()
	suback, err := client.Subscribe(subscribeCtx, &paho.Subscribe{Subscriptions: subs})
	close(subscribeWatchStop)
	<-subscribeWatchDone
	p.mu.Lock()
	if p.phase == pilotConnected {
		_ = conn.SetWriteDeadline(time.Time{})
	}
	phase = p.phase
	p.mu.Unlock()
	if phase != pilotConnected {
		return ErrSubscription
	}
	if err != nil || suback == nil || len(suback.Reasons) != len(subs) {
		return ErrSubscription
	}
	for _, reason := range suback.Reasons {
		if reason >= 0x80 {
			return ErrSubscription
		}
	}
	health, _ := integrations.NewHealthReport(integrations.HealthReady, "mqtt connected")
	if err := host.ReportHealth(startCtx, health); err != nil {
		return err
	}
	return nil
}

func hasNUL(value []byte) bool {
	for _, b := range value {
		if b == 0 {
			return true
		}
	}
	return false
}

func clientDone(p *Pilot) <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.client.Done()
}

func (p *Pilot) allowedObservation(topic Topic) bool {
	for _, f := range p.settings.subscriptions {
		if f.Matches(topic) {
			return true
		}
	}
	return false
}

func (p *Pilot) fail(err error) {
	p.stopAdmissions()
	select {
	case p.fault <- err:
	default:
	}
	p.mu.Lock()
	conn := p.conn
	p.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// The callback must never wait while holding p.mu: the consumer needs it to
// account for a dequeue and signal a free slot. A timer bounds a stalled
// consumer, while shutdown wakes a waiting callback before Paho can join.
func (p *Pilot) admitObservation(queue chan delivery, space <-chan struct{}, item delivery) error {
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	expired := false
	for {
		p.mu.Lock()
		select {
		case <-p.admissionStop:
			p.mu.Unlock()
			return context.Canceled
		default:
		}
		select {
		case queue <- item:
			_ = p.observation.ChangeRuntimeQueue(obs.QueueClass_QUEUE_CLASS_MQTT_INBOUND, 0, 1, 0, 0, 0)
			p.mu.Unlock()
			return nil
		default:
		}
		if expired {
			_ = p.observation.AddQueueRejected(obs.QueueClass_QUEUE_CLASS_MQTT_INBOUND, 1)
			_ = p.observation.AddQueueLost(obs.QueueClass_QUEUE_CLASS_MQTT_INBOUND, 1)
			p.mu.Unlock()
			p.fail(ErrObservationGap)
			return ErrObservationGap
		}
		p.mu.Unlock()
		if timer == nil {
			timer = time.NewTimer(observationAdmissionTimeout)
		}
		select {
		case <-space:
			// A slot may have opened. Retry without renewing this packet's
			// 200 ms wall-clock budget, even if this signal was stale.
		case <-p.admissionStop:
			return context.Canceled
		case <-timer.C:
			expired = true
		}
	}
}

func (p *Pilot) stopAdmissions() {
	p.admissionStopOnce.Do(func() {
		if p.admissionStop != nil {
			close(p.admissionStop)
		}
	})
}

func (p *Pilot) shutdown(graceful bool) {
	p.stopAdmissions()
	p.shutdownOnce.Do(func() {
		p.mu.Lock()
		p.phase = pilotStopping
		conn, client := p.conn, p.client
		p.mu.Unlock()
		if conn == nil {
			return
		}
		if graceful && client != nil {
			select {
			case p.commandGate <- struct{}{}:
				// A single writer owns the deadline. If one is in flight, a
				// forced close preserves its uncertainty and ends it promptly.
				_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
				watchDone := make(chan struct{})
				watch := time.AfterFunc(time.Second, func() { _ = conn.Close(); close(watchDone) })
				_ = client.Disconnect(&paho.Disconnect{ReasonCode: 0})
				if !watch.Stop() {
					<-watchDone
				}
				<-p.commandGate
			default:
			}
		}
		_ = conn.Close()
	})
}

func (p *Pilot) Stop(ctx context.Context) error {
	if p == nil || ctx == nil {
		return ErrInvalidSettings
	}
	p.mu.Lock()
	phase := p.phase
	startCancel := p.startCancel
	if phase == pilotCreated {
		p.phase = pilotStopped
		close(p.startDone)
	}
	p.mu.Unlock()
	if startCancel != nil {
		startCancel()
	}
	if phase == pilotUnspecified {
		return ErrInvalidSettings
	}
	if phase == pilotCreated {
		return p.joinObservationQueue(ctx)
	}
	p.shutdown(true)
	// A physical write may have been uncertain when the socket closed. Join it
	// before returning; no command writer survives the pilot's Stop boundary.
	select {
	case p.commandGate <- struct{}{}:
		<-p.commandGate
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-p.startDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	p.mu.Lock()
	client := p.client
	p.mu.Unlock()
	if client == nil {
		p.mu.Lock()
		p.phase = pilotStopped
		p.mu.Unlock()
		return p.joinObservationQueue(ctx)
	}
	select {
	case <-client.Done():
		p.mu.Lock()
		p.phase = pilotStopped
		p.mu.Unlock()
		return p.joinObservationQueue(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WriteCommand is the only physical-command publication path. QoS0 and no
// outbound queue mean an uncertain network write is never replayed by this
// transport. Success is transport acceptance, never device confirmation.
func (p *Pilot) WriteCommand(ctx context.Context, command CommandPublication) error {
	if p == nil || ctx == nil {
		return ErrInvalidSettings
	}
	if _, err := p.settings.Instance(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.settings.mode != ModeActive {
		return ErrPassiveMode
	}
	if _, err := command.topic.Value(); err != nil || len(command.payload) == 0 || len(command.payload) > maxPayload {
		return ErrInvalidTopic
	}
	allowed := false
	for _, f := range p.settings.commands {
		if f.Matches(command.topic) {
			allowed = true
			break
		}
	}
	if !allowed {
		return ErrInvalidTopic
	}
	var properties *paho.PublishProperties
	if command.correlation.wire != "" || command.responseTopic != (Topic{}) {
		if _, err := command.correlation.causality(); err != nil {
			return err
		}
		if !p.allowedObservation(command.responseTopic) {
			return ErrInvalidTopic
		}
		properties = &paho.PublishProperties{ResponseTopic: command.responseTopic.value, CorrelationData: []byte(command.correlation.wire)}
	}
	select {
	case p.commandGate <- struct{}{}:
		defer func() { <-p.commandGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	client, conn, phase := p.client, p.conn, p.phase
	p.mu.Unlock()
	if client == nil || conn == nil || phase != pilotConnected {
		return ErrNotConnected
	}
	select {
	case <-client.Done():
		return ErrNotConnected
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	deadline, _ := writeCtx.Deadline()
	if err := conn.SetWriteDeadline(deadline); err != nil {
		p.fail(err)
		return ErrCommandWriteUncertain
	}
	watchStop := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-writeCtx.Done():
			_ = conn.Close()
		case <-watchStop:
		}
	}()
	_, err := client.Publish(writeCtx, &paho.Publish{Topic: command.topic.value, Payload: append([]byte(nil), command.payload...), QoS: 0, Retain: false, Properties: properties})
	close(watchStop)
	<-watchDone
	p.mu.Lock()
	if p.phase == pilotConnected {
		_ = conn.SetWriteDeadline(time.Time{})
	}
	p.mu.Unlock()
	if err != nil || writeCtx.Err() != nil {
		p.fail(ErrCommandWriteUncertain)
		return ErrCommandWriteUncertain
	}
	return nil
}

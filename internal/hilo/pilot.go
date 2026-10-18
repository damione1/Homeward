package hilo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/heatingownership"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
)

// AuthorizationBinder is Run's NHA-177 registration view. It cannot invoke a
// login; only an explicit trusted authorization input can do that.
type AuthorizationBinder interface {
	Bind(*integrations.CloudClient) error
	Unbind(*integrations.CloudClient)
}

type Pilot struct {
	settings   Settings
	repository *registry.Repository
	decision   heatingownership.Decision
	catalog    *contracts.CapabilityCatalog
	policy     *integrations.CloudPolicy
	quota      *storage.CloudAttemptQuota
	binder     AuthorizationBinder
	leases     *LeaseRegistry
	mu         sync.Mutex
	client     *integrations.CloudClient
	bound      bool
	bindings   map[string]integrations.Binding
	devices    map[int]hubDevice
	states     map[int]deviceReading
	sseTimes   map[int]time.Time
	hubTimes   map[hubTimeKey]time.Time
	generation uint64
	sseFull    bool
}

func NewPilotFactory(settings Settings, repository *registry.Repository, decision heatingownership.Decision, catalog *contracts.CapabilityCatalog, policy *integrations.CloudPolicy, quota *storage.CloudAttemptQuota, binder AuthorizationBinder, leases *LeaseRegistry) (integrations.PilotFactory, error) {
	if !settings.Valid() || repository == nil || catalog == nil || policy == nil || quota == nil || nilInterface(binder) || leases == nil || decision.HiloActionPolicy() != heatingownership.ActionDenied {
		return nil, ErrSettings
	}
	return func() integrations.Pilot {
		return &Pilot{settings: settings, repository: repository, decision: decision, catalog: catalog, policy: policy, quota: quota, binder: binder, leases: leases, bindings: make(map[string]integrations.Binding), devices: make(map[int]hubDevice), states: make(map[int]deviceReading), sseTimes: make(map[int]time.Time), hubTimes: make(map[hubTimeKey]time.Time)}
	}, nil
}

func (p *Pilot) Start(ctx context.Context, host integrations.SessionHost, tasks integrations.Tasks) error {
	if p == nil || nilInterface(ctx) || nilInterface(host) || nilInterface(tasks) {
		return ErrSettings
	}
	client, err := integrations.NewCloudClient(host, p.policy, nil, p.quota)
	if err != nil {
		return err
	}
	if err := p.binder.Bind(client); err != nil {
		client.Close()
		return err
	}
	p.mu.Lock()
	p.client, p.bound = client, true
	p.mu.Unlock()
	if err := tasks.Go(func(taskCtx context.Context) error { p.run(taskCtx, host); return nil }); err != nil {
		p.binder.Unbind(client)
		client.Close()
		return err
	}
	return nil
}

func (p *Pilot) Stop(context.Context) error {
	if p == nil {
		return nil
	}
	p.leases.Revoke()
	p.mu.Lock()
	client, bound := p.client, p.bound
	p.client, p.bound = nil, false
	p.mu.Unlock()
	if client != nil {
		if bound {
			p.binder.Unbind(client)
		}
		client.Close()
	}
	return nil
}

func (p *Pilot) run(ctx context.Context, host integrations.SessionHost) {
	// HA resets to five seconds after hub.run returns cleanly. The common
	// CloudObservationStream reports remote EOF/close as a terminal transport
	// failure, so an unexpected exit here keeps the bounded error backoff.
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		err := p.cycle(ctx, host)
		p.leases.Revoke() // fence before unavailable after every exit
		if unavailableErr := p.markUnavailable(ctx, host); unavailableErr != nil {
			// A missing unavailable admission leaves the old Host report visible.
			// Never start another snapshot that could bridge that unknown gap.
			p.health(ctx, host, integrations.HealthDegraded, "hilo unavailable report not admitted")
			return
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			p.health(ctx, host, integrations.HealthDegraded, "hilo observation stream unavailable")
		}
		wait := backoff
		if errors.Is(err, ErrReauthorizationRequired) {
			wait = 5 * time.Minute
		}
		var cloud *integrations.CloudError
		if errors.As(err, &cloud) {
			if retryAt, ok := cloud.RetryAt(); ok && time.Until(retryAt) > wait {
				wait = time.Until(retryAt)
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-timer.C:
		}
		if backoff < 300*time.Second {
			backoff *= 2
			if backoff > 300*time.Second {
				backoff = 300 * time.Second
			}
		}
	}
}

func (p *Pilot) health(ctx context.Context, host integrations.SessionHost, status integrations.HealthStatus, detail string) {
	report, err := integrations.NewHealthReport(status, detail)
	if err == nil {
		_ = host.ReportHealth(ctx, report)
	}
}

func (p *Pilot) observe(ctx context.Context, path string, token credentialJSON) (integrations.CloudResponse, error) {
	request, err := integrations.NewCloudRequest(http.MethodGet, p.settings.APIEndpoint(path), bearerHeaders(token, true), nil)
	if err != nil {
		return integrations.CloudResponse{}, ErrProviderShape
	}
	return p.client.Observe(ctx, request)
}

type streamEvent struct {
	kind    integrations.CloudStreamKind
	message integrations.CloudStreamMessage
	frames  []hubFrame
	err     error
}

type hubTimeKey struct {
	deviceID  int
	attribute hubAttribute
}

type cycleFence struct {
	mu         sync.Mutex
	leases     *LeaseRegistry
	cancel     context.CancelFunc
	generation uint64
	lost       bool
	err        error
}

func (f *cycleFence) activate() (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lost {
		return 0, f.err
	}
	generation, err := f.leases.Activate()
	if err == nil {
		f.generation = generation
	}
	return generation, err
}

func (f *cycleFence) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lost {
		return
	}
	if err == nil {
		err = ErrProviderShape
	}
	f.lost, f.err = true, err
	if f.generation != 0 {
		f.leases.RevokeGeneration(f.generation)
	}
	f.cancel()
}

func (f *cycleFence) failure() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *cycleFence) close() {
	f.mu.Lock()
	if f.generation != 0 {
		f.leases.RevokeGeneration(f.generation)
	}
	f.cancel()
	f.mu.Unlock()
}

func pumpStream(ctx context.Context, stream *integrations.CloudObservationStream, kind integrations.CloudStreamKind, events chan<- streamEvent, fence *cycleFence) {
	for ctx.Err() == nil {
		message, err := stream.Receive(ctx)
		if err != nil {
			if ctx.Err() == nil {
				fence.fail(err)
			}
			select {
			case events <- streamEvent{kind: kind, err: err}:
			case <-ctx.Done():
			}
			return
		}
		var frames []hubFrame
		if kind == integrations.CloudStreamWebSocket {
			messageKind, e := message.Kind()
			if e != nil || messageKind != integrations.CloudStreamWebSocketText {
				fence.fail(ErrProviderShape)
				select {
				case events <- streamEvent{kind: kind, err: ErrProviderShape}:
				case <-ctx.Done():
				}
				return
			}
			data, e := message.Bytes()
			if e != nil {
				fence.fail(e)
				select {
				case events <- streamEvent{kind: kind, err: e}:
				case <-ctx.Done():
				}
				return
			}
			frames, e = decodeHubFrames(data)
			if e != nil {
				fence.fail(e)
				select {
				case events <- streamEvent{kind: kind, err: e}:
				case <-ctx.Done():
				}
				return
			}
		}
		select {
		case events <- streamEvent{kind: kind, message: message, frames: frames}:
		case <-ctx.Done():
			return
		default:
			// Overflow is delivery loss; never turn it into a silently stale cache.
			fence.fail(ErrProviderShape)
			select {
			case events <- streamEvent{kind: kind, err: ErrProviderShape}:
			case <-ctx.Done():
			}
			return
		}
	}
}

func (p *Pilot) cycle(ctx context.Context, host integrations.SessionHost) error {
	p.leases.Revoke()
	cycleCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	fence := &cycleFence{leases: p.leases, cancel: cancel}
	token, err := liveCredential(cycleCtx, p.client, p.settings.credential)
	if err != nil {
		return err
	}
	locations, err := p.observe(cycleCtx, "/Automation/v1/api/Locations", token)
	if err != nil {
		return err
	}
	if err := decodeLocations(locations.Bytes(), p.settings.locationID, p.settings.locationHiloID); err != nil {
		return err
	}
	ws, pending, err := p.openDeviceHub(cycleCtx, token)
	if err != nil {
		return err
	}
	defer ws.Close(context.Background())
	pingDone := make(chan struct{})
	go pingHub(cycleCtx, ws, fence.fail, pingDone)
	defer func() { cancel(); <-pingDone }()
	inventory, pending, err := p.awaitInventory(cycleCtx, ws, pending)
	if err != nil {
		return err
	}
	snapshot, snapshotResponse, err := graphqlSnapshot(cycleCtx, p.client, p.settings, token)
	if err != nil {
		return err
	}
	sse, err := openGraphQLStream(cycleCtx, p.client, p.settings, token, p.sseFull)
	if err != nil {
		return err
	}
	defer sse.Close(context.Background())
	events := make(chan streamEvent, 32)
	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		pumpStream(cycleCtx, ws, integrations.CloudStreamWebSocket, events, fence)
	}()
	go func() { defer readers.Done(); pumpStream(cycleCtx, sse, integrations.CloudStreamSSE, events, fence) }()
	defer func() { fence.close(); readers.Wait() }()
	if err := p.installInventory(inventory); err != nil {
		return err
	}
	if err := p.discover(cycleCtx, host); err != nil {
		return err
	}
	generation, err := fence.activate()
	if err != nil {
		return err
	}
	p.generation = generation
	if err := p.publishSnapshot(cycleCtx, host, snapshot); err != nil {
		return err
	}
	if len(pending) != 0 {
		if err := p.applyHubEvent(cycleCtx, host, pending); err != nil {
			return err
		}
	}
	if err := p.client.ConfirmObservationAdmitted(cycleCtx, snapshotResponse); err != nil {
		return err
	}
	p.health(cycleCtx, host, integrations.HealthReady, "hilo observations admitted")
	readers.Add(1)
	// Auxiliary GETs have their own cadence, so a slow weather response cannot
	// delay processing a stream loss and revoking the energy lease.
	go func() {
		defer readers.Done()
		p.pollAuxiliary(cycleCtx, host)
		ticker := time.NewTicker(1800 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-cycleCtx.Done():
				return
			case <-ticker.C:
				p.pollAuxiliary(cycleCtx, host)
			}
		}
	}()
	for cycleCtx.Err() == nil {
		select {
		case <-cycleCtx.Done():
			return fence.failure()
		case event := <-events:
			if event.err != nil {
				return event.err
			}
			if event.kind == integrations.CloudStreamWebSocket {
				if err := p.applyHubEvent(cycleCtx, host, event.frames); err != nil {
					return err
				}
			} else {
				if err := p.applySSEEvent(cycleCtx, host, event.message); err != nil {
					return err
				}
			}
		}
	}
	return fence.failure()
}

func (p *Pilot) installInventory(rows []hubDevice) error {
	devices := make(map[int]hubDevice, len(rows))
	for _, row := range rows {
		devices[row.ID] = row
	}
	p.devices = devices
	p.states = make(map[int]deviceReading, len(devices))
	p.sseTimes = make(map[int]time.Time, len(devices))
	p.hubTimes = make(map[hubTimeKey]time.Time)
	return nil
}

func (p *Pilot) applyHubEvent(ctx context.Context, host integrations.SessionHost, frames []hubFrame) error {
	for _, frame := range frames {
		if frame.Type == 7 || frame.Type == 3 && frame.Error != "" {
			return ErrProviderShape
		}
		if frame.Type != 1 {
			continue
		}
		switch frame.Target {
		case "Heartbeat", "Ping", "":
			continue
		case "DeviceListInitialValuesReceived", "DeviceListUpdatedValuesReceived", "DeviceAdded", "DeviceDeleted", "DevicesListChanged":
			// A changed inventory requires a full fresh snapshot before any
			// further report. It cannot silently mutate identity or model.
			return ErrProviderShape
		case "DevicesValuesReceived", "GatewayValuesReceived":
			if len(frame.Arguments) != 1 {
				return ErrProviderShape
			}
			updates, err := decodeHubValues(frame.Arguments[0])
			if err != nil {
				return err
			}
			for _, update := range updates {
				if update.location != 0 && update.location != p.settings.locationID {
					return ErrProviderShape
				}
				id := update.deviceID
				if frame.Target == "GatewayValuesReceived" {
					for key, device := range p.devices {
						if device.Type == "Gateway" || device.Type == "Hub" {
							id = key // provider gateway ID may replace placeholder 1
							break
						}
					}
				}
				if id == 0 {
					for key, device := range p.devices {
						if update.hiloID != "" && device.HiloID == update.hiloID || update.hiloID == "" && (device.Type == "Gateway" || device.Type == "Hub") {
							id = key
							break
						}
					}
				}
				device, known := p.devices[id]
				if !known || update.hiloID != "" && device.HiloID != update.hiloID {
					return ErrProviderShape
				}
				key := hubTimeKey{id, update.attribute}
				if prior, ok := p.hubTimes[key]; ok && !update.at.After(prior) {
					continue
				}
				reading := update.reading
				if update.attribute == hubCurrentTemperature && (device.Type == "Cee" || device.Type == "SinopeWaterHeater") {
					reading.probe, reading.ambient = reading.ambient, fieldNumber{}
				}
				if err := p.applyReading(ctx, host, id, reading, false, &update.at); err != nil {
					return err
				}
				p.hubTimes[key] = update.at
			}
		default:
			continue // no ChallengeHub, write echo, or extra subscription
		}
	}
	return nil
}

func (p *Pilot) applySSEEvent(ctx context.Context, host integrations.SessionHost, message integrations.CloudStreamMessage) error {
	data, err := message.Bytes()
	if err != nil {
		return err
	}
	return p.applyGraphQLUpdate(ctx, host, data)
}

func (p *Pilot) applyGraphQLUpdate(ctx context.Context, host integrations.SessionHost, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	id, reading, rawTime, retryAPQ, err := decodeGraphQLUpdate(data, p.settings.locationHiloID)
	if retryAPQ {
		p.sseFull = true
		return ErrProviderShape
	}
	if err != nil {
		return err
	}
	if id == "" {
		return nil
	} // SSE control event, never a fresh measurement
	var numeric int
	for key, device := range p.devices {
		if device.HiloID == id {
			numeric = key
			break
		}
	}
	if numeric == 0 {
		return ErrProviderShape
	}
	// A data event without a valid supplier transmission time cannot establish
	// a new source revision. Equal or older events are replays within this cycle.
	if rawTime == nil {
		return ErrProviderShape
	}
	at := parseSourceTime(*rawTime, time.Now().UTC())
	if at == nil {
		return ErrProviderShape
	}
	if previous, ok := p.sseTimes[numeric]; ok && !at.After(previous) {
		return nil
	}
	if err := p.applyReading(ctx, host, numeric, reading, false, at); err != nil {
		return err
	}
	p.sseTimes[numeric] = *at
	return nil
}

func (p *Pilot) publishSnapshot(ctx context.Context, host integrations.SessionHost, rawDevices []json.RawMessage) error {
	seen := make(map[int]bool, len(rawDevices))
	for _, raw := range rawDevices {
		id, reading, err := decodeGraphQLDevice(raw)
		if err != nil {
			return err
		}
		var numeric int
		for key, device := range p.devices {
			if device.HiloID == id {
				numeric = key
				break
			}
		}
		if numeric == 0 {
			kind := graphQLDeviceType(raw)
			if kind == "Gateway" || kind == "Hub" {
				for _, binding := range p.settings.bindings {
					if binding.kind == AliasGateway {
						numeric = binding.deviceID
						if _, exists := p.devices[numeric]; !exists {
							p.devices[numeric] = hubDevice{ID: numeric, HiloID: id, Type: "Gateway"}
						}
						break
					}
				}
			}
		}
		if numeric == 0 {
			continue
		} // unconfigured GraphQL device
		if seen[numeric] {
			return ErrProviderShape
		}
		seen[numeric] = true
		if err := p.applyReading(ctx, host, numeric, reading, true, nil); err != nil {
			return err
		}
	}
	for _, binding := range p.settings.bindings {
		if binding.kind.Direct() && binding.kind != AliasWeather && binding.kind != AliasNotifications && !seen[binding.deviceID] {
			if owned, ok := p.bindings[binding.alias]; ok {
				if err := p.publishValue(ctx, host, owned, endpointFor(classifyAlias(binding.alias)), nil, nil); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (p *Pilot) pollAuxiliary(ctx context.Context, host integrations.SessionHost) {
	token, credentialErr := liveCredential(ctx, p.client, p.settings.credential)
	for _, task := range []struct {
		alias, path string
		decode      func([]byte) (float64, error)
	}{
		{"sensor.notifications_hilo", "/Notifications/Notifications/Locations/" + strconv.Itoa(p.settings.locationID), decodeNotificationCount},
		{"sensor.outdoor_weather_hilo", "/Automation/v1/api/Locations/" + strconv.Itoa(p.settings.locationID) + "/Weather", decodeWeather},
	} {
		binding, ok := p.bindings[task.alias]
		if !ok {
			continue
		}
		if credentialErr != nil {
			_ = p.publishValue(ctx, host, binding, endpointFor(classifyAlias(task.alias)), nil, nil)
			continue
		}
		response, err := p.observe(ctx, task.path, token)
		if err != nil {
			_ = p.publishValue(ctx, host, binding, endpointFor(classifyAlias(task.alias)), nil, nil)
			continue
		}
		number, err := task.decode(response.Bytes())
		if err != nil {
			_ = p.publishValue(ctx, host, binding, endpointFor(classifyAlias(task.alias)), nil, nil)
			continue
		}
		if err := p.publishNumber(ctx, host, binding, endpointFor(classifyAlias(task.alias)), number, nil); err == nil {
			_ = p.client.ConfirmObservationAdmitted(ctx, response)
		}
	}
}

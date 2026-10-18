package neviweb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/registry/v1"
	statewire "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/heatingownership"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
)

// Pilot retains no alternate transport, Host, credential writer or command
// ledger. Its task remains alive through expected cloud outages and quota
// deferrals, so Supervisor restart capacity is reserved for pilot faults.
type Pilot struct {
	settings   Settings
	repository *registry.Repository
	decision   heatingownership.Decision
	catalog    *contracts.CapabilityCatalog
	policy     *integrations.CloudPolicy
	quota      *storage.CloudAttemptQuota
	mu         sync.RWMutex
	client     *integrations.CloudClient
	host       integrations.SessionHost
	bindings   map[string]integrations.Binding
	models     map[string]modelInfo
	declared   map[string]modelInfo
	listed     map[string]bool
	signatures map[string]json.RawMessage
	networkAt  time.Time
	// persisted is the last admitted encrypted credential snapshot. current
	// may carry newer response cookies during this process, but those cookies
	// have no authority after the encrypted credential changes.
	persisted  sessionState
	current    sessionState
	discovered bool
	refresh    bool
}

var ErrPartialObservation = errors.New("neviweb: provider observation has unsupported or malformed fields")

func NewPilotFactory(settings Settings, repository *registry.Repository, decision heatingownership.Decision, catalog *contracts.CapabilityCatalog, policy *integrations.CloudPolicy, quota *storage.CloudAttemptQuota) (integrations.PilotFactory, error) {
	if !settings.valid() || repository == nil || catalog == nil || policy == nil || quota == nil || decision.HeatingOwner() != heatingownership.ProviderNeviweb {
		return nil, ErrSettings
	}
	return func() integrations.Pilot {
		return &Pilot{settings: settings, repository: repository, decision: decision, catalog: catalog, policy: policy, quota: quota, bindings: make(map[string]integrations.Binding), models: make(map[string]modelInfo), declared: make(map[string]modelInfo), listed: make(map[string]bool), signatures: make(map[string]json.RawMessage)}
	}, nil
}

func (p *Pilot) CommandDriver() integrations.CommandDriver { return p }
func (p *Pilot) Start(ctx context.Context, host integrations.SessionHost, tasks integrations.Tasks) error {
	if p == nil || ctx == nil || host == nil || tasks == nil {
		return ErrSettings
	}
	client, err := integrations.NewCloudClient(host, p.policy, nil, p.quota)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.client = client
	p.host = host
	p.mu.Unlock()
	if err := tasks.Go(func(taskCtx context.Context) error { p.run(taskCtx, host); return nil }); err != nil {
		client.Close()
		return err
	}
	return nil
}
func (p *Pilot) Stop(context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	client := p.client
	p.mu.RUnlock()
	if client != nil {
		client.Close()
	}
	return nil
}

func (p *Pilot) run(ctx context.Context, host integrations.SessionHost) {
	for ctx.Err() == nil {
		err := p.poll(ctx, host)
		if errors.Is(err, ErrSessionExpired) && ctx.Err() == nil {
			// The pinned client reconnects on USRSESSEXP. Recover one read
			// cycle immediately; a second expiry waits for the next cadence.
			// Actions never enter this path and are never replayed.
			p.expireSession()
			err = p.poll(ctx, host)
		}
		if err != nil {
			if !errors.Is(err, ErrPartialObservation) {
				p.markUnavailable(ctx, host)
			}
			p.report(ctx, host, integrations.HealthDegraded, "neviweb observation partial or unavailable")
			if errors.Is(err, ErrSessionExpired) {
				p.expireSession()
			}
		} else {
			// The upstream counter warns before its community ceiling. Homeward
			// warns at 80% of its own configured reservation, without claiming
			// visibility into Home Assistant or the provider's global usage.
			count, countErr := p.quota.Today(ctx)
			switch {
			case countErr != nil:
				p.report(ctx, host, integrations.HealthDegraded, "neviweb local request count unavailable")
			case count*5 >= int64(p.settings.dailyLimit)*4:
				p.report(ctx, host, integrations.HealthDegraded, "neviweb local request budget warning")
			default:
				p.report(ctx, host, integrations.HealthReady, "neviweb observations admitted")
			}
		}
		wait := p.settings.poll
		if errors.Is(err, ErrProviderQuota) {
			wait = 24 * time.Hour
		}
		var cloudErr *integrations.CloudError
		if errors.As(err, &cloudErr) {
			if until, ok := cloudErr.RetryAt(); ok {
				remaining := time.Until(until)
				if remaining > wait {
					wait = remaining
				}
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
	}
}

func (p *Pilot) expireSession() {
	p.mu.Lock()
	p.refresh = true
	p.persisted = sessionState{}
	p.current = sessionState{}
	p.mu.Unlock()
}

func (p *Pilot) markUnavailable(ctx context.Context, host integrations.SessionHost) {
	p.mu.Lock()
	owned := make(map[string]integrations.Binding, len(p.bindings))
	for alias, binding := range p.bindings {
		owned[alias] = binding
		p.models[alias] = modelInfo{}
	}
	p.mu.Unlock()
	for _, binding := range p.settings.bindings {
		active, ok := owned[binding.alias]
		if !ok {
			continue
		}
		endpoint := "climate"
		switch binding.kind {
		case BindingSwitch:
			endpoint = "relay"
		case BindingGateway:
			endpoint = "connectivity"
		case BindingDailyCount:
			endpoint = "requests"
		}
		_ = publishValue(ctx, host, active, endpoint, nil, time.Now().UTC())
	}
}

func (p *Pilot) report(ctx context.Context, host integrations.SessionHost, status integrations.HealthStatus, detail string) {
	report, err := integrations.NewHealthReport(status, detail)
	if err == nil {
		_ = host.ReportHealth(ctx, report)
	}
}

func (p *Pilot) fetch(ctx context.Context, path string, query url.Values) (integrations.CloudResponse, error) {
	p.mu.RLock()
	s := p.current
	p.mu.RUnlock()
	if s.phase != sessionActive {
		return integrations.CloudResponse{}, ErrSession
	}
	urlText := p.settings.endpoint(path)
	if query != nil {
		urlText = p.settings.endpointQuery(path, query)
	}
	request, err := integrations.NewCloudRequest(http.MethodGet, urlText, sessionHeaders(s), nil)
	if err != nil {
		return integrations.CloudResponse{}, err
	}
	response, err := p.client.Observe(ctx, request)
	if err != nil {
		return integrations.CloudResponse{}, decodeCloudFailure(err, true)
	}
	if err := providerFailure(response.Bytes()); err != nil {
		return integrations.CloudResponse{}, err
	}
	if merged, e := response.Cookies().MergeHeader(s.cookies); e != nil {
		return integrations.CloudResponse{}, e
	} else if merged != s.cookies {
		p.mu.Lock()
		if p.current == s {
			p.current.cookies = merged
		}
		p.mu.Unlock()
	}
	return response, nil
}

func (p *Pilot) verifyNetwork(ctx context.Context) error {
	locations, err := p.fetch(ctx, "/api/locations", url.Values{"account$id": []string{p.settings.accountText}})
	if err != nil {
		return err
	}
	var rows []map[string]json.RawMessage
	if json.Unmarshal(locations.Bytes(), &rows) != nil {
		return ErrProtocol
	}
	found := false
	for _, row := range rows {
		if rawID(row["id"]) == p.settings.networkID {
			found = true
			break
		}
	}
	if !found {
		return ErrProtocol
	}
	devices, err := p.fetch(ctx, "/api/devices", url.Values{"location$id": []string{p.settings.networkID}})
	if err != nil {
		return err
	}
	if json.Unmarshal(devices.Bytes(), &rows) != nil {
		return ErrProtocol
	}
	listed := make(map[string]bool, len(rows))
	signatures := make(map[string]json.RawMessage, len(rows))
	for _, row := range rows {
		if id := rawID(row["id"]); id != "" {
			listed[id] = true
			signatures[id] = append(json.RawMessage(nil), row["signature"]...)
		}
	}
	p.mu.Lock()
	p.listed = listed
	p.signatures = signatures
	p.networkAt = time.Now().UTC()
	p.mu.Unlock()
	return nil
}

// Device signatures come from /api/devices in the pinned client. Retry a
// missing/unknown signature on the next normal cycle, and check known models
// hourly for replacement without redetecting on every attribute read.
func (p *Pilot) needsNetworkRefresh() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.networkAt.IsZero() || time.Since(p.networkAt) >= time.Hour {
		return true
	}
	for _, binding := range p.settings.bindings {
		if binding.kind != BindingDailyCount && !p.listed[binding.deviceID] {
			return true
		}
		if binding.kind != BindingClimate && binding.kind != BindingSwitch {
			continue
		}
		if _, err := parseModel(p.signatures[binding.deviceID], binding.kind); err != nil {
			return true
		}
	}
	return false
}

func rawID(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var number json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&number) == nil {
		return number.String()
	}
	return ""
}

type cycleSnapshot struct {
	frames   map[string]attributeFrame
	gateway  *bool
	received map[string]time.Time
	last     integrations.CloudResponse
	degraded bool
}

func (p *Pilot) readCycle(ctx context.Context) (cycleSnapshot, error) {
	snapshot := cycleSnapshot{frames: make(map[string]attributeFrame, 9), received: make(map[string]time.Time, 10)}
	for _, binding := range p.settings.bindings {
		if binding.kind != BindingDailyCount {
			p.mu.RLock()
			listed := p.listed[binding.deviceID]
			p.mu.RUnlock()
			if !listed {
				snapshot.degraded = true
				continue
			}
		}
		switch binding.kind {
		case BindingClimate, BindingSwitch:
			p.mu.RLock()
			signature := append(json.RawMessage(nil), p.signatures[binding.deviceID]...)
			p.mu.RUnlock()
			model, modelErr := parseModel(signature, binding.kind)
			attributes := requestedAttributes(binding.kind, model)
			if modelErr != nil || attributes == "" {
				snapshot.degraded = true
				continue
			}
			response, err := p.fetch(ctx, "/api/device/"+binding.deviceID+"/attribute", url.Values{"attributes": []string{attributes}})
			if err != nil {
				if errors.Is(err, ErrUnsupportedAttribute) {
					snapshot.degraded = true
					continue
				}
				return cycleSnapshot{}, err
			}
			snapshot.received[binding.alias] = time.Now().UTC()
			snapshot.last = response
			frame, err := frameFor(binding.kind, response.Bytes(), signature)
			if err == nil {
				snapshot.frames[binding.alias] = frame
				snapshot.degraded = snapshot.degraded || frame.diagnostic
				if binding.kind == BindingClimate && frame.temperature == nil && frame.target == nil && frame.coolTarget == nil && frame.mode == nil {
					snapshot.degraded = true
				}
			} else {
				snapshot.degraded = true
			}
		case BindingGateway:
			response, err := p.fetch(ctx, "/api/device/"+binding.deviceID+"/status", nil)
			if err != nil {
				return cycleSnapshot{}, err
			}
			snapshot.received[binding.alias] = time.Now().UTC()
			snapshot.last = response
			var data map[string]json.RawMessage
			if json.Unmarshal(response.Bytes(), &data) == nil {
				var status string
				if json.Unmarshal(data["status"], &status) == nil {
					switch status {
					case "online":
						v := true
						snapshot.gateway = &v
					case "offline":
						v := false
						snapshot.gateway = &v
					}
				}
			}
			if snapshot.gateway == nil {
				snapshot.degraded = true
			}
		}
	}
	response, err := p.fetch(ctx, "/api/location/"+p.settings.networkID+"/notifications", nil)
	if err != nil {
		return cycleSnapshot{}, err
	}
	snapshot.last = response
	var notification any
	if json.Unmarshal(response.Bytes(), &notification) != nil {
		snapshot.degraded = true
	}
	return snapshot, nil
}

func (p *Pilot) poll(ctx context.Context, host integrations.SessionHost) error {
	p.mu.RLock()
	refresh := p.refresh
	discovered := p.discovered
	p.mu.RUnlock()
	s, err := p.session(ctx, host, refresh)
	if err != nil {
		return err
	}
	p.adoptSession(s)
	if !discovered || refresh || p.needsNetworkRefresh() {
		if err := p.verifyNetwork(ctx); err != nil {
			return err
		}
	}
	snapshot, err := p.readCycle(ctx)
	if err != nil {
		return err
	}
	if !discovered || p.capabilitiesChanged(snapshot) {
		if err := p.discover(ctx, host, snapshot); err != nil {
			return err
		}
	}
	if err := p.publish(ctx, host, snapshot); err != nil {
		return err
	}
	if snapshot.degraded {
		return ErrPartialObservation
	}
	return p.client.ConfirmObservationAdmitted(ctx, snapshot.last)
}

// Discovery updates only when a qualified model or its observed bounds change.
// Host.Discover replaces the same identities and catalog descriptions as one
// batch; a transient missing signature cannot erase previously qualified
// capabilities, and a normal poll does not churn the shared projection.
func (p *Pilot) capabilitiesChanged(snapshot cycleSnapshot) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, binding := range p.settings.bindings {
		if binding.kind != BindingClimate && binding.kind != BindingSwitch {
			continue
		}
		frame, present := snapshot.frames[binding.alias]
		if descriptionModelInfo(frame, present, p.declared[binding.alias]) != p.declared[binding.alias] {
			return true
		}
	}
	return false
}

func descriptionModelInfo(frame attributeFrame, present bool, previous modelInfo) modelInfo {
	if !present || frame.model.family == familyUnknown {
		return previous
	}
	info := frame.model
	if info.family == previous.family {
		// A missing bound on a partial response cannot revoke a previously
		// observed bound. Dispatch still refuses the incomplete current frame.
		if !info.bounded && previous.bounded {
			info.minimum, info.maximum, info.bounded = previous.minimum, previous.maximum, true
		}
		if !info.coolBounded && previous.coolBounded {
			info.coolMinimum, info.coolMaximum, info.coolBounded = previous.coolMinimum, previous.coolMaximum, true
		}
	}
	return info
}

func (p *Pilot) adoptSession(s sessionState) {
	p.mu.Lock()
	observed := s
	if p.persisted == s && p.current.phase == sessionActive {
		s.cookies = p.current.cookies
	}
	p.persisted = observed
	p.current = s
	p.refresh = false
	p.mu.Unlock()
}

func (p *Pilot) descriptions(binding Binding, info modelInfo) []*wire.CapabilityDescription {
	base := func(schema, endpoint string) *wire.CapabilityDescription {
		return &wire.CapabilityDescription{SchemaId: schema, Major: 1, DisplayName: binding.alias, Endpoint: endpoint}
	}
	switch binding.kind {
	case BindingClimate:
		climate := base(contracts.ClimateSchema, "climate")
		constraints := &wire.ClimateConstraints{ObservedTemperatureCelsius: &wire.ClimateTemperatureObservation{}, ObservedSetpointCelsius: &wire.ClimateTemperatureObservation{}}
		constraints.ObservedModeOptions = climateModeOptions(info.family)
		if info.bounded {
			bounds := &wire.NumberBounds{Minimum: info.minimum, Maximum: info.maximum}
			constraints.ObservedSetpointCelsius.Bounds = bounds
			if info.family == familyZigbee || info.family == familyWiFi {
				constraints.SetpointCelsius = bounds
			}
		}
		if info.family == familyZigbee || info.family == familyWiFi {
			constraints.ModeOptions = append([]wire.ClimateMode(nil), constraints.ObservedModeOptions...)
		}
		if info.family == familyHeatCool || info.family == familyZigbeeHeatCool {
			rangeLimits := &wire.ClimateSetpointRangeConstraints{}
			if info.bounded {
				rangeLimits.LowCelsius = &wire.NumberBounds{Minimum: info.minimum, Maximum: info.maximum}
			}
			if info.coolBounded {
				rangeLimits.HighCelsius = &wire.NumberBounds{Minimum: info.coolMinimum, Maximum: info.coolMaximum}
			}
			constraints.ObservedSetpointRangeCelsius = rangeLimits
		}
		climate.Constraints = &wire.CapabilityDescription_Climate{Climate: constraints}
		return []*wire.CapabilityDescription{climate}
	case BindingSwitch:
		d := base(contracts.SwitchSchema, "relay")
		d.Constraints = &wire.CapabilityDescription_Switch{Switch: &wire.SwitchConstraints{SetOn: info.family == familySwitch}}
		return []*wire.CapabilityDescription{d}
	case BindingDailyCount:
		d := base(contracts.MeasurementSchema, "requests")
		m := measurementConstraints("{request}", 0)
		m.Semantic = wire.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL
		d.Constraints = &wire.CapabilityDescription_Measurement{Measurement: m}
		return []*wire.CapabilityDescription{d}
	case BindingGateway:
		d := base(contracts.BinarySchema, "connectivity")
		class := "connectivity"
		d.Constraints = &wire.CapabilityDescription_Binary{Binary: &wire.BinaryConstraints{DeviceClass: &class}}
		return []*wire.CapabilityDescription{d}
	}
	return nil
}

func climateModeOptions(family modelFamily) []wire.ClimateMode {
	switch family {
	case familyZigbee:
		return []wire.ClimateMode{wire.ClimateMode_CLIMATE_MODE_OFF, wire.ClimateMode_CLIMATE_MODE_HEAT}
	case familyWiFi:
		return []wire.ClimateMode{wire.ClimateMode_CLIMATE_MODE_OFF, wire.ClimateMode_CLIMATE_MODE_HEAT, wire.ClimateMode_CLIMATE_MODE_AUTO}
	case familyZigbeeHeatCool:
		return []wire.ClimateMode{wire.ClimateMode_CLIMATE_MODE_OFF, wire.ClimateMode_CLIMATE_MODE_HEAT, wire.ClimateMode_CLIMATE_MODE_COOL}
	case familyHeatCool:
		return []wire.ClimateMode{wire.ClimateMode_CLIMATE_MODE_OFF, wire.ClimateMode_CLIMATE_MODE_HEAT, wire.ClimateMode_CLIMATE_MODE_COOL}
	default:
		return nil
	}
}

func measurementConstraints(unit string, precision uint32) *wire.MeasurementConstraints {
	return &wire.MeasurementConstraints{Unit: unit, Precision: precision, Semantic: wire.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT, Aggregation: wire.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT}
}

func (p *Pilot) discover(ctx context.Context, host integrations.SessionHost, snapshot cycleSnapshot) error {
	candidates := make([]integrations.Candidate, 0, len(p.settings.bindings))
	proposed := make(map[string]modelInfo, len(p.settings.bindings))
	for _, binding := range p.settings.bindings {
		frame, present := snapshot.frames[binding.alias]
		p.mu.RLock()
		info := descriptionModelInfo(frame, present, p.declared[binding.alias])
		p.mu.RUnlock()
		proposed[binding.alias] = info
		desc := p.descriptions(binding, info)
		var candidate integrations.Candidate
		var err error
		if binding.kind == BindingClimate || binding.kind == BindingSwitch {
			candidate, err = p.decision.CandidateFor(ctx, p.repository, p.settings.instance, binding.key, desc, p.catalog)
		} else {
			candidate, err = integrations.NewCandidate(binding.key, desc)
		}
		if err != nil {
			return err
		}
		candidates = append(candidates, candidate)
	}
	admitted, err := host.Discover(ctx, candidates)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for index, binding := range p.settings.bindings {
		p.bindings[binding.alias] = admitted[index]
		p.models[binding.alias] = snapshot.frames[binding.alias].model
		p.declared[binding.alias] = proposed[binding.alias]
	}
	p.discovered = true
	return nil
}

func (p *Pilot) publish(ctx context.Context, host integrations.SessionHost, snapshot cycleSnapshot) error {
	for _, binding := range p.settings.bindings {
		p.mu.RLock()
		owned, ok := p.bindings[binding.alias]
		p.mu.RUnlock()
		if !ok {
			continue
		}
		receivedAt := snapshot.received[binding.alias]
		if receivedAt.IsZero() {
			receivedAt = time.Now().UTC() // local unavailable/count report, never a physical timestamp
		}
		switch binding.kind {
		case BindingClimate:
			frame, ok := snapshot.frames[binding.alias]
			var value *domain.StateValue
			if ok {
				var err error
				value, err = climateStateValue(frame)
				if err != nil {
					return err
				}
			}
			p.mu.Lock()
			p.models[binding.alias] = modelInfo{}
			if value != nil && !frame.diagnostic {
				p.models[binding.alias] = frame.model
			}
			p.mu.Unlock()
			if err := publishValue(ctx, host, owned, "climate", value, receivedAt); err != nil {
				return err
			}
		case BindingSwitch:
			frame, ok := snapshot.frames[binding.alias]
			var value *domain.StateValue
			p.mu.Lock()
			p.models[binding.alias] = modelInfo{}
			p.mu.Unlock()
			if ok && frame.on != nil {
				v := domain.NewSwitchValue(*frame.on)
				value = &v
				p.mu.Lock()
				p.models[binding.alias] = frame.model
				p.mu.Unlock()
			}
			if err := publishValue(ctx, host, owned, "relay", value, receivedAt); err != nil {
				return err
			}
		case BindingGateway:
			var value *domain.StateValue
			if snapshot.gateway != nil {
				v := domain.NewSwitchValue(*snapshot.gateway)
				value = &v
			}
			if err := publishValue(ctx, host, owned, "connectivity", value, receivedAt); err != nil {
				return err
			}
		case BindingDailyCount:
			count, err := p.quota.Today(ctx)
			if err != nil {
				return err
			}
			number := float64(count)
			if err := p.publishNumber(ctx, host, owned, "requests", &number, "{request}", time.Now().UTC()); err != nil {
				return err
			}
		}
	}
	return nil
}

// One value carries every present field for the entity. The provider gives no
// trustworthy observation timestamp, so publishValue records receipt only.
func climateStateValue(frame attributeFrame) (*domain.StateValue, error) {
	var input domain.ClimateValueInput
	if frame.temperature != nil {
		current, err := domain.NewCelsiusTemperature(*frame.temperature)
		if err != nil {
			return nil, ErrProtocol
		}
		input.CurrentTemperature = &current
	}
	var target *float64
	if frame.model.family == familyHeatCool || frame.model.family == familyZigbeeHeatCool {
		modeKey := "heatCoolMode"
		if frame.model.family == familyZigbeeHeatCool {
			modeKey = "systemMode"
		}
		var sourceMode string
		_ = json.Unmarshal(frame.raw[modeKey], &sourceMode)
		switch sourceMode {
		case "auto":
			if frame.target != nil && frame.coolTarget != nil {
				low, err := domain.NewCelsiusTemperature(*frame.target)
				if err != nil {
					return nil, ErrProtocol
				}
				high, err := domain.NewCelsiusTemperature(*frame.coolTarget)
				if err != nil {
					return nil, ErrProtocol
				}
				setpoint, err := domain.NewRangeClimateSetpoint(low, high)
				if err != nil {
					return nil, ErrProtocol
				}
				input.Setpoint = &setpoint
			}
		case "cool":
			target = frame.coolTarget
		case "heat":
			target = frame.target
		}
	} else {
		target = frame.target
	}
	if target != nil {
		temperature, err := domain.NewCelsiusTemperature(*target)
		if err != nil {
			return nil, ErrProtocol
		}
		setpoint, err := domain.NewSingleClimateSetpoint(temperature)
		if err != nil {
			return nil, ErrProtocol
		}
		input.Setpoint = &setpoint
	}
	input.Mode = frame.mode
	if input.CurrentTemperature == nil && input.Setpoint == nil && input.Mode == nil {
		return nil, nil
	}
	climate, err := domain.NewClimateValue(input)
	if err != nil {
		return nil, ErrProtocol
	}
	value, err := domain.NewClimateStateValue(climate)
	if err != nil {
		return nil, ErrProtocol
	}
	return &value, nil
}

func (p *Pilot) publishNumber(ctx context.Context, host integrations.SessionHost, binding integrations.Binding, endpoint string, number *float64, unit string, at time.Time) error {
	if number == nil {
		return publishValue(ctx, host, binding, endpoint, nil, at)
	}
	_, entity, err := binding.IDs()
	if err != nil {
		return err
	}
	m, err := p.catalog.AdmitMeasurement(entity, endpoint, &statewire.Measurement{Value: *number, Unit: unit})
	if err != nil {
		return err
	}
	v, err := domain.NewMeasurementValue(m)
	if err != nil {
		return err
	}
	return publishValue(ctx, host, binding, endpoint, &v, at)
}

func publishValue(ctx context.Context, host integrations.SessionHost, binding integrations.Binding, endpoint string, value *domain.StateValue, at time.Time) error {
	availability := domain.AvailabilityAvailable
	freshness := domain.FreshnessFresh
	if value == nil {
		availability = domain.AvailabilityUnavailable
		freshness = domain.FreshnessStale
	}
	quality, err := domain.NewQuality(availability, freshness, domain.ProvenanceObserved)
	if err != nil {
		return err
	}
	observation, err := integrations.NewStateObservation(endpoint, quality, value, nil, at, nil)
	if err != nil {
		return err
	}
	return host.Publish(ctx, binding, observation)
}

func (p *Pilot) Dispatch(ctx context.Context, command integrations.AdmittedCommand) error {
	if p == nil || ctx == nil || !command.ReadyForTransport() {
		return integrations.ErrInactiveSession
	}
	_, _, entity, err := command.Identity()
	if err != nil {
		return err
	}
	request, err := command.Request()
	if err != nil {
		return err
	}
	p.mu.RLock()
	var selected Binding
	var model modelInfo
	listed := false
	found := false
	for _, binding := range p.settings.bindings {
		owned, ok := p.bindings[binding.alias]
		if !ok {
			continue
		}
		_, id, e := owned.IDs()
		if e == nil && id == entity {
			selected = binding
			model = p.models[binding.alias]
			listed = p.listed[binding.deviceID]
			found = true
			break
		}
	}
	s := p.current
	client := p.client
	p.mu.RUnlock()
	if !found || !listed || client == nil || s.phase != sessionActive || (selected.kind != BindingClimate && selected.kind != BindingSwitch) {
		return ErrSession
	}
	var payload map[string]any
	switch selected.kind {
	case BindingSwitch:
		if model.family != familySwitch || request.GetCapabilitySchemaId() != contracts.SwitchSchema || request.GetEndpoint() != "relay" || request.GetArguments().GetSetSwitch() == nil {
			return ErrProtocol
		}
		value := "off"
		if request.GetArguments().GetSetSwitch().On {
			value = "on"
		}
		payload = map[string]any{"onOff": value}
	case BindingClimate:
		if request.GetCapabilitySchemaId() != contracts.ClimateSchema || request.GetEndpoint() != "climate" || request.GetArguments().GetSetClimate() == nil {
			return ErrProtocol
		}
		change := request.GetArguments().GetSetClimate()
		if change.SetpointCelsius == nil && change.Mode == nil {
			return ErrProtocol
		}
		payload = make(map[string]any, 2)
		if change.SetpointCelsius != nil {
			if !model.bounded || (model.family != familyZigbee && model.family != familyWiFi) {
				return ErrProtocol
			}
			v := *change.SetpointCelsius
			if math.IsNaN(v) || math.IsInf(v, 0) || v < model.minimum || v > model.maximum {
				return ErrProtocol
			}
			payload["roomSetpoint"] = v
		}
		if change.Mode != nil {
			if model.family != familyZigbee && model.family != familyWiFi {
				return ErrProtocol
			}
			mode, e := modeAttribute(model.family, *change.Mode)
			if e != nil {
				return e
			}
			for key, value := range mode {
				payload[key] = value
			}
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return ErrProtocol
	}
	headers := sessionHeaders(s)
	headers.Set("Content-Type", "application/json")
	wireRequest, err := integrations.NewCloudRequest(http.MethodPut, p.settings.endpoint("/api/device/"+selected.deviceID+"/attribute"), headers, body)
	if err != nil {
		return err
	}
	response, err := client.Act(ctx, command, wireRequest)
	if err != nil {
		return err
	}
	if err := providerFailure(response.Bytes()); err != nil {
		return err
	}
	return command.Acknowledge()
}

func modeAttribute(family modelFamily, mode wire.ClimateMode) (map[string]any, error) {
	key := "systemMode"
	value := ""
	switch family {
	case familyZigbee:
		switch mode {
		case wire.ClimateMode_CLIMATE_MODE_OFF:
			value = "off"
		case wire.ClimateMode_CLIMATE_MODE_HEAT:
			value = "heat"
		}
	case familyWiFi:
		key = "setpointMode"
		switch mode {
		case wire.ClimateMode_CLIMATE_MODE_OFF:
			value = "off"
		case wire.ClimateMode_CLIMATE_MODE_HEAT:
			value = "manual"
		case wire.ClimateMode_CLIMATE_MODE_AUTO:
			value = "auto"
		}
	case familyHeatCool:
		key = "heatCoolMode"
		switch mode {
		case wire.ClimateMode_CLIMATE_MODE_OFF:
			value = "off"
		case wire.ClimateMode_CLIMATE_MODE_HEAT:
			value = "heat"
		case wire.ClimateMode_CLIMATE_MODE_COOL:
			value = "cool"
		case wire.ClimateMode_CLIMATE_MODE_AUTO:
			value = "auto"
		}
	}
	if value == "" {
		return nil, ErrProtocol
	}
	return map[string]any{key: value}, nil
}

var _ integrations.Pilot = (*Pilot)(nil)
var _ integrations.SupervisedCommandPilot = (*Pilot)(nil)
var _ integrations.CommandDriver = (*Pilot)(nil)

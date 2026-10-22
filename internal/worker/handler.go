package worker

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"time"

	"google.golang.org/protobuf/proto"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/integrations/v1"
	registryv1 "github.com/damione1/Homeward/gen/home/registry/v1"
	statev1 "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
)

func rpcInvalid() error {
	return connect.NewError(connect.CodeInvalidArgument, integrations.ErrInvalidInput)
}
func rpcUnavailable() error { return connect.NewError(connect.CodeFailedPrecondition, ErrUnavailable) }
func rpcHost(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, integrations.ErrInactiveSession) || errors.Is(err, integrations.ErrAdmissionsClosed) {
		return rpcUnavailable()
	}
	if errors.Is(err, integrations.ErrForeignBinding) || errors.Is(err, integrations.ErrForeignSecretHandle) {
		return connect.NewError(connect.CodePermissionDenied, ErrUnavailable)
	}
	return connect.NewError(connect.CodeInvalidArgument, integrations.ErrInvalidInput)
}

func (p *pilot) Handshake(_ context.Context, r *connect.Request[wire.HandshakeRequest]) (*connect.Response[wire.HandshakeResponse], error) {
	release, err := p.reserveRPC()
	if err != nil {
		return nil, err
	}
	defer release()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.active || p.handshaken {
		return nil, rpcUnavailable()
	}
	if subtle.ConstantTimeCompare([]byte(r.Header().Get("Authorization")), []byte("Bearer "+p.token)) != 1 {
		return nil, connect.NewError(connect.CodeUnauthenticated, ErrUnavailable)
	}
	msg := r.Msg
	if proto.Size(msg)+5 > maxFrame {
		return nil, connect.NewError(connect.CodeResourceExhausted, ErrCapacity)
	}
	const hostMajor uint32 = 1
	const driverMajor uint32 = 1
	var expected string
	switch {
	case msg.HostMajor == hostMajor && msg.HostMinor == 0 && msg.DriverMajor == driverMajor && msg.DriverMinor == 0:
		expected = schemaDigestV10Hex
	case msg.HostMajor == hostMajor && msg.HostMinor == 1 && msg.DriverMajor == driverMajor && msg.DriverMinor == 1:
		expected = schemaDigestV11Hex
	case msg.HostMajor == hostMajor && msg.HostMinor == 2 && msg.DriverMajor == driverMajor && msg.DriverMinor == 2:
		expected = schemaDigestV12Hex
	case msg.HostMajor == hostMajor && msg.HostMinor == 3 && msg.DriverMajor == driverMajor && msg.DriverMinor == 3:
		expected = schemaDigestV13Hex
	default:
		return nil, connect.NewError(connect.CodeFailedPrecondition, ErrInvalidConfig)
	}
	digest, _ := hex.DecodeString(expected)
	if len(msg.SchemaSha256) != len(digest) || subtle.ConstantTimeCompare(msg.SchemaSha256, digest) != 1 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, ErrInvalidConfig)
	}
	if msg.MaxFrameBytes < 1024 || len(msg.Capabilities) == 0 || len(msg.Capabilities) > 6 {
		return nil, rpcInvalid()
	}
	limit := msg.MaxFrameBytes
	if limit > maxFrame {
		limit = maxFrame
	}
	granted := make([]wire.Capability, 0, len(msg.Capabilities))
	seen := make(map[wire.Capability]bool)
	for _, capability := range msg.Capabilities {
		if seen[capability] {
			return nil, rpcInvalid()
		}
		seen[capability] = true
		switch capability {
		case wire.Capability_CAPABILITY_DISCOVER, wire.Capability_CAPABILITY_PUBLISH,
			wire.Capability_CAPABILITY_EMIT, wire.Capability_CAPABILITY_HEALTH,
			wire.Capability_CAPABILITY_COMMAND:
			granted = append(granted, capability)
		case wire.Capability_CAPABILITY_SECRET_READ:
			if len(p.config.secretNames) != 0 {
				granted = append(granted, capability)
			}
		case wire.Capability_CAPABILITY_UNSPECIFIED:
			return nil, rpcInvalid()
		default:
			return nil, rpcInvalid()
		}
	}
	if len(granted) == 0 {
		return nil, rpcInvalid()
	}
	for _, binding := range p.config.bindings {
		if binding.lightProfile == configuredLightProfileCasetaHA100V1 && msg.HostMinor != 3 {
			return nil, connect.NewError(connect.CodeFailedPrecondition, ErrCasetaHAProfileUnavailable)
		}
		if binding.fanProfile == configuredFanProfileCasetaHA4V1 && msg.HostMinor != 1 && msg.HostMinor != 3 {
			return nil, connect.NewError(connect.CodeFailedPrecondition, ErrCasetaHAFanProfileUnavailable)
		}
	}
	response := &wire.HandshakeResponse{HostMajor: hostMajor, HostMinor: msg.HostMinor,
		DriverMajor: driverMajor, DriverMinor: msg.DriverMinor, SchemaSha256: digest, MaxFrameBytes: limit,
		Capabilities: granted, HeartbeatSeconds: uint32(heartbeatInterval / time.Second),
		LeaseSeconds: uint32(leaseTimeout / time.Second)}
	if proto.Size(response)+5 > int(limit) {
		return nil, connect.NewError(connect.CodeResourceExhausted, ErrCapacity)
	}
	p.capabilities = make(map[wire.Capability]bool, len(granted))
	for _, capability := range granted {
		p.capabilities[capability] = true
	}
	p.frameLimit, p.lastHeartbeat, p.handshaken, p.hostMinor = limit, time.Now(), true, msg.HostMinor
	close(p.handshakeDone)
	return connect.NewResponse(response), nil
}

func (p *pilot) Discover(ctx context.Context, r *connect.Request[wire.DiscoverRequest]) (*connect.Response[wire.DiscoverResponse], error) {
	release, err := p.reserveRPC()
	if err != nil {
		return nil, err
	}
	defer release()
	p.discoverMu.Lock()
	defer p.discoverMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := p.admittedFrame(r.Header().Get("Authorization"), wire.Capability_CAPABILITY_DISCOVER, r.Msg); err != nil {
		return nil, err
	}
	if len(r.Msg.Candidates) == 0 || len(r.Msg.Candidates) > 256 {
		return nil, rpcInvalid()
	}
	candidates := make([]integrations.Candidate, 0, len(r.Msg.Candidates))
	keys := make([]integrations.SourceKey, 0, len(r.Msg.Candidates))
	for _, input := range r.Msg.Candidates {
		if input == nil || input.Source == nil {
			return nil, rpcInvalid()
		}
		// Host 1.0–1.3 never admitted the native helper capability. The
		// current protobuf can decode it transitively, but that does not
		// enlarge any historical worker profile.
		for _, description := range input.Descriptions {
			// NHA-136 is a built-in profile. Decoding its additive fields cannot
			// enlarge immutable external Host/driver 1.0–1.3 contracts.
			if description != nil {
				if climate := description.GetClimate(); climate != nil && (climate.SetpointRangeCelsius != nil || len(climate.FanTargetOptions) != 0 || climate.ObservedHumidityPercent != nil || len(climate.ObservedFanTargetOptions) != 0) {
					return nil, rpcInvalid()
				}
			}
			if contracts.NativeSolarDescription(description) {
				return nil, rpcInvalid()
			}
			if description != nil && (description.SchemaId == contracts.HelperSchema || description.GetHelper() != nil) {
				return nil, rpcInvalid()
			}
		}
		key, err := integrations.NewSourceKey(input.Source.Device, input.Source.Entity)
		if err != nil {
			return nil, rpcInvalid()
		}
		candidate, err := integrations.NewCandidate(key, input.Descriptions)
		if err != nil {
			return nil, rpcInvalid()
		}
		for _, configured := range p.config.bindings {
			if configured.key != key {
				continue
			}
			switch {
			case configured.lightProfile == configuredLightProfileCasetaHA100V1:
				if p.hostMinor != 3 {
					return nil, connect.NewError(connect.CodeFailedPrecondition, ErrCasetaHAProfileUnavailable)
				}
				candidate, err = candidate.WithCasetaHA100V1(configured.alias)
				if err != nil {
					return nil, connect.NewError(connect.CodeFailedPrecondition, ErrCasetaHAProfileUnavailable)
				}
			case configured.fanProfile == configuredFanProfileCasetaHA4V1:
				if p.hostMinor != 1 && p.hostMinor != 3 {
					return nil, connect.NewError(connect.CodeFailedPrecondition, ErrCasetaHAFanProfileUnavailable)
				}
				candidate, err = candidate.WithCasetaHAFan4V1(configured.alias)
				if err != nil {
					return nil, connect.NewError(connect.CodeFailedPrecondition, ErrCasetaHAFanProfileUnavailable)
				}
			}
			break
		}
		if p.hostMinor == 0 {
			for _, description := range input.Descriptions {
				if description != nil && description.SchemaId == contracts.FanSchema {
					return nil, rpcInvalid()
				}
			}
		}
		if p.hostMinor < 2 {
			for _, description := range input.Descriptions {
				if description != nil {
					if climate := description.GetClimate(); climate != nil && (climate.ObservedTemperatureCelsius != nil || climate.ObservedSetpointCelsius != nil || climate.ObservedSetpointRangeCelsius != nil || len(climate.ObservedModeOptions) != 0 || climate.ObservesFanActivity) {
						return nil, rpcInvalid()
					}
				}
			}
		}
		if p.hostMinor < 3 {
			for _, description := range input.Descriptions {
				if description != nil {
					if light := description.GetLight(); light != nil && light.TransitionSeconds != nil {
						return nil, rpcInvalid()
					}
				}
			}
		}
		candidates = append(candidates, candidate)
		keys = append(keys, key)
	}
	p.mu.Lock()
	newKeys := 0
	for _, key := range keys {
		if _, exists := p.sources[key]; !exists {
			newKeys++
		}
	}
	if !p.active || len(p.bindings)+newKeys > maxBindings {
		p.mu.Unlock()
		return nil, connect.NewError(connect.CodeResourceExhausted, ErrCapacity)
	}
	// Allocate every opaque handle and prove the whole response fits before
	// Host.Discover commits its atomic ownership/projection batch.
	prepared := make(map[integrations.SourceKey][]byte, newKeys)
	response := &wire.DiscoverResponse{Bindings: make([]*wire.Binding, len(keys))}
	for i, key := range keys {
		handle, exists := p.sources[key]
		if !exists {
			handle, exists = prepared[key]
			if !exists {
				handle = make([]byte, 32)
				if _, err := rand.Read(handle); err != nil {
					p.mu.Unlock()
					return nil, connect.NewError(connect.CodeInternal, ErrUnavailable)
				}
				prepared[key] = handle
			}
		}
		response.Bindings[i] = &wire.Binding{Handle: append([]byte(nil), handle...)}
	}
	if err := p.frameLocked(response); err != nil {
		p.mu.Unlock()
		return nil, err
	}
	p.mu.Unlock()
	callCtx, cancel := p.callContext(ctx)
	defer cancel()
	bindings, err := p.host.Discover(callCtx, candidates)
	if err != nil {
		return nil, rpcHost(err)
	}
	if len(bindings) != len(keys) {
		return nil, rpcInvalid()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.active {
		return nil, rpcUnavailable()
	}
	stagedBindings := make(map[string]integrations.Binding, len(p.bindings)+newKeys)
	for handle, binding := range p.bindings {
		stagedBindings[handle] = binding
	}
	stagedSources := make(map[integrations.SourceKey][]byte, len(p.sources)+newKeys)
	for key, handle := range p.sources {
		stagedSources[key] = handle
	}
	stagedEntities := make(map[string][]byte, len(p.entityHandles)+newKeys)
	for entity, handle := range p.entityHandles {
		stagedEntities[entity] = handle
	}
	for i, binding := range bindings {
		_, bindingKey, err := binding.Source()
		if err != nil || bindingKey != keys[i] {
			return nil, rpcInvalid()
		}
		handle, exists := stagedSources[bindingKey]
		if exists {
			if stagedBindings[string(handle)] != binding {
				return nil, rpcInvalid()
			}
		} else {
			handle = prepared[bindingKey]
			if len(handle) != 32 {
				return nil, rpcInvalid()
			}
			stagedBindings[string(handle)] = binding
			stagedSources[bindingKey] = handle
		}
		_, entity, err := binding.IDs()
		if err != nil {
			return nil, rpcHost(err)
		}
		entityText, err := entity.Value()
		if err != nil {
			return nil, rpcHost(err)
		}
		if prior, present := stagedEntities[entityText]; !present {
			stagedEntities[entityText] = handle
		} else if prior != nil && string(prior) != string(handle) {
			stagedEntities[entityText] = nil // two distinct sources are ambiguous
		}
		if string(response.Bindings[i].Handle) != string(handle) {
			return nil, rpcInvalid()
		}
	}
	p.bindings, p.sources, p.entityHandles = stagedBindings, stagedSources, stagedEntities
	return connect.NewResponse(response), nil
}

func (p *pilot) binding(handle []byte) (integrations.Binding, string, error) {
	if len(handle) != 32 {
		return integrations.Binding{}, "", rpcInvalid()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.active {
		return integrations.Binding{}, "", rpcUnavailable()
	}
	binding, ok := p.bindings[string(handle)]
	if !ok {
		return integrations.Binding{}, "", connect.NewError(connect.CodePermissionDenied, integrations.ErrForeignBinding)
	}
	_, entity, err := binding.IDs()
	if err != nil {
		return integrations.Binding{}, "", rpcHost(err)
	}
	text, err := entity.Value()
	if err != nil {
		return integrations.Binding{}, "", rpcHost(err)
	}
	return binding, text, nil
}

func (p *pilot) causality(text, entity string) (domain.Causality, error) {
	if text == "" {
		return domain.Causality{}, nil
	}
	p.mu.Lock()
	entry, ok := p.correlations[text]
	if ok && time.Now().After(entry.expires) {
		delete(p.correlations, text)
		ok = false
	}
	p.mu.Unlock()
	if !ok || (entity != "" && entry.entity != entity) {
		return domain.Causality{}, connect.NewError(connect.CodePermissionDenied, ErrUnavailable)
	}
	id, err := domain.ParseCorrelationID(text)
	if err != nil {
		return domain.Causality{}, rpcInvalid()
	}
	return domain.NewCausality(nil, &id)
}

func (p *pilot) Publish(ctx context.Context, r *connect.Request[wire.PublishRequest]) (*connect.Response[wire.PublishResponse], error) {
	release, err := p.reserveRPC()
	if err != nil {
		return nil, err
	}
	defer release()
	if err := p.admittedFrame(r.Header().Get("Authorization"), wire.Capability_CAPABILITY_PUBLISH, r.Msg); err != nil {
		return nil, err
	}
	input := r.Msg.Observation
	if input == nil || input.Quality == nil {
		return nil, rpcInvalid()
	}
	if climate := input.GetClimate(); climate != nil && (climate.HumidityPercent != nil || climate.FanTarget != nil) {
		return nil, rpcInvalid()
	}
	if p.hostMinor == 0 {
		if _, fan := input.Value.(*wire.Observation_Fan); fan {
			return nil, rpcInvalid()
		}
	}
	if p.hostMinor < 2 {
		if _, climate := input.Value.(*wire.Observation_Climate); climate {
			return nil, rpcInvalid()
		}
	}
	binding, entity, err := p.binding(input.BindingHandle)
	if err != nil {
		return nil, err
	}
	quality, err := parseQuality(input.Quality)
	if err != nil {
		return nil, rpcInvalid()
	}
	value, err := parseValue(input)
	if err != nil {
		return nil, rpcInvalid()
	}
	var observed *time.Time
	if input.ObservedAt != nil {
		if err := input.ObservedAt.CheckValid(); err != nil {
			return nil, rpcInvalid()
		}
		stamp := input.ObservedAt.AsTime()
		observed = &stamp
	}
	observation, err := integrations.NewStateObservation(input.Endpoint, quality, value, observed, time.Now().UTC(), input.SourceSeq)
	if err != nil {
		return nil, rpcInvalid()
	}
	if input.CommandCorrelation != nil {
		cause, err := p.causality(*input.CommandCorrelation, entity)
		if err != nil {
			return nil, err
		}
		observation, err = observation.WithCausality(cause)
		if err != nil {
			return nil, rpcInvalid()
		}
	}
	callCtx, cancel := p.callContext(ctx)
	defer cancel()
	if err := p.host.Publish(callCtx, binding, observation); err != nil {
		return nil, rpcHost(err)
	}
	return connect.NewResponse(&wire.PublishResponse{}), nil
}

func (p *pilot) Emit(ctx context.Context, r *connect.Request[wire.EmitRequest]) (*connect.Response[wire.EmitResponse], error) {
	release, err := p.reserveRPC()
	if err != nil {
		return nil, err
	}
	defer release()
	if err := p.admittedFrame(r.Header().Get("Authorization"), wire.Capability_CAPABILITY_EMIT, r.Msg); err != nil {
		return nil, err
	}
	input := r.Msg.Event
	if input == nil || input.ObservedAt == nil || input.ObservedAt.CheckValid() != nil {
		return nil, rpcInvalid()
	}
	binding, entity, err := p.binding(input.BindingHandle)
	if err != nil {
		return nil, err
	}
	event, err := integrations.NewDeviceEvent(input.Code, input.ObservedAt.AsTime(), input.SourceSeq)
	if err != nil {
		return nil, rpcInvalid()
	}
	if input.CommandCorrelation != nil {
		cause, err := p.causality(*input.CommandCorrelation, entity)
		if err != nil {
			return nil, err
		}
		event, err = event.WithCausality(cause)
		if err != nil {
			return nil, rpcInvalid()
		}
	}
	callCtx, cancel := p.callContext(ctx)
	defer cancel()
	if err := p.host.Emit(callCtx, binding, event); err != nil {
		return nil, rpcHost(err)
	}
	return connect.NewResponse(&wire.EmitResponse{}), nil
}

func (p *pilot) ReportHealth(ctx context.Context, r *connect.Request[wire.ReportHealthRequest]) (*connect.Response[wire.ReportHealthResponse], error) {
	release, err := p.reserveRPC()
	if err != nil {
		return nil, err
	}
	defer release()
	if err := p.admittedFrame(r.Header().Get("Authorization"), wire.Capability_CAPABILITY_HEALTH, r.Msg); err != nil {
		return nil, err
	}
	var status integrations.HealthStatus
	switch r.Msg.Status {
	case wire.HealthStatus_HEALTH_STATUS_READY:
		status = integrations.HealthReady
	case wire.HealthStatus_HEALTH_STATUS_DEGRADED:
		status = integrations.HealthDegraded
	case wire.HealthStatus_HEALTH_STATUS_UNAVAILABLE:
		status = integrations.HealthUnavailable
	default:
		return nil, rpcInvalid()
	}
	report, err := integrations.NewHealthReport(status, r.Msg.Detail)
	if err != nil {
		return nil, rpcInvalid()
	}
	if p.hostMinor >= 1 && status != integrations.HealthReady {
		p.mu.Lock()
		p.ready = false
		p.mu.Unlock()
	}
	if r.Msg.CommandCorrelation != nil {
		cause, err := p.causality(*r.Msg.CommandCorrelation, "")
		if err != nil {
			return nil, err
		}
		report, err = report.WithCausality(cause)
		if err != nil {
			return nil, rpcInvalid()
		}
	}
	callCtx, cancel := p.callContext(ctx)
	defer cancel()
	if err := p.host.ReportHealth(callCtx, report); err != nil {
		return nil, rpcHost(err)
	}
	if p.hostMinor >= 1 && status == integrations.HealthReady {
		p.mu.Lock()
		p.ready = p.active
		p.mu.Unlock()
	}
	return connect.NewResponse(&wire.ReportHealthResponse{}), nil
}

func (p *pilot) ReadSecret(ctx context.Context, r *connect.Request[wire.ReadSecretRequest]) (*connect.Response[wire.ReadSecretResponse], error) {
	release, err := p.reserveRPC()
	if err != nil {
		return nil, err
	}
	defer release()
	if err := p.admittedFrame(r.Header().Get("Authorization"), wire.Capability_CAPABILITY_SECRET_READ, r.Msg); err != nil {
		return nil, err
	}
	if _, ok := p.config.secretNames[r.Msg.Name]; !ok {
		return nil, connect.NewError(connect.CodePermissionDenied, ErrUnavailable)
	}
	name, err := storage.ParseSecretName(r.Msg.Name)
	if err != nil {
		return nil, rpcInvalid()
	}
	handle, err := storage.NewSecretHandle(p.config.instance, name)
	if err != nil {
		return nil, rpcInvalid()
	}
	provider, err := p.host.Secrets()
	if err != nil {
		return nil, rpcHost(err)
	}
	callCtx, cancel := p.callContext(ctx)
	defer cancel()
	value, err := provider.Read(callCtx, handle)
	if err != nil {
		return nil, rpcHost(err)
	}
	bytes, err := value.Bytes()
	if err != nil || len(bytes) > int(p.frameLimit) {
		return nil, connect.NewError(connect.CodeResourceExhausted, ErrCapacity)
	}
	response := &wire.ReadSecretResponse{Value: bytes}
	if err := p.frame(response); err != nil {
		return nil, err
	}
	return connect.NewResponse(response), nil
}

func (p *pilot) Heartbeat(_ context.Context, r *connect.Request[wire.HeartbeatRequest]) (*connect.Response[wire.HeartbeatResponse], error) {
	release, err := p.reserveRPC()
	if err != nil {
		return nil, err
	}
	defer release()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.active || !p.handshaken || p.leaseExpiredLocked() {
		return nil, rpcUnavailable()
	}
	if subtle.ConstantTimeCompare([]byte(r.Header().Get("Authorization")), []byte("Bearer "+p.token)) != 1 {
		return nil, connect.NewError(connect.CodeUnauthenticated, ErrUnavailable)
	}
	if err := p.frameLocked(r.Msg); err != nil {
		return nil, err
	}
	p.lastHeartbeat = time.Now()
	return connect.NewResponse(&wire.HeartbeatResponse{}), nil
}

func parseQuality(input *statev1.Quality) (domain.Quality, error) {
	var availability domain.Availability
	switch input.Availability {
	case statev1.Availability_AVAILABILITY_AVAILABLE:
		availability = domain.AvailabilityAvailable
	case statev1.Availability_AVAILABILITY_UNKNOWN:
		availability = domain.AvailabilityUnknown
	case statev1.Availability_AVAILABILITY_UNAVAILABLE:
		availability = domain.AvailabilityUnavailable
	default:
		return domain.Quality{}, integrations.ErrInvalidInput
	}
	var freshness domain.Freshness
	switch input.Freshness {
	case statev1.Freshness_FRESHNESS_FRESH:
		freshness = domain.FreshnessFresh
	case statev1.Freshness_FRESHNESS_STALE:
		freshness = domain.FreshnessStale
	default:
		return domain.Quality{}, integrations.ErrInvalidInput
	}
	var provenance domain.Provenance
	switch input.Provenance {
	case statev1.Provenance_PROVENANCE_OBSERVED:
		provenance = domain.ProvenanceObserved
	case statev1.Provenance_PROVENANCE_RESTORED:
		provenance = domain.ProvenanceRestored
	case statev1.Provenance_PROVENANCE_ESTIMATED:
		provenance = domain.ProvenanceEstimated
	default:
		return domain.Quality{}, integrations.ErrInvalidInput
	}
	return domain.NewQuality(availability, freshness, provenance)
}

func parseValue(input *wire.Observation) (*domain.StateValue, error) {
	switch value := input.Value.(type) {
	case *wire.Observation_Climate:
		if value == nil {
			return nil, integrations.ErrInvalidInput
		}
		climate, err := contracts.AdmitWireClimateValue(value.Climate)
		if err != nil {
			return nil, err
		}
		v, err := domain.NewClimateStateValue(climate)
		if err != nil {
			return nil, err
		}
		return &v, nil
	case *wire.Observation_SwitchOn:
		v := domain.NewSwitchValue(value.SwitchOn)
		return &v, nil
	case *wire.Observation_Light:
		if value.Light == nil || value.Light.On == nil {
			return nil, integrations.ErrInvalidInput
		}
		light, err := domain.NewLightValue(*value.Light.On, value.Light.BrightnessPercent, value.Light.ColorTempMireds)
		if err != nil {
			return nil, err
		}
		v, err := domain.NewLightStateValue(light)
		if err != nil {
			return nil, err
		}
		return &v, nil
	case *wire.Observation_Fan:
		if value.Fan == nil {
			return nil, integrations.ErrInvalidInput
		}
		speed, err := contracts.AdmitWireFanSpeed(value.Fan.Speed)
		if err != nil {
			return nil, err
		}
		v, err := domain.NewFanStateValue(speed)
		if err != nil {
			return nil, err
		}
		return &v, nil
	case *wire.Observation_Measurement:
		if value.Measurement == nil {
			return nil, integrations.ErrInvalidInput
		}
		unit, err := domain.NewUnitCatalog().Parse(value.Measurement.Unit)
		if err != nil {
			return nil, err
		}
		var semantic domain.SemanticClass
		switch value.Measurement.Semantic {
		case registryv1.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT:
			semantic = domain.SemanticMeasurement
		case registryv1.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL:
			semantic = domain.SemanticTotal
		case registryv1.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL_INCREASING:
			semantic = domain.SemanticTotalIncreasing
		default:
			return nil, integrations.ErrInvalidInput
		}
		var aggregation domain.AggregationKind
		switch value.Measurement.Aggregation {
		case registryv1.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT:
			aggregation = domain.AggregationCurrent
		case registryv1.MeasurementAggregation_MEASUREMENT_AGGREGATION_SUM:
			aggregation = domain.AggregationSum
		case registryv1.MeasurementAggregation_MEASUREMENT_AGGREGATION_MEAN:
			aggregation = domain.AggregationMean
		default:
			return nil, integrations.ErrInvalidInput
		}
		measurement, err := domain.NewMeasurement(value.Measurement.Value, unit, int(value.Measurement.Precision), semantic, aggregation)
		if err != nil {
			return nil, err
		}
		v, err := domain.NewMeasurementValue(measurement)
		if err != nil {
			return nil, err
		}
		return &v, nil
	case nil:
		return nil, nil
	default:
		return nil, integrations.ErrInvalidInput
	}
}

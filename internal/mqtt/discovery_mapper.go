package mqtt

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/damione1/Homeward/compat/ha/template"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/observability"
)

type discoveryRegistry interface {
	MatchMQTTSource(context.Context, domain.IntegrationInstanceID, []string, string) (integrations.SourceKey, error)
	MQTTTopicSources(context.Context, domain.IntegrationInstanceID, string) ([]integrations.SourceKey, error)
	RememberMQTTTopics(context.Context, domain.IntegrationInstanceID, string, []integrations.SourceKey) error
	Tombstone(context.Context, domain.EntityID) error
}

var errDiscoveryHostPublish = errors.New("mqtt: Host publication failed")

type DiscoveryDiagnostic uint8

const (
	DiagnosticUnspecified DiscoveryDiagnostic = iota
	DiagnosticUnknownDiscovery
	DiagnosticInvalidDiscovery
	DiagnosticUnconfiguredSource
	DiagnosticUnsupportedEffect
	DiagnosticUnsupportedColorMode
	DiagnosticInvalidState
	DiagnosticAvailability
	DiagnosticRetirement
	DiagnosticSnapshotOverflow
	DiagnosticUnconfiguredActionAnchor
	DiagnosticAmbiguousActionDevice
	DiagnosticActionOverflow
	DiagnosticInvalidAction
)

func (d DiscoveryDiagnostic) String() string {
	switch d {
	case DiagnosticUnknownDiscovery:
		return "unknown_discovery"
	case DiagnosticInvalidDiscovery:
		return "invalid_discovery"
	case DiagnosticUnconfiguredSource:
		return "unconfigured_source"
	case DiagnosticUnsupportedEffect:
		return "unsupported_effect"
	case DiagnosticUnsupportedColorMode:
		return "unsupported_color_mode"
	case DiagnosticInvalidState:
		return "invalid_state"
	case DiagnosticAvailability:
		return "invalid_availability"
	case DiagnosticRetirement:
		return "retirement_failure"
	case DiagnosticSnapshotOverflow:
		return "snapshot_overflow"
	case DiagnosticUnconfiguredActionAnchor:
		return "unconfigured_action_anchor"
	case DiagnosticAmbiguousActionDevice:
		return "ambiguous_action_device"
	case DiagnosticActionOverflow:
		return "action_overflow"
	case DiagnosticInvalidAction:
		return "invalid_action"
	default:
		return "unspecified"
	}
}

// DiscoveryDiagnosticRecord contains only validated provenance, never the
// discovery or state payload. Component names are diagnostic labels, not IDs.
type DiscoveryDiagnosticRecord struct {
	Code      DiscoveryDiagnostic
	Topic     Topic
	Component string
}

type componentRuntime struct {
	correlationScope correlationScope
	profile          discoveryComponent
	key              integrations.SourceKey
	binding          integrations.Binding
	entity           domain.EntityID
	availability     []domain.Availability
	latest           int
	lastValue        *domain.StateValue
	lastQuality      domain.Quality
}

// cachedSnapshot is an observation snapshot, never an edge-capable MQTT
// Observation. Replaying a live packet downgrades freshness to stale.
type cachedSnapshot struct {
	payload []byte
	quality domain.Quality
	order   uint64
}

// DiscoveryMapper is bound to one supervised pilot session. Its bounded graph
// is rebuilt from fully admitted documents; it grants no identity itself.
type DiscoveryMapper struct {
	mu               sync.Mutex
	instance         domain.IntegrationInstanceID
	mode             Mode
	commands         []TopicFilter
	transitionLights map[integrations.SourceKey]transitionProfile
	repository       discoveryRegistry
	logger           *slog.Logger
	journal          *observability.Journal
	documents        map[Topic][]*componentRuntime
	documentDigests  map[Topic][sha256.Size]byte
	snapshots        map[Topic]cachedSnapshot
	snapshotBytes    int
	arrival          uint64
	states           map[Topic][]*componentRuntime
	availabilities   map[Topic][]*componentRuntime
	entities         map[domain.EntityID]*componentRuntime
	actionDocuments  map[Topic][]actionDeclaration
	actionDevices    map[domain.DeviceID]*actionDevice
	actionRoutes     map[Topic][]actionRoute
	diagnostics      []DiscoveryDiagnosticRecord
}

func newDiscoveryMapper(settings Settings, repository discoveryRegistry, logger *slog.Logger) (*DiscoveryMapper, error) {
	if _, err := settings.Instance(); err != nil || missingInterface(repository) {
		return nil, ErrInvalidSettings
	}
	if logger == nil {
		logger = slog.Default()
	}
	optIn := make(map[integrations.SourceKey]transitionProfile, len(settings.transitionLights))
	for key, profile := range settings.transitionLights {
		optIn[key] = profile
	}
	return &DiscoveryMapper{instance: settings.instance, mode: settings.mode, commands: append([]TopicFilter(nil), settings.commands...), transitionLights: optIn, repository: repository, logger: logger, documents: make(map[Topic][]*componentRuntime), documentDigests: make(map[Topic][sha256.Size]byte), snapshots: make(map[Topic]cachedSnapshot), states: make(map[Topic][]*componentRuntime), availabilities: make(map[Topic][]*componentRuntime), entities: make(map[domain.EntityID]*componentRuntime), actionDocuments: make(map[Topic][]actionDeclaration), actionDevices: make(map[domain.DeviceID]*actionDevice), actionRoutes: make(map[Topic][]actionRoute)}, nil
}

func (m *DiscoveryMapper) admitTransition(key integrations.SourceKey, c discoveryComponent) transitionProfile {
	if m.mode != ModeActive || c.kind != componentLight || !c.hasCommand ||
		!c.brightness || !c.brightnessOnly || c.colorTemp || c.brightnessScale != 254 ||
		c.manufacturer != "IKEA" || c.modelID != "ICPSHC24-30EU-IL-1/ICPSHC24-10EU-IL-2" ||
		c.transitionAdvertised != nil && !*c.transitionAdvertised {
		return transitionUnspecified
	}
	if m.transitionLights[key] == transitionIkeaDriver30W {
		return transitionIkeaDriver30W
	}
	return transitionUnspecified
}

func lightEncodingFor(c discoveryComponent) (integrations.LightEncoding, error) {
	if c.kind != componentLight || !c.hasCommand || math.IsNaN(c.brightnessScale) || math.IsInf(c.brightnessScale, 0) ||
		c.brightnessScale < 1 || c.brightnessScale > 65535 || math.Trunc(c.brightnessScale) != c.brightnessScale {
		return integrations.LightEncoding{}, errDiscoveryProfile
	}
	return integrations.NewMQTTLightEncoding(uint32(c.brightnessScale), c.fingerprint, c.admittedTransition == transitionIkeaDriver30W)
}

func (m *DiscoveryMapper) cacheSnapshot(topic Topic, payload []byte, quality domain.Quality) error {
	if old, exists := m.snapshots[topic]; exists {
		m.snapshotBytes -= len(old.payload)
		delete(m.snapshots, topic)
	}
	if len(m.snapshots) >= 2048 || len(payload) > 16<<20-m.snapshotBytes {
		m.diagnose(DiagnosticSnapshotOverflow, topic, "")
		return ErrObservationGap
	}
	_, _, provenance, err := quality.Components()
	if err != nil {
		return err
	}
	stale, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessStale, provenance)
	if err != nil {
		return err
	}
	m.arrival++
	m.snapshots[topic] = cachedSnapshot{payload: append([]byte(nil), payload...), quality: stale, order: m.arrival}
	m.snapshotBytes += len(payload)
	return nil
}

func (m *DiscoveryMapper) Diagnostics() []DiscoveryDiagnostic {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	codes := make([]DiscoveryDiagnostic, len(m.diagnostics))
	for i, record := range m.diagnostics {
		codes[i] = record.Code
	}
	return codes
}

func (m *DiscoveryMapper) DiagnosticRecords() []DiscoveryDiagnosticRecord {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]DiscoveryDiagnosticRecord(nil), m.diagnostics...)
}

func (m *DiscoveryMapper) diagnose(code DiscoveryDiagnostic, topic Topic, component string) {
	if m == nil {
		return
	}
	safeCode, err := discoveryLogCode(code)
	if err != nil {
		return
	}
	if len(component) > 128 || !utf8.ValidString(component) {
		component = ""
	}
	for _, r := range component {
		if !unicode.IsPrint(r) {
			component = ""
			break
		}
	}
	if len(m.diagnostics) == 128 {
		copy(m.diagnostics, m.diagnostics[1:])
		m.diagnostics = m.diagnostics[:127]
	}
	m.diagnostics = append(m.diagnostics, DiscoveryDiagnosticRecord{Code: code, Topic: topic, Component: component})
	if m.logger != nil {
		m.logger.Warn("MQTT discovery diagnostic", "code", code.String())
	}
	m.emitDiscoveryDiagnostic(safeCode)
}

func (m *DiscoveryMapper) rebuild() error {
	states := make(map[Topic][]*componentRuntime)
	availability := make(map[Topic][]*componentRuntime)
	entities := make(map[domain.EntityID]*componentRuntime)
	count := 0
	for _, components := range m.documents {
		for _, runtime := range components {
			count++
			if count > 1024 {
				return errDiscoveryProfile
			}
			states[runtime.profile.stateTopic] = append(states[runtime.profile.stateTopic], runtime)
			for _, item := range runtime.profile.availability {
				availability[item.topic] = append(availability[item.topic], runtime)
			}
			if _, duplicate := entities[runtime.entity]; duplicate {
				return errDiscoveryProfile
			}
			entities[runtime.entity] = runtime
		}
	}
	m.states, m.availabilities, m.entities = states, availability, entities
	return m.rebuildActionRoutes()
}

func (m *DiscoveryMapper) Observe(ctx context.Context, host integrations.SessionHost, observation Observation) error {
	if m == nil || ctx == nil || missingInterface(host) {
		return ErrInvalidSettings
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	topic, payload, quality, err := observation.Record()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, _, err := parseDiscoveryTopic(topic); err == nil {
		if len(payload) == 0 {
			if err := m.setActionDocument(topic, nil); err != nil {
				return err
			}
			return m.retireTopic(ctx, host, topic, nil)
		}
		return m.discover(ctx, host, topic, payload)
	}
	if topicText, err := topic.Value(); err == nil && strings.HasPrefix(topicText, "homeassistant/") {
		parts := strings.Split(topicText, "/")
		if len(parts) < 2 || (parts[1] != "device" && parts[1] != "device_automation" && parts[1] != "binary_sensor" && parts[1] != "sensor" && parts[1] != "switch" && parts[1] != "light") {
			m.diagnose(DiagnosticUnknownDiscovery, topic, "")
		} else {
			m.diagnose(DiagnosticInvalidDiscovery, topic, "")
		}
		return nil
	}
	if handled, err := m.observeAction(ctx, host, observation, topic, payload); handled || err != nil {
		return err
	}
	if err := m.cacheSnapshot(topic, payload, quality); err != nil {
		return err
	}
	if components := m.availabilities[topic]; len(components) != 0 {
		for _, component := range components {
			if err := m.observeAvailability(ctx, host, component, topic, payload, quality); err != nil {
				if errors.Is(err, errDiscoveryHostPublish) || ctx.Err() != nil {
					return err
				}
				m.diagnose(DiagnosticAvailability, topic, component.profile.uniqueID)
			}
		}
	}
	if components := m.states[topic]; len(components) != 0 {
		for _, component := range components {
			if err := m.observeStateWithCausality(ctx, host, component, payload, quality, observation.stateCausality(component.correlationScope)); err != nil {
				if errors.Is(err, errDiscoveryHostPublish) || ctx.Err() != nil {
					return err
				}
				m.diagnose(DiagnosticInvalidState, topic, component.profile.uniqueID)
			}
		}
	}
	return nil
}

func (m *DiscoveryMapper) discover(ctx context.Context, host integrations.SessionHost, topic Topic, payload []byte) error {
	parsed, actions, excluded, err := parseDiscoveryDocument(topic, payload)
	if err != nil {
		var component discoveryComponentError
		if errors.As(err, &component) {
			m.diagnose(DiagnosticInvalidDiscovery, topic, component.key)
		} else {
			m.diagnose(DiagnosticInvalidDiscovery, topic, "")
		}
		return nil
	}
	for _, component := range excluded {
		m.diagnose(DiagnosticUnknownDiscovery, topic, component)
	}
	digest := sha256.Sum256(payload)
	if prior, exists := m.documentDigests[topic]; exists && prior == digest {
		return m.setActionDocument(topic, actions)
	}
	previousRuntime := make(map[integrations.SourceKey]*componentRuntime)
	for _, existing := range m.documents[topic] {
		previousRuntime[existing.key] = existing
	}
	if len(m.entities)-len(previousRuntime)+len(parsed) > 1024 {
		m.diagnose(DiagnosticInvalidDiscovery, topic, "")
		return nil
	}
	if len(parsed) == 0 {
		if err := m.retireTopic(ctx, host, topic, nil); err != nil {
			return err
		}
		return m.setActionDocument(topic, actions)
	}
	candidates := make([]integrations.Candidate, 0, len(parsed))
	runtimes := make([]*componentRuntime, 0, len(parsed))
	newKeys := make(map[integrations.SourceKey]struct{}, len(parsed))
	for _, profile := range parsed {
		key, err := m.repository.MatchMQTTSource(ctx, m.instance, profile.identifiers, profile.uniqueID)
		if err != nil {
			m.diagnose(DiagnosticUnconfiguredSource, topic, profile.uniqueID)
			return nil
		}
		if _, duplicate := newKeys[key]; duplicate {
			m.diagnose(DiagnosticInvalidDiscovery, topic, profile.uniqueID)
			return nil
		}
		newKeys[key] = struct{}{}
		profile.admittedTransition = m.admitTransition(key, profile)
		for otherTopic, existing := range m.documents {
			if otherTopic == topic {
				continue
			}
			for _, other := range existing {
				if other.key == key {
					m.diagnose(DiagnosticInvalidDiscovery, topic, profile.uniqueID)
					return nil
				}
			}
		}
		description, err := discoveryDescription(profile, m.mode, m.commands)
		if err != nil {
			m.diagnose(DiagnosticInvalidDiscovery, topic, profile.uniqueID)
			return nil
		}
		candidate, err := integrations.NewCandidate(key, []*registry.CapabilityDescription{description})
		if err != nil {
			m.diagnose(DiagnosticInvalidDiscovery, topic, profile.uniqueID)
			return nil
		}
		if m.mode == ModeActive && profile.kind == componentLight && profile.hasCommand {
			encoding, encodingErr := lightEncodingFor(profile)
			if encodingErr != nil {
				m.diagnose(DiagnosticInvalidDiscovery, topic, profile.uniqueID)
				return nil
			}
			candidate, err = candidate.WithLightEncoding(encoding)
			if err != nil {
				m.diagnose(DiagnosticInvalidDiscovery, topic, profile.uniqueID)
				return nil
			}
		}
		candidates = append(candidates, candidate)
		scope, scopeErr := newCorrelationScope()
		if scopeErr != nil {
			return scopeErr
		}
		runtimes = append(runtimes, &componentRuntime{profile: profile, key: key, correlationScope: scope, latest: -1, availability: make([]domain.Availability, len(profile.availability))})
	}
	// The complete new document is valid. Any live component it omits must
	// lose Host ownership before the following durable topic lookup can block.
	// Unknown cold-start components are resolved from SQL immediately after.
	withdrawal, ok := host.(integrations.ConfiguredSourceWithdrawal)
	if !ok {
		return integrations.ErrInvalidInput
	}
	var fenceFailures []error
	for key := range previousRuntime {
		if _, retained := newKeys[key]; retained {
			continue
		}
		if _, err := withdrawal.FenceSource(ctx, key); err != nil {
			fenceFailures = append(fenceFailures, err)
		}
	}
	if len(fenceFailures) != 0 {
		m.diagnose(DiagnosticRetirement, topic, "")
		return errors.Join(fenceFailures...)
	}
	topicText, _ := topic.Value()
	previous, err := m.repository.MQTTTopicSources(ctx, m.instance, topicText)
	if err != nil {
		m.diagnose(DiagnosticRetirement, topic, "")
		return err
	}
	// Validate every component before replacing the admitted generation. The
	// durable topic list is changed only after Host has admitted the complete
	// candidate batch; a rejected update cannot erase old tombstone provenance.
	keys := make([]integrations.SourceKey, 0, len(runtimes))
	for _, runtime := range runtimes {
		keys = append(keys, runtime.key)
	}
	bindings, err := host.Discover(ctx, candidates)
	if err != nil {
		m.diagnose(DiagnosticInvalidDiscovery, topic, "")
		return nil
	}
	if len(bindings) != len(runtimes) {
		return integrations.ErrIdentityConflict
	}
	if err := m.repository.RememberMQTTTopics(ctx, m.instance, topicText, keys); err != nil {
		// Host admission preceded the SQL failure. Fence every candidate before
		// returning so none can run with an unrecorded discovery generation.
		withdrawal, ok := host.(integrations.ConfiguredSourceWithdrawal)
		if !ok {
			return errors.Join(err, integrations.ErrInvalidInput)
		}
		fenceCtx, fenceCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer fenceCancel()
		for _, key := range keys {
			_, fenceErr := withdrawal.FenceSource(fenceCtx, key)
			err = errors.Join(err, fenceErr)
		}
		return err
	}
	for i, runtime := range runtimes {
		runtime.binding = bindings[i]
		_, runtime.entity, err = bindings[i].IDs()
		if err != nil {
			return err
		}
		if old := previousRuntime[runtime.key]; old != nil && old.profile.fingerprint == runtime.profile.fingerprint && old.binding == runtime.binding {
			// Preserve an in-flight echo only across the same admitted profile.
			// Replacement and A→B→A allocate new scopes, even when Host reuses
			// the SourceKey/session binding for unchanged capability metadata.
			runtime.correlationScope = old.correlationScope
			runtime.availability = append([]domain.Availability(nil), old.availability...)
			runtime.latest = old.latest
			runtime.lastQuality = old.lastQuality
			if old.lastValue != nil {
				copied := *old.lastValue
				runtime.lastValue = &copied
			}
		} else {
			for j := range runtime.availability {
				runtime.availability[j] = domain.AvailabilityUnknown
			}
		}
		if runtime.profile.unsupportedEffects {
			m.diagnose(DiagnosticUnsupportedEffect, topic, runtime.profile.uniqueID)
		}
		if runtime.profile.unsupportedColorModes {
			m.diagnose(DiagnosticUnsupportedColorMode, topic, runtime.profile.uniqueID)
		}
	}
	m.documents[topic] = runtimes
	m.documentDigests[topic] = digest
	if err := m.rebuild(); err != nil {
		return err
	}
	for _, runtime := range runtimes {
		if old := previousRuntime[runtime.key]; old == nil || old.profile.fingerprint != runtime.profile.fingerprint {
			if err := m.replaySnapshots(ctx, host, runtime); err != nil {
				return err
			}
		}
	}
	var removed []integrations.SourceKey
	for _, key := range previous {
		if _, present := newKeys[key]; !present {
			removed = append(removed, key)
		}
	}
	if len(removed) != 0 {
		if err := m.retireTopic(ctx, host, topic, removed); err != nil {
			return err
		}
	}
	return m.setActionDocument(topic, actions)
}

func (m *DiscoveryMapper) retireTopic(ctx context.Context, host integrations.SessionHost, topic Topic, selected []integrations.SourceKey) error {
	text, _ := topic.Value()
	withdrawal, ok := host.(integrations.ConfiguredSourceWithdrawal)
	if !ok {
		return integrations.ErrInvalidInput
	}
	keys := make([]integrations.SourceKey, 0)
	entities := make([]domain.EntityID, 0)
	seen := make(map[integrations.SourceKey]struct{})
	var failures []error
	fence := func(candidates []integrations.SourceKey) {
		for _, key := range candidates {
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			keys = append(keys, key)
			entity, err := withdrawal.FenceSource(ctx, key)
			if _, idErr := entity.Value(); idErr == nil {
				entities = append(entities, entity)
			}
			if err != nil {
				failures = append(failures, err)
			}
		}
	}
	if selected == nil {
		// A live deletion already has admitted keys in this session. Fence
		// those before consulting SQL, which can block or fail. Durable
		// provenance then supplements this set for cold-start deletion.
		known := make([]integrations.SourceKey, 0, len(m.documents[topic]))
		for _, runtime := range m.documents[topic] {
			known = append(known, runtime.key)
		}
		fence(known)
		durable, err := m.repository.MQTTTopicSources(ctx, m.instance, text)
		if err != nil {
			m.diagnose(DiagnosticRetirement, topic, "")
			return errors.Join(append(failures, err)...)
		}
		fence(durable)
	} else {
		fence(selected)
	}
	if selected == nil {
		delete(m.documents, topic)
		delete(m.documentDigests, topic)
	} else {
		old := m.documents[topic]
		kept := make([]*componentRuntime, 0, len(old))
		for _, runtime := range old {
			retired := false
			for _, key := range keys {
				if key == runtime.key {
					retired = true
					break
				}
			}
			if !retired {
				kept = append(kept, runtime)
			}
		}
		m.documents[topic] = kept
	}
	if err := m.rebuild(); err != nil {
		failures = append(failures, err)
	}
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer finishCancel()
	for _, entity := range entities {
		if err := m.repository.Tombstone(finishCtx, entity); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) != 0 {
		m.diagnose(DiagnosticRetirement, topic, "")
	}
	return errors.Join(failures...)
}

func (m *DiscoveryMapper) replaySnapshots(ctx context.Context, host integrations.SessionHost, runtime *componentRuntime) error {
	type replay struct {
		topic        Topic
		cached       cachedSnapshot
		availability bool
	}
	items := make([]replay, 0, len(runtime.profile.availability)+1)
	if cached, found := m.snapshots[runtime.profile.stateTopic]; found {
		items = append(items, replay{topic: runtime.profile.stateTopic, cached: cached})
	}
	for _, config := range runtime.profile.availability {
		if cached, found := m.snapshots[config.topic]; found {
			items = append(items, replay{topic: config.topic, cached: cached, availability: true})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].cached.order < items[j].cached.order })
	for _, item := range items {
		var err error
		if item.availability {
			err = m.observeAvailability(ctx, host, runtime, item.topic, item.cached.payload, item.cached.quality)
		} else {
			err = m.observeState(ctx, host, runtime, item.cached.payload, item.cached.quality)
		}
		if err != nil {
			if errors.Is(err, errDiscoveryHostPublish) || ctx.Err() != nil {
				return err
			}
			m.diagnose(DiagnosticInvalidState, item.topic, runtime.profile.uniqueID)
		}
	}
	return nil
}

func (r *componentRuntime) effectiveAvailability() domain.Availability {
	if len(r.availability) == 0 {
		return domain.AvailabilityAvailable
	}
	switch r.profile.availabilityMode {
	case availabilityLatest:
		if r.latest < 0 {
			return domain.AvailabilityUnknown
		}
		return r.availability[r.latest]
	case availabilityAll:
		unknown := false
		for _, value := range r.availability {
			if value == domain.AvailabilityUnavailable {
				return value
			}
			if value != domain.AvailabilityAvailable {
				unknown = true
			}
		}
		if unknown {
			return domain.AvailabilityUnknown
		}
		return domain.AvailabilityAvailable
	case availabilityAny:
		unknown := false
		for _, value := range r.availability {
			if value == domain.AvailabilityAvailable {
				return value
			}
			if value != domain.AvailabilityUnavailable {
				unknown = true
			}
		}
		if unknown {
			return domain.AvailabilityUnknown
		}
		return domain.AvailabilityUnavailable
	default:
		return domain.AvailabilityUnknown
	}
}

func templateResult(ctx context.Context, program template.Program, payload []byte) (template.Result, error) {
	if !utf8.Valid(payload) || len(payload) > 8192 {
		return template.Result{}, errDiscoveryValue
	}
	value, err := template.String(string(payload))
	if err != nil {
		return template.Result{}, errDiscoveryValue
	}
	variables := []template.Binding{{Name: "value", Value: value}}
	if parsed, err := template.DecodeJSON(payload); err == nil {
		variables = append(variables, template.Binding{Name: "value_json", Value: parsed})
	}
	inputs, err := template.NewInputs(nil, variables, time.Now().UTC(), time.UTC)
	if err != nil {
		return template.Result{}, errDiscoveryValue
	}
	result, err := template.Evaluate(ctx, program, inputs, template.DefaultLimits())
	if err != nil {
		return template.Result{}, errDiscoveryValue
	}
	return result, nil
}

func scalarFromResult(result template.Result, wanted scalar) bool {
	native := result.Native()
	if wanted.kind == scalarBool {
		value, ok := native.Bool()
		return ok && value == wanted.boolean
	}
	return result.Text() == wanted.text
}

func (m *DiscoveryMapper) observeAvailability(ctx context.Context, host integrations.SessionHost, runtime *componentRuntime, topic Topic, payload []byte, quality domain.Quality) error {
	index := -1
	var config availabilityConfig
	for i, item := range runtime.profile.availability {
		if item.topic == topic {
			index, config = i, item
			break
		}
	}
	if index < 0 {
		return errDiscoveryValue
	}
	var online, offline bool
	if config.hasTemplate {
		result, err := templateResult(ctx, config.program, payload)
		if err != nil {
			return err
		}
		online, offline = scalarFromResult(result, config.available), scalarFromResult(result, config.unavailable)
	} else {
		var value any = string(payload)
		if config.available.kind == scalarBool || config.unavailable.kind == scalarBool {
			if !utf8.Valid(payload) {
				return errDiscoveryValue
			}
			if string(payload) == "true" {
				value = true
			} else if string(payload) == "false" {
				value = false
			}
		}
		online, offline = scalarMatches(value, config.available), scalarMatches(value, config.unavailable)
	}
	if online == offline {
		return errDiscoveryValue
	}
	if online {
		runtime.availability[index] = domain.AvailabilityAvailable
	} else {
		runtime.availability[index] = domain.AvailabilityUnavailable
	}
	runtime.latest = index
	if runtime.effectiveAvailability() == domain.AvailabilityUnavailable {
		runtime.lastValue = nil
	}
	if runtime.lastValue != nil {
		quality = runtime.lastQuality
	}
	return m.publishCurrent(ctx, host, runtime, quality)
}

func (m *DiscoveryMapper) publishCurrent(ctx context.Context, host integrations.SessionHost, runtime *componentRuntime, base domain.Quality) error {
	return m.publishCurrentWithCausality(ctx, host, runtime, base, domain.Causality{})
}

func (m *DiscoveryMapper) publishCurrentWithCausality(ctx context.Context, host integrations.SessionHost, runtime *componentRuntime, base domain.Quality, causality domain.Causality) error {
	_, freshness, provenance, err := base.Components()
	if err != nil {
		return err
	}
	availability := runtime.effectiveAvailability()
	var value *domain.StateValue
	if availability == domain.AvailabilityAvailable && runtime.lastValue != nil {
		value = runtime.lastValue
	}
	if value == nil && availability == domain.AvailabilityAvailable {
		availability = domain.AvailabilityUnknown
	}
	quality, err := domain.NewQuality(availability, freshness, provenance)
	if err != nil {
		return err
	}
	observation, err := integrations.NewStateObservation("mqtt", quality, value, nil, time.Now().UTC(), nil)
	if err != nil {
		return err
	}
	// Only the current direct fresh state call supplies causality. Availability
	// and snapshot paths always use the metadata-free wrapper above.
	if value != nil && freshness == domain.FreshnessFresh && provenance == domain.ProvenanceObserved {
		observation, err = observation.WithCausality(causality)
		if err != nil {
			return err
		}
	}
	if err := host.Publish(ctx, runtime.binding, observation); err != nil {
		return fmt.Errorf("%w: %w", errDiscoveryHostPublish, err)
	}
	return nil
}

func measurementValue(c discoveryComponent, text string) (domain.StateValue, error) {
	number, err := numericText(text)
	if err != nil {
		return domain.StateValue{}, err
	}
	profile, err := sensorSemantics(c)
	if err != nil {
		return domain.StateValue{}, err
	}
	unit, err := domain.NewUnitCatalog().Parse(profile.constraints.Unit)
	if err != nil {
		return domain.StateValue{}, errDiscoveryValue
	}
	measurement, err := domain.NewMeasurement(number, unit, int(profile.constraints.Precision), profile.semantic, profile.aggregation)
	if err != nil {
		return domain.StateValue{}, err
	}
	return domain.NewMeasurementValue(measurement)
}

func boolValue(c discoveryComponent, payload []byte, result *template.Result) (domain.StateValue, error) {
	var on, off bool
	if result != nil {
		on, off = scalarFromResult(*result, c.payloadOn), scalarFromResult(*result, c.payloadOff)
	} else {
		if !utf8.Valid(payload) {
			return domain.StateValue{}, errDiscoveryValue
		}
		var value any = string(payload)
		if c.payloadOn.kind == scalarBool || c.payloadOff.kind == scalarBool {
			if string(payload) == "true" {
				value = true
			} else if string(payload) == "false" {
				value = false
			}
		}
		on, off = scalarMatches(value, c.payloadOn), scalarMatches(value, c.payloadOff)
	}
	if on == off {
		return domain.StateValue{}, errDiscoveryValue
	}
	return domain.NewSwitchValue(on), nil
}

func lightState(c discoveryComponent, payload []byte, previous *domain.StateValue) (domain.StateValue, error) {
	object, err := decodeDiscoveryJSON(payload)
	if err != nil {
		return domain.StateValue{}, errDiscoveryValue
	}
	var oldOn bool
	var brightness *float64
	var colorTemp *uint32
	if previous != nil {
		old, ok := previous.Light()
		if !ok {
			return domain.StateValue{}, errDiscoveryValue
		}
		oldOn, brightness, colorTemp, err = old.Components()
		if err != nil {
			return domain.StateValue{}, err
		}
	}
	on := oldOn
	if raw, exists := object["state"]; exists {
		state, ok := raw.(string)
		if !ok || (state != "ON" && state != "OFF") {
			return domain.StateValue{}, errDiscoveryValue
		}
		on = state == "ON"
		if !on {
			brightness, colorTemp = nil, nil
		}
	} else if previous == nil {
		return domain.StateValue{}, errDiscoveryValue
	}
	if raw, exists := object["brightness"]; exists {
		if !c.brightness {
			return domain.StateValue{}, errDiscoveryValue
		}
		number, ok := raw.(json.Number)
		if !ok {
			return domain.StateValue{}, errDiscoveryValue
		}
		scale, err := strconv.ParseFloat(string(number), 64)
		if err != nil || math.IsInf(scale, 0) || math.IsNaN(scale) || scale < 0 || scale > c.brightnessScale {
			return domain.StateValue{}, errDiscoveryValue
		}
		value := scale * 100 / c.brightnessScale
		brightness = &value
	}
	if raw, exists := object["color_temp"]; exists {
		if !c.colorTemp {
			return domain.StateValue{}, errDiscoveryValue
		}
		number, ok := raw.(json.Number)
		if !ok {
			return domain.StateValue{}, errDiscoveryValue
		}
		parsed, err := strconv.ParseUint(string(number), 10, 32)
		if err != nil || parsed < uint64(c.minMireds) || parsed > uint64(c.maxMireds) {
			return domain.StateValue{}, errDiscoveryValue
		}
		value := uint32(parsed)
		colorTemp = &value
	}
	value, err := domain.NewLightValue(on, brightness, colorTemp)
	if err != nil {
		return domain.StateValue{}, err
	}
	return domain.NewLightStateValue(value)
}

func (m *DiscoveryMapper) observeState(ctx context.Context, host integrations.SessionHost, runtime *componentRuntime, payload []byte, quality domain.Quality) error {
	return m.observeStateWithCausality(ctx, host, runtime, payload, quality, domain.Causality{})
}

func (m *DiscoveryMapper) observeStateWithCausality(ctx context.Context, host integrations.SessionHost, runtime *componentRuntime, payload []byte, quality domain.Quality, causality domain.Causality) error {
	profile := runtime.profile
	var evaluated *template.Result
	if profile.hasValueTemplate {
		result, err := templateResult(ctx, profile.valueProgram, payload)
		if err != nil {
			return err
		}
		evaluated = &result
	}
	var value domain.StateValue
	var err error
	switch profile.kind {
	case componentBinary, componentSwitch:
		value, err = boolValue(profile, payload, evaluated)
	case componentSensor:
		text := string(payload)
		if evaluated != nil {
			text = evaluated.Text()
		}
		value, err = measurementValue(profile, text)
	case componentLight:
		if evaluated != nil {
			return errDiscoveryValue
		}
		previous := runtime.lastValue
		if previous != nil {
			_, oldFreshness, oldProvenance, oldErr := runtime.lastQuality.Components()
			_, newFreshness, newProvenance, newErr := quality.Components()
			if oldErr != nil || newErr != nil || oldFreshness != newFreshness || oldProvenance != newProvenance {
				previous = nil
			}
		}
		value, err = lightState(profile, payload, previous)
		// A partial light report may inherit display fields. An echo must
		// never turn those cached fields into fresh confirmation evidence.
		if err == nil && causality != (domain.Causality{}) {
			direct, directErr := lightState(profile, payload, nil)
			currentLight, _ := value.Light()
			directLight, _ := direct.Light()
			if directErr != nil || currentLight != directLight {
				causality = domain.Causality{}
			}
		}
	default:
		return errDiscoveryValue
	}
	if err != nil {
		return err
	}
	runtime.lastValue = &value
	runtime.lastQuality = quality
	return m.publishCurrentWithCausality(ctx, host, runtime, quality, causality)
}

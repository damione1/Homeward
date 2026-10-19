package helpers

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"time"

	"github.com/damione1/Homeward/contracts"
	helperwire "github.com/damione1/Homeward/gen/home/helpers/v1"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/scope"
)

type pilotEntry struct {
	definition Definition
	key        integrations.SourceKey
	binding    integrations.Binding
	handle     Handle
}

// Pilot is a local, session-owned source. No operation has an external device
// transport. The supervisor owns retry and lifecycle; repository owns CAS.
type Pilot struct {
	repository  *Repository
	buttons     scope.HelperButtonPolicy
	definitions []Definition
	mu          sync.RWMutex
	host        integrations.SessionHost
	entries     map[domain.EntityID]pilotEntry
	fault       chan error
	ready       bool
}

func Definitions(candidate bundle.Candidate) ([]Definition, error) {
	if !candidate.Valid() {
		return nil, failure(InvalidDefinition)
	}
	result := make([]Definition, 0)
	add := func(definition Definition, err error) error {
		if err != nil || !definition.Valid() {
			return failure(InvalidDefinition)
		}
		result = append(result, definition)
		return nil
	}
	for _, item := range candidate.Booleans() {
		if err := add(BooleanDefinition(item)); err != nil {
			return nil, err
		}
	}
	for _, item := range candidate.Numbers() {
		if err := add(NumberDefinition(item)); err != nil {
			return nil, err
		}
	}
	for _, item := range candidate.Counters() {
		if err := add(CounterDefinition(item)); err != nil {
			return nil, err
		}
	}
	for _, item := range candidate.Buttons() {
		if err := add(ButtonDefinition(item)); err != nil {
			return nil, err
		}
	}
	for _, item := range candidate.Datetimes() {
		if err := add(DatetimeDefinition(item)); err != nil {
			return nil, err
		}
	}
	for _, item := range candidate.Selects() {
		if err := add(SelectDefinition(item)); err != nil {
			return nil, err
		}
	}
	for _, item := range candidate.Texts() {
		if err := add(TextDefinition(item)); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func nilPilotInterface(v any) bool {
	if v == nil {
		return true
	}
	value := reflect.ValueOf(v)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func NewPilotFactory(repository *Repository, definitions []Definition, buttons scope.HelperButtonPolicy) (integrations.PilotFactory, error) {
	if repository == nil || len(definitions) == 0 || len(definitions) > 256 {
		return nil, failure(InvalidInput)
	}
	seen := make(map[domain.HAAlias]bool, len(definitions))
	copyDefinitions := append([]Definition(nil), definitions...)
	for _, definition := range copyDefinitions {
		if !definition.Valid() || seen[definition.alias()] {
			return nil, failure(InvalidDefinition)
		}
		seen[definition.alias()] = true
	}
	return func() integrations.Pilot {
		return &Pilot{repository: repository, definitions: append([]Definition(nil), copyDefinitions...), buttons: buttons}
	}, nil
}

func (p *Pilot) CommandDriver() integrations.CommandDriver { return p }

// Each helper is its own logical source device. Sharing a DeviceID would make
// their identical schema/endpoint command slots ambiguous in the sequencer.
// The alias is an opaque stable identity here, never an effect destination.
func ConfiguredSourceKey(alias domain.HAAlias) (integrations.SourceKey, error) {
	name, err := alias.Value()
	if err != nil {
		return integrations.SourceKey{}, err
	}
	return integrations.NewSourceKey(name, "value")
}

func (p *Pilot) Start(ctx context.Context, host integrations.SessionHost, tasks integrations.Tasks) error {
	if p == nil || p.repository == nil || nilContext(ctx) || nilPilotInterface(host) || nilPilotInterface(tasks) {
		return failure(InvalidInput)
	}
	candidates := make([]integrations.Candidate, 0, len(p.definitions))
	keys := make([]integrations.SourceKey, 0, len(p.definitions))
	for _, definition := range p.definitions {
		key, err := ConfiguredSourceKey(definition.alias())
		if err != nil {
			return err
		}
		description, err := Description(definition, p.buttons)
		if err != nil {
			return err
		}
		candidate, err := integrations.NewCandidate(key, []*registrywire.CapabilityDescription{description})
		if err != nil {
			return err
		}
		keys = append(keys, key)
		candidates = append(candidates, candidate)
	}
	bindings, err := host.Discover(ctx, candidates)
	if err != nil {
		return err
	}
	if len(bindings) != len(p.definitions) {
		return failure(BindingMismatch)
	}
	entries := make(map[domain.EntityID]pilotEntry, len(bindings))
	for index, binding := range bindings {
		_, entity, err := binding.IDs()
		if err != nil {
			return err
		}
		handle, err := p.repository.Bind(ctx, entity, p.definitions[index])
		if err != nil {
			return err
		}
		entry := pilotEntry{definition: p.definitions[index], key: keys[index], binding: binding, handle: handle}
		snapshot, err := p.repository.Restore(ctx, handle, entry.definition)
		if err != nil {
			return err
		}
		if err := publishSnapshot(ctx, host, entry, snapshot, domain.ProvenanceRestored, domain.Causality{}); err != nil {
			return err
		}
		entries[entity] = entry
	}
	// A command can finish after Start returns. Make an indeterminate COMMIT or
	// failed projection a supervised session failure so Host withdraws stale
	// observations and the next pilot restores SQL before accepting commands.
	fault := make(chan error, 1)
	if err := tasks.Go(func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return nil
		case err := <-fault:
			return err
		}
	}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	p.host, p.entries, p.fault, p.ready = host, entries, fault, true
	p.mu.Unlock()
	return nil
}

func (p *Pilot) Stop(context.Context) error {
	if p == nil {
		return failure(InvalidInput)
	}
	p.mu.Lock()
	p.ready = false
	p.host = nil
	p.entries = nil
	p.fault = nil
	p.mu.Unlock()
	return nil
}

func publishSnapshot(ctx context.Context, host integrations.SessionHost, entry pilotEntry, snapshot Snapshot, provenance domain.Provenance, causality domain.Causality) error {
	value, present := snapshot.Value()
	availability, freshness := domain.AvailabilityUnknown, domain.FreshnessStale
	var stateValue *domain.StateValue
	if present {
		number, ok := snapshot.Revision().Number()
		if !ok {
			return failure(Corrupt)
		}
		state, err := domain.NewHelperStateValue(value, number)
		if err != nil {
			return err
		}
		stateValue = &state
		availability, freshness = domain.AvailabilityAvailable, domain.FreshnessFresh
	}
	quality, err := domain.NewQuality(availability, freshness, provenance)
	if err != nil {
		return err
	}
	observation, err := integrations.NewStateObservation("value", quality, stateValue, nil, time.Now().UTC(), nil)
	if err != nil {
		return err
	}
	if _, _, err := causality.Components(); err == nil {
		observation, err = observation.WithCausality(causality)
		if err != nil {
			return err
		}
	}
	return host.Publish(ctx, entry.binding, observation)
}

func admittedOperation(operation helperwire.HelperOperation) Operation {
	switch operation {
	case helperwire.HelperOperation_HELPER_OPERATION_SET:
		return OperationSet
	case helperwire.HelperOperation_HELPER_OPERATION_TOGGLE:
		return OperationToggle
	case helperwire.HelperOperation_HELPER_OPERATION_INCREMENT:
		return OperationIncrement
	case helperwire.HelperOperation_HELPER_OPERATION_DECREMENT:
		return OperationDecrement
	case helperwire.HelperOperation_HELPER_OPERATION_RESET:
		return OperationReset
	case helperwire.HelperOperation_HELPER_OPERATION_PRESS:
		return OperationPress
	case helperwire.HelperOperation_HELPER_OPERATION_NEXT:
		return OperationNext
	case helperwire.HelperOperation_HELPER_OPERATION_PREVIOUS:
		return OperationPrevious
	default:
		return OperationInvalid
	}
}

func (p *Pilot) failSession(err error) {
	p.mu.Lock()
	p.ready = false
	if p.fault != nil {
		select {
		case p.fault <- err:
		default:
		}
	}
	p.mu.Unlock()
}

// The local repository's successful CAS is a protocol-level receipt for this
// helper alone. It says nothing about an external device or automation.
type helperCommitConfirmation struct {
	commandID domain.CommandID
	entity    domain.EntityID
	handle    Handle
	kind      Kind
	operation helperwire.HelperOperation
	expected  uint64
	value     Value
	committed Snapshot
}

func (c helperCommitConfirmation) QualifyProtocolConfirmation(command integrations.AdmittedCommand) error {
	id, _, entity, err := command.Identity()
	if err != nil || id != c.commandID || entity != c.entity || !c.committed.Valid() ||
		c.committed.handle != c.handle || c.handle.entity != c.entity || c.committed.kind != c.kind {
		return failure(Corrupt)
	}
	request, err := command.Request()
	if err != nil || request.CapabilitySchemaId != contracts.HelperSchema || request.Endpoint != "value" ||
		request.Arguments == nil || request.Arguments.GetHelper() == nil {
		return failure(Corrupt)
	}
	action := request.Arguments.GetHelper()
	if action.ExpectedHelperRevision == nil || *action.ExpectedHelperRevision != c.expected || action.Operation != c.operation {
		return failure(Corrupt)
	}
	if c.operation == helperwire.HelperOperation_HELPER_OPERATION_SET {
		requested, err := contracts.AdmitHelperSetValue(action.Value)
		if err != nil || requested != c.value {
			return failure(Corrupt)
		}
	} else if action.Value != nil {
		return failure(Corrupt)
	}
	revision, ok := c.committed.Revision().Number()
	if !ok || revision != c.expected+1 {
		return failure(Corrupt)
	}
	if value, present := c.committed.Value(); !present || value != c.value {
		return failure(Corrupt)
	}
	return nil
}

func (p *Pilot) Dispatch(ctx context.Context, command integrations.AdmittedCommand) error {
	if p == nil || nilContext(ctx) || !command.ReadyForTransport() {
		return integrations.ErrInactiveSession
	}
	commandID, _, entity, err := command.Identity()
	if err != nil {
		return err
	}
	request, err := command.Request()
	if err != nil {
		return err
	}
	if request.CapabilitySchemaId != contracts.HelperSchema || request.Endpoint != "value" || request.Arguments == nil {
		return failure(InvalidInput)
	}
	action := request.Arguments.GetHelper()
	if action == nil || action.ExpectedHelperRevision == nil || *action.ExpectedHelperRevision > math.MaxInt64 {
		return failure(InvalidInput)
	}
	p.mu.RLock()
	entry, found := p.entries[entity]
	host, ready := p.host, p.ready
	p.mu.RUnlock()
	if !found || !ready || nilPilotInterface(host) {
		return integrations.ErrInactiveSession
	}
	if entry.definition.kind == KindButtonPress && p.buttons.Decision(entry.definition.alias()) != scope.HelperButtonLocalPress {
		return failure(NoAuthorizedTarget)
	}
	operation := admittedOperation(action.Operation)
	if operation == OperationInvalid {
		return failure(InvalidValue)
	}
	var target Value
	if operation == OperationSet {
		target, err = contracts.AdmitHelperSetValue(action.Value)
		if err != nil {
			return failure(InvalidValue)
		}
	} else if action.Value != nil {
		return failure(InvalidValue)
	}
	current, err := p.repository.Restore(ctx, entry.handle, entry.definition)
	if err != nil {
		return err
	}
	revision, ok := current.Revision().Number()
	if !ok || revision != *action.ExpectedHelperRevision {
		return failure(Conflict)
	}
	value, err := Compute(entry.definition, current, operation, target, time.Now().UTC())
	if err != nil {
		return err
	}
	if !command.ReadyForTransport() {
		return integrations.ErrInactiveSession
	}
	committed, err := p.repository.CompareAndSwap(ctx, entry.handle, entry.definition, current.Revision(), value)
	if err != nil {
		var helperError *Error
		if errors.As(err, &helperError) && helperError.Code() == CommitUnknown {
			p.failSession(err)
		}
		return err
	}
	causality, err := command.Causality()
	if err != nil {
		p.failSession(err)
		return err
	}
	if err := publishSnapshot(ctx, host, entry, committed, domain.ProvenanceObserved, causality); err != nil {
		p.failSession(err)
		return err
	}
	if err := command.Acknowledge(); err != nil {
		return err
	}
	return command.ConfirmProtocol(helperCommitConfirmation{
		commandID: commandID, entity: entity, handle: entry.handle, kind: entry.definition.kind,
		operation: action.Operation, expected: *action.ExpectedHelperRevision, value: value, committed: committed,
	})
}

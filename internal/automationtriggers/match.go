package automationtriggers

import (
	"bytes"
	"math"
	"strconv"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/mqtt"
	"github.com/damione1/Homeward/internal/sequencer"
)

// observedText preserves unknown, unavailable and missing attributes as
// distinct states. Restored or stale values never become an active edge.
func observedText(attribute ir.TriggerAttribute, state domain.ObservedState) (string, bool) {
	_, quality, _, _, _, err := state.Components()
	if err != nil {
		return "", false
	}
	availability, freshness, provenance, err := quality.Components()
	if err != nil || freshness != domain.FreshnessFresh || provenance != domain.ProvenanceObserved {
		return "", false
	}
	if attribute == ir.AttributeUnspecified {
		switch availability {
		case domain.AvailabilityUnknown:
			return "unknown", true
		case domain.AvailabilityUnavailable:
			return "unavailable", true
		case domain.AvailabilityAvailable:
		default:
			return "", false
		}
	}
	value, ok := state.CurrentValue()
	if !ok {
		return "", false
	}
	switch attribute {
	case ir.AttributeBrightness:
		light, ok := value.Light()
		if !ok {
			return "", false
		}
		_, brightness, _, err := light.Components()
		if err != nil || brightness == nil {
			return "", false
		}
		return strconv.FormatFloat(*brightness, 'f', -1, 64), true
	case ir.AttributeColorTemp:
		light, ok := value.Light()
		if !ok {
			return "", false
		}
		_, _, temp, err := light.Components()
		if err != nil || temp == nil {
			return "", false
		}
		return strconv.FormatUint(uint64(*temp), 10), true
	case ir.AttributeUnspecified:
		switch kind, err := value.Kind(); {
		case err != nil:
			return "", false
		case kind == domain.ValueSwitch:
			on, _ := value.Switch()
			if on {
				return "on", true
			}
			return "off", true
		case kind == domain.ValueLight:
			light, _ := value.Light()
			on, _, _, err := light.Components()
			if err != nil {
				return "", false
			}
			if on {
				return "on", true
			}
			return "off", true
		case kind == domain.ValueMeasurement:
			measurement, _ := value.Measurement()
			components, err := measurement.Components()
			if err != nil {
				return "", false
			}
			return strconv.FormatFloat(components.Value, 'f', -1, 64), true
		case kind == domain.ValueFan:
			fan, _ := value.Fan()
			text, err := fan.String()
			return text, err == nil
		case kind == domain.ValueClimate:
			return "", false // No HA climate mode string is inferred from a typed setpoint.
		case kind == domain.ValueSolar:
			solar, ok := value.Solar()
			if !ok {
				return "", false
			}
			input, err := solar.Components()
			if err != nil {
				return "", false
			}
			switch input.Horizon {
			case domain.SolarAboveHorizon:
				return "above_horizon", true
			case domain.SolarBelowHorizon:
				return "below_horizon", true
			case domain.SolarHorizonUnspecified:
				return "", false
			default:
				return "", false
			}
		case kind == domain.ValueSolarEvent:
			event, ok := value.SolarEvent()
			if !ok {
				return "", false
			}
			_, occurrence, err := event.Components()
			if err != nil {
				return "", false
			}
			at, reason, occurrenceKind, err := occurrence.Components()
			if err != nil {
				return "", false
			}
			switch occurrenceKind {
			case domain.SolarOccurrencePresent:
				return at.UTC().Format(time.RFC3339Nano), true
			case domain.SolarOccurrenceAbsent:
				return "unknown", reason == domain.SolarNoEventWithinSearchHorizon
			case domain.SolarOccurrenceUnspecified:
				return "", false
			default:
				return "", false
			}
		default:
			return "", false
		}
	default:
		return "", false
	}
}

func numericMatch(trigger ir.Trigger, state domain.ObservedState) (float64, bool, bool) {
	if trigger.Kind() != ir.TriggerNumericState {
		return 0, false, false
	}
	value, ok := state.CriticalValue()
	if !ok {
		return 0, false, false
	}
	var number float64
	switch trigger.Attribute() {
	case ir.AttributeUnspecified:
		measurement, ok := value.Measurement()
		if !ok {
			return 0, false, false
		}
		components, err := measurement.Components()
		if err != nil {
			return 0, false, false
		}
		number = components.Value
	case ir.AttributeBrightness:
		light, ok := value.Light()
		if !ok {
			return 0, false, false
		}
		_, brightness, _, err := light.Components()
		if err != nil || brightness == nil {
			return 0, false, false
		}
		number = *brightness
	case ir.AttributeColorTemp:
		light, ok := value.Light()
		if !ok {
			return 0, false, false
		}
		_, _, colorTemp, err := light.Components()
		if err != nil || colorTemp == nil {
			return 0, false, false
		}
		number = float64(*colorTemp)
	default:
		return 0, false, false
	}
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, false, false
	}
	if above, present := trigger.Above(); present && !(number > above) {
		return number, false, true
	}
	if below, present := trigger.Below(); present && !(number < below) {
		return number, false, true
	}
	return number, true, true
}

func numericMeasurementUnit(trigger ir.Trigger, state domain.ObservedState) (domain.Unit, bool) {
	if trigger.Kind() != ir.TriggerNumericState || trigger.Attribute() != ir.AttributeUnspecified {
		return domain.Unit{}, false
	}
	value, ok := state.CriticalValue()
	if !ok {
		return domain.Unit{}, false
	}
	measurement, ok := value.Measurement()
	if !ok {
		return domain.Unit{}, false
	}
	components, err := measurement.Components()
	if err != nil {
		return domain.Unit{}, false
	}
	return components.Unit, true
}

func (s *Service) processEntryLocked(entry sequencer.Entry) {
	if s.suspended || !s.admissionsOpen {
		return
	}
	epoch, sequence, err := entry.Cursor().Components()
	if err != nil {
		s.suspendLocked(DegradedSource)
		return
	}
	currentEpoch, currentSeq, err := s.cursor.Components()
	if err != nil || currentEpoch != epoch || sequence > currentSeq+1 {
		s.suspendLocked(DegradedGap)
		return
	}
	if sequence <= currentSeq {
		return
	}
	switch entry.Kind() {
	case sequencer.KindState:
		entity, state, _, ok := entry.State()
		if !ok {
			s.suspendLocked(DegradedSource)
			return
		}
		charge, err := sequencer.EstimateTriggerStateBytes(entity, state)
		if err != nil {
			s.suspendLocked(DegradedSource)
			return
		}
		prospective := s.estimatedChargeLocked(s.occurrenceCountLocked(), s.seenBytes) + charge - s.stateCharges[entity]
		if prospective > MaxEngineCharge-256 {
			s.suspendLocked(DegradedCapacity)
			return
		}
		previous := s.states[entity]
		s.projectionBytes += charge - s.stateCharges[entity]
		s.stateCharges[entity] = charge
		s.states[entity] = state
		s.cursor = entry.Cursor()
		for i, r := range s.plan.rules {
			if r.entity != entity {
				continue
			}
			switch r.trigger.Kind() {
			case ir.TriggerState:
				s.processStateLocked(i, previous, state, entry)
			case ir.TriggerNumericState:
				s.processNumericLocked(i, state, entry)
			}
		}
	case sequencer.KindEvent:
		s.cursor = entry.Cursor()
		entity, event, ok := entry.Event()
		if !ok {
			s.suspendLocked(DegradedSource)
			return
		}
		code, _, _, err := event.Components()
		if err != nil {
			s.suspendLocked(DegradedSource)
			return
		}
		instance, _, _, ok := entry.Origin()
		if !ok {
			s.suspendLocked(DegradedSource)
			return
		}
		record, hasRecord := s.registry[entity]
		for i, r := range s.plan.rules {
			switch r.trigger.Kind() {
			case ir.TriggerDeviceMQTT:
				if !hasRecord || instance != s.plan.mqttInstance {
					continue
				}
				device, _, err := record.IDs()
				if err != nil || device != r.device || code != "z2m."+r.trigger.Subtype() {
					continue
				}
				s.fireLocked(i, entry.Cursor(), entity, nil)
			case ir.TriggerEvent:
				name, err := r.trigger.EventType().Value()
				if err == nil && name == code {
					s.fireLocked(i, entry.Cursor(), entity, nil)
				}
			}
		}
	case sequencer.KindRegistry:
		records, ok := entry.Registry()
		removed, removedOK := entry.RegistryRemoved()
		if !ok || !removedOK {
			s.suspendLocked(DegradedSource)
			return
		}
		charges := make([]int, len(records))
		prospective := s.estimatedChargeLocked(s.occurrenceCountLocked(), s.seenBytes)
		for index, record := range records {
			_, entity, err := record.IDs()
			if err != nil {
				s.suspendLocked(DegradedSource)
				return
			}
			prospective -= s.recordCharges[entity]
			if record.Retired() {
				prospective -= s.stateCharges[entity]
			} else {
				charges[index], err = sequencer.EstimateTriggerRegistryBytes(record)
				if err != nil {
					s.suspendLocked(DegradedSource)
					return
				}
				prospective += charges[index]
			}
		}
		for _, entity := range removed {
			if _, err := entity.Value(); err != nil {
				s.suspendLocked(DegradedSource)
				return
			}
			prospective -= s.recordCharges[entity] + s.stateCharges[entity]
		}
		if prospective > MaxEngineCharge-256 {
			s.suspendLocked(DegradedCapacity)
			return
		}
		for index, record := range records {
			_, entity, _ := record.IDs()
			if record.Retired() {
				s.removeEntityLocked(entity)
			} else {
				s.projectionBytes += charges[index] - s.recordCharges[entity]
				s.registry[entity] = record
				s.recordCharges[entity] = charges[index]
			}
		}
		for _, entity := range removed {
			s.removeEntityLocked(entity)
		}
		s.cursor = entry.Cursor()
	case sequencer.KindNativeMQTTEdge:
		edge, ok := entry.NativeMQTTEdge()
		instance, source, _, hasOrigin := entry.Origin()
		if !ok || !hasOrigin || instance != s.plan.mqttInstance {
			s.suspendLocked(DegradedSource)
			return
		}
		session, stampedSeq, err := source.Components()
		topicText, payload, edgeSeq, edgeErr := edge.Components()
		if err != nil || edgeErr != nil || stampedSeq == nil || *stampedSeq != edgeSeq {
			s.suspendLocked(DegradedSource)
			return
		}
		topic, err := mqtt.ParseTopic(topicText)
		if err != nil {
			s.suspendLocked(DegradedSource)
			return
		}
		s.cursor = entry.Cursor()
		for i, r := range s.plan.rules {
			if r.trigger.Kind() != ir.TriggerMQTT || !r.trigger.Topic().Matches(topic) {
				continue
			}
			last := s.mqttWater[i]
			if last.session == session && edgeSeq <= last.seq {
				continue
			}
			s.mqttWater[i] = mqttHighWater{session: session, seq: edgeSeq}
			if expected, hasPayload := r.trigger.Payload(); hasPayload && !bytes.Equal(payload, []byte(expected)) {
				continue
			}
			s.fireLocked(i, entry.Cursor(), domain.EntityID{}, payload)
			if s.suspended {
				return
			}
		}
	case sequencer.KindHealth, sequencer.KindCapabilities:
		// These entries advance the cursor without becoming trigger events.
		s.cursor = entry.Cursor()
	case sequencer.KindUnspecified:
		s.suspendLocked(DegradedSource)
	default:
		// The native MQTT variant joins this exhaustive switch with the shared
		// sequencer glue. Unknown future variants are never admitted as a fire.
		s.suspendLocked(DegradedSource)
	}
}

// removeEntityLocked invalidates only the trigger projection and waiters for
// an identity retired at this sequence position. Other entities keep their
// already admitted monotonic deadlines.
func (s *Service) removeEntityLocked(entity domain.EntityID) {
	s.projectionBytes -= s.stateCharges[entity] + s.recordCharges[entity]
	delete(s.states, entity)
	delete(s.stateCharges, entity)
	delete(s.registry, entity)
	delete(s.recordCharges, entity)
	for i, r := range s.plan.rules {
		if r.entity != entity {
			continue
		}
		delete(s.pending, i)
		delete(s.armed, i)
		delete(s.numericUnits, i)
	}
	s.signalWake()
}

func (s *Service) processStateLocked(i int, old, next domain.ObservedState, entry sequencer.Entry) {
	trigger := s.plan.rules[i].trigger
	oldText, oldPresent := observedText(trigger.Attribute(), old)
	newText, newPresent := observedText(trigger.Attribute(), next)
	if pending, exists := s.pending[i]; exists {
		still := newPresent && (pending.oldText != "" && triggerFromOnly(trigger) && newText != pending.oldText ||
			!triggerFromOnly(trigger) && newText == pending.newText)
		if !still {
			delete(s.pending, i)
			s.signalWake()
		} else {
			// An unrelated attribute update is another HA state_changed edge,
			// but cannot reset an already satisfied bounded for interval.
			return
		}
	}
	if !newPresent {
		return
	}
	from, hasFrom := trigger.From()
	to, hasTo := trigger.To()
	if trigger.Attribute() != ir.AttributeUnspecified && oldPresent && oldText == newText {
		return
	}
	if hasFrom && (!oldPresent || oldText != from) {
		return
	}
	if hasTo && newText != to {
		return
	}
	if hasFrom || hasTo {
		if oldPresent && oldText == newText {
			return
		}
	}
	if duration, hasFor := trigger.For(); hasFor {
		s.startForLocked(i, duration, entry.AdmissionTick(), oldText, newText)
	} else {
		s.fireLocked(i, entry.Cursor(), s.plan.rules[i].entity, nil)
	}
}

func triggerFromOnly(trigger ir.Trigger) bool {
	_, hasFrom := trigger.From()
	_, hasTo := trigger.To()
	return hasFrom && !hasTo
}

func (s *Service) processNumericLocked(i int, next domain.ObservedState, entry sequencer.Entry) {
	if unit, present := numericMeasurementUnit(s.plan.rules[i].trigger, next); present {
		if previous, known := s.numericUnits[i]; known && previous != unit {
			s.suspendLocked(DegradedSource)
			return
		}
		s.numericUnits[i] = unit
	}
	_, matching, valid := numericMatch(s.plan.rules[i].trigger, next)
	if !valid {
		if _, exists := s.pending[i]; exists {
			delete(s.pending, i)
			s.signalWake()
		}
		return
	}
	if !matching {
		s.armed[i] = true
		if _, exists := s.pending[i]; exists {
			delete(s.pending, i)
			s.signalWake()
		}
		return
	}
	if !s.armed[i] {
		return
	}
	s.armed[i] = false
	if duration, hasFor := s.plan.rules[i].trigger.For(); hasFor {
		s.startForLocked(i, duration, entry.AdmissionTick(), "", "")
	} else {
		s.fireLocked(i, entry.Cursor(), s.plan.rules[i].entity, nil)
	}
}

func (s *Service) startForLocked(i int, duration time.Duration, tick time.Duration, oldText, newText string) {
	_, replacing := s.pending[i]
	if tick < 0 || duration <= 0 || int64(tick) > (1<<63-1)-int64(duration) ||
		!replacing && (len(s.pending) >= MaxTriggers || s.estimatedChargeLocked(s.occurrenceCountLocked(), s.seenBytes)+128 > MaxEngineCharge-256) {
		s.suspendLocked(DegradedCapacity)
		return
	}
	s.pending[i] = pendingFor{deadline: tick + duration, generation: s.generation, oldText: oldText, newText: newText}
	s.signalWake()
}

func (s *Service) fireLocked(i int, cursor sequencer.Cursor, entity domain.EntityID, payload []byte) {
	if i >= s.permanentRules && s.waitOwners[i] == nil {
		return
	}

	r := s.plan.rules[i]
	result := Result{kind: ResultFired, automation: r.automation, triggerKey: r.trigger.Key(),
		at: s.clock.Now().UTC(), cursor: cursor, entity: entity, payload: payload}
	if owner := s.waitOwners[i]; owner != nil {
		s.fireWaitLocked(owner, result, r)
		return
	}
	s.enqueueLocked(result)
}

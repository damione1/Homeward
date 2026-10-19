package scenes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	hascene "github.com/damione1/Homeward/compat/ha/scene"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"google.golang.org/protobuf/proto"
)

// PreparationSlot belongs to one occurrence. Its first result, including a
// refusal, is retained by the caller; the service keeps no unbounded plan map.
type PreparationSlot struct {
	once          sync.Once
	service       *Service
	ref           SceneRef
	actor         domain.ActorID
	invocation    domain.CommandID
	expiresAt     time.Time
	hasTransition bool
	transition    float64
	prepared      PreparedScene
	err           error
}

func NewPreparationSlot(service *Service, ref SceneRef, actor domain.ActorID, invocation domain.CommandID, transition *float64, expiresAt time.Time) (*PreparationSlot, error) {
	if service == nil || nilPort(service.clock) || expiresAt.IsZero() || !expiresAt.After(service.clock.Now()) {
		return nil, ErrInvalidScene
	}
	if _, err := ref.ID(); err != nil {
		return nil, err
	}
	if _, err := actor.Value(); err != nil {
		return nil, err
	}
	if _, err := invocation.Value(); err != nil {
		return nil, err
	}
	if transition != nil && (math.IsNaN(*transition) || math.IsInf(*transition, 0) || *transition < 0 || *transition > 6553) {
		return nil, ErrInvalidScene
	}
	slot := &PreparationSlot{service: service, ref: ref, actor: actor, invocation: invocation, expiresAt: expiresAt}
	if transition != nil {
		slot.hasTransition, slot.transition = true, *transition
	}
	return slot, nil
}

func (slot *PreparationSlot) PrepareOnce(ctx context.Context) (PreparedScene, error) {
	if slot == nil || slot.service == nil || ctx == nil {
		return PreparedScene{}, ErrInvalidScene
	}
	slot.once.Do(func() {
		var transition *float64
		if slot.hasTransition {
			transition = &slot.transition
		}
		plan, err := slot.service.prepare(ctx, slot.ref, slot.actor, slot.invocation, transition, slot.expiresAt)
		slot.prepared, slot.err = PreparedScene{plan: plan}, err
	})
	if slot.err == nil {
		if err := slot.service.authorizePreparedResult(ctx, slot.prepared.plan); err != nil {
			return PreparedScene{}, err
		}
	}
	return slot.prepared, slot.err
}

func memberCommandID(invocation domain.CommandID, index int) (domain.CommandID, error) {
	text, err := invocation.Value()
	if err != nil {
		return domain.CommandID{}, err
	}
	// The ordinal preserves two different members even if a malformed source
	// repeats an alias. The full invocation is included without truncation.
	sum := sha256.Sum256([]byte(text + "\x00" + strconv.Itoa(index)))
	return domain.ParseCommandID("scene-" + hex.EncodeToString(sum[:]))
}

func (s *Service) currentSpecs(ctx context.Context, ref SceneRef) ([]memberSpec, error) {
	if _, err := ref.ID(); err != nil {
		return nil, err
	}
	if ref.kind == SceneDeclarative {
		definition, ok := s.catalogue.Definition(ref.id)
		if !ok || definition.Source().Digest() != ref.digest {
			return nil, ErrSceneUnavailable
		}
		members := definition.Members()
		specs := make([]memberSpec, len(members))
		for i, member := range members {
			specs[i] = memberSpec{origin: originHA, alias: member.Alias(), source: member.Source(), ha: member}
		}
		return specs, nil
	}
	var specs []memberSpec
	err := s.generation.WithCurrent(ctx, func(revision uint64) error {
		epoch, err := s.capture.CurrentAdmissionEpoch()
		if err != nil || epoch == "" || revision == 0 {
			return ErrSceneUnavailable
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return ErrSceneClosed
		}
		s.purgeLocked(s.clock.Now(), epoch, revision)
		entry := s.temporary[ref.id]
		if entry == nil || entry.ref != ref {
			return ErrSceneUnavailable
		}
		specs = append([]memberSpec(nil), entry.members...)
		return nil
	})
	return specs, err
}

func (s *Service) prepare(ctx context.Context, ref SceneRef, actor domain.ActorID, invocation domain.CommandID, transition *float64, expiresAt time.Time) (*scenePlan, error) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, ErrSceneClosed
	}
	specs, err := s.currentSpecs(ctx, ref)
	if err != nil {
		return nil, err
	}
	if len(specs) == 0 || len(specs) > maxSceneMembers {
		return nil, ErrInvalidScene
	}
	plan := &scenePlan{service: s, ref: ref, actor: actor, invocation: invocation, expiresAt: expiresAt,
		members: make([]preparedMember, 0, len(specs))}
	for i, spec := range specs {
		commandID, err := memberCommandID(invocation, i)
		if err != nil {
			return nil, err
		}
		member := s.prepareMember(ctx, spec, actor, commandID, transition, expiresAt)
		member.capturedEntity = spec.capturedEntity
		plan.members = append(plan.members, member)
	}
	return plan, nil
}

// startApply tracks the whole invocation, including ledger-only consultation.
// A deferred finish also releases it when a pilot panic propagates.
func (s *Service) startApply(ctx context.Context) (context.Context, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, ErrSceneClosed
	}
	if len(s.applying) >= maxConcurrentApplies {
		return nil, nil, ErrSceneCapacity
	}
	runCtx, cancel := context.WithCancel(ctx)
	lease := &applyLease{cancel: cancel}
	s.applying[lease] = struct{}{}
	s.active.Add(1)
	return runCtx, func() {
		cancel()
		s.mu.Lock()
		delete(s.applying, lease)
		s.mu.Unlock()
		s.active.Done()
	}, nil
}

// admitMember is the first-dispatch scene admission point. Delete, replace
// and Close use the same mutex. A member admitted before withdrawal may finish
// uncertain; later members only consult an existing ledger record.
func (s *Service) admitMember(ctx context.Context, plan *scenePlan) error {
	return s.generation.WithCurrent(ctx, func(revision uint64) error {
		if revision == 0 {
			return ErrSceneUnavailable
		}
		epoch, err := s.capture.CurrentAdmissionEpoch()
		if err != nil || epoch == "" {
			return ErrSceneUnavailable
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return ErrSceneClosed
		}
		now := s.clock.Now()
		if !plan.expiresAt.After(now) {
			return ErrSceneUnavailable
		}
		if plan.ref.kind == SceneTemporary {
			s.purgeLocked(now, epoch, revision)
			entry := s.temporary[plan.ref.id]
			if entry == nil || entry.ref != plan.ref {
				return ErrSceneUnavailable
			}
		} else {
			definition, ok := s.catalogue.Definition(plan.ref.id)
			if !ok || definition.Source().Digest() != plan.ref.digest {
				return ErrSceneUnavailable
			}
		}
		return nil
	})
}

func (s *Service) recordedMember(ctx context.Context, member preparedMember) (*actions.CommandResponse, bool, error) {
	switch member.kind {
	case preparedLight:
		return s.commands.RecordedPreparedLight(ctx, member.light)
	case preparedFan:
		return s.commands.RecordedPreparedFan(ctx, member.fan)
	case preparedDirect:
		return s.commands.RecordedPreparedDirect(ctx, member.direct)
	default:
		return nil, false, ErrInvalidScene
	}
}

func (s *Service) dispatchMember(ctx context.Context, member preparedMember) (*actions.CommandResponse, error) {
	switch member.kind {
	case preparedLight:
		return s.commands.DispatchPrepared(ctx, member.light)
	case preparedFan:
		return s.commands.DispatchPreparedFan(ctx, member.fan)
	case preparedDirect:
		return s.commands.DispatchPreparedDirect(ctx, member.direct)
	default:
		return nil, ErrInvalidScene
	}
}

func refused(spec memberSpec, code RefusalCode, field, reason string) preparedMember {
	return preparedMember{kind: preparedRefused, alias: spec.alias, source: spec.source,
		code: code, field: field, reason: reason}
}

func refusedCause(spec memberSpec, code RefusalCode, field string, cause error) preparedMember {
	member := refused(spec, code, field, cause.Error())
	member.cause = cause
	return member
}

func (s *Service) prepareMember(ctx context.Context, spec memberSpec,
	actor domain.ActorID, id domain.CommandID, transition *float64, expiresAt time.Time) preparedMember {
	if spec.origin == originInvalid {
		return refused(spec, RefusalSource, "capture", spec.omissionReason)
	}
	if spec.origin == originHA {
		diagnostics := spec.ha.Diagnostics()
		if len(diagnostics) != 0 {
			return refused(spec, RefusalSource, diagnostics[0].Field(), diagnostics[0].Reason())
		}
		switch spec.ha.Kind() {
		case hascene.MemberLight:
			intent, _ := spec.ha.Light()
			on, brightness, ct := intent.Components()
			capture, err := s.commands.CaptureLight(ctx, spec.alias)
			if err != nil {
				return refusedCause(spec, RefusalRoute, "light", err)
			}
			prepared, err := capture.PrepareHAWithCT(actor, id, expiresAt, on, brightness, ct, transition, false)
			if err != nil {
				return refused(spec, RefusalCapability, "light", err.Error())
			}
			return preparedMember{kind: preparedLight, alias: spec.alias, source: spec.source, light: prepared}
		case hascene.MemberFan:
			intent, _ := spec.ha.Fan()
			capture, err := s.commands.CaptureFan(ctx, spec.alias)
			if err != nil {
				return refusedCause(spec, RefusalRoute, "fan", err)
			}
			var pct *uint8
			if intent.On() {
				value := intent.Percentage()
				pct = &value
			}
			prepared, err := capture.PrepareHA(actor, id, expiresAt, intent.On(), pct)
			if err != nil {
				return refused(spec, RefusalCapability, "percentage", err.Error())
			}
			return preparedMember{kind: preparedFan, alias: spec.alias, source: spec.source, fan: prepared}
		case hascene.MemberSwitch:
			on, _ := spec.ha.Switch()
			return s.prepareDirect(ctx, spec, actor, id, expiresAt, on, domain.FanSpeedUnspecified)
		case hascene.MemberClimate:
			intent, _ := spec.ha.Climate()
			_, _, humidity, fanMode, presetMode := intent.Components()
			var fields []string
			if humidity != nil {
				fields = append(fields, "humidity")
			}
			if fanMode != nil {
				fields = append(fields, "fan_mode")
			}
			if presetMode != nil {
				fields = append(fields, "preset_mode")
			}
			if len(fields) == 0 {
				fields = []string{"climate"}
			}
			return refused(spec, RefusalCapability, strings.Join(fields, ","), "climate intention is not representable by the admitted command profile")
		}
	}
	if spec.origin == originNative {
		kind, err := spec.native.Kind()
		if err != nil {
			return refused(spec, RefusalSource, "value", "invalid captured value")
		}
		switch kind {
		case domain.ValueSolar, domain.ValueSolarEvent:
			return refused(spec, RefusalCapability, "member", "solar observations cannot be reproduced as commands")
		case domain.ValueLight:
			light, _ := spec.native.Light()
			on, percent, ct, err := light.Components()
			if err != nil {
				return refused(spec, RefusalSource, "light", err.Error())
			}
			capture, err := s.commands.CaptureLight(ctx, spec.alias)
			if err != nil {
				return refusedCause(spec, RefusalRoute, "light", err)
			}
			prepared, err := capture.PrepareNative(actor, id, expiresAt, on, percent, ct, transition)
			if err != nil {
				return refused(spec, RefusalCapability, "light", err.Error())
			}
			return preparedMember{kind: preparedLight, alias: spec.alias, source: spec.source, light: prepared}
		case domain.ValueSwitch:
			on, _ := spec.native.Switch()
			return s.prepareDirect(ctx, spec, actor, id, expiresAt, on, domain.FanSpeedUnspecified)
		case domain.ValueFan:
			speed, _ := spec.native.Fan()
			return s.prepareDirect(ctx, spec, actor, id, expiresAt, false, speed)
		}
	}
	return refused(spec, RefusalCapability, "member", "member cannot be reproduced")
}

func (s *Service) prepareDirect(ctx context.Context, spec memberSpec, actor domain.ActorID,
	id domain.CommandID, expiresAt time.Time, switchOn bool, speed domain.FanSpeed) preparedMember {
	kind := integrations.DirectSwitch
	if speed != domain.FanSpeedUnspecified {
		kind = integrations.DirectFan
	}
	capture, err := s.commands.CaptureDirect(ctx, spec.alias, kind)
	if err != nil {
		return refusedCause(spec, RefusalRoute, "target", err)
	}
	var prepared integrations.PreparedDirect
	if kind == integrations.DirectFan {
		prepared, err = capture.PrepareFan(actor, id, expiresAt, speed)
	} else {
		prepared, err = capture.PrepareSwitch(actor, id, expiresAt, switchOn)
	}
	if err != nil {
		return refused(spec, RefusalCapability, "command", err.Error())
	}
	member := preparedMember{kind: preparedDirect, alias: spec.alias, source: spec.source, direct: prepared}
	if kind == integrations.DirectSwitch {
		member.satisfied = &satisfactionSlot{}
	}
	return member
}

// ApplyPrepared retains the original prepared intentions. A switch may make
// one fresh equality proof at admission; all other effects use their existing
// CommandRouter and ledger. ACK is never promoted to physical confirmation.
func (s *Service) ApplyPrepared(ctx context.Context, prepared PreparedScene) (ApplyResult, error) {
	if s == nil || ctx == nil || prepared.plan == nil || prepared.plan.service != s {
		return ApplyResult{}, ErrInvalidScene
	}
	runCtx, finish, err := s.startApply(ctx)
	if err != nil {
		return ApplyResult{}, err
	}
	defer finish()
	plan := prepared.plan
	if err := s.authorizePreparedResult(runCtx, plan); err != nil {
		return ApplyResult{}, err
	}

	result := ApplyResult{members: make([]MemberResult, 0, len(plan.members))}
	succeeded := 0
	for _, member := range plan.members {
		item, err := s.applyMember(runCtx, plan, member)
		if err != nil {
			return ApplyResult{}, err
		}
		if item.outcome == OutcomeAlreadySatisfied || item.outcome == OutcomeAccepted || item.outcome == OutcomeDispatched ||
			item.outcome == OutcomeAcknowledged || item.outcome == OutcomeConfirmed {
			succeeded++
		}
		result.members = append(result.members, item)
	}
	if err := s.authorizePreparedResult(runCtx, plan); err != nil {
		return ApplyResult{}, err
	}
	switch {
	case succeeded == len(result.members):
		result.aggregate = AggregateComplete
	case succeeded > 0:
		result.aggregate = AggregatePartial
	default:
		result.aggregate = AggregateNone
	}
	return result, nil
}

func (s *Service) applyMember(ctx context.Context, plan *scenePlan, member preparedMember) (MemberResult, error) {
	item := MemberResult{alias: member.alias, source: member.source, code: member.code,
		field: member.field, reason: member.reason, outcome: OutcomeRefused}
	if member.kind == preparedRefused {
		return item, nil
	}
	if member.satisfied != nil {
		member.satisfied.mu.Lock()
		defer member.satisfied.mu.Unlock()
		if member.satisfied.proven {
			item.outcome = OutcomeAlreadySatisfied
			item.proofRevision = member.satisfied.revision
			return item, nil
		}
	}
	var response *actions.CommandResponse
	var err error
	admissionErr := s.admitMember(ctx, plan)
	if admissionErr == nil {
		if member.satisfied != nil {
			var found bool
			response, found, err = s.commands.RecordedPreparedDirect(ctx, member.direct)
			if err == nil && !found {
				var satisfied bool
				var revision uint64
				satisfied, revision, err = member.direct.ProveSwitchSatisfied(ctx)
				if err != nil {
					return switchProofRefusal(item, err), nil
				}
				if satisfied {
					member.satisfied.proven, member.satisfied.revision = true, revision
					item.outcome, item.proofRevision = OutcomeAlreadySatisfied, revision
					return item, nil
				}
				response, err = s.dispatchMember(ctx, member)
			}
		} else {
			response, err = s.dispatchMember(ctx, member)
		}
	} else {
		if errors.Is(admissionErr, ErrSceneClosed) {
			return MemberResult{}, ErrSceneClosed
		}
		var found bool
		response, found, err = s.recordedMember(ctx, member)
		if !found && err == nil {
			item.code, item.field, item.reason = RefusalSceneChanged, "scene", admissionErr.Error()
			return item, nil
		}
	}
	return commandMemberResult(item, response, err), nil
}

func switchProofRefusal(item MemberResult, err error) MemberResult {
	item.reason = err.Error()
	switch {
	case errors.Is(err, integrations.ErrCommandRevisionChanged):
		item.code, item.field = RefusalSceneChanged, "expected_revision"
	case errors.Is(err, integrations.ErrDirectObservationUnavailable):
		item.code, item.field = RefusalQuality, "state"
	case errors.Is(err, integrations.ErrDirectProofExpired):
		item.code, item.field = RefusalCommand, "expires_at"
	default:
		item.code, item.field = RefusalRoute, "target"
	}
	return item
}

func commandMemberResult(item MemberResult, response *actions.CommandResponse, err error) MemberResult {
	if response != nil {
		item.response = proto.Clone(response).(*actions.CommandResponse)
	}
	if err != nil {
		item.outcome, item.code, item.field, item.reason = OutcomeIndeterminate, RefusalCommand, "command", err.Error()
	} else if response == nil {
		item.outcome, item.code, item.field, item.reason = OutcomeIndeterminate, RefusalCommand, "command", "command returned no response"
	} else {
		switch response.Status {
		case actions.ActionStatus_ACTION_STATUS_ACCEPTED:
			item.outcome = OutcomeAccepted
		case actions.ActionStatus_ACTION_STATUS_DISPATCHED:
			item.outcome = OutcomeDispatched
		case actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED:
			item.outcome = OutcomeAcknowledged
		case actions.ActionStatus_ACTION_STATUS_CONFIRMED:
			item.outcome = OutcomeConfirmed
		case actions.ActionStatus_ACTION_STATUS_INDETERMINATE:
			item.outcome = OutcomeIndeterminate
		default:
			item.outcome, item.code, item.field, item.reason = OutcomeRefused, RefusalCommand, "command", "command was refused or cancelled"
		}
	}
	return item
}

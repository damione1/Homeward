package conformance

import (
	"context"
	"errors"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"google.golang.org/protobuf/proto"
)

// NewClimateFixtureProfile selects the same Host suite's single thermostat
// fixture. It does not invent synthetic switch/light support for a climate driver.
// Initial observed temperature and setpoint are 21.5 Celsius; commands exercise
// 22.5/heat and 21.5/off, with separate PUT acknowledgement and GET barriers.
func NewClimateFixtureProfile(source FixtureSource) (FixtureProfile, error) {
	if source.Schema != contracts.ClimateSchema {
		return FixtureProfile{}, integrations.ErrInvalidInput
	}
	// Reuse the existing source admission without duplicating its string rules.
	probe := source
	probe.Schema = contracts.SwitchSchema
	other := LightSource()
	if other.Device == probe.Device && other.Entity == probe.Entity {
		other.Entity = "climate-validation-peer"
	}
	if _, err := NewFixtureProfile(probe, other, LightBrightnessOnly); err != nil {
		return FixtureProfile{}, err
	}
	copySource := source
	return FixtureProfile{climate: &copySource}, nil
}
func (p FixtureProfile) validate() error {
	if p.climate != nil {
		_, err := NewClimateFixtureProfile(*p.climate)
		return err
	}
	_, err := NewFixtureProfile(p.Switch(), p.Light(), p.LightShape())
	return err
}
func (p FixtureProfile) sources() []FixtureSource {
	if p.climate != nil {
		return []FixtureSource{*p.climate}
	}
	return []FixtureSource{p.Switch(), p.Light()}
}
func (p FixtureProfile) aliases() []string {
	if p.climate != nil {
		return []string{"climate.home"}
	}
	return []string{"switch.nha095_relay_1", "light.nha095_light_2"}
}

type climateDispatch struct {
	requestID string
	request   *actions.CommandRequest
	done      <-chan dispatchResult
	cancel    context.CancelFunc
}

func (r *rig) beginClimate(ctx context.Context, scenario Case, id string, on bool) (*climateDispatch, *Failure) {
	request := r.request(id, 0, on)
	commandID, err := domain.ParseCommandID(id)
	if err != nil {
		failure := fail(scenario, FailureSetup, "command ID: %v", err)
		return nil, &failure
	}
	commandCtx, cancel := context.WithCancel(ctx)
	done := r.dispatchAsync(commandCtx, request)
	pending := &climateDispatch{id, request, done, cancel}
	if err := r.adapter.Delivered(ctx, commandID); err != nil {
		cancel()
		<-done
		failure := fail(scenario, FailureDispatch, "climate PUT delivery: %v", err)
		return nil, &failure
	}
	if failure := r.expectStatus(scenario, id, actions.ActionStatus_ACTION_STATUS_DISPATCHED, FailureDispatch); failure != nil {
		cancel()
		<-done
		return nil, failure
	}
	if err := r.adapter.Apply(ctx, commandID, StimulusAcknowledge); err != nil {
		cancel()
		<-done
		failure := fail(scenario, FailureDispatch, "climate ACK barrier: %v", err)
		return nil, &failure
	}
	if failure := r.expectStatus(scenario, id, r.pending, FailureUnprovenConfirmation); failure != nil {
		cancel()
		<-done
		return nil, failure
	}
	return pending, nil
}
func (r *rig) finishClimate(ctx context.Context, scenario Case, pending *climateDispatch, stimulus Stimulus, want actions.ActionStatus) *Failure {
	defer pending.cancel()
	id, _ := domain.ParseCommandID(pending.requestID)
	if err := r.adapter.Apply(ctx, id, stimulus); err != nil {
		pending.cancel()
		<-pending.done
		failure := fail(scenario, FailureMissingConfirmation, "readback barrier: %v", err)
		return &failure
	}
	select {
	case result := <-pending.done:
		if want == actions.ActionStatus_ACTION_STATUS_CONFIRMED && (result.err != nil || result.response == nil || result.response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED) {
			failure := fail(scenario, FailureDispatch, "climate dispatch completion: %v/%v", result.response, result.err)
			return &failure
		}
	case <-ctx.Done():
		pending.cancel()
		<-pending.done
		failure := fail(scenario, FailureDispatch, "climate dispatch timeout: %v", ctx.Err())
		return &failure
	}
	return r.expectStatus(scenario, pending.requestID, want, FailureMissingConfirmation)
}
func (r *rig) checkClimateCase(ctx context.Context, scenario Case) *Failure {
	initial, ok := r.fixture.State(r.entities[0])
	if !ok {
		failure := fail(scenario, FailureDiscovery, "climate initial state absent")
		return &failure
	}
	value, fresh := initial.CriticalValue()
	if !fresh {
		failure := fail(scenario, FailureDiscovery, "climate initial state is not fresh observed")
		return &failure
	}
	if _, isClimate := value.Climate(); !isClimate {
		failure := fail(scenario, FailureDiscovery, "climate pilot published another state kind")
		return &failure
	}
	switch scenario {
	case CaseCore:
		first, failure := r.beginClimate(ctx, scenario, "nha136-core-nominal", true)
		if failure != nil {
			return failure
		}
		if failure := r.finishClimate(ctx, scenario, first, StimulusMatch, actions.ActionStatus_ACTION_STATUS_CONFIRMED); failure != nil {
			return failure
		}
		before := r.adapter.Deliveries()
		response, err := r.service.Dispatch(ctx, r.actor, proto.Clone(first.request).(*actions.CommandRequest))
		if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || r.adapter.Deliveries() != before {
			failure := fail(scenario, FailureDuplicate, "climate duplicate dispatched again: %v", err)
			return &failure
		}
		timeout, failure := r.beginClimate(ctx, scenario, "nha136-core-timeout", false)
		if failure != nil {
			return failure
		}
		if err := r.clock.advance(2 * time.Minute); err != nil {
			timeout.cancel()
			<-timeout.done
			failure := fail(scenario, FailureTimeout, "clock: %v", err)
			return &failure
		}
		if failure := r.expectStatus(scenario, timeout.requestID, actions.ActionStatus_ACTION_STATUS_INDETERMINATE, FailureTimeout); failure != nil {
			timeout.cancel()
			<-timeout.done
			return failure
		}
		if failure := r.finishClimate(ctx, scenario, timeout, StimulusMatch, actions.ActionStatus_ACTION_STATUS_INDETERMINATE); failure != nil {
			return failure
		}
		return nil
	case CaseClimatePartial:
		partial, failure := r.beginClimate(ctx, scenario, "nha136-core-partial", true)
		if failure != nil {
			return failure
		}
		return r.finishClimate(ctx, scenario, partial, StimulusPartialClimate, actions.ActionStatus_ACTION_STATUS_INDETERMINATE)
	case CaseCapacity:
		first, failure := r.beginClimate(ctx, scenario, "nha136-capacity-first", true)
		if failure != nil {
			return failure
		}
		response, err := r.service.Dispatch(ctx, r.actor, r.request("nha136-capacity-second", 0, true))
		if !errors.Is(err, integrations.ErrVolatileLedgerFull) || response != nil {
			first.cancel()
			<-first.done
			failure := fail(scenario, FailureCapacity, "second climate intention not refused: %v", err)
			return &failure
		}
		return r.finishClimate(ctx, scenario, first, StimulusMatch, actions.ActionStatus_ACTION_STATUS_CONFIRMED)
	case CaseReconnect:
		_, _, _, oldSource, _, err := initial.Components()
		if err != nil {
			failure := fail(scenario, FailureReconnect, "source: %v", err)
			return &failure
		}
		oldSession, _, _ := oldSource.Components()
		before := r.adapter.Deliveries()
		if err := r.adapter.Crash(ctx); err != nil {
			failure := fail(scenario, FailureReconnect, "crash: %v", err)
			return &failure
		}
		if err := r.clock.waitArmed(ctx); err != nil {
			failure := fail(scenario, FailureReconnect, "retry barrier: %v", err)
			return &failure
		}
		if err := r.clock.advance(time.Second); err != nil {
			failure := fail(scenario, FailureReconnect, "retry clock: %v", err)
			return &failure
		}
		if err := r.adapter.Ready(ctx); err != nil {
			failure := fail(scenario, FailureReconnect, "new pilot: %v", err)
			return &failure
		}
		state, ok := r.fixture.State(r.entities[0])
		if !ok {
			failure := fail(scenario, FailureReconnect, "new state absent")
			return &failure
		}
		_, _, _, source, _, _ := state.Components()
		session, _, err := source.Components()
		if err != nil || session == oldSession || r.adapter.Deliveries() != before {
			failure := fail(scenario, FailureReconnect, "identity/session/replay fence failed")
			return &failure
		}
		command, failure := r.beginClimate(ctx, scenario, "nha136-reconnect-command", true)
		if failure != nil {
			return failure
		}
		return r.finishClimate(ctx, scenario, command, StimulusMatch, actions.ActionStatus_ACTION_STATUS_CONFIRMED)
	case CaseUnspecified:
	default:
	}
	failure := fail(scenario, FailureSetup, "invalid climate case")
	return &failure
}

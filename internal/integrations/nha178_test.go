package integrations_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"google.golang.org/protobuf/proto"
)

func nha178LedgerRig(t *testing.T, driver *nha025Driver) (*integrations.Manager, *integrations.VolatileCommandService, *actions.CommandRequest) {
	t.Helper()
	ctx := context.Background()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance := nha016Instance(t, "nha178-light-ledger")
	key, err := integrations.NewSourceKey("synthetic-light", "dimmer")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := fixture.Resolve(ctx, instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	record := nha022Record(t, ids[0], instance, key, "light.nha178", "light.nha178", false)
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	step := 1.0
	description := &registry.CapabilityDescription{SchemaId: contracts.LightSchema, Major: 1,
		DisplayName: "Synthetic dimmer", Endpoint: "level",
		Constraints: &registry.CapabilityDescription_Light{Light: &registry.LightConstraints{
			SetOn:             true,
			BrightnessPercent: &registry.NumberBounds{Minimum: 0, Maximum: 100},
			TransitionSeconds: &registry.NumberBounds{Minimum: 0, Maximum: 6553, Step: &step},
		}}}
	candidate, err := integrations.NewCandidate(key, []*registry.CapabilityDescription{description})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Discover(ctx, []integrations.Candidate{candidate}); err != nil {
		t.Fatal(err)
	}
	if err := host.AttachCommandDriver(session, driver); err != nil {
		t.Fatal(err)
	}
	start, err := fixture.Sequencer().Start()
	if err != nil {
		t.Fatal(err)
	}
	epoch, _, err := start.Components()
	if err != nil {
		t.Fatal(err)
	}
	brightness, seconds := 42.0, 2.0
	args := &actions.ActionArgs{Arguments: &actions.ActionArgs_SetLight{SetLight: &actions.SetLight{
		BrightnessPercent: &brightness, TransitionSeconds: &seconds,
	}}}
	request := nha022Request(t, ids[0].Entity, epoch, contracts.LightSchema, "level", args)
	service, err := integrations.NewVolatileCommandService(host, &nha022Generation{revision: 7}, integrations.DefaultVolatileCommandLimits())
	if err != nil {
		t.Fatal(err)
	}
	return host, service, request
}

func TestNHA178TransitionLedgerDoesNotReplayAfterLostResponse(t *testing.T) {
	var effects atomic.Int32
	entered := make(chan struct{})
	driver := &nha025Driver{}
	driver.call = func(ctx context.Context, command integrations.AdmittedCommand) error {
		request, err := command.Request()
		if err != nil {
			return err
		}
		light := request.GetArguments().GetSetLight()
		if light == nil || light.TransitionSeconds == nil || *light.TransitionSeconds != 2 {
			return integrations.ErrInvalidInput
		}
		if effects.Add(1) == 1 {
			close(entered)
		}
		<-ctx.Done() // The fake effect happened, but its reply was lost.
		return ctx.Err()
	}
	_, service, request := nha178LedgerRig(t, driver)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		response *actions.CommandResponse
		err      error
	}
	completed := make(chan result, 1)
	actor := nha022Actor(t)
	go func() {
		response, err := service.Dispatch(ctx, actor, request)
		completed <- result{response, err}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("simulated effect did not start")
	}
	cancel()
	var first result
	select {
	case first = <-completed:
	case <-time.After(3 * time.Second):
		t.Fatal("lost response did not terminate")
	}
	if !errors.Is(first.err, integrations.ErrCommandOutcomeIndeterminate) || first.response == nil || first.response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
		t.Fatalf("lost reply was falsely resolved: %v %v", first.response, first.err)
	}
	replayed, replayErr := service.Dispatch(context.Background(), actor, proto.Clone(request).(*actions.CommandRequest))
	if !errors.Is(replayErr, integrations.ErrCommandOutcomeIndeterminate) || replayed == nil || replayed.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE || effects.Load() != 1 || driver.Count() != 1 {
		t.Fatalf("ledger replayed transition: %v %v effects=%d calls=%d", replayed, replayErr, effects.Load(), driver.Count())
	}
}

func TestNHA178TransitionACKIsNotPhysicalConfirmation(t *testing.T) {
	driver := &nha025Driver{}
	driver.call = func(_ context.Context, command integrations.AdmittedCommand) error {
		return command.Acknowledge()
	}
	host, service, request := nha178LedgerRig(t, driver)
	actor := nha022Actor(t)
	response, err := service.Dispatch(context.Background(), actor, request)
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("simulated ACK command: %v %v", response, err)
	}
	id, err := domain.ParseCommandID(request.CommandId)
	if err != nil {
		t.Fatal(err)
	}
	status, err := host.ActionStatus(id)
	if err != nil || status.Status != actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED {
		t.Fatalf("ACK became physical confirmation: %v %v", status, err)
	}
	replayed, err := service.Dispatch(context.Background(), actor, proto.Clone(request).(*actions.CommandRequest))
	if err != nil || !proto.Equal(response, replayed) || driver.Count() != 1 {
		t.Fatalf("ACK replayed transition: %v %v calls=%d", replayed, err, driver.Count())
	}
}

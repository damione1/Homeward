package server

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// This RED uses NHA-042's actual Run, PostgreSQL, pilot, Controller, 040
// trigger wait, 044 template wait, and NHA-024 service. It deliberately asks
// for the domestic contract while today's Actor/Permit path still denies it.
func TestNHA029InternalWaitedActionIgnoresAuthorRights(t *testing.T) {
	for _, test := range []struct {
		name              string
		omitStateRead     bool
		withdrawAfterWait bool
	}{
		{name: "rights_withdrawn_after_real_wait", withdrawAfterWait: true},
		{name: "state_read_absent_before_real_wait", omitStateRead: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			environment := nha042ServerEnvironmentFor(t)
			var native *authorization.Authority
			parent := "- action: script.turn_on\n  target: {entity_id: script.nha042_child}\n  data: {}\n- wait_template: '{{ false }}'\n"
			child := "- wait_for_trigger:\n  - trigger: event\n    event_type: nha042_release\n" + nha042ServerCommand
			rig := nha042StartServer(t, environment, parent, child, nha042ServerOptions{
				sharedJobs: true, omitStateRead: test.omitStateRead,
				change: func(plan *RuntimePlan) {
					plan.ConfigureAuthorization = func(_ context.Context, composition AuthorizationComposition) error {
						actual, ok := composition.(runtimeAuthorizationComposition)
						if !ok || actual.views == nil || actual.views.authority == nil {
							return authorization.ErrInvalid
						}
						native = actual.views.authority
						return nil
					}
				},
			})
			rig.run.waitReady(t)
			if native == nil {
				t.Fatal("Run did not attach its native human Authority")
			}
			nha034WaitSignal(t, rig.pilot.ready, "actual simulated pilot")
			_, entity, err := rig.pilot.binding.IDs()
			if err != nil {
				t.Fatal(err)
			}
			commands, present := rig.jobServices.Commands()
			if !present || commands == nil || rig.commands != rig.jobComposition.CommandComposition() {
				t.Fatal("script and job do not borrow the same NHA-024 service")
			}

			rig.pilot.emit(t, "nha042_start")
			nha042ServerWait(t, "real 040 and 044 wait handles", func() bool {
				return nha042ServerNode(rig.composition, "script.nha042_parent", ir.ActionWaitTemplate, automationruns.NodeWaiting) &&
					nha042ServerNode(rig.composition, "script.nha042_child", ir.ActionWait, automationruns.NodeWaiting)
			})
			if effects := rig.driver.snapshot(); len(effects) != 0 {
				t.Fatalf("effect occurred before the wait release: %d", len(effects))
			}
			if test.withdrawAfterWait {
				nha029WithdrawHumanPolicy(t, environment, rig, native)
				nha029RequireHumanDirectDenied(t, environment, rig, commands, 0, test.name)
			} else {
				nha029RequireHumanStateReadDenied(t, environment, native, entity)
			}

			rig.pilot.emit(t, "nha042_release")
			nha042ServerWait(t, "released child action outcome", func() bool {
				return nha042ServerNode(rig.composition, "script.nha042_child", ir.ActionCall, automationruns.NodeCompleted) ||
					nha042ServerNode(rig.composition, "script.nha042_child", ir.ActionCall, automationruns.NodeFailed)
			})
			if !nha042ServerNode(rig.composition, "script.nha042_child", ir.ActionCall, automationruns.NodeCompleted) {
				t.Fatalf("wait resumed but internal action still depended on author Actor/Permit; effects=%d", len(rig.driver.snapshot()))
			}
			ids := rig.driver.snapshot()
			if len(ids) != 1 {
				t.Fatalf("waited internal action reached fake driver %d times, want exactly once", len(ids))
			}
			state, response := nha042ServerIntent(t, environment.observer, ids[0])
			if state != "result" || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
				t.Fatalf("same NHA-024 ledger did not finalize effect: %s %v", state, response.Status)
			}
			id, err := ids[0].Value()
			if err != nil {
				t.Fatal(err)
			}
			var origin string
			var actor, originKey sql.NullString
			if err := environment.observer.QueryRow(context.Background(),
				`SELECT origin_kind::text,actor_id,origin_key FROM public.command_intents WHERE command_id=$1`, id).
				Scan(&origin, &actor, &originKey); err != nil {
				t.Fatal(err)
			}
			if origin != "internal_automation" || actor.Valid || !originKey.Valid || originKey.String == "" {
				t.Fatalf("runtime intention retained a human owner: origin=%s actorPresent=%t keyPresent=%t", origin, actor.Valid, originKey.Valid)
			}
			if !test.withdrawAfterWait {
				nha029WithdrawHumanPolicy(t, environment, rig, native)
				nha029RequireHumanDirectDenied(t, environment, rig, commands, 1, test.name)
			}
			rig.run.cancel()
			if err := rig.run.waitDone(t); err != nil {
				t.Fatal(err)
			}
			nha034WaitSignal(t, rig.composition.Done(), "real waited run drain")
			nha034WaitRunStoreClosed(t, rig.store)
		})
	}
}

func nha029WithdrawHumanPolicy(t *testing.T, environment *nha042ServerEnvironment, rig *nha042ServerRig, native *authorization.Authority) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mutation, err := native.BeginMutation(environment.account)
	if err != nil {
		t.Fatal(err)
	}
	defer mutation.Release()
	source, err := authorization.EmptySource(environment.account)
	if err != nil {
		t.Fatal(err)
	}
	if err := mutation.ReservePolicy(source); err != nil {
		t.Fatal(err)
	}
	previous, err := rig.store.GetAuthorizationPolicy(ctx, environment.account, environment.account)
	if err != nil {
		t.Fatal(err)
	}
	version, err := previous.Version()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := authorization.ExpectVersion(version)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := rig.store.ReplaceAuthorizationPolicy(ctx, environment.account, environment.account, source, expected)
	if err != nil {
		t.Fatal(err)
	}
	current, changed, err := outcome.Result()
	if err != nil || !changed {
		t.Fatalf("human policy withdrawal was not committed: changed=%t err=%v", changed, err)
	}
	state, err := current.State()
	if err != nil {
		t.Fatal(err)
	}
	if err := native.Publish(state); err != nil {
		t.Fatal(err)
	}
}

func nha029RequireHumanStateReadDenied(t *testing.T, environment *nha042ServerEnvironment, native *authorization.Authority, entity domain.EntityID) {
	t.Helper()
	current, err := native.Current(environment.principal)
	if err != nil {
		t.Fatal(err)
	}
	permission, err := authorization.NewPermission(authv1.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY, authv1.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		t.Fatal(err)
	}
	slot, err := authorization.NewCapabilitySlot(contracts.SwitchSchema, "relay")
	if err != nil {
		t.Fatal(err)
	}
	object, err := authorization.EntityObject(entity, slot)
	if err != nil {
		t.Fatal(err)
	}
	if err := current.Authorize(permission, object); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("human StateRead unexpectedly admitted: %v", err)
	}
}

func nha029RequireHumanDirectDenied(t *testing.T, environment *nha042ServerEnvironment, rig *nha042ServerRig, commands *integrations.DurableCommandService, before int, suffix string) {
	t.Helper()
	epoch, err := rig.identity.Sequence().CurrentAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	request := &actions.CommandRequest{
		CommandId: "nha029-human-direct-" + suffix, AdmissionEpoch: epoch, ConfigRevision: 7,
		Target:             &registrywire.EntityRef{Reference: &registrywire.EntityRef_HaAlias{HaAlias: nha042ServerAlias}},
		CapabilitySchemaId: contracts.SwitchSchema, Endpoint: "relay",
		ExpiresAt: timestamppb.New(time.Now().Add(time.Minute)),
		Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: true}}},
	}
	actor, err := environment.principal.Actor()
	if err != nil {
		t.Fatal(err)
	}
	base, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx, err := authorization.WithPrincipal(base, environment.principal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := commands.Dispatch(ctx, actor, request); !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
		t.Fatalf("direct human command crossed withdrawn rights: %v", err)
	}
	if effects := rig.driver.snapshot(); len(effects) != before {
		t.Fatalf("denied human request reached driver: before=%d after=%d", before, len(effects))
	}
	var count int
	if err := environment.observer.QueryRow(ctx, `SELECT count(*) FROM public.command_intents WHERE command_id=$1`, request.CommandId).Scan(&count); err != nil || count != 0 {
		t.Fatalf("denied human request acquired a ledger intent: count=%d err=%v", count, err)
	}
}

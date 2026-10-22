package server

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	helpers "github.com/damione1/Homeward/gen/home/helpers/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/jackc/pgx/v5"
)

// The simulator implements MQTT5 request/reply explicitly. This makes no claim
// that Zigbee2MQTT or any installed device implements this echo protocol.
func TestNHA114ActualRunAuthenticatedVerticalCorrelatedObservationAndTrace(t *testing.T) {
	f := nha114Start(t)
	f.installAutomation(t)
	f.cli(t)
	f.exerciseHelpers(t)
	f.publish(t, nha114StateTopic(0), []byte("OFF"), nil)
	f.waitSwitch(t, 0, false)
	f.noCommands(t, 0)
	command := f.command(t, "nha114-api-request", nha114Aliases[0], true)
	f.submit(t, command)
	packet := f.nextCommand(t)
	if packet.Topic != "zigbee2mqtt/nha114-0/set" || string(packet.Payload) != "ON" || packet.Retain || packet.Properties == nil || packet.Properties.ResponseTopic != nha114StateTopic(0) || len(packet.Properties.CorrelationData) == 0 {
		t.Fatal("admitted command lacks its native MQTT5 reply metadata")
	}
	f.noCommands(t, 1)
	if p := f.progress(t, command.CommandId); p.GetProgress() == nil || p.GetProgress().Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatal("broker receipt was mistaken for confirmation")
	}

	// The compatible value is observed through the real broker, but has no
	// correlation. Its deliberate live transition may run the automation; the
	// subsequent correlated occurrence is joined by its exact public cursor.
	f.publish(t, nha114StateTopic(0), []byte("ON"), nil)
	f.waitSwitch(t, 0, true)
	if p := f.progress(t, command.CommandId); p.GetProgress().Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatal("value-only observation confirmed the API command")
	}
	f.publish(t, nha114StateTopic(0), []byte("OFF"), nil)
	f.waitSwitch(t, 0, false)
	before := f.view(t)
	// Publish after the snapshot, then read its real replay suffix. Opening
	// an idle Connect stream synchronously would wait for headers before the
	// caller could send the very state packet that makes those headers arrive.
	f.publish(t, packet.Properties.ResponseTopic, []byte("ON"), packet.Properties.CorrelationData)
	watch, err := f.states.WatchView(f.ctx, nha056SessionRequest(&state.WatchViewRequest{Cursor: before.Cursor, Filter: &state.ViewFilter{}}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close()
	var event *state.ViewCursor
	for watch.Receive() {
		frame := watch.Msg()
		if frame.GetResyncRequired() != nil {
			t.Fatal("unexpected resync during correlated event", frame.GetResyncRequired().Reason)
		}
		for _, s := range frame.GetDelta().States {
			if s.Entity.GetEntityId() == f.ids[0] && s.GetSwitchOn() {
				event = frame.Cursor
			}
		}
		if event != nil {
			break
		}
	}
	if event == nil {
		t.Fatal("no public state-event receipt", watch.Err())
	}
	watch.Close()
	nha114Eventually(t, "fresh explicitly correlated state did not confirm", func() bool {
		return f.progress(t, command.CommandId).GetProgress().Status == actions.ActionStatus_ACTION_STATUS_CONFIRMED
	})
	runID := f.traceForEvent(t, event)
	if runID == 0 {
		t.Fatal("API command/event had no actual public automation RunID")
	}
	if event.ConfigRevision != command.ConfigRevision || event.Epoch != command.AdmissionEpoch {
		t.Fatal("request and observed event cross generation/epoch")
	}
	f.noCommands(t, 1)
	t.Logf("synthetic proof: request=%s command=%s event=%s/%d automation_run=%d command_packets=1", command.CommandId, command.CommandId, event.Epoch, event.GlobalSequence, runID)
	f.drainAndCount(t, 1)
}

func (f *nha114Fixture) exerciseHelpers(t *testing.T) {
	t.Helper()
	for _, item := range []struct {
		alias, id string
		action    *helpers.HelperAction
	}{
		{"input_boolean.away_mode", "nha114-helper-set", &helpers.HelperAction{Operation: helpers.HelperOperation_HELPER_OPERATION_SET, Value: &helpers.HelperValue{Value: &helpers.HelperValue_Boolean{Boolean: true}}}},
		{"input_button.trash_out", "nha114-button-press", &helpers.HelperAction{Operation: helpers.HelperOperation_HELPER_OPERATION_PRESS}},
	} {
		var observed *state.ObservedState
		nha114Eventually(t, "published helper declaration/state is not publicly visible", func() bool {
			for _, entity := range f.view(t).Entities {
				if entity.CurrentAlias != item.alias || entity.State == nil {
					continue
				}
				for _, description := range entity.Capabilities {
					if description.SchemaId != contracts.HelperSchema || description.Endpoint != "value" || description.GetHelper() == nil {
						continue
					}
					for _, operation := range description.GetHelper().Operations {
						if operation == item.action.Operation {
							observed = entity.State
							return true
						}
					}
				}
			}
			return false
		})
		// Helper CAS has its own revision, distinct from ObservedState.revision.
		// An unpressed button has no value on the public wire. Verify its actual
		// SQL absence before using the existing097 absent-revision CAS value.
		expected := observed.GetHelper().GetHelperRevision()
		rows, sqlRevision := f.helperSQLRevision(t, item.alias)
		if observed.GetHelper() == nil {
			if item.action.Operation != helpers.HelperOperation_HELPER_OPERATION_PRESS || rows != 0 || sqlRevision != 0 {
				t.Fatal("public helper absence does not match an unpressed SQL button")
			}
		} else if expected == 0 || rows != 1 || sqlRevision != expected {
			t.Fatal("public helper CAS revision differs from the actual SQL value")
		}
		item.action.ExpectedHelperRevision = &expected
		command := f.command(t, item.id, item.alias, true)
		command.CapabilitySchemaId = contracts.HelperSchema
		command.Endpoint = "value"
		command.Arguments = &actions.ActionArgs{Arguments: &actions.ActionArgs_Helper{Helper: item.action}}
		r, err := f.actions.SubmitAction(f.ctx, nha056SessionRequest(&actions.SubmitActionRequest{Command: command}, f.token))
		if err != nil {
			t.Fatal("published helper/button action through actual API", err)
		}
		response := r.Msg.GetResult()
		if response.GetCommandId() != item.id || response.GetStatus() != actions.ActionStatus_ACTION_STATUS_DISPATCHED || response.GetError() != nil {
			t.Fatalf("helper API request=%s status=%s code=%s field=%s", item.id, response.GetStatus(), response.GetError().GetCode(), response.GetError().GetFieldPath())
		}
		nha114Eventually(t, "helper commit did not produce actual confirmed live progress", func() bool {
			result := f.progress(t, item.id)
			if result.Durable.GetState() != actions.DurableActionState_DURABLE_ACTION_STATE_RESULT || result.Durable.GetError() != actions.DurableActionError_DURABLE_ACTION_ERROR_NONE || result.Durable.GetResponse().GetCommandId() != item.id || result.Durable.GetResponse().GetStatus() != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
				t.Fatal("helper durable result did not retain the successful dispatched request", result.Durable)
			}
			return result.GetProgress() != nil && result.GetProgress().Status == actions.ActionStatus_ACTION_STATUS_CONFIRMED
		})
		nha114Eventually(t, "helper commit did not publish expected value and next CAS revision", func() bool {
			for _, entity := range f.view(t).Entities {
				if entity.CurrentAlias != item.alias || entity.State == nil {
					continue
				}
				value := entity.State.GetHelper()
				if value == nil || value.HelperRevision != expected+1 {
					continue
				}
				if item.action.Operation == helpers.HelperOperation_HELPER_OPERATION_SET {
					return value.GetBoolean()
				}
				return value.GetButtonPressedAt() != nil
			}
			return false
		})
		if rows, revision := f.helperSQLRevision(t, item.alias); rows != 1 || revision != expected+1 {
			t.Fatal("helper successful commit did not persist exactly the next SQL revision")
		}
	}
	nha114Eventually(t, "published helper/button values did not traverse actual API/SQL/runtime", func() bool {
		boolean, button := false, false
		for _, e := range f.view(t).Entities {
			if e.State == nil {
				continue
			}
			if e.CurrentAlias == "input_boolean.away_mode" {
				boolean = e.State.GetHelper().GetBoolean()
			}
			if e.CurrentAlias == "input_button.trash_out" {
				button = e.State.GetHelper().GetButtonPressedAt() != nil
			}
		}
		return boolean && button
	})
	f.noCommands(t, 0)
}

func (f *nha114Fixture) helperSQLRevision(t *testing.T, alias string) (int64, uint64) {
	t.Helper()
	var rows int64
	var revision uint64
	err := f.actual.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(f.ctx, `SELECT count(*), COALESCE(max(v.revision),0)
FROM public.helper_values v JOIN public.registry_entities e ON e.id=v.entity_id
WHERE e.manifest_alias=$1`, alias).Scan(&rows, &revision)
	})
	if err != nil {
		t.Fatal("read actual helper CAS revision", err)
	}
	return rows, revision
}

func (f *nha114Fixture) waitSwitch(t *testing.T, index int, on bool) {
	t.Helper()
	nha114Eventually(t, "synthetic state was not admitted", func() bool {
		for _, entity := range f.view(t).Entities {
			if entity.EntityId == f.ids[index] && entity.State != nil && entity.State.Quality.Freshness == state.Freshness_FRESHNESS_FRESH && entity.State.GetSwitchOn() == on {
				return true
			}
		}
		return false
	})
}

func (f *nha114Fixture) traceForEvent(t *testing.T, event *state.ViewCursor) uint64 {
	t.Helper()
	epoch, err := hex.DecodeString(event.Epoch)
	if err != nil || len(epoch) != 16 {
		t.Fatal("invalid actual event epoch", err)
	}
	var found uint64
	read := make(map[uint64]bool)
	nha114Eventually(t, "public trace never exposed the exact source event position", func() bool {
		metrics, err := f.observation.GetMetrics(f.ctx, nha056SessionRequest(&obs.GetMetricsRequest{}, f.token))
		if err != nil {
			t.Fatal("public controller handle", err)
		}
		handle := metrics.Msg.Metrics.Runs.GetInstalled().GetController()
		if handle == nil {
			return false
		}
		// This bounded fixture admits two intentional ON transitions. Query the
		// public trace for candidate IDs and trust only its returned source position
		// and RunId, never a timestamp or an inferred last-run association.
		for id := uint64(1); id <= 4; id++ {
			if read[id] {
				continue
			}
			response, err := f.observation.ReadRunTrace(f.ctx, nha056SessionRequest(&obs.ReadRunTraceRequest{Controller: handle, RunId: id}, f.token))
			if err != nil {
				t.Fatal("public run trace", err)
			}
			page := response.Msg.GetPage()
			if page == nil {
				continue
			}
			// One retained capture per run. Reopening immutable initial source
			// positions would consume the public service's bounded capture pool.
			read[id] = true
			for _, row := range page.Rows {
				position := row.GetController().GetTriggerPosition()
				if position != nil && bytes.Equal(position.Epoch, epoch) && position.Sequence == event.GlobalSequence {
					if row.RunId != page.RunId || row.RunId != id {
						t.Fatal("trace position has a foreign RunID")
					}
					found = row.RunId
					return true
				}
			}
		}
		return false
	})
	return found
}

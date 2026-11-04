package server

import (
	"bytes"
	"encoding/hex"
	"google.golang.org/protobuf/types/known/timestamppb"
	"testing"
	"time"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/sequencer"
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
	if p := f.progress(t, command.RequestId); nha114Metadata(t, p).Phase != api.CommandPhase_COMMAND_PHASE_DISPATCHED {
		t.Fatal("broker receipt was mistaken for confirmation")
	}

	// The compatible value is observed through the real broker, but has no
	// correlation. Its deliberate live transition may run the automation; the
	// subsequent correlated occurrence is joined by its exact public cursor.
	f.publish(t, nha114StateTopic(0), []byte("ON"), nil)
	f.waitSwitch(t, 0, true)
	if p := f.progress(t, command.RequestId); nha114Metadata(t, p).Phase != api.CommandPhase_COMMAND_PHASE_DISPATCHED {
		t.Fatal("value-only observation confirmed the API command")
	}
	f.publish(t, nha114StateTopic(0), []byte("OFF"), nil)
	f.waitSwitch(t, 0, false)
	before := f.view(t)
	nativeBefore, err := f.actual.sequence.Capture()
	if err != nil {
		t.Fatal(err)
	}
	// The public checkpoint is opaque. The fixture independently joins the exact
	// native journal occurrence to its automation trace; it never decodes a public token.
	f.publish(t, packet.Properties.ResponseTopic, []byte("ON"), packet.Properties.CorrelationData)
	watch, err := f.views.WatchEntityView(f.ctx, nha056SessionRequest(&api.WatchEntityViewRequest{Name: before.Name, ResumeToken: before.Resume}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Close()
	received := false
	for watch.Receive() {
		frame := watch.Msg()
		entry := frame.GetUpsert()
		if entry.GetEntity().GetName() != f.entityName(t, f.ids[0]) {
			continue
		}
		for _, capability := range entry.Capabilities {
			if capability.GetPower().GetObservation().GetValue() == api.PowerValue_POWER_VALUE_ON {
				if frame.ResumeToken == "" || frame.ResumeToken == before.Resume {
					t.Fatal("state receipt omitted its advancing checkpoint")
				}
				received = true
			}
		}
		if received {
			break
		}
	}
	if !received {
		t.Fatal("no public state-event receipt", watch.Err())
	}
	watch.Close()
	nha114Eventually(t, "fresh explicitly correlated state did not confirm", func() bool {
		return nha114Metadata(t, f.progress(t, command.RequestId)).Phase == api.CommandPhase_COMMAND_PHASE_CONFIRMED
	})
	var event sequencer.Cursor
	nha114Eventually(t, "correlated receipt absent from native journal", func() bool {
		entries, _, err := f.actual.sequence.ReadAfter(nativeBefore.Cursor, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			id, state, _, ok := entry.State()
			text, _ := id.Value()
			value, present := state.CurrentValue()
			on, switchValue := value.Switch()
			if ok && text == f.ids[0] && present && switchValue && on {
				event = entry.Cursor()
				return true
			}
		}
		return false
	})
	runID := f.traceForEvent(t, event)
	if runID == 0 {
		t.Fatal("API command/event had no actual public automation RunID")
	}
	beforeEpoch, _, _ := nativeBefore.Cursor.Components()
	eventEpoch, eventSequence, err := event.Components()
	if err != nil || beforeEpoch != eventEpoch {
		t.Fatal("correlated receipt crossed native epoch", err)
	}
	if metadata := nha114Metadata(t, f.progress(t, command.RequestId)); metadata.Capability != command.Name || metadata.RequestId != command.RequestId {
		t.Fatal("confirmation changed the captured intention")
	}
	f.noCommands(t, 1)
	t.Logf("synthetic proof: request=%s operation=%s native_event=%s/%d automation_run=%d command_packets=1", command.RequestId, f.operationNames[command.RequestId], eventEpoch, eventSequence, runID)
	f.drainAndCount(t, 1)
}

func (f *nha114Fixture) exerciseHelpers(t *testing.T) {
	t.Helper()
	for _, item := range []struct {
		alias, label string
		operation    api.HelperOperation
	}{
		{"input_boolean.away_mode", "nha114-helper-set", api.HelperOperation_HELPER_OPERATION_SET},
		{"input_button.trash_out", "nha114-button-press", api.HelperOperation_HELPER_OPERATION_PRESS},
	} {
		entityName := f.aliasName(t, item.alias)
		var capability *api.Capability
		nha114Eventually(t, "published helper declaration is not publicly visible", func() bool {
			for _, entry := range f.view(t).Entities {
				if entry.Entity.GetName() != entityName {
					continue
				}
				for _, candidate := range entry.Capabilities {
					for _, operation := range candidate.GetHelper().GetDefinition().GetOperations() {
						if operation == item.operation {
							capability = candidate
							return true
						}
					}
				}
			}
			return false
		})
		observed := capability.GetHelper().GetObservation().GetValue()
		expected := observed.GetHelperRevision()
		rows, revision := f.helperSQLRevision(t, item.alias)
		if observed == nil {
			if item.operation != api.HelperOperation_HELPER_OPERATION_PRESS || rows != 0 || revision != 0 {
				t.Fatal("public helper absence differs from unpressed SQL button")
			}
		} else if expected <= 0 || rows != 1 || revision != uint64(expected) {
			t.Fatal("public helper CAS differs from actual SQL value")
		}
		helper := &api.SetHelperCommand{Operation: item.operation, ExpectedHelperRevision: &expected}
		if item.operation == api.HelperOperation_HELPER_OPERATION_SET {
			helper.Value = &api.HelperInputValue{Value: &api.HelperInputValue_BooleanValue{BooleanValue: true}}
		}
		command := &api.ExecuteCapabilityRequest{Name: capability.Name, Etag: capability.Etag, RequestId: nha114RequestID(item.label), ExpireTime: timestamppb.New(time.Now().Add(time.Minute)), Command: &api.ExecuteCapabilityRequest_Helper{Helper: helper}}
		f.submit(t, command)
		nha114Eventually(t, "helper commit did not produce confirmed operation", func() bool {
			operation := f.progress(t, command.RequestId)
			if operation.GetError() != nil {
				t.Fatal("helper operation failed", operation.GetError())
			}
			return operation.Done && nha114Metadata(t, operation).Phase == api.CommandPhase_COMMAND_PHASE_CONFIRMED
		})
		// The native durable admission stays distinct from later confirmation.
		if len(f.durableResponse(t, command.RequestId)) == 0 {
			t.Fatal("helper original durable result disappeared")
		}
		nha114Eventually(t, "helper value and next CAS revision did not publish", func() bool {
			for _, entry := range f.view(t).Entities {
				if entry.Entity.GetName() != entityName {
					continue
				}
				for _, candidate := range entry.Capabilities {
					value := candidate.GetHelper().GetObservation().GetValue()
					if value.GetHelperRevision() != expected+1 {
						continue
					}
					if item.operation == api.HelperOperation_HELPER_OPERATION_SET {
						return value.GetBooleanValue()
					}
					return value.GetTriggerTime() != nil
				}
			}
			return false
		})
		if rows, revision := f.helperSQLRevision(t, item.alias); rows != 1 || revision != uint64(expected+1) {
			t.Fatal("helper did not persist exactly next SQL revision")
		}
	}
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
	expected := api.PowerValue_POWER_VALUE_OFF
	if on {
		expected = api.PowerValue_POWER_VALUE_ON
	}
	nha114Eventually(t, "synthetic state was not admitted", func() bool {
		for _, entry := range f.view(t).Entities {
			if entry.Entity.GetName() != f.entityName(t, f.ids[index]) {
				continue
			}
			for _, capability := range entry.Capabilities {
				observation := capability.GetPower().GetObservation()
				if observation != nil && observation.Value != nil && observation.GetValue() == expected && observation.GetQuality().GetFreshness() == api.Freshness_FRESHNESS_CURRENT {
					return true
				}
			}
		}
		return false
	})
}

func (f *nha114Fixture) traceForEvent(t *testing.T, event sequencer.Cursor) uint64 {
	t.Helper()
	eventEpoch, eventSequence, err := event.Components()
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := hex.DecodeString(eventEpoch)
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
				if position != nil && bytes.Equal(position.Epoch, epoch) && position.Sequence == eventSequence {
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

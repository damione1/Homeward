package integrations_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/damione1/Homeward/contracts"
	helperwire "github.com/damione1/Homeward/gen/home/helpers/v1"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	solarwire "github.com/damione1/Homeward/gen/home/solar/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
	"runtime"
	"testing"
	"time"
	"unsafe"
)

func TestNHA105ProductionLayoutAndRetention(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) == 8 && (unsafe.Sizeof(domain.StateValue{}) != 208 || unsafe.Sizeof(domain.ObservedState{}) != 600 || unsafe.Sizeof(sequencer.Entry{}) != 1096 || unsafe.Sizeof(sequencer.RecorderState{}) != 616) {
		t.Fatal("integrated production layout differs from ratified B")
	}
	for _, family := range []string{"helper", "solar", "event"} {
		for _, n := range []int{0, 1, 1024} {
			t.Run(fmt.Sprintf("%s/n%d", family, n), func(t *testing.T) {
				runtime.GC()
				var before runtime.MemStats
				runtime.ReadMemStats(&before)
				host, fixture, err := fake.NewFixture()
				if err != nil {
					t.Fatal(err)
				}
				instance, _ := domain.ParseIntegrationInstanceID("nha105-memory")
				session, err := host.OpenSession(instance)
				if err != nil {
					t.Fatal(err)
				}
				var description *registrywire.CapabilityDescription
				switch family {
				case "helper":
					description = &registrywire.CapabilityDescription{SchemaId: contracts.HelperSchema, Major: 1, DisplayName: "Synthetic helper", Endpoint: "value", Constraints: &registrywire.CapabilityDescription_Helper{Helper: &helperwire.HelperConstraints{Kind: helperwire.HelperKind_HELPER_KIND_BOOLEAN, Operations: []helperwire.HelperOperation{helperwire.HelperOperation_HELPER_OPERATION_SET}}}}
				case "solar":
					description = &registrywire.CapabilityDescription{SchemaId: contracts.SolarSchema, Major: 1, DisplayName: "Synthetic sun", Endpoint: "value", Constraints: &registrywire.CapabilityDescription_Solar{Solar: &solarwire.SolarConstraints{}}}
				case "event":
					description = &registrywire.CapabilityDescription{SchemaId: contracts.SolarEventSchema, Major: 1, DisplayName: "Synthetic dawn", Endpoint: "value", Constraints: &registrywire.CapabilityDescription_SolarEvent{SolarEvent: &solarwire.SolarEventConstraints{Event: solarwire.SolarEventKind_SOLAR_EVENT_KIND_DAWN}}}
				}
				bindings := make([]integrations.Binding, 0, n)
				for first := 0; first < n; first += 256 {
					end := first + 256
					if end > n {
						end = n
					}
					candidates := make([]integrations.Candidate, 0, end-first)
					for i := first; i < end; i++ {
						key, _ := integrations.NewSourceKey(fmt.Sprintf("synthetic-%d", i), "value")
						candidate, err := integrations.NewCandidate(key, []*registrywire.CapabilityDescription{description})
						if err != nil {
							t.Fatal(err)
						}
						candidates = append(candidates, candidate)
					}
					batch, err := session.Discover(context.Background(), candidates)
					if err != nil {
						t.Fatal(err)
					}
					bindings = append(bindings, batch...)
				}
				quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
				at := time.Now().UTC()
				solar := nha105NativeSolar(t)
				missing, _ := domain.SolarOccurrenceMissing(domain.SolarNoEventWithinSearchHorizon)
				for _, binding := range bindings {
					var value domain.StateValue
					switch family {
					case "helper":
						value, _ = domain.NewHelperStateValue(domain.NewHelperBoolean(false), 1)
					case "solar":
						value, _ = domain.NewSolarStateValue(solar)
					case "event":
						event, _ := domain.NewSolarEventValue(domain.SolarDawn, missing)
						value, _ = domain.NewSolarEventStateValue(event)
					}
					observation, err := integrations.NewStateObservation("value", quality, &value, &at, at, nil)
					if err != nil {
						t.Fatal(err)
					}
					if err := session.Publish(context.Background(), binding, observation); err != nil {
						t.Fatal(err)
					}
				}
				capture, err := fixture.Sequencer().CaptureRecorder(nil, sequencer.Cursor{}, 4<<20, 4<<20)
				if err != nil {
					t.Fatal(err)
				}
				defer capture.Subscription.Close()
				if len(capture.States) != n {
					t.Fatal("integrated retention lost states")
				}
				encoded, recordCharge := 0, 0
				records := make([]storage.RecorderSnapshotRecord, 0, n)
				for _, state := range capture.States {
					row, err := storage.NewRecorderSnapshotRecord(state.State, 4<<20)
					if err != nil {
						t.Fatal(err)
					}
					records = append(records, row)
					encoded += row.EncodedBytes()
					recordCharge += row.RetainedBytes()
				}
				if n > 0 {
					oldWire, err := contracts.EncodeObservedState(capture.States[0].State)
					if err != nil {
						t.Fatal(err)
					}
					oldBytes, err := proto.Marshal(oldWire)
					if err != nil {
						t.Fatal(err)
					}
					var changed domain.StateValue
					switch family {
					case "helper":
						changed, _ = domain.NewHelperStateValue(domain.NewHelperBoolean(true), 2)
					case "solar":
						parts, _ := solar.Components()
						parts.ElevationDegrees = 1
						next, _ := domain.NewSolarValue(parts)
						changed, _ = domain.NewSolarStateValue(next)
					case "event":
						present, _ := domain.SolarOccurrenceAt(at.Add(time.Hour))
						event, _ := domain.NewSolarEventValue(domain.SolarDawn, present)
						changed, _ = domain.NewSolarEventStateValue(event)
					}
					received := at.Add(time.Second)
					observation, err := integrations.NewStateObservation("value", quality, &changed, &received, received, nil)
					if err != nil {
						t.Fatal(err)
					}
					if err := session.Publish(context.Background(), bindings[0], observation); err != nil {
						t.Fatal(err)
					}
					stillOld, err := contracts.EncodeObservedState(capture.States[0].State)
					if err != nil {
						t.Fatal(err)
					}
					stillOldBytes, err := proto.Marshal(stillOld)
					if err != nil || string(stillOldBytes) != string(oldBytes) {
						t.Fatal("new publication mutated old retained capture")
					}
				}
				runtime.GC()
				var after runtime.MemStats
				runtime.ReadMemStats(&after)
				// This is the entire live Host graph with paired empty controls, not
				// the exclusive wrapper size or a physical interpretation of quotas.
				report := struct {
					Family               string
					N                    int
					LogicalImage         int
					ProtobufBytes        int
					EncodedRecordsCharge int
					Before               runtime.MemStats
					After                runtime.MemStats
				}{family, n, capture.EstimatedBytes, encoded, recordCharge, before, after}
				data, err := json.Marshal(report)
				if err != nil {
					t.Fatal(err)
				}
				t.Log(string(data))
				runtime.KeepAlive(host)
				runtime.KeepAlive(fixture)
				runtime.KeepAlive(capture)
				runtime.KeepAlive(records)
			})
		}
	}
}

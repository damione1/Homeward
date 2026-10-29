package server

// Opt-in qualification data only. The ordinary Run/Lists/Recorder/History
// services retain all admission and persistence; no physical driver is installed.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	listswire "github.com/damione1/Homeward/gen/home/lists/v1"
	"github.com/damione1/Homeward/gen/home/lists/v1/listsv1connect"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
)

const nhf023ItemCount = 10000
const nhf023PointCount = 20000

type nhf023Workload struct {
	meter domain.HAAlias
	list  domain.ListID
}

func nhf023ReadWorkload(path string) (*nhf023Workload, error) {
	if path == "" {
		return nil, nil
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("absolute synthetic workload source required")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return nil, errors.New("bounded regular synthetic workload source required")
	}
	var source struct {
		SyntheticOnly bool   `json:"synthetic_only"`
		MeterAlias    string `json:"meter_alias"`
	}
	decoder := json.NewDecoder(io.LimitReader(file, 4097))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&source); err != nil {
		return nil, err
	}
	if decoder.Decode(new(any)) != io.EOF || !source.SyntheticOnly {
		return nil, errors.New("explicit synthetic-only workload required")
	}
	alias, err := domain.ParseHAAlias(source.MeterAlias)
	if err != nil {
		return nil, err
	}
	return &nhf023Workload{meter: alias}, nil
}

func (w *nhf023Workload) isMeter(alias string) bool {
	if w == nil {
		return false
	}
	value, err := w.meter.Value()
	return err == nil && alias == value
}

func (w *nhf023Workload) capabilities(profile nhf005UIProfile, alias string, index int) []*registrywire.CapabilityDescription {
	if !w.isMeter(alias) {
		return profile.capabilities(alias, index)
	}
	return []*registrywire.CapabilityDescription{{SchemaId: contracts.MeasurementSchema, Major: 1, Endpoint: "relay", DisplayName: "Compteur synthétique de charge",
		Constraints: &registrywire.CapabilityDescription_Measurement{Measurement: &registrywire.MeasurementConstraints{Unit: "kWh", Precision: 3,
			Semantic: registrywire.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL, Aggregation: registrywire.MeasurementAggregation_MEASUREMENT_AGGREGATION_SUM}}}}
}

func nhf023MeterObservation(value float64, at time.Time, sequence *uint64) (integrations.StateObservation, error) {
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		return integrations.StateObservation{}, err
	}
	unit, err := domain.NewUnitCatalog().Parse("kWh")
	if err != nil {
		return integrations.StateObservation{}, err
	}
	measurement, err := domain.NewMeasurement(value, unit, 3, domain.SemanticTotal, domain.AggregationSum)
	if err != nil {
		return integrations.StateObservation{}, err
	}
	state, err := domain.NewMeasurementValue(measurement)
	if err != nil {
		return integrations.StateObservation{}, err
	}
	return integrations.NewStateObservation("relay", quality, &state, &at, at, sequence)
}

func (w *nhf023Workload) observation(profile nhf005UIProfile, alias string, sequence *uint64, at time.Time) (integrations.StateObservation, error) {
	if !w.isMeter(alias) {
		return profile.observation(alias, sequence, at)
	}
	value := 0.
	if sequence != nil {
		value = float64(nhf023PointCount)/1000 + float64(*sequence)/1000
	}
	return nhf023MeterObservation(value, at, sequence)
}

func (w *nhf023Workload) seedMeter(t *testing.T, ctx context.Context, source *integrations.Session, binding integrations.Binding, runtime *observationRuntime) {
	t.Helper()
	if runtime == nil {
		t.Fatal("ordinary observation runtime required for workload")
	}
	_, entity, err := binding.IDs()
	if err != nil {
		t.Fatal(err)
	}
	// The ordinary fixture already published point zero. Every remaining point
	// goes through Host -> sequencer -> real Recorder, never synthetic SQL rows.
	for i := 1; i < nhf023PointCount; i++ {
		observation, err := nhf023MeterObservation(float64(i)/1000, time.Now().UTC(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = source.Publish(ctx, binding, observation); err != nil {
			t.Fatal("workload meter publication", err)
		}
		if i%256 == 255 || i == nhf023PointCount-1 {
			capture, err := runtime.sequence.CaptureEntityCatalogue(entity, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			_, through, err := capture.Cursor().Components()
			if err != nil {
				t.Fatal(err)
			}
			nha042ServerWait(t, "multi-domain Recorder prefix", func() bool {
				status, err := runtime.recording.Status()
				return err == nil && status.Checkpoint >= through
			})
		}
	}
}

func (w *nhf023Workload) seedList(t *testing.T, ctx context.Context, store *storage.Store, client *http.Client, origin, access string) {
	t.Helper()
	api := listsv1connect.NewListServiceClient(client, origin)
	response, err := api.CreateList(ctx, nha162Bearer(&listswire.CreateListRequest{Name: "Liste synthétique de charge"}, access))
	if err != nil || response.Msg.GetReceipt().GetListId() == nil {
		t.Fatal("ordinary workload CreateList", err)
	}
	id, err := domain.ParseListID(response.Msg.Receipt.ListId.Value)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := id.Value()
	// Only the newly allocated fixture's own Store is bulk-seeded. Subsequent
	// native changes must use the ordinary authenticated CAS ListService.
	err = store.InTransaction(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO public.homeward_list_items(list_id,item_uid,origin,summary,completed,position,lifecycle,revision)
		 SELECT l.list_id,'workload-'||lpad(n::text,5,'0'),l.origin,'Tâche synthétique '||lpad(n::text,5,'0'),false,n-1,1,1
		 FROM public.homeward_lists l CROSS JOIN generate_series(1,$2::integer) n WHERE l.list_id=$1`, value, nhf023ItemCount); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE public.homeward_lists SET revision=$2 WHERE list_id=$1`, value, nhf023ItemCount+1)
		return err
	})
	if err != nil {
		t.Fatal("private workload task setup", err)
	}
	w.list = id
}

func (w *nhf023Workload) statistics(ctx context.Context, store *storage.Store) (map[string]any, error) {
	id, err := w.list.Value()
	if err != nil {
		return nil, err
	}
	var count, completed, revision int64
	err = store.InTransaction(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT l.revision,count(i.item_uid),count(i.item_uid) FILTER(WHERE i.completed)
		 FROM public.homeward_lists l LEFT JOIN public.homeward_list_items i ON i.list_id=l.list_id
		 WHERE l.list_id=$1 GROUP BY l.revision`, id).Scan(&revision, &count, &completed)
	})
	return map[string]any{"item_count": count, "completed_items": completed, "list_revision": revision}, err
}

func TestNHF023WorkloadSourceAdmission(t *testing.T) {
	if value, err := nhf023ReadWorkload(""); value != nil || err != nil {
		t.Fatal("ordinary fixture unexpectedly enabled workload")
	}
	path := filepath.Join(t.TempDir(), "source.json")
	for _, raw := range []string{`{}`, `{"synthetic_only":false,"meter_alias":"sensor.test"}`, `{"synthetic_only":true,"meter_alias":"bad"}`,
		`{"synthetic_only":true,"meter_alias":"sensor.test","unknown":true}`, `{"synthetic_only":true,"meter_alias":"sensor.test"} {}`, string(bytes.Repeat([]byte("x"), 4097))} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := nhf023ReadWorkload(path); err == nil {
			t.Fatal("unadmitted synthetic workload source")
		}
	}
	if err := os.WriteFile(path, []byte(`{"synthetic_only":true,"meter_alias":"sensor.test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	value, err := nhf023ReadWorkload(path)
	if err != nil || !value.isMeter("sensor.test") || value.isMeter("sensor.other") {
		t.Fatal("typed source admission", err)
	}
}

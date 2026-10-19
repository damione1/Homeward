package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	helperwire "github.com/damione1/Homeward/gen/home/helpers/v1"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	statewire "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/helpers"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtt"
	"github.com/damione1/Homeward/internal/recorder"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The wrapper rejects one post-COMMIT publication, never a retirement report.
// The underlying sequencer remains the sole projection and Recorder source.
type nha097FaultSink struct {
	*sequencer.Sequencer
	failNext  atomic.Bool
	rejected  atomic.Int32
	publishes atomic.Int32
	lastError atomic.Value
}

func (s *nha097FaultSink) reject(accepted integrations.AcceptedState) bool {
	if !accepted.Retirement() && s.failNext.CompareAndSwap(true, false) {
		s.rejected.Add(1)
		return true
	}
	return false
}
func (s *nha097FaultSink) AcceptState(ctx context.Context, entity domain.EntityID, accepted integrations.AcceptedState) error {
	if !accepted.Retirement() {
		s.publishes.Add(1)
	}
	if s.reject(accepted) {
		return errors.New("synthetic projection failure after SQL COMMIT")
	}
	err := s.Sequencer.AcceptState(ctx, entity, accepted)
	if err != nil {
		s.lastError.Store(err.Error())
	}
	return err
}
func (s *nha097FaultSink) AcceptStateWithActionReceipt(ctx context.Context, entity domain.EntityID, accepted integrations.AcceptedState) (integrations.ActionReceipt, error) {
	if !accepted.Retirement() {
		s.publishes.Add(1)
	}
	if s.reject(accepted) {
		return integrations.ActionReceipt{}, errors.New("synthetic projection failure after SQL COMMIT")
	}
	receipt, err := s.Sequencer.AcceptStateWithActionReceipt(ctx, entity, accepted)
	if err != nil {
		s.lastError.Store(err.Error())
	}
	return receipt, err
}
func (s *nha097FaultSink) AcceptEvent(ctx context.Context, entity domain.EntityID, event integrations.AcceptedEvent) error {
	return s.Sequencer.AcceptEvent(ctx, entity, event)
}
func (s *nha097FaultSink) AcceptHealth(ctx context.Context, instance domain.IntegrationInstanceID, report integrations.HealthReport) error {
	return s.Sequencer.AcceptHealth(ctx, instance, report)
}

type nha097RetryTimer struct {
	entered chan struct{}
	release chan time.Time
}

func (t *nha097RetryTimer) After(time.Duration) <-chan time.Time {
	t.entered <- struct{}{}
	return t.release
}

type nha097CommandRig struct {
	ctx        context.Context
	store      *storage.Store
	settings   storage.Settings
	identity   integrations.IdentityAuthority
	instance   domain.IntegrationInstanceID
	catalog    *contracts.CapabilityCatalog
	sequence   *sequencer.Sequencer
	sink       *nha097FaultSink
	host       *integrations.Manager
	service    *integrations.DurableCommandService
	supervisor *integrations.Supervisor
	observer   *pgx.Conn
	recording  *recorder.Recorder
}

func nha097Commands(t *testing.T, timer integrations.Timer) nha097CommandRig {
	return nha097CommandsWithCandidate(t, timer, bundle.Candidate{})
}

func nha097CommandsWithCandidate(t *testing.T, timer integrations.Timer, candidate bundle.Candidate) nha097CommandRig {
	t.Helper()
	cluster := testpg.Start(t)
	configuration := nha030ServerConfig(t, cluster.Port)
	settings, err := configuration.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	sequence, err := sequencer.New(sequencer.RealClock{}, 16384)
	if err != nil {
		t.Fatal(err)
	}
	recording, err := prepareRecorder(ctx, sequence, store, recorder.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := recording.Stop(stopCtx); err != nil {
			t.Errorf("Recorder drain: %v", err)
		}
	})
	if !candidate.Valid() {
		candidate, err = bundle.CompileV2(filepath.Join("..", "config", "bundle", "testdata", "nha097"), "configuration.yaml")
		if err != nil {
			t.Fatal(err)
		}
	}
	plan, err := defaultConfiguredRuntimePlan(mqtt.Settings{}, false, nil, store, sequence)
	if err != nil {
		t.Fatal(err)
	}
	if err := addActiveHelpers(&plan, candidate, store); err != nil {
		t.Fatal(err)
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	sink := &nha097FaultSink{Sequencer: sequence}
	host, err := integrations.NewHost(plan.Identity, sink, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Configure(ctx, host, store); err != nil {
		t.Fatal(err)
	}
	service, err := integrations.NewDurableCommandService(ctx, host, nha094Generation{}, store)
	if err != nil {
		t.Fatal(err)
	}
	if timer == nil {
		timer = integrations.RealTimer{}
	}
	supervisor, err := integrations.NewSupervisor(host, plan.Pilots, integrations.DefaultRetryPolicy(), timer, integrations.RandomJitter{})
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := supervisor.Stop(stopCtx); err != nil {
			t.Errorf("Supervisor drain: %v", err)
		}
	})
	instance, _ := domain.ParseIntegrationInstanceID("homeward-helpers-v1")
	nha077Await(t, "helper pilot running", func() bool {
		status, ok := supervisor.Status(instance)
		return ok && status.Phase == integrations.PilotRunning
	})
	return nha097CommandRig{ctx, store, settings, plan.Identity, instance, catalog, sequence, sink, host, service, supervisor, nha030ServerObserver(t, cluster.Port), recording}
}

func (r nha097CommandRig) entity(t *testing.T, alias string) domain.EntityID {
	t.Helper()
	parsed, err := domain.ParseHAAlias(alias)
	if err != nil {
		t.Fatal(err)
	}
	key, err := helpers.ConfiguredSourceKey(parsed)
	if err != nil {
		t.Fatal(err)
	}
	identities, err := r.identity.Resolve(r.ctx, r.instance, []integrations.SourceKey{key})
	if err != nil || len(identities) != 1 {
		t.Fatalf("configured helper identity %s: %v", alias, err)
	}
	return identities[0].Entity
}

func (r nha097CommandRig) request(t *testing.T, alias, id string, revision uint64, operation helperwire.HelperOperation, value *helperwire.HelperValue) (*actions.CommandRequest, domain.CommandID) {
	t.Helper()
	entity := r.entity(t, alias)
	entityText, _ := entity.Value()
	epoch, err := r.sequence.CurrentAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	commandID, err := domain.ParseCommandID(id)
	if err != nil {
		t.Fatal(err)
	}
	return &actions.CommandRequest{CommandId: id, AdmissionEpoch: epoch, ConfigRevision: 7,
		Target:             &registrywire.EntityRef{Reference: &registrywire.EntityRef_EntityId{EntityId: entityText}},
		CapabilitySchemaId: contracts.HelperSchema, Endpoint: "value",
		Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_Helper{Helper: &helperwire.HelperAction{
			Operation: operation, Value: value, ExpectedHelperRevision: &revision,
		}}}, ExpiresAt: timestamppb.New(time.Now().Add(time.Minute))}, commandID
}

func (r nha097CommandRig) revision(t *testing.T, alias string) int64 {
	t.Helper()
	var revision int64
	err := r.observer.QueryRow(r.ctx, `SELECT v.revision FROM public.helper_values v JOIN public.registry_entities e ON e.id=v.entity_id WHERE e.manifest_alias=$1`, alias).Scan(&revision)
	if err != nil {
		t.Fatal(err)
	}
	return revision
}

func (r nha097CommandRig) dispatch(t *testing.T, actor domain.ActorID, request *actions.CommandRequest, id domain.CommandID) {
	t.Helper()
	response, err := r.service.Dispatch(r.ctx, actor, request)
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		var sqlRevision int64
		sqlErr := r.observer.QueryRow(r.ctx, `SELECT revision FROM public.helper_values WHERE entity_id=$1`, request.GetTarget().GetEntityId()).Scan(&sqlRevision)
		progress, progressErr := r.host.ActionStatus(id)
		phase, _ := r.supervisor.Status(r.instance)
		entity, _ := domain.ParseEntityID(request.GetTarget().GetEntityId())
		state, statePresent, stateErr := r.sequence.State(entity)
		value, critical := state.CriticalValue()
		observed, observedRevision, observedHelper := value.Helper()
		requested, requestErr := contracts.AdmitHelperSetValue(request.GetArguments().GetHelper().Value)
		catalogErr := r.catalog.AdmitDomainHelper(entity, "value", requested)
		_, domainErr := domain.NewHelperStateValue(requested, uint64(sqlRevision))
		t.Fatalf("local helper dispatch: %v %v; SQL revision=%d (%v), action=%+v (%v), pilot=%+v, sink calls=%d last=%v, requested=%v (%v) catalog=%v domain=%v, state=%t/%v critical=%t helper=%v/%d/%t", response, err, sqlRevision, sqlErr, progress, progressErr, phase, r.sink.publishes.Load(), r.sink.lastError.Load(), requested, requestErr, catalogErr, domainErr, statePresent, stateErr, critical, observed, observedRevision, observedHelper)
	}
	progress, err := r.host.ActionStatus(id)
	if err != nil || progress.Status != actions.ActionStatus_ACTION_STATUS_CONFIRMED {
		t.Fatalf("confirmed SQL CAS did not produce terminal local proof: %+v %v", progress, err)
	}
	intent, found, err := r.service.Status(r.ctx, id)
	if err != nil || !found || intent.State != storage.IntentResult || intent.Response == nil || intent.Response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("durable command result: %+v found=%t err=%v", intent, found, err)
	}
}

func TestNHA097DurableHelperCommandsConfirmAndReopen(t *testing.T) {
	rig := nha097Commands(t, nil)
	actor, _ := domain.ParseActorID("nha097-local-command-actor")
	set := helperwire.HelperOperation_HELPER_OPERATION_SET
	date := &helperwire.CivilDate{Year: 2026, Month: 9, Day: 2}
	clock := &helperwire.CivilTime{Hour: 8, Minute: 30}
	cases := []struct {
		alias string
		value *helperwire.HelperValue
		prior uint64
	}{
		{"input_boolean.away_mode", &helperwire.HelperValue{Value: &helperwire.HelperValue_Boolean{Boolean: false}}, 0},
		{"input_boolean.guests", &helperwire.HelperValue{Value: &helperwire.HelperValue_Boolean{Boolean: true}}, 0},
		{"counter.bathroom_heating_used_today", &helperwire.HelperValue{Value: &helperwire.HelperValue_Counter{Counter: 0}}, 1},
		{"input_text.guest_bedroom_remote_last_event", &helperwire.HelperValue{Value: &helperwire.HelperValue_Text{Text: ""}}, 0},
		{"input_select.time_of_day", &helperwire.HelperValue{Value: &helperwire.HelperValue_SelectedOption{SelectedOption: "Morning"}}, 1},
		{"input_datetime.gazon_date_de_semis", &helperwire.HelperValue{Value: &helperwire.HelperValue_Date{Date: date}}, 1},
		{"input_datetime.bathroom_heating_time", &helperwire.HelperValue{Value: &helperwire.HelperValue_Time{Time: clock}}, 0},
		{"input_datetime.sdb1_occupancy_last_time", &helperwire.HelperValue{Value: &helperwire.HelperValue_Datetime{Datetime: &helperwire.CivilDateTime{Date: date, Time: clock}}}, 0},
	}
	for index, item := range cases {
		request, id := rig.request(t, item.alias, "nha097-local-set-"+string(rune('a'+index)), item.prior, set, item.value)
		rig.dispatch(t, actor, request, id)
		if got := rig.revision(t, item.alias); got != int64(item.prior+1) {
			t.Fatalf("SQL CAS revision %s: got=%d", item.alias, got)
		}
		state, present, err := rig.sequence.State(rig.entity(t, item.alias))
		if err != nil || !present {
			t.Fatalf("published helper state %s: %v", item.alias, err)
		}
		observed, fresh := state.CriticalValue()
		helper, helperRevision, helperPresent := observed.Helper()
		wanted, wantErr := contracts.AdmitHelperSetValue(item.value)
		if !fresh || !helperPresent || helperRevision != item.prior+1 || wantErr != nil || helper != wanted {
			t.Fatalf("published helper revision %s: %d fresh=%t present=%t", item.alias, helperRevision, fresh, helperPresent)
		}
		if index == 0 {
			response, err := rig.service.Dispatch(rig.ctx, actor, request)
			if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || rig.revision(t, item.alias) != 1 {
				t.Fatalf("same CommandID re-executed local CAS: %v %v", response, err)
			}
		}
	}
	for index, alias := range []string{"input_button.start_bird_s_radio", "input_button.pause_bird_s_radio", "input_button.trash_out"} {
		request, id := rig.request(t, alias, "nha097-local-press-"+string(rune('a'+index)), 0, helperwire.HelperOperation_HELPER_OPERATION_PRESS, nil)
		rig.dispatch(t, actor, request, id)
		if rig.revision(t, alias) != 1 {
			t.Fatalf("local button %s did not commit once", alias)
		}
		state, present, err := rig.sequence.State(rig.entity(t, alias))
		if err != nil || !present {
			t.Fatalf("local button %s not published: %v", alias, err)
		}
		observed, fresh := state.CriticalValue()
		helper, revision, ok := observed.Helper()
		if _, pressed := helper.ButtonPress(); !fresh || !ok || !pressed || revision != 1 {
			t.Fatalf("button %s lacks local press receipt", alias)
		}
	}
	cast, _ := rig.request(t, "input_button.cast_to_nest_hub", "nha097-cast-refused", 0, helperwire.HelperOperation_HELPER_OPERATION_PRESS, nil)
	castResponse, castErr := rig.service.Dispatch(rig.ctx, actor, cast)
	if castErr != nil || castResponse == nil || castResponse.Status != actions.ActionStatus_ACTION_STATUS_FAILED ||
		castResponse.Error == nil || castResponse.Error.Detail == nil || castResponse.Error.Detail.Reason != "NoAuthorizedTarget" {
		t.Fatalf("Cast acquired an action or lost its closed reason: %v %v", castResponse, castErr)
	}
	var castRows int
	if err := rig.observer.QueryRow(rig.ctx, `SELECT count(*) FROM public.helper_values v JOIN public.registry_entities e ON e.id=v.entity_id WHERE e.manifest_alias='input_button.cast_to_nest_hub'`).Scan(&castRows); err != nil || castRows != 0 {
		t.Fatalf("Cast mutated a helper row: %d %v", castRows, err)
	}
	stale, _ := rig.request(t, "input_boolean.away_mode", "nha097-conflict-no-retry", 0, set,
		&helperwire.HelperValue{Value: &helperwire.HelperValue_Boolean{Boolean: true}})
	conflictResponse, conflictErr := rig.service.Dispatch(rig.ctx, actor, stale)
	if !errors.Is(conflictErr, integrations.ErrCommandOutcomeIndeterminate) ||
		conflictResponse == nil || conflictResponse.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE || rig.revision(t, "input_boolean.away_mode") != 1 {
		t.Fatalf("stale CAS mutated or retried: %v %v", conflictResponse, conflictErr)
	}
	invalidSelect, _ := rig.request(t, "input_select.time_of_day", "nha097-invalid-select", 2, set,
		&helperwire.HelperValue{Value: &helperwire.HelperValue_SelectedOption{SelectedOption: "Not an option"}})
	invalidResponse, invalidErr := rig.service.Dispatch(rig.ctx, actor, invalidSelect)
	if invalidErr != nil || invalidResponse == nil || invalidResponse.Status != actions.ActionStatus_ACTION_STATUS_FAILED || rig.revision(t, "input_select.time_of_day") != 2 {
		t.Fatalf("invalid option reached SQL: %v %v", invalidResponse, invalidErr)
	}
	for index, bad := range []struct {
		alias    string
		revision uint64
		value    *helperwire.HelperValue
	}{
		{"input_text.guest_bedroom_remote_last_event", 1, &helperwire.HelperValue{Value: &helperwire.HelperValue_Text{Text: strings.Repeat("x", 101)}}},
		{"input_datetime.gazon_date_de_semis", 2, &helperwire.HelperValue{Value: &helperwire.HelperValue_Datetime{Datetime: &helperwire.CivilDateTime{Date: date, Time: clock}}}},
		{"input_datetime.bathroom_heating_time", 1, &helperwire.HelperValue{Value: &helperwire.HelperValue_Date{Date: date}}},
		{"input_datetime.sdb1_occupancy_last_time", 1, &helperwire.HelperValue{Value: &helperwire.HelperValue_Date{Date: date}}},
		{"input_datetime.gazon_date_de_semis", 2, &helperwire.HelperValue{Value: &helperwire.HelperValue_Date{Date: &helperwire.CivilDate{Year: 2026, Month: 2, Day: 30}}}},
	} {
		request, _ := rig.request(t, bad.alias, "nha097-invalid-bound-"+string(rune('a'+index)), bad.revision, set, bad.value)
		response, err := rig.service.Dispatch(rig.ctx, actor, request)
		if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_FAILED || rig.revision(t, bad.alias) != int64(bad.revision) {
			t.Fatalf("invalid helper value crossed Host or SQL for %s: %v %v", bad.alias, response, err)
		}
	}
	// Independently open PostgreSQL and rebind stable IDs. This is a real SQL
	// read after the durable route has committed, not the pilot's cache.
	reopened, err := storage.Open(rig.ctx, rig.settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	repository, err := helpers.NewRepository(reopened)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := bundle.CompileV2(filepath.Join("..", "config", "bundle", "testdata", "nha097"), "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := helpers.Definitions(candidate)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range cases {
		for _, definition := range definitions {
			alias, _ := definition.Alias()
			text, _ := alias.Value()
			if text != item.alias {
				continue
			}
			handle, err := repository.Bind(rig.ctx, rig.entity(t, item.alias), definition)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := repository.Restore(rig.ctx, handle, definition)
			if err != nil {
				t.Fatal(err)
			}
			if revision, ok := snapshot.Revision().Number(); !ok || revision != item.prior+1 {
				t.Fatalf("reopened SQL revision %s: %d present=%t", item.alias, revision, ok)
			}
			wanted, _ := contracts.AdmitHelperSetValue(item.value)
			if value, present := snapshot.Value(); !present || value != wanted {
				t.Fatalf("reopened SQL changed helper value %s", item.alias)
			}
		}
	}
	// Freeze one image of every committed variant before session retirement.
	// The image uses Recorder's production snapshot codec and SQL batch path.
	type imageState struct {
		entity   string
		value    domain.HelperValue
		revision uint64
		row      storage.RecorderSnapshotRecord
	}
	var imageStates []imageState
	imageAliases := []string{
		"input_button.start_bird_s_radio", "input_button.pause_bird_s_radio", "input_button.trash_out",
	}
	for _, item := range cases {
		imageAliases = append(imageAliases, item.alias)
	}
	for _, alias := range imageAliases {
		entity := rig.entity(t, alias)
		entityText, _ := entity.Value()
		state, present, err := rig.sequence.State(entity)
		if err != nil || !present {
			t.Fatalf("helper snapshot source %s: %v", alias, err)
		}
		current, present := state.CurrentValue()
		value, revision, helperPresent := current.Helper()
		if !present || !helperPresent {
			t.Fatalf("helper snapshot lacks value %s", alias)
		}
		row, err := storage.NewRecorderSnapshotRecord(state, 4<<20)
		if err != nil {
			t.Fatalf("helper snapshot codec %s: %v", alias, err)
		}
		imageStates = append(imageStates, imageState{entityText, value, revision, row})
	}
	sort.Slice(imageStates, func(i, j int) bool { return imageStates[i].entity < imageStates[j].entity })
	// Recorder writes the same observed helper kind and deterministic payload.
	stopCtx, cancel := context.WithTimeout(rig.ctx, 8*time.Second)
	defer cancel()
	if err := rig.supervisor.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	if err := rig.recording.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	var payload, digest []byte
	entityText, _ := rig.entity(t, "input_boolean.away_mode").Value()
	if err := rig.observer.QueryRow(rig.ctx, `SELECT payload,payload_digest FROM public.state_history WHERE entity_id=$1 AND value_kind=6 ORDER BY sequence DESC LIMIT 1`, entityText).Scan(&payload, &digest); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	if string(sum[:]) != string(digest) {
		t.Fatal("Recorder payload digest drift")
	}
	var wire statewire.ObservedState
	if err := proto.Unmarshal(payload, &wire); err != nil {
		t.Fatal(err)
	}
	read, err := contracts.AdmitWireState(&wire, nil, &contracts.StateCapabilityAdmission{Catalog: rig.catalog, Endpoint: "value"})
	if err != nil {
		t.Fatal(err)
	}
	value, present := read.CurrentValue()
	helper, revision, presentHelper := value.Helper()
	if !present || !presentHelper || revision != 1 {
		t.Fatalf("Recorder re-admission lost false/CAS0: %v %d", helper, revision)
	}
	if boolean, ok := helper.Boolean(); !ok || boolean {
		t.Fatalf("Recorder re-admission changed false: %v", helper)
	}
	epoch, err := storage.ParseRecorderEpoch("0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f")
	if err != nil {
		t.Fatal(err)
	}
	if _, outcome, err := rig.store.OpenRecorderEpoch(rig.ctx, epoch, time.Now()); outcome != storage.TransactionCommitted || err != nil {
		t.Fatalf("snapshot epoch: %v %v", outcome, err)
	}
	rows := make([]storage.RecorderSnapshotRecord, len(imageStates))
	for index, item := range imageStates {
		rows[index] = item.row
	}
	batch, err := storage.NewRecorderBatch(epoch, 0, 0, nil, nil, 0, &storage.RecorderImage{Boundary: 0, States: rows})
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := rig.store.PersistRecorderBatch(rig.ctx, batch); outcome != storage.TransactionCommitted || err != nil {
		t.Fatalf("helper snapshot batch: %v %v", outcome, err)
	}
	epochText, _ := epoch.Hex()
	for _, item := range imageStates {
		var payload, digest []byte
		if err := rig.observer.QueryRow(rig.ctx, `SELECT payload,payload_digest FROM public.recorder_snapshots WHERE epoch=decode($1,'hex') AND entity_id=$2`, epochText, item.entity).Scan(&payload, &digest); err != nil {
			t.Fatalf("reopen helper snapshot %s: %v", item.entity, err)
		}
		sum := sha256.Sum256(payload)
		if string(sum[:]) != string(digest) {
			t.Fatalf("helper snapshot digest drift %s", item.entity)
		}
		var wire statewire.ObservedState
		if err := proto.Unmarshal(payload, &wire); err != nil {
			t.Fatal(err)
		}
		read, err := contracts.AdmitWireState(&wire, nil, &contracts.StateCapabilityAdmission{Catalog: rig.catalog, Endpoint: "value"})
		if err != nil {
			t.Fatalf("re-admit helper snapshot %s: %v", item.entity, err)
		}
		current, present := read.CurrentValue()
		value, revision, helperPresent := current.Helper()
		if !present || !helperPresent || revision != item.revision || value != item.value {
			t.Fatalf("helper snapshot changed %s: revision=%d present=%t", item.entity, revision, helperPresent)
		}
	}
}

func TestNHA097PilotFaultsWithdrawAndRestoreWithoutRetryingMutation(t *testing.T) {
	for _, mode := range []string{"commit-unknown", "publish-failed"} {
		t.Run(mode, func(t *testing.T) {
			timer := &nha097RetryTimer{entered: make(chan struct{}, 2), release: make(chan time.Time, 2)}
			rig := nha097Commands(t, timer)
			alias := "input_boolean.default_dashboard" // restored true at SQL revision 1
			actor, _ := domain.ParseActorID("nha097-local-fault-actor")
			if mode == "commit-unknown" {
				_, err := rig.observer.Exec(rig.ctx, `CREATE SEQUENCE public.nha097_commit_attempts;
					CREATE FUNCTION public.nha097_deferred_failure() RETURNS trigger LANGUAGE plpgsql AS $$
					BEGIN PERFORM nextval('public.nha097_commit_attempts'); RAISE EXCEPTION 'synthetic deferred failure'; END $$;
					CREATE CONSTRAINT TRIGGER nha097_deferred_failure AFTER UPDATE ON public.helper_values
					DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha097_deferred_failure()`)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				rig.sink.failNext.Store(true)
			}
			request, id := rig.request(t, alias, "nha097-fault-"+mode, 1, helperwire.HelperOperation_HELPER_OPERATION_SET,
				&helperwire.HelperValue{Value: &helperwire.HelperValue_Boolean{Boolean: false}})
			response, err := rig.service.Dispatch(rig.ctx, actor, request)
			if !errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
				t.Fatalf("fault was acknowledged: %v %v", response, err)
			}
			if mode == "commit-unknown" {
				if rig.revision(t, alias) != 1 {
					t.Fatal("deferred COMMIT failure changed SQL revision")
				}
			} else if rig.sink.rejected.Load() != 1 || rig.revision(t, alias) != 2 {
				t.Fatal("known COMMIT was not kept after failed publication")
			}
			progress, err := rig.host.ActionStatus(id)
			if err != nil || progress.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
				t.Fatalf("uncertain mutation was confirmed: %+v %v", progress, err)
			}
			select {
			case <-timer.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("pilot fault did not reach supervisor retry")
			}
			state, present, err := rig.sequence.State(rig.entity(t, alias))
			if err != nil || !present {
				t.Fatalf("retired state missing: %v", err)
			}
			_, quality, _, _, _, err := state.Components()
			if err != nil {
				t.Fatal(err)
			}
			availability, freshness, _, _ := quality.Components()
			if availability != domain.AvailabilityUnavailable || freshness != domain.FreshnessStale {
				t.Fatalf("stale Fresh state survived fault: %v/%v", availability, freshness)
			}
			if _, fresh := state.CriticalValue(); fresh {
				t.Fatal("retired helper remained actionable")
			}
			if mode == "commit-unknown" {
				var attempts int64
				if err := rig.observer.QueryRow(rig.ctx, `SELECT last_value FROM public.nha097_commit_attempts`).Scan(&attempts); err != nil || attempts != 1 {
					t.Fatalf("unknown COMMIT retried: %d %v", attempts, err)
				}
				if _, err := rig.observer.Exec(rig.ctx, `DROP TRIGGER nha097_deferred_failure ON public.helper_values`); err != nil {
					t.Fatal(err)
				}
			}
			timer.release <- time.Now()
			nha077Await(t, "SQL restored into a new helper session", func() bool {
				status, ok := rig.supervisor.Status(rig.instance)
				if !ok || status.Phase != integrations.PilotRunning || status.Attempts < 2 {
					return false
				}
				state, present, err := rig.sequence.State(rig.entity(t, alias))
				if err != nil || !present {
					return false
				}
				value, present := state.CurrentValue()
				helper, revision, ok := value.Helper()
				if !present || !ok {
					return false
				}
				boolean, ok := helper.Boolean()
				if mode == "commit-unknown" {
					return ok && boolean && revision == 1
				}
				return ok && !boolean && revision == 2
			})
			if mode == "publish-failed" && rig.revision(t, alias) != 2 {
				t.Fatal("recovery re-emitted a command instead of restoring SQL")
			}
		})
	}
}

func TestNHA097CounterAlignmentIsSetOnlyOnDurableRoute(t *testing.T) {
	root := t.TempDir()
	source := []byte("counter:\n  bathroom_heating_used_today: {name: Test counter, minimum: 0, maximum: 6, step: 2, initial: 0, restore: true}\n")
	if err := os.WriteFile(filepath.Join(root, "configuration.yaml"), source, 0600); err != nil {
		t.Fatal(err)
	}
	candidate, err := bundle.CompileV2(root, "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	rig := nha097CommandsWithCandidate(t, nil, candidate)
	actor, _ := domain.ParseActorID("nha097-counter-command-actor")
	alias := "counter.bathroom_heating_used_today"
	invalid, _ := rig.request(t, alias, "nha097-counter-nonaligned", 1, helperwire.HelperOperation_HELPER_OPERATION_SET,
		&helperwire.HelperValue{Value: &helperwire.HelperValue_Counter{Counter: 3}})
	response, err := rig.service.Dispatch(rig.ctx, actor, invalid)
	if !errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) ||
		response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE || rig.revision(t, alias) != 1 {
		t.Fatalf("counter SET nonaligned crossed SQL: %v %v", response, err)
	}
	increment, id := rig.request(t, alias, "nha097-counter-increment", 1, helperwire.HelperOperation_HELPER_OPERATION_INCREMENT, nil)
	rig.dispatch(t, actor, increment, id)
	if rig.revision(t, alias) != 2 {
		t.Fatal("counter increment did not commit one step")
	}
	aligned, id := rig.request(t, alias, "nha097-counter-aligned", 2, helperwire.HelperOperation_HELPER_OPERATION_SET,
		&helperwire.HelperValue{Value: &helperwire.HelperValue_Counter{Counter: 4}})
	rig.dispatch(t, actor, aligned, id)
	if rig.revision(t, alias) != 3 {
		t.Fatal("aligned counter SET did not commit")
	}
}

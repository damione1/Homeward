package helpers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type nha033Fixture struct {
	ctx      context.Context
	store    *storage.Store
	repo     *Repository
	registry *registry.Repository
	settings storage.Settings
	cluster  testpg.Cluster
}

func nha033Setup(t *testing.T, synthetic bool) nha033Fixture {
	t.Helper()
	cluster := testpg.Start(t)
	settings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port,
		Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"},
		func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := os.ReadFile("../../scope/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if synthetic {
		manifestBytes = nha033SyntheticManifest(t)
	}
	manifest, err := registry.ParseManifest(manifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	identities, err := registry.NewRepository(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	return nha033Fixture{ctx: ctx, store: store, repo: repo, registry: identities, settings: settings, cluster: cluster}
}

// This is a separate test manifest, not an amendment of the frozen scope. The
// real retained scope has no input_number; all fixture provenance stays synthetic.
func nha033SyntheticManifest(t *testing.T) []byte {
	t.Helper()
	type entity struct {
		ID     string `json:"entity_id"`
		Source string `json:"source"`
	}
	ids := []string{"input_boolean.away_mode", "input_boolean.guests", "input_number.nha033_number", "counter.bathroom_heating_used_today"}
	for len(ids) < 850 {
		ids = append(ids, fmt.Sprintf("sensor.nha033_synthetic_%03d", len(ids)))
	}
	sort.Strings(ids)
	manifest := struct {
		Version  int      `json:"version"`
		Digest   string   `json:"ids_sha256"`
		Entities []entity `json:"entities"`
	}{Version: 1}
	digest := sha256.Sum256([]byte(strings.Join(ids, "\n") + "\n"))
	manifest.Digest = hex.EncodeToString(digest[:])
	for _, id := range ids {
		manifest.Entities = append(manifest.Entities, entity{ID: id, Source: "nha033-synthetic-only"})
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func nha033Compile(t *testing.T, source string) bundle.Candidate {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "configuration.yaml"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	candidate, err := bundle.Compile(root, "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return candidate
}

func nha033Boolean(t *testing.T, initial string) Definition {
	t.Helper()
	fields := "{}"
	if initial != "" {
		fields = "{initial: " + initial + "}"
	}
	definition, err := BooleanDefinition(nha033Compile(t, "input_boolean:\n  away_mode: "+fields+"\n").Booleans()[0])
	if err != nil {
		t.Fatal(err)
	}
	return definition
}

func nha033Number(t *testing.T, maximum, initial string) Definition {
	t.Helper()
	fields := "min: -10, max: " + maximum + ", step: 0.5"
	if initial != "" {
		fields += ", initial: " + initial
	}
	definition, err := NumberDefinition(nha033Compile(t, "input_number:\n  nha033_number: {"+fields+"}\n").Numbers()[0])
	if err != nil {
		t.Fatal(err)
	}
	return definition
}

func nha033Counter(t *testing.T, initial string) Definition {
	t.Helper()
	fields := "minimum: -10, maximum: 100, step: 2"
	if initial != "" {
		fields += ", initial: " + initial
	}
	definition, err := CounterDefinition(nha033Compile(t, "counter:\n  bathroom_heating_used_today: {"+fields+"}\n").Counters()[0])
	if err != nil {
		t.Fatal(err)
	}
	return definition
}

func (f nha033Fixture) bind(t *testing.T, definition Definition) Handle {
	t.Helper()
	alias, _ := definition.alias().Value()
	instance, err := domain.ParseIntegrationInstanceID("nha033-local-only")
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("nha033-fixture", alias)
	if err != nil {
		t.Fatal(err)
	}
	configured, err := f.registry.ConfigureBinding(f.ctx, instance, key, alias)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := f.registry.Resolve(f.ctx, instance, []integrations.SourceKey{key})
	if err != nil || len(resolved) != 1 || configured != resolved[0] {
		t.Fatalf("real registry admission: %v", err)
	}
	handle, err := f.repo.Bind(f.ctx, resolved[0].Entity, definition)
	if err != nil {
		t.Fatal(err)
	}
	return handle
}

func nha033SQL(t *testing.T, f nha033Fixture, statement string, arguments ...any) {
	t.Helper()
	if err := f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(f.ctx, statement, arguments...)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func nha033Code(t *testing.T, err error, code ErrorCode) {
	t.Helper()
	var typed *Error
	if !errors.As(err, &typed) || typed.Code() != code {
		t.Fatalf("want code %d, got %v", code, err)
	}
}

func nha033Snapshot(t *testing.T, snapshot Snapshot, value Value, revision uint64) {
	t.Helper()
	actual, present := snapshot.Value()
	actualRevision, validRevision := snapshot.Revision().Number()
	if !snapshot.Valid() || !present || actual != value || !validRevision || actualRevision != revision {
		t.Fatalf("snapshot value/revision: present=%t value=%v revision=%d valid=%t", present, actual, actualRevision, validRevision)
	}
	if _, present := snapshot.UpdatedAt(); !present {
		t.Fatal("committed value has no update time")
	}
}

func nha033NumberValue(t *testing.T, value float64) Value {
	t.Helper()
	result, err := Number(value)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestNHA033ClosedTypesAndSQLBoundaries(t *testing.T) {
	if (Value{}).Valid() || (Definition{}).Valid() || (Snapshot{}).Valid() || (Handle{}).valid() {
		t.Fatal("Go zero value was operationally admitted")
	}
	if _, valid := (Revision{}).Number(); valid {
		t.Fatal("Go zero revision was treated as absence")
	}
	for _, value := range []Value{Boolean(false), nha033NumberValue(t, 0), Counter(0), Counter(math.MinInt64), Counter(math.MaxInt64)} {
		tag, wire, err := encode(value)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decode(tag, wire)
		if err != nil || decoded != value {
			t.Fatalf("SQL roundtrip: %v", err)
		}
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, err := Number(value)
		nha033Code(t, err, InvalidValue)
		_, err = decode(2, sqlValue{number: pgtype.Float8{Float64: value, Valid: true}})
		nha033Code(t, err, Corrupt)
	}
	for _, tag := range []int16{-1, 0, 1, 2, 3, 4, 127} {
		if _, err := decode(tag, sqlValue{}); err == nil {
			t.Fatalf("unknown/null variant admitted: %d", tag)
		}
	}
	_, err := decode(1, sqlValue{boolean: pgtype.Bool{Valid: true}, counter: pgtype.Int8{Valid: true}})
	nha033Code(t, err, Corrupt)
	_, err = BooleanDefinition(bundle.BoolHelper{})
	nha033Code(t, err, InvalidDefinition)
	_, err = NumberDefinition(bundle.NumberHelper{})
	nha033Code(t, err, InvalidDefinition)
	_, err = CounterDefinition(bundle.CounterHelper{})
	nha033Code(t, err, InvalidDefinition)
	_, err = NewRepository(nil)
	nha033Code(t, err, InvalidInput)
	var nilRepository *Repository
	_, err = nilRepository.Restore(context.Background(), Handle{}, Definition{})
	nha033Code(t, err, InvalidInput)
	var typedNil *Error
	if typedNil.Code() != ErrorInvalid || typedNil.Error() == "" {
		t.Fatal("typed nil diagnostic is unsafe")
	}
}

func TestNHA033RestoreReopensStoreAndKeepsCommittedValue(t *testing.T) {
	f := nha033Setup(t, false)
	root := t.TempDir()
	path := filepath.Join(root, "configuration.yaml")
	source := []byte("input_boolean:\n  away_mode: {initial: false}\ncounter:\n  bathroom_heating_used_today: {initial: 0, minimum: 0, maximum: 100, step: 2}\n")
	if err := os.WriteFile(path, source, 0600); err != nil {
		t.Fatal(err)
	}
	candidate, err := bundle.Compile(root, "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	boolean, _ := BooleanDefinition(candidate.Booleans()[0])
	counter, _ := CounterDefinition(candidate.Counters()[0])
	handle := f.bind(t, boolean)
	countHandle := f.bind(t, counter)
	first, err := f.repo.Restore(f.ctx, handle, boolean)
	if err != nil {
		t.Fatal(err)
	}
	nha033Snapshot(t, first, Boolean(false), 1)
	initialCount, err := f.repo.Restore(f.ctx, countHandle, counter)
	if err != nil {
		t.Fatal(err)
	}
	nha033Snapshot(t, initialCount, Counter(0), 1)
	changed, err := f.repo.CompareAndSwap(f.ctx, handle, boolean, first.Revision(), Boolean(true))
	if err != nil {
		t.Fatal(err)
	}
	nha033Snapshot(t, changed, Boolean(true), 2)
	count, err := f.repo.CompareAndSwap(f.ctx, countHandle, counter, initialCount.Revision(), Counter(8))
	if err != nil {
		t.Fatal(err)
	}
	nha033Snapshot(t, count, Counter(8), 2)
	if actual, err := os.ReadFile(path); err != nil || string(actual) != string(source) {
		t.Fatalf("mutations rewrote YAML: %v", err)
	}
	// The operator changes both YAML initials. Stop the real Store, reopen it,
	// recompile the changed YAML, and construct a new repository on that pool.
	changedSource := []byte("input_boolean:\n  away_mode: {initial: true}\ncounter:\n  bathroom_heating_used_today: {initial: 2, minimum: 0, maximum: 100, step: 2}\n")
	if err := os.WriteFile(path, changedSource, 0600); err != nil {
		t.Fatal(err)
	}
	// Make the persisted boolean differ from the NEW initial too.
	last, err := f.repo.CompareAndSwap(f.ctx, handle, boolean, changed.Revision(), Boolean(false))
	if err != nil {
		t.Fatal(err)
	}
	f.store.Close()
	reopened, err := storage.Open(f.ctx, f.settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	repo, err := NewRepository(reopened)
	if err != nil {
		t.Fatal(err)
	}
	recompiled, err := bundle.Compile(root, "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	newBoolean, _ := BooleanDefinition(recompiled.Booleans()[0])
	newCounter, _ := CounterDefinition(recompiled.Counters()[0])
	for _, test := range []struct {
		handle Handle
		def    Definition
		want   Value
		rev    uint64
	}{{handle, newBoolean, Boolean(false), 3}, {countHandle, newCounter, Counter(8), 2}} {
		entity, _ := test.handle.Entity()
		rebound, err := repo.Bind(f.ctx, entity, test.def)
		if err != nil || rebound != test.handle {
			t.Fatalf("reopen changed identity: %v", err)
		}
		restored, err := repo.Restore(f.ctx, rebound, test.def)
		if err != nil {
			t.Fatal(err)
		}
		nha033Snapshot(t, restored, test.want, test.rev)
	}
	if number, _ := last.Revision().Number(); number != 3 {
		t.Fatal("last committed revision was lost")
	}
	if actual, err := os.ReadFile(path); err != nil || string(actual) != string(changedSource) {
		t.Fatalf("restore rewrote YAML: %v", err)
	}
}

func TestNHA033AbsenceAndConcurrentFirstCAS(t *testing.T) {
	f := nha033Setup(t, true)
	definitions := []Definition{nha033Boolean(t, ""), nha033Number(t, "10", ""), nha033Counter(t, "")}
	for _, definition := range definitions {
		t.Run(fmt.Sprint(definition.Kind()), func(t *testing.T) {
			handle := f.bind(t, definition)
			absent, err := f.repo.Restore(f.ctx, handle, definition)
			if err != nil || !absent.Valid() {
				t.Fatalf("absence: %v", err)
			}
			if _, present := absent.Value(); present {
				t.Fatal("absence became false/zero")
			}
			if number, valid := absent.Revision().Number(); !valid || number != 0 {
				t.Fatal("absence has no admitted CAS token")
			}
			if _, present := absent.UpdatedAt(); present {
				t.Fatal("absence has a fabricated update time")
			}
			_, err = f.repo.Reset(f.ctx, handle, definition, absent.Revision())
			nha033Code(t, err, InitialAbsent)
			value := Boolean(false)
			switch definition.Kind() {
			case KindNumber:
				value = nha033NumberValue(t, 0)
			case KindCounter:
				value = Counter(0)
			}
			nha033Race(t, f, handle, definition, absent.Revision(), value, 1)
			present, err := f.repo.Restore(f.ctx, handle, definition)
			if err != nil {
				t.Fatal(err)
			}
			nha033Snapshot(t, present, value, 1)
			nha033Race(t, f, handle, definition, present.Revision(), value, 2)
		})
	}
}

func nha033Race(t *testing.T, f nha033Fixture, handle Handle, definition Definition, expected Revision, value Value, revision uint64) {
	t.Helper()
	otherStore, err := storage.Open(f.ctx, f.settings)
	if err != nil {
		t.Fatal(err)
	}
	defer otherStore.Close()
	other, err := NewRepository(otherStore)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	type result struct {
		snapshot Snapshot
		err      error
	}
	results := make(chan result, 2)
	for _, repo := range []*Repository{f.repo, other} {
		go func() {
			<-start
			snapshot, err := repo.CompareAndSwap(f.ctx, handle, definition, expected, value)
			results <- result{snapshot, err}
		}()
	}
	close(start)
	successes, conflicts := 0, 0
	for range 2 {
		select {
		case result := <-results:
			if result.err == nil {
				successes++
				nha033Snapshot(t, result.snapshot, value, revision)
			} else {
				conflicts++
				nha033Code(t, result.err, Conflict)
				if result.snapshot.Valid() {
					t.Fatal("conflict exposed an uncommitted snapshot")
				}
			}
		case <-f.ctx.Done():
			t.Fatal("CAS clients did not complete")
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("CAS: success=%d conflict=%d", successes, conflicts)
	}
}

func TestNHA033NumberConstraintsResetAndReopen(t *testing.T) {
	f := nha033Setup(t, true)
	definition := nha033Number(t, "10", "0")
	handle := f.bind(t, definition)
	initial, err := f.repo.Restore(f.ctx, handle, definition)
	if err != nil {
		t.Fatal(err)
	}
	nha033Snapshot(t, initial, nha033NumberValue(t, 0), 1)
	value := nha033NumberValue(t, 7.25)
	changed, err := f.repo.CompareAndSwap(f.ctx, handle, definition, initial.Revision(), value)
	if err != nil {
		t.Fatal(err)
	}
	nha033Snapshot(t, changed, value, 2)
	for _, invalid := range []Value{Value{}, Boolean(false), Counter(0), nha033NumberValue(t, -10.1), nha033NumberValue(t, 10.1)} {
		refused, err := f.repo.CompareAndSwap(f.ctx, handle, definition, changed.Revision(), invalid)
		nha033Code(t, err, InvalidValue)
		if refused.Valid() {
			t.Fatal("invalid value exposed a snapshot")
		}
	}
	f.store.Close()
	store, err := storage.Open(f.ctx, f.settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repo, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	active := nha033Number(t, "10", "2")
	restored, err := repo.Restore(f.ctx, handle, active)
	if err != nil {
		t.Fatal(err)
	}
	nha033Snapshot(t, restored, value, 2)
	reset, err := repo.Reset(f.ctx, handle, active, restored.Revision())
	if err != nil {
		t.Fatal(err)
	}
	nha033Snapshot(t, reset, nha033NumberValue(t, 2), 3)
	equalReset, err := repo.Reset(f.ctx, handle, active, reset.Revision())
	if err != nil {
		t.Fatal(err)
	}
	nha033Snapshot(t, equalReset, nha033NumberValue(t, 2), 4)
	incompatible := nha033Number(t, "1", "0")
	refused, err := repo.Restore(f.ctx, handle, incompatible)
	nha033Code(t, err, Incompatible)
	if refused.Valid() {
		t.Fatal("incompatible restore exposed an initial")
	}
	_, err = repo.CompareAndSwap(f.ctx, handle, incompatible, equalReset.Revision(), nha033NumberValue(t, 0))
	nha033Code(t, err, Incompatible)
	still, err := repo.Restore(f.ctx, handle, active)
	if err != nil {
		t.Fatal(err)
	}
	nha033Snapshot(t, still, nha033NumberValue(t, 2), 4)
}

func TestNHA033CounterBoundsAndAdmittedInitial(t *testing.T) {
	f := nha033Setup(t, false)
	definition := nha033Counter(t, "1") // bundle admits 1 even with step 2.
	handle := f.bind(t, definition)
	initial, err := f.repo.Restore(f.ctx, handle, definition)
	if err != nil {
		t.Fatal(err)
	}
	nha033Snapshot(t, initial, Counter(1), 1)
	for _, invalid := range []Value{Counter(-11), Counter(101), Boolean(true), nha033NumberValue(t, 2)} {
		_, err := f.repo.CompareAndSwap(f.ctx, handle, definition, initial.Revision(), invalid)
		nha033Code(t, err, InvalidValue)
	}
	last, err := f.repo.CompareAndSwap(f.ctx, handle, definition, initial.Revision(), Counter(100))
	if err != nil {
		t.Fatal(err)
	}
	nha033Snapshot(t, last, Counter(100), 2)
	if got, err := f.repo.Restore(f.ctx, handle, nha033Counter(t, "3")); err != nil || got.Revision() != last.Revision() {
		t.Fatalf("changed initial overwrote counter: %v", err)
	}
}

func TestNHA033StableRegistryIdentityRenameAndRebinding(t *testing.T) {
	f := nha033Setup(t, false)
	definition := nha033Boolean(t, "false")
	handle := f.bind(t, definition)
	initial, err := f.repo.Restore(f.ctx, handle, definition)
	if err != nil {
		t.Fatal(err)
	}
	entity, _ := handle.Entity()
	if err := f.registry.Rename(f.ctx, entity, "input_boolean.nha033_renamed"); err != nil {
		t.Fatal(err)
	}
	rebound, err := f.repo.Bind(f.ctx, entity, definition)
	if err != nil || rebound != handle {
		t.Fatalf("rename changed stable binding: %v", err)
	}
	changed, err := f.repo.CompareAndSwap(f.ctx, rebound, definition, initial.Revision(), Boolean(true))
	if err != nil {
		t.Fatal(err)
	}
	nha033Snapshot(t, changed, Boolean(true), 2)
	for _, text := range []string{"ent_unknown_but_syntactically_valid", "input_boolean.away_mode", "input_boolean.nha033_renamed"} {
		fake, err := domain.ParseEntityID(text)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.repo.Bind(f.ctx, fake, definition)
		nha033Code(t, err, Unregistered)
	}
	otherCandidate := nha033Compile(t, "input_boolean:\n  guests: {initial: false}\n")
	otherDefinition, _ := BooleanDefinition(otherCandidate.Booleans()[0])
	other := f.bind(t, otherDefinition)
	otherEntity, _ := other.Entity()
	_, err = f.repo.Bind(f.ctx, otherEntity, definition)
	nha033Code(t, err, BindingMismatch)
	_, err = f.repo.Restore(f.ctx, handle, otherDefinition)
	nha033Code(t, err, BindingMismatch)
	_, err = f.repo.CompareAndSwap(f.ctx, other, otherDefinition, changed.Revision(), Boolean(true))
	nha033Code(t, err, InvalidInput)
	for _, alias := range []string{"input_boolean.away_mode", "input_boolean.nha033_renamed"} {
		if err := f.registry.Rename(f.ctx, otherEntity, alias); !errors.Is(err, registry.ErrAliasConflict) {
			t.Fatalf("historical/current alias was rebound to B: %v", err)
		}
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha033-other-instance")
	key, _ := integrations.NewSourceKey("nha033-other-device", "nha033-other-source")
	if _, err := f.registry.ConfigureBinding(f.ctx, instance, key, "input_boolean.away_mode"); !errors.Is(err, registry.ErrBindingConflict) {
		t.Fatalf("original declaration rebound to B: %v", err)
	}
	still, err := f.repo.Restore(f.ctx, handle, definition)
	if err != nil {
		t.Fatal(err)
	}
	nha033Snapshot(t, still, Boolean(true), 2)
	if err := f.registry.Tombstone(f.ctx, entity); err != nil {
		t.Fatal(err)
	}
	_, err = f.repo.Restore(f.ctx, handle, definition)
	nha033Code(t, err, Retired)
	_, err = f.repo.CompareAndSwap(f.ctx, handle, definition, changed.Revision(), Boolean(false))
	nha033Code(t, err, Retired)
	// A previously admitted handle does not bypass later source-binding removal.
	otherText, _ := otherEntity.Value()
	nha033SQL(t, f, `DELETE FROM public.registry_source_bindings WHERE entity_id=$1`, otherText)
	_, err = f.repo.Restore(f.ctx, other, otherDefinition)
	nha033Code(t, err, Unregistered)
}

func TestNHA033ConcurrentInitialRestoreAndInvalidBoundaries(t *testing.T) {
	f := nha033Setup(t, false)
	definition := nha033Boolean(t, "false")
	handle := f.bind(t, definition)
	start := make(chan struct{})
	type result struct {
		snapshot Snapshot
		err      error
	}
	results := make(chan result, 2)
	second, err := NewRepository(f.store)
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range []*Repository{f.repo, second} {
		go func() {
			<-start
			snapshot, err := repo.Restore(f.ctx, handle, definition)
			results <- result{snapshot, err}
		}()
	}
	close(start)
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		nha033Snapshot(t, result.snapshot, Boolean(false), 1)
	}
	_, err = f.repo.Restore(nil, handle, definition)
	nha033Code(t, err, InvalidInput)
	_, err = f.repo.Restore(f.ctx, handle, Definition{})
	nha033Code(t, err, InvalidDefinition)
	_, err = f.repo.CompareAndSwap(f.ctx, handle, definition, Revision{}, Boolean(true))
	nha033Code(t, err, InvalidInput)
	zeroStore, _ := NewRepository(&storage.Store{})
	_, err = zeroStore.Restore(f.ctx, handle, definition)
	nha033Code(t, err, Unavailable)
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	_, err = f.repo.Restore(ctx, handle, definition)
	nha033Code(t, err, Unavailable)
}

func TestNHA033RejectsCorruptionWithoutFallback(t *testing.T) {
	f := nha033Setup(t, false)
	definition := nha033Boolean(t, "false")
	handle := f.bind(t, definition)
	initial, err := f.repo.Restore(f.ctx, handle, definition)
	if err != nil {
		t.Fatal(err)
	}
	entity, _ := handle.entity.Value()
	// The authoritative SQL shape prevents malformed payloads normally.
	for _, statement := range []string{
		`UPDATE public.helper_values SET kind=99 WHERE entity_id=$1`,
		`UPDATE public.helper_values SET boolean_value=NULL WHERE entity_id=$1`,
		`UPDATE public.helper_values SET counter_value=0 WHERE entity_id=$1`,
		`UPDATE public.helper_values SET kind=2,boolean_value=NULL,number_value='NaN'::double precision WHERE entity_id=$1`,
		`UPDATE public.helper_values SET kind=2,boolean_value=NULL,number_value='Infinity'::double precision WHERE entity_id=$1`,
		`UPDATE public.helper_values SET revision=0 WHERE entity_id=$1`,
	} {
		if err := f.store.InTransaction(f.ctx, func(tx pgx.Tx) error { _, err := tx.Exec(f.ctx, statement, entity); return err }); err == nil {
			t.Fatal("schema accepted malformed helper value")
		}
	}
	// Simulate externally corrupted data after bypassing one SQL constraint.
	nha033SQL(t, f, `ALTER TABLE public.helper_values DROP CONSTRAINT helper_values_payload`)
	for _, statement := range []string{
		`UPDATE public.helper_values SET kind=99,boolean_value=false,number_value=NULL,counter_value=NULL WHERE entity_id=$1`,
		`UPDATE public.helper_values SET kind=1,boolean_value=NULL,number_value=NULL,counter_value=NULL WHERE entity_id=$1`,
		`UPDATE public.helper_values SET kind=1,boolean_value=false,number_value=NULL,counter_value=0 WHERE entity_id=$1`,
		`UPDATE public.helper_values SET kind=2,boolean_value=NULL,number_value='NaN'::double precision,counter_value=NULL WHERE entity_id=$1`,
	} {
		nha033SQL(t, f, statement, entity)
		refused, err := f.repo.Restore(f.ctx, handle, definition)
		nha033Code(t, err, Corrupt)
		if refused.Valid() {
			t.Fatal("corruption became an initial snapshot")
		}
		_, err = f.repo.CompareAndSwap(f.ctx, handle, definition, initial.Revision(), Boolean(true))
		nha033Code(t, err, Corrupt)
	}
	// A well-shaped SQL payload of a different variant is incompatible with the
	// active helper, rather than corrupt. Restore must still refuse to overwrite.
	nha033SQL(t, f, `UPDATE public.helper_values SET kind=3,boolean_value=NULL,number_value=NULL,counter_value=0 WHERE entity_id=$1`, entity)
	_, err = f.repo.Restore(f.ctx, handle, definition)
	nha033Code(t, err, Incompatible)
	nha033SQL(t, f, `UPDATE public.helper_values SET kind=1,boolean_value=false,counter_value=NULL,updated_at='0001-01-01 00:00:00+00' WHERE entity_id=$1`, entity)
	_, err = f.repo.Restore(f.ctx, handle, definition)
	nha033Code(t, err, Corrupt)
	nha033SQL(t, f, `UPDATE public.helper_values SET updated_at='Infinity'::timestamptz WHERE entity_id=$1`, entity)
	_, err = f.repo.Restore(f.ctx, handle, definition)
	nha033Code(t, err, Corrupt)
	nha033SQL(t, f, `ALTER TABLE public.helper_values DROP CONSTRAINT helper_values_revision_check`)
	nha033SQL(t, f, `UPDATE public.helper_values SET updated_at=clock_timestamp(),revision=0 WHERE entity_id=$1`, entity)
	_, err = f.repo.Restore(f.ctx, handle, definition)
	nha033Code(t, err, Corrupt)
	nha033SQL(t, f, `UPDATE public.helper_values SET updated_at=clock_timestamp(),revision=9223372036854775807 WHERE entity_id=$1`, entity)
	exhausted, err := f.repo.Restore(f.ctx, handle, definition)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.repo.CompareAndSwap(f.ctx, handle, definition, exhausted.Revision(), Boolean(true))
	nha033Code(t, err, RevisionExhausted)
}

func TestNHA033AcknowledgementFollowsSQLCommitBarrier(t *testing.T) {
	f := nha033Setup(t, false)
	definition := nha033Boolean(t, "false")
	handle := f.bind(t, definition)
	initial, err := f.repo.Restore(f.ctx, handle, definition)
	if err != nil {
		t.Fatal(err)
	}
	nha033SQL(t, f, `CREATE FUNCTION public.nha033_commit_barrier() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_advisory_xact_lock(481316,33033); RETURN NEW; END $$;
		CREATE CONSTRAINT TRIGGER nha033_commit_barrier AFTER UPDATE ON public.helper_values
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha033_commit_barrier()`)
	locked := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(f.ctx, `SELECT pg_advisory_xact_lock(481316,33033)`); err != nil {
				return err
			}
			close(locked)
			select {
			case <-release:
				return nil
			case <-f.ctx.Done():
				return f.ctx.Err()
			}
		})
	}()
	releaseBarrier := func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}
	t.Cleanup(releaseBarrier)
	select {
	case <-locked:
	case err := <-holderDone:
		t.Fatalf("barrier holder: %v", err)
	case <-f.ctx.Done():
		t.Fatal("barrier holder timed out")
	}
	type result struct {
		snapshot Snapshot
		err      error
	}
	ack := make(chan result, 1)
	go func() {
		snapshot, err := f.repo.CompareAndSwap(f.ctx, handle, definition, initial.Revision(), Boolean(true))
		ack <- result{snapshot, err}
	}()
	// The deferred trigger runs during COMMIT. Observe its actual SQL waiter,
	// rather than relying on elapsed time to assume the write reached COMMIT.
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		err := f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(f.ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory'
				AND classid=481316 AND objid=33033 AND NOT granted)`).Scan(&blocked)
		})
		if err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case result := <-ack:
			t.Fatalf("mutation acknowledged before deferred SQL barrier: %v", result.err)
		case <-ticker.C:
		case <-f.ctx.Done():
			t.Fatal("mutation did not reach deferred COMMIT barrier")
		}
	}
	select {
	case result := <-ack:
		t.Fatalf("mutation acknowledged while COMMIT was blocked: %v", result.err)
	default:
	}
	entity, _ := handle.entity.Value()
	var visible bool
	var revision int64
	if err := f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(f.ctx, `SELECT boolean_value,revision FROM public.helper_values WHERE entity_id=$1`, entity).Scan(&visible, &revision)
	}); err != nil || visible || revision != 1 {
		t.Fatalf("uncommitted value escaped to another transaction: value=%t rev=%d err=%v", visible, revision, err)
	}
	releaseBarrier()
	if err := <-holderDone; err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-ack:
		if result.err != nil {
			t.Fatal(result.err)
		}
		nha033Snapshot(t, result.snapshot, Boolean(true), 2)
	case <-f.ctx.Done():
		t.Fatal("commit was not acknowledged after barrier release")
	}
	// Nominal witness: another independently committed read sees exactly that
	// revision after the successful acknowledgement.
	restored, err := f.repo.Restore(f.ctx, handle, definition)
	if err != nil {
		t.Fatal(err)
	}
	nha033Snapshot(t, restored, Boolean(true), 2)
}

func TestNHA033RollbackAndIndeterminateCommitNeverAcknowledge(t *testing.T) {
	f := nha033Setup(t, false)
	definition := nha033Boolean(t, "false")
	handle := f.bind(t, definition)
	initial, err := f.repo.Restore(f.ctx, handle, definition)
	if err != nil {
		t.Fatal(err)
	}
	nha033SQL(t, f, `CREATE FUNCTION public.nha033_callback_failure() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'synthetic callback failure'; END $$;
		CREATE TRIGGER nha033_callback_failure BEFORE UPDATE ON public.helper_values
		FOR EACH ROW EXECUTE FUNCTION public.nha033_callback_failure()`)
	refused, err := f.repo.CompareAndSwap(f.ctx, handle, definition, initial.Revision(), Boolean(true))
	nha033Code(t, err, Unavailable)
	if refused.Valid() {
		t.Fatal("callback rollback exposed a successful snapshot")
	}
	rolledBack, err := f.repo.Restore(f.ctx, handle, definition)
	if err != nil {
		t.Fatal(err)
	}
	nha033Snapshot(t, rolledBack, Boolean(false), 1)
	nha033SQL(t, f, `DROP TRIGGER nha033_callback_failure ON public.helper_values;
		CREATE SEQUENCE public.nha033_commit_attempts;
		CREATE FUNCTION public.nha033_deferred_failure() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM nextval('public.nha033_commit_attempts'); RAISE EXCEPTION 'synthetic deferred failure'; END $$;
		CREATE CONSTRAINT TRIGGER nha033_deferred_failure AFTER UPDATE ON public.helper_values
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha033_deferred_failure()`)
	// This is an actual deferred SQL error during COMMIT, not a simulated lost
	// network response. Store conservatively classifies it as Indeterminate;
	// the repository preserves that result, and an exact primary read follows.
	refused, err = f.repo.CompareAndSwap(f.ctx, handle, definition, initial.Revision(), Boolean(true))
	nha033Code(t, err, CommitUnknown)
	if refused.Valid() {
		t.Fatal("indeterminate COMMIT exposed a successful snapshot")
	}
	primary, err := storage.Open(f.ctx, f.settings)
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	reopened, err := NewRepository(primary)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := reopened.Restore(f.ctx, handle, definition)
	if err != nil {
		t.Fatal(err)
	}
	nha033Snapshot(t, observed, Boolean(false), 1)
	var attempts int64
	if err := primary.InTransaction(f.ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(f.ctx, `SELECT last_value FROM public.nha033_commit_attempts`).Scan(&attempts)
	}); err != nil || attempts != 1 {
		t.Fatalf("repository retried an unknown COMMIT: attempts=%d err=%v", attempts, err)
	}
	nha033SQL(t, f, `DROP TRIGGER nha033_deferred_failure ON public.helper_values`)
	witness, err := reopened.CompareAndSwap(f.ctx, handle, definition, observed.Revision(), Boolean(true))
	if err != nil {
		t.Fatal(err)
	}
	nha033Snapshot(t, witness, Boolean(true), 2)
}

func TestNHA033StoppedDatabaseCannotClaimDurability(t *testing.T) {
	f := nha033Setup(t, false)
	definition := nha033Boolean(t, "false")
	handle := f.bind(t, definition)
	initial, err := f.repo.Restore(f.ctx, handle, definition)
	if err != nil {
		t.Fatal(err)
	}
	witness, err := f.repo.CompareAndSwap(f.ctx, handle, definition, initial.Revision(), Boolean(true))
	if err != nil {
		t.Fatal(err)
	}
	nha033Snapshot(t, witness, Boolean(true), 2)
	if err := f.cluster.Stop(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		refused, err := f.repo.CompareAndSwap(f.ctx, handle, definition, witness.Revision(), Boolean(false))
		nha033Code(t, err, Unavailable)
		if refused.Valid() {
			t.Fatal("stopped DB returned a durable success")
		}
	}
	refused, err := f.repo.Restore(f.ctx, handle, definition)
	nha033Code(t, err, Unavailable)
	if refused.Valid() {
		t.Fatal("stopped DB returned a fabricated restore")
	}
}

// Every method panics deliberately: rejecting the interface must precede even
// ctx.Err(), not merely protect a later transaction from a nil Done channel.
type nha033TypedNilContext struct{}

func (*nha033TypedNilContext) Deadline() (time.Time, bool) { panic("typed nil context invoked") }
func (*nha033TypedNilContext) Done() <-chan struct{}       { panic("typed nil context invoked") }
func (*nha033TypedNilContext) Err() error                  { panic("typed nil context invoked") }
func (*nha033TypedNilContext) Value(any) any               { panic("typed nil context invoked") }

func TestNHA033TypedNilContextFailsBeforeTransaction(t *testing.T) {
	f := nha033Setup(t, false)
	definition := nha033Boolean(t, "false")
	handle := f.bind(t, definition)
	entity, _ := handle.Entity()
	initial, err := f.repo.Restore(f.ctx, handle, definition)
	if err != nil {
		t.Fatal(err)
	}
	var typedNil context.Context = (*nha033TypedNilContext)(nil)
	for _, test := range []struct {
		name string
		call func(*testing.T) error
	}{
		{"Bind", func(t *testing.T) error {
			refused, err := f.repo.Bind(typedNil, entity, definition)
			if refused.valid() {
				t.Fatal("invalid context exposed an admitted handle")
			}
			return err
		}},
		{"Restore", func(t *testing.T) error {
			refused, err := f.repo.Restore(typedNil, handle, definition)
			if refused.Valid() {
				t.Fatal("invalid context exposed a snapshot")
			}
			return err
		}},
		{"CompareAndSwap", func(t *testing.T) error {
			refused, err := f.repo.CompareAndSwap(typedNil, handle, definition, initial.Revision(), Boolean(true))
			if refused.Valid() {
				t.Fatal("invalid context exposed a mutation snapshot")
			}
			return err
		}},
		{"Reset", func(t *testing.T) error {
			refused, err := f.repo.Reset(typedNil, handle, definition, initial.Revision())
			if refused.Valid() {
				t.Fatal("invalid context exposed a reset snapshot")
			}
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			nha033Code(t, test.call(t), InvalidInput)
			unchanged, err := f.repo.Restore(f.ctx, handle, definition)
			if err != nil {
				t.Fatal(err)
			}
			nha033Snapshot(t, unchanged, Boolean(false), 1)
		})
	}
}

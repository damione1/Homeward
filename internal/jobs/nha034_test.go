package jobs

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

func nha034Definition(t *testing.T, name string, now time.Time) Definition {
	t.Helper()
	id, _ := ParseJobID(name)
	version, _ := NewVersion(1)
	key, _ := ParseBackupKey("fake-backup")
	work, _ := NewSystemBackup(key)
	definition, err := AdmitDefinition(DefinitionInput{id, version, now, "America/Toronto", now.Add(time.Hour), RunBeforeExpiry(), work})
	if err != nil {
		t.Fatal(err)
	}
	return definition
}
func nha034IDs(name string) (OccurrenceID, IdempotencyKey) {
	occurrence, _ := ParseOccurrenceID(name + "-occurrence")
	key, _ := ParseIdempotencyKey(name + "-idempotency")
	return occurrence, key
}
func nha034Store(t *testing.T) (*storage.Store, *Repository, testpg.Cluster) {
	t.Helper()
	cluster := testpg.Start(t)
	settings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "NHA034_TEST_PASSWORD", SSLMode: "disable"}, func(name string) (string, bool) {
		return testpg.Password(), name == "NHA034_TEST_PASSWORD"
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatal(err)
	}
	return store, repository, cluster
}
func nha034Exec(t *testing.T, store *storage.Store, query string) {
	t.Helper()
	if err := store.InTransaction(context.Background(), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), query)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
func nha034Schedule(t *testing.T, repository *Repository, definition Definition, name string) OccurrenceID {
	t.Helper()
	occurrence, key := nha034IDs(name)
	result, outcome, err := repository.Schedule(context.Background(), definition, occurrence, key)
	if err != nil || outcome != storage.TransactionCommitted || result != Created {
		t.Fatalf("schedule: result=%d outcome=%d err=%v", result, outcome, err)
	}
	return occurrence
}
func nha034Get(t *testing.T, repository *Repository, occurrence OccurrenceID) Record {
	t.Helper()
	record, found, err := repository.Get(context.Background(), occurrence)
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	return record
}
func nha034Claim(t *testing.T, repository *Repository, now time.Time) Claim {
	t.Helper()
	claim, result, outcome, err := repository.Claim(context.Background(), now)
	if err != nil || outcome != storage.TransactionCommitted || result != ClaimAcquired {
		t.Fatalf("claim: result=%d outcome=%d err=%v", result, outcome, err)
	}
	return claim
}

func TestNHA034AdmittedDefinitionsZerosCopiesAndClosedSets(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	definition := nha034Definition(t, "admission", now)
	for name, mutate := range map[string]func(*DefinitionInput){
		"zero id":       func(i *DefinitionInput) { i.ID = JobID{} },
		"zero version":  func(i *DefinitionInput) { i.Version = Version{} },
		"zero deadline": func(i *DefinitionInput) { i.DeadlineUTC = time.Time{} },
		"deadline truncates to zero": func(i *DefinitionInput) {
			i.DeadlineUTC = time.Time{}.Add(time.Nanosecond)
		},
		"local instant":         func(i *DefinitionInput) { i.DeadlineUTC = now.In(time.FixedZone("offset", 3600)) },
		"absent timezone":       func(i *DefinitionInput) { i.Timezone = "" },
		"ambient timezone":      func(i *DefinitionInput) { i.Timezone = "Local" },
		"unknown timezone":      func(i *DefinitionInput) { i.Timezone = "Homeward/Unknown" },
		"zero expiration":       func(i *DefinitionInput) { i.ExpiresAtUTC = time.Time{} },
		"equal expiration":      func(i *DefinitionInput) { i.ExpiresAtUTC = now },
		"absent late policy":    func(i *DefinitionInput) { i.LatePolicy = LatePolicy{} },
		"unknown late policy":   func(i *DefinitionInput) { i.LatePolicy = LatePolicy{kind: 255} },
		"ambiguous late fields": func(i *DefinitionInput) { i.LatePolicy.tolerance = time.Second },
		"zero work":             func(i *DefinitionInput) { i.Work = Work{} },
		"unknown work":          func(i *DefinitionInput) { i.Work.kind = 255 },
	} {
		t.Run(name, func(t *testing.T) {
			input := definition.Input()
			mutate(&input)
			if _, err := AdmitDefinition(input); err == nil {
				t.Fatal("invalid definition admitted")
			}
		})
	}
	if _, err := NewVersion(math.MaxUint64); err == nil {
		t.Fatal("overflowing SQL version admitted")
	}
	for _, value := range []int16{0, -1, 9, 257} {
		if _, err := stateFromSQL(value); err == nil {
			t.Fatalf("unknown state %d admitted", value)
		}
	}
	for _, state := range []State{Pending, Claimed, Dispatching, Succeeded, Rejected, Expired, Skipped, Indeterminate} {
		if got, err := stateFromSQL(int16(state)); err != nil || got != state {
			t.Fatalf("state conversion %d: %d %v", state, got, err)
		}
	}
	owner, _ := domain.ParseActorID("real-account")
	session, _ := contracts.ParseSessionID("real-session")
	grant, _ := NewSessionGrant(owner, session)
	wire := &actions.CommandRequest{ConfigRevision: 7, Target: &registry.EntityRef{Reference: &registry.EntityRef_EntityId{EntityId: "entity-1"}}, CapabilitySchemaId: contracts.SwitchSchema, Endpoint: "relay", Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: false}}}}
	work, err := NewSessionCommand(grant, wire)
	if err != nil {
		t.Fatal(err)
	}
	wire.Arguments.GetSetSwitch().On = true
	_, first, err := work.SessionCommand()
	if err != nil || first.Arguments.GetSetSwitch().On {
		t.Fatal("caller mutation changed immutable command intent")
	}
	first.Arguments.GetSetSwitch().On = true
	_, second, _ := work.SessionCommand()
	if second.Arguments.GetSetSwitch().On {
		t.Fatal("getter leaked command storage")
	}
	second.Arguments.Arguments = (*actions.ActionArgs_SetSwitch)(nil)
	if _, err := NewSessionCommand(grant, second); err == nil {
		t.Fatal("typed nil command branch admitted")
	}
	if (Attempt{}).valid() || (Claim{}).valid() || (Definition{}).valid() {
		t.Fatal("zero capability admitted")
	}
	var clock *nha034Clock
	if _, err := NewExecutor(nil, &nha034Backup{}, clock); err == nil {
		t.Fatal("typed nil clock admitted")
	}
}

func TestNHA034DefinitionPersistsBeforeAckAndConflictsAreAtomic(t *testing.T) {
	store, repository, _ := nha034Store(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	definition := nha034Definition(t, "persisted", now)
	// A nonzero Go instant can normalize to the forbidden zero SQL value. Even
	// an internally fabricated definition must fail before either INSERT.
	invalid := nha034Definition(t, "zero-after-normalization", now)
	invalid.deadline = time.Time{}.Add(time.Nanosecond)
	invalidOccurrence, invalidKey := nha034IDs("zero-after-normalization")
	if result, outcome, err := repository.Schedule(context.Background(), invalid, invalidOccurrence, invalidKey); result != CreateUnspecified || outcome != storage.TransactionNotCommitted || !errors.Is(err, failure(Invalid)) {
		t.Fatalf("normalized zero reached storage: result=%d outcome=%d err=%v", result, outcome, err)
	}
	var definitions, occurrences int
	if err := store.InTransaction(context.Background(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM public.scheduled_job_definitions),count(*) FROM public.scheduled_jobs`).Scan(&definitions, &occurrences)
	}); err != nil || definitions != 0 || occurrences != 0 {
		t.Fatalf("invalid normalized instant mutated SQL: definitions=%d occurrences=%d err=%v", definitions, occurrences, err)
	}
	occurrence := nha034Schedule(t, repository, definition, "persisted")
	// A new repository reads every declared field after the successful ACK.
	reopened, _ := NewRepository(store)
	record := nha034Get(t, reopened, occurrence)
	if !record.definition.equal(definition) || record.state != Pending || record.token != 0 || record.until != nil {
		t.Fatal("acknowledged occurrence did not preserve exact definition")
	}
	_, key := nha034IDs("persisted")
	if result, outcome, err := reopened.Schedule(context.Background(), definition, occurrence, key); err != nil || result != AlreadyPresent || outcome != storage.TransactionCommitted {
		t.Fatalf("same identity replay: %d %d %v", result, outcome, err)
	}
	input := definition.Input()
	input.DeadlineUTC = input.DeadlineUTC.Add(time.Second)
	changed, _ := AdmitDefinition(input)
	if _, outcome, err := reopened.Schedule(context.Background(), changed, occurrence, key); !errors.Is(err, failure(Conflict)) || outcome != storage.TransactionNotCommitted {
		t.Fatalf("mutated version: %d %v", outcome, err)
	}
	other := nha034Definition(t, "conflicting-key", now)
	otherOccurrence, _ := nha034IDs("conflicting-key")
	if _, outcome, err := reopened.Schedule(context.Background(), other, otherOccurrence, key); !errors.Is(err, failure(Conflict)) || outcome != storage.TransactionNotCommitted {
		t.Fatalf("shared idempotency: %d %v", outcome, err)
	}
	var orphanDefinitions int
	if err := store.InTransaction(context.Background(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM public.scheduled_job_definitions WHERE job_id='conflicting-key'`).Scan(&orphanDefinitions)
	}); err != nil || orphanDefinitions != 0 {
		t.Fatalf("conflict was not atomic: count=%d err=%v", orphanDefinitions, err)
	}
	// A later immutable version is distinct, while the first version and its
	// explicitly scheduled occurrence remain unchanged. No active-version or
	// recurrence semantics are inferred by this one-shot repository.
	nextInput := definition.Input()
	nextInput.Version, _ = NewVersion(2)
	nextInput.DeadlineUTC = nextInput.DeadlineUTC.Add(time.Minute)
	next, err := AdmitDefinition(nextInput)
	if err != nil {
		t.Fatal(err)
	}
	nextOccurrence := nha034Schedule(t, reopened, next, "persisted-version-two")
	if got := nha034Get(t, reopened, nextOccurrence); !got.definition.equal(next) {
		t.Fatal("new immutable version did not round-trip")
	}
	if got := nha034Get(t, reopened, occurrence); !got.definition.equal(definition) {
		t.Fatal("new version rewrote original occurrence")
	}
	extraOccurrence, extraKey := nha034IDs("extra-same-version")
	if _, outcome, err := reopened.Schedule(context.Background(), definition, extraOccurrence, extraKey); outcome != storage.TransactionNotCommitted || !errors.Is(err, failure(Conflict)) {
		t.Fatalf("one-shot version admitted second occurrence: %d %v", outcome, err)
	}
}

func TestNHA034TwoWorkersClaimOnceAndOldTokensAreFenced(t *testing.T) {
	store, repository, _ := nha034Store(t)
	other, _ := NewRepository(store)
	now := time.Now().UTC().Truncate(time.Microsecond)
	occurrence := nha034Schedule(t, repository, nha034Definition(t, "claims", now), "claims")
	barrier := make(chan struct{})
	type answer struct {
		claim   Claim
		result  ClaimResult
		outcome storage.TransactionOutcome
		err     error
	}
	answers := make(chan answer, Workers)
	for _, worker := range []*Repository{repository, other} {
		go func(repository *Repository) {
			<-barrier
			claim, result, outcome, err := repository.Claim(context.Background(), now)
			answers <- answer{claim, result, outcome, err}
		}(worker)
	}
	close(barrier)
	var old Claim
	winners := 0
	for range Workers {
		answer := <-answers
		if answer.err != nil || answer.outcome != storage.TransactionCommitted {
			t.Fatalf("concurrent claim: %+v", answer)
		}
		switch answer.result {
		case ClaimAcquired:
			winners++
			old = answer.claim
		case NoWork:
		default:
			t.Fatalf("unexpected claim result %d", answer.result)
		}
	}
	if winners != 1 {
		t.Fatalf("valid lease owners=%d", winners)
	}
	later := now.Add(LeaseDuration + time.Microsecond)
	current := nha034Claim(t, other, later)
	if current.record.token != old.record.token+1 {
		t.Fatal("claim generation did not advance")
	}
	if _, _, err := repository.Renew(context.Background(), old, later); !errors.Is(err, failure(LeaseLost)) {
		t.Fatalf("stale renewal: %v", err)
	}
	if _, _, err := repository.BeginDispatch(context.Background(), old, later); !errors.Is(err, failure(LeaseLost)) {
		t.Fatalf("stale dispatch: %v", err)
	}
	if state, _, err := other.BeginDispatch(context.Background(), current, later); err != nil || state != Dispatching {
		t.Fatalf("current dispatch: %d %v", state, err)
	}
	if _, err := repository.Finish(context.Background(), old, Succeeded, later); !errors.Is(err, failure(LeaseLost)) {
		t.Fatalf("stale finalization: %v", err)
	}
	if got := nha034Get(t, repository, occurrence); got.token != current.record.token || got.state != Dispatching {
		t.Fatal("stale worker changed current owner")
	}
	if _, err := other.Finish(context.Background(), current, Succeeded, later); err != nil {
		t.Fatal(err)
	}
}

func TestNHA034ReopenCrashBoundariesAndLatePolicies(t *testing.T) {
	store, repository, _ := nha034Store(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	// Before claim: immutable occurrence is available to the next repository.
	occurrence := nha034Schedule(t, repository, nha034Definition(t, "crash", now), "crash")
	reopened, _ := NewRepository(store)
	first := nha034Claim(t, reopened, now)
	// After claim, before dispatch: a successor takes a new token after expiry.
	later := now.Add(LeaseDuration + time.Microsecond)
	second := nha034Claim(t, reopened, later)
	if first.record.occurrence != second.record.occurrence || first.record.key != second.record.key {
		t.Fatal("pre-dispatch recovery changed occurrence identity")
	}
	if state, _, err := reopened.BeginDispatch(context.Background(), second, later); err != nil || state != Dispatching {
		t.Fatalf("dispatch boundary: %d %v", state, err)
	}
	// Simulated process loss: abandon this committed dispatch without Finish.
	restarted, _ := NewRepository(store)
	if _, result, _, err := restarted.Claim(context.Background(), later.Add(LeaseDuration)); err != nil || result != NoWork {
		t.Fatalf("uncertain effect became claimable: %d %v", result, err)
	}
	if got := nha034Get(t, restarted, occurrence); got.state != Indeterminate || got.token != second.record.token {
		t.Fatal("dispatching recovery was not indeterminate")
	}
	if _, err := restarted.Finish(context.Background(), second, Succeeded, later.Add(LeaseDuration)); !errors.Is(err, failure(LeaseLost)) {
		t.Fatalf("expired dispatcher finalized: %v", err)
	}
	for _, test := range []struct {
		name    string
		late    LatePolicy
		expires time.Duration
		at      time.Duration
		want    State
	}{
		{"expired", RunBeforeExpiry(), time.Second, time.Second, Expired},
		{"skipped", func() LatePolicy { p, _ := SkipLate(time.Second); return p }(), time.Hour, 2 * time.Second, Skipped},
		{"poll-grace", func() LatePolicy { p, _ := SkipLate(time.Second); return p }(), time.Hour, PollInterval, Dispatching},
		{"catch-up-explicit", RunBeforeExpiry(), time.Hour, 5 * time.Minute, Dispatching},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := nha034Definition(t, test.name, now).Input()
			input.LatePolicy, input.ExpiresAtUTC = test.late, now.Add(test.expires)
			definition, err := AdmitDefinition(input)
			if err != nil {
				t.Fatal(err)
			}
			occurrence := nha034Schedule(t, restarted, definition, test.name)
			claim, result, _, err := restarted.Claim(context.Background(), now.Add(test.at))
			if err != nil {
				t.Fatal(err)
			}
			if test.want == Dispatching {
				if result != ClaimAcquired {
					t.Fatalf("claim=%d", result)
				}
				if _, _, err := restarted.BeginDispatch(context.Background(), claim, now.Add(test.at)); err != nil {
					t.Fatal(err)
				}
				if _, err := restarted.Finish(context.Background(), claim, Succeeded, now.Add(test.at)); err != nil {
					t.Fatal(err)
				}
			} else if result != OccurrenceSettled {
				t.Fatalf("late policy did not settle: %d", result)
			}
			want := test.want
			if want == Dispatching {
				want = Succeeded
			}
			if got := nha034Get(t, restarted, occurrence); got.state != want {
				t.Fatalf("state=%d want=%d", got.state, want)
			}
		})
	}
}

func TestNHA034CommitFailureAndDatabaseLossNeverAcknowledge(t *testing.T) {
	store, repository, cluster := nha034Store(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	nha034Exec(t, store, `CREATE FUNCTION public.nha034_fail_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'deferred test failure'; END $$;
        CREATE CONSTRAINT TRIGGER nha034_fail_commit AFTER INSERT ON public.scheduled_jobs DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha034_fail_commit()`)
	occurrence, key := nha034IDs("failed-commit")
	if result, outcome, err := repository.Schedule(context.Background(), nha034Definition(t, "failed-commit", now), occurrence, key); result != CreateUnspecified || outcome != storage.TransactionIndeterminate || !errors.Is(err, failure(CommitUnknown)) {
		t.Fatalf("commit failure acknowledged: %d %d %v", result, outcome, err)
	}
	if _, found, err := repository.Get(context.Background(), occurrence); err != nil || found {
		t.Fatalf("rolled-back occurrence exists: %v %v", found, err)
	}
	nha034Exec(t, store, `DROP TRIGGER nha034_fail_commit ON public.scheduled_jobs; DROP FUNCTION public.nha034_fail_commit()`)
	// A nominal witness first establishes that this repository can commit.
	nha034Schedule(t, repository, nha034Definition(t, "nominal", now), "nominal")
	if err := cluster.Stop(); err != nil {
		t.Fatal(err)
	}
	occurrence, key = nha034IDs("db-down")
	started := time.Now()
	if result, outcome, err := repository.Schedule(context.Background(), nha034Definition(t, "db-down", now), occurrence, key); result != CreateUnspecified || outcome == storage.TransactionCommitted || err == nil {
		t.Fatalf("DB loss acknowledged: %d %d %v", result, outcome, err)
	}
	if time.Since(started) > SQLTimeout+time.Second {
		t.Fatal("DB operation exceeded SQL deadline")
	}
	// The failed call has no timer/queue ownership; only SQL can create work.
	if _, err := NewRepository(nil); err == nil {
		t.Fatal("missing DB admitted")
	}
}

type nha034Clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *nha034Clock) Now() time.Time    { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *nha034Clock) set(now time.Time) { c.mu.Lock(); c.now = now; c.mu.Unlock() }

type nha034Timer struct {
	entered chan time.Duration
	tick    chan struct{}
}

func (t *nha034Timer) Wait(ctx context.Context, duration time.Duration) error {
	select {
	case t.entered <- duration:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-t.tick:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type nha034Backup struct {
	mu      sync.Mutex
	keys    map[IdempotencyKey]BackupKey
	effects atomic.Int32
	entered chan struct{}
	release <-chan struct{}
}

func (b *nha034Backup) ExecuteBackup(ctx context.Context, backup BackupKey, key IdempotencyKey) (Outcome, error) {
	if b.entered != nil {
		select {
		case b.entered <- struct{}{}:
		case <-ctx.Done():
			return Uncertain, ctx.Err()
		}
	}
	if b.release != nil {
		select {
		case <-b.release:
		case <-ctx.Done():
			return Uncertain, ctx.Err()
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.keys == nil {
		b.keys = make(map[IdempotencyKey]BackupKey)
	}
	if existing, present := b.keys[key]; present {
		if existing != backup {
			return Refused, failure(Conflict)
		}
		return Completed, nil
	}
	b.keys[key] = backup
	b.effects.Add(1)
	return Completed, nil
}

func TestNHA034TwoWorkersBoundedPollingAndFakeIdempotentBackup(t *testing.T) {
	_, repository, _ := nha034Store(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	clock := &nha034Clock{now: now}
	timer := &nha034Timer{entered: make(chan time.Duration, 8), tick: make(chan struct{})}
	backup := &nha034Backup{}
	executor, _ := NewExecutor(nil, backup, clock)
	scheduler, err := NewScheduler(repository, executor, clock, timer)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := scheduler.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := scheduler.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	for range Workers {
		select {
		case duration := <-timer.entered:
			if duration != PollInterval {
				t.Fatalf("poll=%s", duration)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	// Both workers are now parked. A due job cannot be consumed until a tick.
	occurrence := nha034Schedule(t, repository, nha034Definition(t, "bounded", now), "bounded")
	if got := nha034Get(t, repository, occurrence); got.state != Pending || backup.effects.Load() != 0 {
		t.Fatal("worker bypassed polling timer")
	}
	timer.tick <- struct{}{}
	select {
	case <-timer.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if got := nha034Get(t, repository, occurrence); got.state != Succeeded || backup.effects.Load() != 1 {
		t.Fatalf("backup result=%d effects=%d", got.state, backup.effects.Load())
	}
	// Same execution port, same occurrence/key: the fake's declared idempotent
	// effect stays unique even if its caller asks to consult it twice.
	claimRecord := nha034Get(t, repository, occurrence)
	until := now.Add(LeaseDuration)
	claimRecord.state, claimRecord.token, claimRecord.until = Claimed, 1, &until
	for range 2 {
		if outcome, err := executor.Execute(ctx, Attempt{claimRecord}); outcome != Completed || err != nil {
			t.Fatalf("idempotent backup: %d %v", outcome, err)
		}
	}
	if backup.effects.Load() != 1 {
		t.Fatal("fake repeated a deduplicated effect")
	}
	if err := scheduler.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-scheduler.Done():
	default:
		t.Fatal("workers did not drain")
	}
}

func TestNHA034SchemaClosedVariantsAndImmutableDefinitions(t *testing.T) {
	store, repository, _ := nha034Store(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	occurrence := nha034Schedule(t, repository, nha034Definition(t, "schema", now), "schema")
	for _, query := range []string{
		`UPDATE public.scheduled_job_definitions SET timezone='UTC' WHERE job_id='schema'`,
		`UPDATE public.scheduled_jobs SET idempotency_key='other' WHERE occurrence_id='schema-occurrence'`,
		`UPDATE public.scheduled_jobs SET state=0 WHERE occurrence_id='schema-occurrence'`,
		`UPDATE public.scheduled_jobs SET state=9 WHERE occurrence_id='schema-occurrence'`,
		`UPDATE public.scheduled_jobs SET state=2 WHERE occurrence_id='schema-occurrence'`,
	} {
		if err := store.InTransaction(context.Background(), func(tx pgx.Tx) error { _, err := tx.Exec(context.Background(), query); return err }); err == nil {
			t.Fatalf("schema admitted invalid mutation: %s", query)
		}
	}
	if got := nha034Get(t, repository, occurrence); got.state != Pending {
		t.Fatal("schema failure changed committed occurrence")
	}
}

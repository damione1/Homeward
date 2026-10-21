package server

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
)

// Each subselect shares the independent observer's statement snapshot. Hash
// only configuration authority/membership, technical bindings and activation
// audits; autonomous history emissions are deliberately not configuration.
const nha029C5PrecommitDigestSQL = `SELECT sha256(convert_to(jsonb_build_array(
 (SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb) FROM public.config_documents x),
 (SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb) FROM public.config_versions x),
 (SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb) FROM public.config_generations x),
 (SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb) FROM public.config_generation_entries x),
 (SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb) FROM public.config_active x),
 (SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb) FROM public.config_activation_attempts x),
 (SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb) FROM public.config_activation_policy_changes x),
 (SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb) FROM public.registry_source_bindings x),
 (SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb) FROM public.registry_aliases x)
)::text,'UTF8'))`

func nha029C5PrecommitWaitingNodes(t *testing.T, interpreter *automationruns.Interpreter, ids [2]automationruns.RunID) [2]automationruns.NodeTrace {
	t.Helper()
	traces, evicted := interpreter.NodeTraces()
	if evicted != 0 {
		t.Fatal("precommit refusal evicted its real wait observations")
	}
	var result [2]automationruns.NodeTrace
	var waiting [2]int
	for _, trace := range traces {
		for index, id := range ids {
			if trace.RunID() != id {
				continue
			}
			if trace.Stage() == automationruns.NodeCancelled || trace.Stage() == automationruns.NodeFailed {
				t.Fatal("precommit refusal canceled or failed an original run")
			}
			if trace.Kind() != ir.ActionWait && trace.Kind() != ir.ActionWaitTemplate {
				continue
			}
			if trace.Stage() == automationruns.NodeWaiting {
				result[index] = trace
				waiting[index]++
			} else if trace.Stage() != automationruns.NodeEntered {
				t.Fatal("precommit refusal finished an original wait")
			}
		}
	}
	if waiting != [2]int{1, 1} {
		t.Fatalf("precommit refusal lost or re-registered original waits: %v", waiting)
	}
	return result
}

func nha029C5PrecommitNoSQLWriter(t *testing.T, ctx context.Context, r *nha029OwnerRig) {
	t.Helper()
	var writers int
	if err := r.environment.observer.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND classid=481316 AND objid=151 AND objsubid=2`).Scan(&writers); err != nil || writers != 0 {
		t.Fatalf("precommit pause entered the actual configuration transaction lock: writers=%d err=%v", writers, err)
	}
}

// An admitted occurrence holds the real Run-owned024 drain before SQL. A new
// child invocation returning DeadlineExceeded with no handle proves that the
// actual Controller admission pause has been reached. If a probe wins before
// the pause, its separate wait-only run is explicitly canceled and joined;
// it cannot dispatch, and its ID is never one of the original two waits.
func TestNHA029C5OwnerCallerCancellationDuringPauseDoesNotBeginTransaction(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name := "cancel"
		if deadline {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			environment := nha042ServerEnvironmentFor(t)
			r := nha029StartOwnerEnvironment(t, environment, nha029OwnerParent, nha029OwnerChild, nil, true)
			r.waitBoth(t)
			original := r.composition.scripts.interpreter
			ids := nha029C5HistoryWaits(t, original)
			waits := nha029C5PrecommitWaitingNodes(t, original, ids)
			controller := r.controller()
			active := nha029C5HistoryActive(t, ctx, r)
			stamp, err := r.owner.CurrentConfig(ctx)
			if err != nil {
				t.Fatal(err)
			}
			counts := nha029C5HistoryCounts(t, ctx, r)
			digest := nha029C5RestoreDigest(t, ctx, environment.observer, nha029C5PrecommitDigestSQL)
			commands, present := r.jobServices.Commands()
			if !present || commands == nil || r.commands != r.jobComposition.CommandComposition() {
				t.Fatal("precommit refusal did not borrow Run's unique script/job024")
			}
			internal, err := commands.InternalAutomationPort()
			if err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			occurrenceResult, occurrenceDone := make(chan error, 1), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			go func() {
				defer close(occurrenceDone)
				occurrenceResult <- internal.WithOccurrence(ctx, "nha029-c5-real-precommit-drain", func(context.Context) error {
					close(entered)
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			}()
			t.Cleanup(func() { unblock(); <-occurrenceDone })
			nha034WaitSignal(t, entered, "actual024 occurrence before Owner pause")
			r.edit(t, "# candidate B must never enter transaction\n")
			var callback atomic.Bool
			request := r.request(t, nil, nil, func(*registry.ConfigWriter) error {
				callback.Store(true)
				return nil
			})
			caller, stopCaller := context.WithTimeout(ctx, 5*time.Second)
			defer stopCaller()
			result := make(chan struct {
				result configactivation.Result
				err    error
			}, 1)
			activationDone := make(chan struct{})
			go func() {
				defer close(activationDone)
				value, err := r.owner.Activate(caller, request)
				result <- struct {
					result configactivation.Result
					err    error
				}{value, err}
			}()
			t.Cleanup(func() { stopCaller(); <-activationDone })
			child, err := ir.ParseScriptID("script.nha042_child")
			if err != nil {
				t.Fatal(err)
			}
			paused := false
			for !paused {
				probe, stopProbe := context.WithTimeout(ctx, 100*time.Millisecond)
				handle, err := controller.InvokeScript(probe, child, nil, automationruns.Run{}, ir.ScriptCallDirect)
				stopProbe()
				if handle != nil {
					if handle.ID() == ids[0] || handle.ID() == ids[1] {
						t.Fatal("pause probe reused an original RunID")
					}
					handle.Cancel()
					select {
					case <-handle.Done():
					case <-ctx.Done():
						t.Fatal("pre-pause probe did not join")
					}
					continue
				}
				if !errors.Is(err, context.DeadlineExceeded) || caller.Err() != nil {
					t.Fatalf("did not positively observe Owner's pretransaction admission pause: %v", err)
				}
				paused = true
			}
			if callback.Load() || nha029C5HistoryCounts(t, ctx, r) != counts || nha029C5RestoreDigest(t, ctx, environment.observer, nha029C5PrecommitDigestSQL) != digest {
				t.Fatal("Owner began SQL/audit before the actual occurrence drain completed")
			}
			nha029C5PrecommitNoSQLWriter(t, ctx, r)
			pausedStamp, err := r.owner.CurrentConfig(ctx)
			if err != nil || pausedStamp != stamp || r.controller() != controller || nha029C5HistoryActive(t, ctx, r) != active {
				t.Fatalf("pretransaction pause changed SQL/stamp/Controller: %v", err)
			}
			if !reflect.DeepEqual(nha029C5PrecommitWaitingNodes(t, original, ids), waits) {
				t.Fatal("precommit pause replaced an original040/044 wait")
			}
			if !deadline {
				stopCaller()
			}
			outcome, refusal := nha029OwnerAwait(t, result)
			want := context.Canceled
			if deadline {
				want = context.DeadlineExceeded
			}
			if !errors.Is(refusal, want) || outcome.Outcome() != configactivation.RejectedBeforeCommit || outcome.AttemptOutcome() != storage.TransactionOutcomeUnspecified {
				t.Fatalf("caller refusal fabricated a transaction/success: outcome=%v transaction=%v err=%v", outcome.Outcome(), outcome.AttemptOutcome(), refusal)
			}
			if !outcome.Attempt().Valid() {
				t.Fatal("refusal preceded prepared candidate and actual pause")
			}
			if _, present, err := r.store.ReadActivationAttempt(ctx, outcome.Attempt()); err != nil || present {
				t.Fatalf("precommit refusal persisted a success attempt: present=%t err=%v", present, err)
			}
			currentStamp, err := r.owner.CurrentConfig(ctx)
			if err != nil || currentStamp != stamp || r.controller() != controller || nha029C5HistoryActive(t, ctx, r) != active {
				t.Fatalf("precommit refusal changed SQL/stamp/Controller: %v", err)
			}
			if callback.Load() || nha029C5HistoryCounts(t, ctx, r) != counts || nha029C5RestoreDigest(t, ctx, environment.observer, nha029C5PrecommitDigestSQL) != digest || len(r.driver.snapshot()) != 0 {
				t.Fatal("precommit refusal wrote configuration/audit or emitted an action")
			}
			nha029C5PrecommitNoSQLWriter(t, ctx, r)
			if !reflect.DeepEqual(nha029C5PrecommitWaitingNodes(t, original, ids), waits) {
				t.Fatal("precommit refusal replaced or canceled an original wait")
			}
			if _, err := r.identity.Sequence().Capture(); err != nil {
				t.Fatalf("precommit refusal did not restore capture: %v", err)
			}
			if err := internal.WithOccurrence(ctx, "nha029-c5-admission-after-refusal", func(context.Context) error { return nil }); err != nil {
				t.Fatalf("precommit refusal did not reopen the same024 while original lease remained live: %v", err)
			}
			unblock()
			if err := <-occurrenceResult; err != nil {
				t.Fatal(err)
			}
			r.close(t)
			if traces, _ := original.NodeTraces(); len(traces) != 0 {
				t.Fatal("closed precommit refusal retained its native image")
			}
		})
	}
}

package configdocuments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
)

func nha055GuardedFileUnchanged(t *testing.T, path string, raw []byte, info os.FileInfo) {
	t.Helper()
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, raw) {
		t.Fatal("pre-write refusal changed exact editable bytes", err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(info, after) || !info.ModTime().Equal(after.ModTime()) || info.Mode() != after.Mode() {
		t.Fatal("pre-write refusal replaced or touched the editable inode", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".homeward-config-") {
			t.Fatal("pre-write refusal left a stage, displaced source or pending intent")
		}
	}
}

func TestNHA055WorkspaceBeforeWriteGuardRunsUnderTheExistingMutex(t *testing.T) {
	w, path, raw := nha149Workspace(t)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	refused := errors.New("NHA055 late trusted guard refusal")
	called, locked := false, false
	edit, err := w.beginEdit(sha256.Sum256(raw), sha256.Sum256(raw), []byte("# refused candidate\nexternal_workers: []\n"), func() error {
		called = true
		locked = !w.mu.TryLock()
		if !locked {
			w.mu.Unlock()
		}
		nha055GuardedFileUnchanged(t, path, raw, info)
		return refused
	})
	if edit != nil || !errors.Is(err, refused) || !called || !locked {
		t.Fatal("workspace did not propagate the trusted guard under its writer mutex", err)
	}
	nha055GuardedFileUnchanged(t, path, raw, info)
}

// This context belongs only to the test of the service's actual staging helper.
// Its second Err call is the new guard under w.mu, AFTER both optimistic checks
// have succeeded. Old staging without the guard completes instead of reaching
// this channel and must fail the oracle. No global hook or clock is replaced.
type nha055PrewriteBarrierContext struct {
	context.Context
	calls   int
	reached chan struct{}
	resume  chan struct{}
}

func (c *nha055PrewriteBarrierContext) Err() error {
	c.calls++
	if c.calls == 2 {
		close(c.reached)
		<-c.resume
	}
	return c.Context.Err()
}

func nha055GuardedSQLCounts(t *testing.T, service *Service, ctx context.Context) [4]int64 {
	t.Helper()
	var counts [4]int64
	outcome, err := service.store.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM public.config_versions),(SELECT count(*) FROM public.config_generations),(SELECT count(*) FROM public.config_activation_attempts),(SELECT count(*) FROM public.command_intents)`).Scan(&counts[0], &counts[1], &counts[2], &counts[3])
	})
	if err != nil || outcome != storage.TransactionCommitted {
		t.Fatal("real SQL26 guard observation failed", err)
	}
	return counts
}

// The existing WatcherClock port drives complete real watcher iterations. The
// waiter signals only after observation/SQL/activation decisions have finished.
type nha055GuardedWatcherClock struct {
	now    time.Time
	paused chan struct{}
	steps  chan time.Time
}

func (c *nha055GuardedWatcherClock) Now() time.Time { return c.now }
func (c *nha055GuardedWatcherClock) Wait(ctx context.Context, _ time.Duration) error {
	select {
	case c.paused <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case c.now = <-c.steps:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func nha055GuardedWatcherSeesOnlyCleanBase(t *testing.T, service *Service, ctx context.Context) {
	t.Helper()
	start := time.Date(2026, 10, 22, 13, 0, 0, 0, time.UTC)
	clock := &nha055GuardedWatcherClock{now: start, paused: make(chan struct{}), steps: make(chan time.Time)}
	watcher, err := NewWatcherWithClock(service, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := watcher.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := watcher.Close(closeCtx); err != nil {
			t.Error(err)
		}
	}()
	for step := 0; step < 3; step++ {
		if step != 0 {
			select {
			case clock.steps <- start.Add(time.Duration(step) * (watcherStability + watcherRetry)):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		select {
		case <-clock.paused:
		case <-ctx.Done():
			t.Fatal("real watcher did not complete its guarded-source observation", ctx.Err())
		}
		current, err := service.Read(ctx, operationalID())
		if err != nil || current.Dirty() != Clean || current.Workspace().ReconciliationPending() {
			t.Fatal("watcher observed a refused candidate rather than the untouched admitted source", err)
		}
	}
}

// Composed service-staging helper -> real workspace/SQL26 -> real watcher. This
// is deliberately not a full Run/TLS claim: Owner wraps request contexts before
// the stage callback, so the precise Err barrier belongs at this private seam.
func TestNHA055ServiceWorkspaceGuardRejectsExpiryAndCancellationAfterOptimisticChecks(t *testing.T) {
	for _, mode := range []string{"expiry", "cancellation"} {
		t.Run(mode, func(t *testing.T) {
			service, ctx, path, raw := nha055Documents(t)
			before, expected, basis := nha055DocumentBasis(t, service, ctx, time.Now().Add(time.Minute))
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			counts := nha055GuardedSQLCounts(t, service, ctx)
			// Only the expiry oracle needs wall time. The barrier proves its
			// initial deadline check passed, even if execution is suspended later.
			if mode == "expiry" {
				basis, err = NewWriteBasis(before.Generation(), before.ManifestSHA256(), before.CatalogCut().SHA256(), time.Now().Add(2*time.Second))
				if err != nil {
					t.Fatal(err)
				}
			}
			requestCtx, cancelRequest := context.WithCancel(ctx)
			defer cancelRequest()
			barrier := &nha055PrewriteBarrierContext{Context: requestCtx, reached: make(chan struct{}), resume: make(chan struct{})}
			var once sync.Once
			resume := func() { once.Do(func() { close(barrier.resume) }) }
			defer resume()
			type attempt struct {
				staged bool
				err    error
			}
			done := make(chan attempt, 1)
			go func() {
				edit, err := service.beginGuardedWorkspaceEdit(barrier, expected.workspace, expected.source, append(bytes.Clone(raw), []byte("# late refusal must never become watcher input\n")...), basis)
				staged := edit != nil
				if edit != nil {
					// An old buggy stage must not keep the mutex held when the
					// oracle fails. Its candidate would still remain dirty.
					err = errors.Join(err, edit.Finish(expected.source, true))
				}
				done <- attempt{staged: staged, err: err}
			}()
			joined := false
			defer func() {
				resume()
				if !joined {
					select {
					case <-done:
					case <-ctx.Done():
						t.Error("guarded staging goroutine did not join")
					}
				}
			}()
			select {
			case <-barrier.reached:
			case early := <-done:
				joined = true
				t.Fatalf("stage completed without reaching the final guard after both optimistic checks: staged=%t err=%v", early.staged, early.err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if service.workspace.mu.TryLock() {
				service.workspace.mu.Unlock()
				t.Fatal("pre-write authority check ran outside the existing workspace writer mutex")
			}
			nha055GuardedFileUnchanged(t, path, raw, info)
			if mode == "expiry" {
				timer := time.NewTimer(time.Until(basis.expires))
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			} else {
				cancelRequest()
			}
			resume()
			select {
			case result := <-done:
				joined = true
				if result.staged {
					t.Fatal("late expiry/cancellation staged a candidate")
				}
				if mode == "expiry" {
					nha149Failure(t, result.err, Conflict)
				} else if !errors.Is(result.err, context.Canceled) {
					t.Fatal("late cancellation did not preserve its context error", result.err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			nha055DocumentsUnchanged(t, service, ctx, before, path, raw, info)
			if nha055GuardedSQLCounts(t, service, ctx) != counts {
				t.Fatal("refused stage changed versions, generations, activation attempts or command intents")
			}
			nha055GuardedWatcherSeesOnlyCleanBase(t, service, ctx)
			nha055DocumentsUnchanged(t, service, ctx, before, path, raw, info)
			if nha055GuardedSQLCounts(t, service, ctx) != counts {
				t.Fatal("a later watcher iteration admitted the refused candidate")
			}
		})
	}
}

package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func nha029BootstrapSource(t *testing.T, store *Store, ctx context.Context, binding ConfigDocumentBinding, raw []byte,
	expected ExpectedBootstrapActive) ConfigVersionRef {
	t.Helper()
	var ref ConfigVersionRef
	outcome, err := store.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
		var saveErr error
		ref, saveErr = tx.SaveBootstrapLaunchSource(binding, raw, sha256.Sum256(raw), nha151Metadata(t))
		if saveErr != nil {
			return saveErr
		}
		return tx.ActivateBootstrapSource(expected, ref)
	})
	if outcome != TransactionCommitted || err != nil {
		t.Fatalf("bootstrap source activation: outcome=%v err=%v", outcome, err)
	}
	return ref
}

type nha029BootstrapRead struct {
	snapshot ConfigBootstrapSnapshot
	found    bool
	err      error
}

func nha029WaitForBootstrapReaderLock(t *testing.T, store *Store, ctx context.Context, reader <-chan nha029BootstrapRead) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case early := <-reader:
			t.Fatalf("bootstrap reconciliation returned before writer finished: found=%t err=%v", early.found, early.err)
		default:
		}
		var blocked int
		err := store.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted`).Scan(&blocked)
		if err != nil {
			t.Fatal(err)
		}
		if blocked > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("fresh bootstrap reader never waited on the writer's advisory lock")
}

func TestNHA029ReadActiveBootstrapAfterWriterWaitsForCommitAndRollback(t *testing.T) {
	for _, commit := range []bool{true, false} {
		name := "rollback"
		if commit {
			name = "commit"
		}
		t.Run(name, func(t *testing.T) {
			store, ctx := nha151Store(t)
			binding := nha151Binding(t, "bootstrap.launch", "bootstrap.launch.yaml")
			firstRaw := []byte("listen: '127.0.0.1:1001'\n")
			firstRef := nha029BootstrapSource(t, store, ctx, binding, firstRaw, ExpectNoBootstrapActive())
			first, found, err := store.ReadActiveBootstrap(ctx)
			if err != nil || !found || first.Ref() != firstRef {
				t.Fatalf("first bootstrap source: found=%t err=%v", found, err)
			}
			expected, err := ExpectBootstrapActive(first.Ref(), first.SHA256())
			if err != nil {
				t.Fatal(err)
			}
			secondRaw := []byte("listen: '127.0.0.1:1002'\n")
			writerReady := make(chan ConfigVersionRef, 1)
			releaseWriter := make(chan bool, 1)
			writerDone := make(chan struct {
				outcome TransactionOutcome
				err     error
			}, 1)
			rollback := errors.New("deliberate NHA-029 bootstrap rollback")
			metadata := nha151Metadata(t)
			go func() {
				outcome, writeErr := store.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
					ref, err := tx.SaveBootstrapLaunchSource(binding, secondRaw, sha256.Sum256(secondRaw), metadata)
					if err != nil {
						return err
					}
					if err := tx.ActivateBootstrapSource(expected, ref); err != nil {
						return err
					}
					writerReady <- ref
					if !<-releaseWriter {
						return rollback
					}
					return nil
				})
				writerDone <- struct {
					outcome TransactionOutcome
					err     error
				}{outcome, writeErr}
			}()
			writerReleased, writerJoined := false, false
			t.Cleanup(func() {
				if !writerReleased {
					releaseWriter <- false
				}
				if !writerJoined {
					select {
					case <-writerDone:
					case <-time.After(5 * time.Second):
						t.Error("bootstrap writer did not join")
					}
				}
			})
			var secondRef ConfigVersionRef
			select {
			case secondRef = <-writerReady:
			case result := <-writerDone:
				writerJoined = true
				t.Fatalf("writer failed before holding barrier: %v", result.err)
			case <-time.After(5 * time.Second):
				t.Fatal("writer did not hold configuration barrier")
			}
			readerStarted := make(chan struct{})
			readerDone := make(chan nha029BootstrapRead, 1)
			go func() {
				close(readerStarted)
				snapshot, found, err := store.ReadActiveBootstrapAfterWriter(ctx)
				readerDone <- nha029BootstrapRead{snapshot, found, err}
			}()
			<-readerStarted
			nha029WaitForBootstrapReaderLock(t, store, ctx, readerDone)
			select {
			case early := <-readerDone:
				t.Fatalf("bootstrap reader returned while its advisory lock was ungranted: %v", early.err)
			default:
			}
			releaseWriter <- commit
			writerReleased = true
			written := <-writerDone
			writerJoined = true
			if commit && (written.outcome != TransactionCommitted || written.err != nil) {
				t.Fatalf("writer commit: outcome=%v err=%v", written.outcome, written.err)
			}
			if !commit && (written.outcome != TransactionNotCommitted || !errors.Is(written.err, rollback)) {
				t.Fatalf("writer rollback: outcome=%v err=%v", written.outcome, written.err)
			}
			var read nha029BootstrapRead
			select {
			case read = <-readerDone:
			case <-time.After(5 * time.Second):
				t.Fatal("reader did not resume after writer finished")
			}
			wantRef, wantHash := firstRef, first.SHA256()
			if commit {
				wantRef, wantHash = secondRef, sha256.Sum256(secondRaw)
			}
			if read.err != nil || !read.found || read.snapshot.Ref() != wantRef || read.snapshot.SHA256() != wantHash ||
				read.snapshot.Path() != binding.Path() {
				t.Fatalf("fresh bootstrap snapshot after %s: found=%t err=%v", name, read.found, read.err)
			}
			if !commit {
				var versions int
				if err := store.InTransaction(ctx, func(tx pgx.Tx) error {
					return tx.QueryRow(ctx, `SELECT count(*) FROM public.config_versions WHERE document_id=$1`, binding.ID().String()).Scan(&versions)
				}); err != nil || versions != 1 {
					t.Fatalf("rollback left a bootstrap version: versions=%d err=%v", versions, err)
				}
			}
		})
	}
}

func TestNHA029ReadActiveBootstrapAfterWriterDeadlineAndInvalidContext(t *testing.T) {
	store, ctx := nha151Store(t)
	binding := nha151Binding(t, "bootstrap.launch", "bootstrap.launch.yaml")
	firstRaw := []byte("listen: '127.0.0.1:1001'\n")
	first := nha029BootstrapSource(t, store, ctx, binding, firstRaw, ExpectNoBootstrapActive())
	read, found, err := store.ReadActiveBootstrap(ctx)
	if err != nil || !found || read.Ref() != first {
		t.Fatalf("first bootstrap source: found=%t err=%v", found, err)
	}
	expected, err := ExpectBootstrapActive(read.Ref(), read.SHA256())
	if err != nil {
		t.Fatal(err)
	}
	writerReady := make(chan struct{})
	release := make(chan struct{})
	writerDone := make(chan error, 1)
	metadata := nha151Metadata(t)
	go func() {
		_, writeErr := store.InConfigTransactionOutcome(ctx, func(tx *ConfigTx) error {
			raw := []byte("listen: '127.0.0.1:1002'\n")
			ref, err := tx.SaveBootstrapLaunchSource(binding, raw, sha256.Sum256(raw), metadata)
			if err != nil {
				return err
			}
			if err := tx.ActivateBootstrapSource(expected, ref); err != nil {
				return err
			}
			close(writerReady)
			<-release
			return fmt.Errorf("deliberate rollback after reader deadline")
		})
		writerDone <- writeErr
	}()
	var releaseOnce sync.Once
	writerJoined := false
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		if !writerJoined {
			select {
			case <-writerDone:
			case <-time.After(5 * time.Second):
				t.Error("deadline fixture writer did not join")
			}
		}
	})
	select {
	case <-writerReady:
	case writeErr := <-writerDone:
		writerJoined = true
		t.Fatalf("deadline fixture writer failed before barrier: %v", writeErr)
	case <-time.After(5 * time.Second):
		t.Fatal("deadline fixture writer did not hold barrier")
	}
	// The deadline expires only after this reader is observed waiting on 151.
	deadlineCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	readerDone := make(chan nha029BootstrapRead, 1)
	go func() {
		snapshot, present, readErr := store.ReadActiveBootstrapAfterWriter(deadlineCtx)
		readerDone <- nha029BootstrapRead{snapshot, present, readErr}
	}()
	nha029WaitForBootstrapReaderLock(t, store, ctx, readerDone)
	var blocked nha029BootstrapRead
	select {
	case blocked = <-readerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("reader did not return after its advisory-lock deadline")
	}
	if blocked.found || blocked.snapshot.Ref().Valid() {
		t.Fatal("timed-out reader returned a conclusive bootstrap pointer")
	}
	nha151Kind(t, blocked.err, Unavailable)
	releaseOnce.Do(func() { close(release) })
	if writeErr := <-writerDone; writeErr == nil {
		t.Fatal("deadline fixture writer unexpectedly committed")
	}
	writerJoined = true
	type nilContext struct{ context.Context }
	var typedNil *nilContext
	if _, _, err := store.ReadActiveBootstrapAfterWriter(typedNil); err == nil {
		t.Fatal("typed-nil bootstrap reader context was admitted")
	} else {
		nha151Kind(t, err, ConfigInvalid)
	}
	if _, _, err := store.ReadActiveBootstrapAfterWriter(nil); err == nil {
		t.Fatal("nil bootstrap reader context was admitted")
	} else {
		nha151Kind(t, err, ConfigInvalid)
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if _, _, err := store.ReadActiveBootstrapAfterWriter(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled reader context was not propagated: %v", err)
	}
}

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/storage"
)

func TestNHA055RealOwnerReservesPublicationBeforeWorkspaceStage(t *testing.T) {
	r := nha029StartOwner(t, nha029OwnerParent, nha029OwnerChild, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stamp, err := r.owner.CurrentConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	number, manifest, err := stamp.Components()
	if err != nil {
		t.Fatal(err)
	}
	generation, _ := storage.NewConfigGenerationRevision(int64(number))
	precondition, err := configactivation.NewWorkspacePrecondition(generation, manifest, sha256.Sum256(r.raw))
	if err != nil {
		t.Fatal(err)
	}
	request, err := r.request(t, nil, nil, nil).WithWorkspacePrecondition(precondition)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	stageFailure := errors.New("NHA055 synthetic pre-write refusal")
	type answer struct {
		result configactivation.Result
		err    error
	}
	done := make(chan answer, 1)
	go func() {
		result, err := r.owner.ActivateWorkspaceEdit(ctx, request, func(stageCtx context.Context) error {
			close(entered)
			select {
			case <-release:
				return stageFailure
			case <-stageCtx.Done():
				return stageCtx.Err()
			}
		})
		done <- answer{result, err}
	}()
	nha034WaitSignal(t, entered, "055 staged callback owns real 029 operation")
	result, err := r.owner.Activate(ctx, r.request(t, nil, nil, nil))
	if !errors.Is(err, configactivation.ErrActivationBusy) || result.Outcome() != configactivation.RejectedBeforeCommit {
		t.Fatal("another Owner activation entered between guard and stage", err)
	}
	close(release)
	var completed answer
	select {
	case completed = <-done:
	case <-ctx.Done():
		t.Fatal("workspace-stage operation did not release")
	}
	if !errors.Is(completed.err, stageFailure) || completed.result.Attempt().Valid() || completed.result.AttemptOutcome() != storage.TransactionNotCommitted {
		t.Fatal("pre-write refusal crossed SQL/reservation or lost its cause", completed.err)
	}
	after, err := r.owner.CurrentConfig(ctx)
	exact, readErr := os.ReadFile(r.path)
	if err != nil || readErr != nil || after != stamp || !bytes.Equal(exact, r.raw) || len(r.driver.snapshot()) != 0 {
		t.Fatal("pre-write refusal changed generation, file or physical outputs", err, readErr)
	}
	r.close(t)
}

func TestNHA055RealOwnerStaleGenerationAndCancellationNeverEnterStage(t *testing.T) {
	r := nha029StartOwner(t, nha029OwnerParent, nha029OwnerChild, nil)
	ctx := context.Background()
	stamp, err := r.owner.CurrentConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	number, manifest, err := stamp.Components()
	if err != nil {
		t.Fatal(err)
	}
	for _, canceled := range []bool{false, true} {
		generation, _ := storage.NewConfigGenerationRevision(int64(number) + 1)
		precondition, err := configactivation.NewWorkspacePrecondition(generation, manifest, sha256.Sum256(r.raw))
		if err != nil {
			t.Fatal(err)
		}
		request, err := r.request(t, nil, nil, nil).WithWorkspacePrecondition(precondition)
		if err != nil {
			t.Fatal(err)
		}
		attemptCtx, cancel := context.WithCancel(ctx)
		if canceled {
			cancel()
		}
		called := false
		result, err := r.owner.ActivateWorkspaceEdit(attemptCtx, request, func(context.Context) error { called = true; return nil })
		cancel()
		if called || err == nil || result.Attempt().Valid() || result.Outcome() != configactivation.RejectedBeforeCommit {
			t.Fatal("stale/canceled mutation entered stage or created a SQL attempt", canceled, err)
		}
		if !canceled && !errors.Is(err, configactivation.ErrWorkspacePreconditionConflict) || canceled && !errors.Is(err, context.Canceled) {
			t.Fatal("pre-write failure lost its typed cause", canceled, err)
		}
	}
	after, err := r.owner.CurrentConfig(ctx)
	exact, readErr := os.ReadFile(r.path)
	if err != nil || readErr != nil || after != stamp || !bytes.Equal(exact, r.raw) || len(r.driver.snapshot()) != 0 {
		t.Fatal("stale/canceled refusal changed authority, file or physical outputs", err, readErr)
	}
	r.close(t)
}

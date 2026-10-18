package storage

import (
	"context"
	"errors"
	"testing"
)

func TestNHA171FalseReleaseIsUnconfirmedAndIdempotent(t *testing.T) {
	port := testCluster(t)
	settings := testSettings(t, port)
	store, err := Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runtime, err := store.AcquireCommandRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	// This is a real same-session SQL state injection: Close must reject the
	// server's false response even though the lock was released beforehand.
	var removed bool
	if err := runtime.conn.QueryRow(context.Background(), `SELECT pg_advisory_unlock($1::integer, $2::integer)`, commandRuntimeLockClass, commandRuntimeLockObject).Scan(&removed); err != nil || !removed {
		t.Fatalf("fixture failed to release the original lock: removed=%t err=%v", removed, err)
	}
	err = runtime.Close()
	var releaseErr *RuntimeReleaseError
	if !errors.As(err, &releaseErr) {
		t.Fatalf("false PostgreSQL release response claimed success: %v", err)
	}
	if runtime.conn != nil || runtime.releaseState != runtimeReleaseUnconfirmed {
		t.Fatal("uncertain session remained in the pool/runtime")
	}
	if again := runtime.Close(); again != err {
		t.Fatalf("repeat Close changed the release result: first=%v next=%v", err, again)
	}
	witness, err := Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer witness.Close()
	successor, err := witness.AcquireCommandRuntime(context.Background())
	if err != nil {
		t.Fatalf("independent witness could not acquire the already released lock: %v", err)
	}
	if err := successor.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNHA171ZeroRuntimeCannotClaimSuccessfulRelease(t *testing.T) {
	var zero CommandRuntime
	if err := zero.Close(); err == nil {
		t.Fatal("zero runtime claimed a successful release")
	} else {
		var storageErr *Error
		if !errors.As(err, &storageErr) || storageErr.Kind() != InvalidSettings {
			t.Fatalf("zero runtime Close = %v", err)
		}
	}
	var nilRuntime *CommandRuntime
	if err := nilRuntime.Close(); err == nil {
		t.Fatal("nil runtime claimed a successful release")
	}
}

package server

import (
	"context"
	"errors"
	"github.com/damione1/Homeward/internal/config"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
)

func TestNHA201PublicResponseKeepsNativeReadUntilHTTPCompletes(t *testing.T) {
	var closed atomic.Int32
	var interrupted atomic.Int32
	ctx, scope := newPublicResponseScope(t.Context(), func() { interrupted.Add(1) })
	native, cancel := context.WithCancel(ctx)
	retained, err := retainPublicResponse(ctx, native, func() bool { return native.Err() == nil }, func() { closed.Add(1); cancel() })
	if err != nil || !retained {
		t.Fatalf("transfer: %v %v", retained, err)
	}
	if closed.Load() != 0 {
		t.Fatal("native owner released at handler return")
	}
	scope.close()
	scope.close()
	if closed.Load() != 1 || interrupted.Load() != 0 {
		t.Fatalf("close=%d interrupt=%d", closed.Load(), interrupted.Load())
	}
	if ctx.Err() == nil {
		t.Fatal("HTTP lifetime survived completion")
	}
}

func TestNHA201PublicResponseNativeWithdrawalCancelsAndInterrupts(t *testing.T) {
	for _, test := range []struct {
		name   string
		cause  error
		reason auth.AuthorizationTerminationReason
	}{
		{"rights", authorization.ErrDenied, auth.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_RIGHTS_CHANGED},
		{"authority", authorization.ErrUnavailable, auth.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_AUTHORITY_UNAVAILABLE},
	} {
		t.Run(test.name, func(t *testing.T) {
			interrupted := make(chan struct{})
			var once sync.Once
			ctx, scope := newPublicResponseScope(t.Context(), func() { once.Do(func() { close(interrupted) }) })
			defer scope.close()
			native, cancel := context.WithCancelCause(ctx)
			var closed atomic.Int32
			retained, err := retainPublicResponse(ctx, native, func() bool { return native.Err() == nil }, func() { closed.Add(1) })
			if err != nil || !retained {
				t.Fatalf("transfer %v %v", retained, err)
			}
			cancel(test.cause)
			select {
			case <-interrupted:
			case <-time.After(time.Second):
				t.Fatal("blocked writer not interrupted")
			}
			var cause authorizationTerminationCause
			if !errors.As(context.Cause(ctx), &cause) || cause.reason != test.reason {
				t.Fatalf("cause %v", context.Cause(ctx))
			}
			if closed.Load() != 0 {
				t.Fatal("owner released before blocked HTTP handler returned")
			}
			scope.close()
			if closed.Load() != 1 {
				t.Fatal("native owner not released")
			}
		})
	}
}

func TestNHA201PublicResponseTransferFailsClosedAndRemainsBounded(t *testing.T) {
	native, cancel := context.WithCancel(t.Context())
	defer cancel()
	released := 0
	retained, err := retainPublicResponse(t.Context(), native, func() bool { return true }, func() { released++ })
	if err != nil || retained || released != 0 {
		t.Fatal("private direct caller lost cleanup ownership")
	}
	ctx, scope := newPublicResponseScope(t.Context(), nil)
	defer scope.close()
	retained, err = retainPublicResponse(ctx, native, func() bool { return false }, func() { released++ })
	if !errors.Is(err, authorization.ErrDenied) || retained || released != 0 {
		t.Fatal("revoked read retained")
	}
	for range maxPublicResponseLeases {
		retained, err = retainPublicResponse(ctx, native, func() bool { return true }, func() { released++ })
		if err != nil || !retained {
			t.Fatalf("bounded transfer %v", err)
		}
	}
	retained, err = retainPublicResponse(ctx, native, func() bool { return true }, func() { released++ })
	if !errors.Is(err, authorization.ErrCapacity) || retained {
		t.Fatal("unbounded response owner")
	}
	scope.close()
	if released != maxPublicResponseLeases {
		t.Fatalf("released %d", released)
	}
}

func TestNHA201PublicResponsePreservesDeadlineAndJoinsWithdrawalRace(t *testing.T) {
	for range 32 {
		ctx, scope := newPublicResponseScope(t.Context(), func() {})
		native, cancel := context.WithCancelCause(ctx)
		var closed atomic.Int32
		retained, err := retainPublicResponse(ctx, native, func() bool { return true }, func() { closed.Add(1) })
		if err != nil || !retained {
			t.Fatal(err)
		}
		var race sync.WaitGroup
		race.Add(2)
		go func() { defer race.Done(); cancel(authorization.ErrDenied) }()
		go func() { defer race.Done(); scope.close() }()
		race.Wait()
		if closed.Load() != 1 {
			t.Fatal("response owner leaked or double released")
		}
	}
	ctx, scope := newPublicResponseScope(t.Context(), nil)
	defer scope.close()
	native, cancel := context.WithCancelCause(ctx)
	retained, err := retainPublicResponse(ctx, native, func() bool { return true }, func() {})
	if err != nil || !retained {
		t.Fatal(err)
	}
	cancel(context.DeadlineExceeded)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("deadline not relayed")
	}
	if !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
		t.Fatalf("deadline replaced: %v", context.Cause(ctx))
	}
}

// Models net/http's buffered unary write: handler Write returns immediately,
// while Flush remains blocked until the existing cancellation deadline fires.
type responseScopeBufferedWriter struct {
	*httptest.ResponseRecorder
	entered     chan struct{}
	interrupted chan struct{}
	once        sync.Once
	released    *atomic.Int32
	t           *testing.T
}

func (w *responseScopeBufferedWriter) SetReadDeadline(time.Time) error { return nil }
func (w *responseScopeBufferedWriter) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() && time.Until(deadline) <= publicErrorWriteGrace+10*time.Millisecond {
		w.once.Do(func() { close(w.interrupted) })
	}
	return nil
}
func (w *responseScopeBufferedWriter) FlushError() error {
	if w.released.Load() != 0 {
		w.t.Error("permit released before buffered flush")
	}
	close(w.entered)
	select {
	case <-w.interrupted:
	case <-time.After(2 * time.Second):
		w.t.Error("buffered flush ignored native withdrawal")
	}
	if w.released.Load() != 0 {
		w.t.Error("permit released during buffered flush")
	}
	return context.DeadlineExceeded
}
func TestNHA201PublicResponseFlushRetainsScopeAndWriteInterrupt(t *testing.T) {
	var released atomic.Int32
	native, withdraw := context.WithCancel(context.Background())
	defer withdraw()
	writer := &responseScopeBufferedWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), interrupted: make(chan struct{}), released: &released, t: t}
	route := publicRoute{key: RouteKey{kind: RouteKindConnect, method: RouteMethodPost, path: "/synthetic.Flush"}, access: RouteAccessAnonymous, flow: RouteFlowUnary,
		handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			retained, err := retainPublicResponse(r.Context(), native, func() bool { return native.Err() == nil }, func() { released.Add(1) })
			if err != nil || !retained {
				t.Error("native permit transfer", retained, err)
				return
			}
			_, _ = w.Write([]byte("small unary body"))
		}),
	}
	compiled := &compiledPublic{routes: map[routeLookup]publicRoute{{method: http.MethodPost, path: "/synthetic.Flush"}: route}, auditor: &nha139Auditor{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		compiled.handler(config.DefaultPublicSettings(), newPublicAdmissions()).ServeHTTP(writer, httptest.NewRequest(http.MethodPost, "/synthetic.Flush", nil))
	}()
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("unary response never flushed")
	}
	if released.Load() != 0 {
		t.Fatal("buffered response lost its native permit")
	}
	withdraw()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("buffered send did not end")
	}
	if released.Load() != 1 {
		t.Fatal("response permit not released exactly once", released.Load())
	}
}

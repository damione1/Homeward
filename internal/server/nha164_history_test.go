package server

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	history "github.com/damione1/Homeward/gen/home/history/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/historyquery"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/view"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type nha164ReaderSpy struct {
	calls    atomic.Int32
	traces   func(context.Context, storage.HistoryPageQuery) (storage.HistoryPage, error)
	recorder func(context.Context, historyquery.Query, *storage.RecorderQueryCursor) (storage.RecorderQueryPage, error)
}

func (s *nha164ReaderSpy) WithHistoryReadReservation(ctx context.Context, read func(context.Context) error) error {
	return read(ctx)
}
func (s *nha164ReaderSpy) ReadExecutionHistoryPage(ctx context.Context, q storage.HistoryPageQuery) (storage.HistoryPage, error) {
	s.calls.Add(1)
	if s.traces != nil {
		return s.traces(ctx, q)
	}
	epoch, _ := storage.ParseHistoryEpoch("00000000000000000000000000000164")
	now := time.Now().UTC()
	return storage.HistoryPage{Epoch: epoch, EpochState: storage.HistoryEpochCurrent, AsOf: now, Cutoff: now.Add(-7 * 24 * time.Hour)}, nil
}
func (s *nha164ReaderSpy) ReadHistoricalRecorderPageAt(ctx context.Context, q historyquery.Query, c *storage.RecorderQueryCursor, _ *storage.RecorderLiveHigh) (storage.RecorderQueryPage, error) {
	s.calls.Add(1)
	if s.recorder != nil {
		return s.recorder(ctx, q, c)
	}
	return storage.RecorderQueryPage{}, historyquery.ErrUnavailable
}

func nha164AuthorityState(t *testing.T, source authorization.PolicySource, role auth.AccountRole, revision uint64) authorization.AccountState {
	t.Helper()
	hash, err := source.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	version, err := authorization.NewVersion(revision, hash)
	if err != nil {
		t.Fatal(err)
	}
	state, err := authorization.NewAccountState(source, role, auth.AccountStatus_ACCOUNT_STATUS_ACTIVE, 1, version, revision)
	if err != nil {
		t.Fatal(err)
	}
	return state
}
func nha164Handler(t *testing.T, role auth.AccountRole, family auth.ScopeFamily, operation auth.ScopeOperation) (*historyHandler, context.Context, authorization.PolicySource, *nha164ReaderSpy) {
	t.Helper()
	account, err := contracts.ParseAccountID("nha164-account")
	if err != nil {
		t.Fatal(err)
	}
	source, err := authorization.EmptySource(account)
	if err != nil {
		t.Fatal(err)
	}
	authority := authorization.NewAuthority()
	if err = authority.Hydrate([]authorization.AccountState{nha164AuthorityState(t, source, role, 1)}); err != nil {
		t.Fatal(err)
	}
	permission, err := authorization.NewPermission(family, operation)
	if err != nil {
		t.Fatal(err)
	}
	scopes, err := authorization.NewScopeSet([]authorization.Permission{permission})
	if err != nil {
		t.Fatal(err)
	}
	key, err := contracts.ParseMcpKeyID("nha164-scoped-fixture")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := authorization.ScopedPrincipal(account, key, scopes)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := principal.Actor()
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := authorization.WithPrincipal(context.WithValue(context.Background(), actorContextKey{}, actor), principal)
	if err != nil {
		t.Fatal(err)
	}
	sequence, err := sequencer.New(sequencer.RealClock{}, 16)
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := view.NewConfigStamp(1, [32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	codec, err := historyquery.NewCursorCodec()
	if err != nil {
		t.Fatal(err)
	}
	spy := &nha164ReaderSpy{}
	return &historyHandler{runtime: &historyRuntime{reader: spy, authority: authority, sequence: sequence, codec: codec, currentConfig: func(context.Context) (view.ConfigStamp, error) { return stamp, nil }}}, ctx, source, spy
}
func nha164TraceRequest() *connect.Request[history.QueryExecutionTracesRequest] {
	now := time.Now().UTC()
	return connect.NewRequest(&history.QueryExecutionTracesRequest{Period: &history.Period{Start: timestamppb.New(now.Add(-time.Hour)), End: timestamppb.New(now.Add(time.Hour))}})
}
func TestNHA164DeniedTraceRequestsNeverEnterReader(t *testing.T) {
	for _, tc := range []struct {
		role      auth.AccountRole
		family    auth.ScopeFamily
		operation auth.ScopeOperation
	}{
		{auth.AccountRole_ACCOUNT_ROLE_USER, auth.ScopeFamily_SCOPE_FAMILY_HISTORY, auth.ScopeOperation_SCOPE_OPERATION_READ},
		{auth.AccountRole_ACCOUNT_ROLE_ADMIN, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, auth.ScopeOperation_SCOPE_OPERATION_WRITE},
		{auth.AccountRole_ACCOUNT_ROLE_ADMIN, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER},
	} {
		handler, ctx, _, spy := nha164Handler(t, tc.role, tc.family, tc.operation)
		response, err := handler.QueryExecutionTraces(ctx, nha164TraceRequest())
		if response != nil || connect.CodeOf(err) != connect.CodePermissionDenied || spy.calls.Load() != 0 {
			t.Fatalf("denied input entered reader: %v calls=%d", err, spy.calls.Load())
		}
	}
	handler, ctx, _, spy := nha164Handler(t, auth.AccountRole_ACCOUNT_ROLE_ADMIN, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, auth.ScopeOperation_SCOPE_OPERATION_READ)
	if response, err := handler.QueryExecutionTraces(ctx, nha164TraceRequest()); err != nil || response.Msg.GetPage() == nil || spy.calls.Load() != 1 {
		t.Fatalf("exact administrative READ refused: %v", err)
	}
}
func TestNHA164RevocationDuringReadWithholdsResult(t *testing.T) {
	handler, ctx, source, spy := nha164Handler(t, auth.AccountRole_ACCOUNT_ROLE_ADMIN, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, auth.ScopeOperation_SCOPE_OPERATION_READ)
	entered, release := make(chan struct{}), make(chan struct{})
	spy.traces = func(context.Context, storage.HistoryPageQuery) (storage.HistoryPage, error) {
		close(entered)
		<-release
		epoch, _ := storage.ParseHistoryEpoch("00000000000000000000000000000164")
		now := time.Now().UTC()
		return storage.HistoryPage{Epoch: epoch, EpochState: storage.HistoryEpochCurrent, AsOf: now, Cutoff: now.Add(-7 * 24 * time.Hour)}, nil
	}
	returned := make(chan error, 1)
	go func() {
		response, err := handler.QueryExecutionTraces(ctx, nha164TraceRequest())
		if response != nil {
			returned <- errors.New("revoked response escaped")
			return
		}
		returned <- err
	}()
	<-entered
	if err := handler.runtime.authority.Publish(nha164AuthorityState(t, source, auth.AccountRole_ACCOUNT_ROLE_USER, 2)); err != nil {
		t.Fatal(err)
	}
	if err := handler.runtime.authority.Publish(nha164AuthorityState(t, source, auth.AccountRole_ACCOUNT_ROLE_ADMIN, 3)); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-returned:
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("regrant rescued revoked read: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reader did not stop")
	}
}
func TestNHA164UnavailableReaderRedactsProviderCanaries(t *testing.T) {
	handler, ctx, _, spy := nha164Handler(t, auth.AccountRole_ACCOUNT_ROLE_ADMIN, auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, auth.ScopeOperation_SCOPE_OPERATION_READ)
	const canary = "SECRET_DSN_HEADER_CONFIG_ARGS_CAMERA_FRAME_CANARY"
	spy.traces = func(context.Context, storage.HistoryPageQuery) (storage.HistoryPage, error) {
		return storage.HistoryPage{}, errors.New(canary)
	}
	response, err := handler.QueryExecutionTraces(ctx, nha164TraceRequest())
	if response != nil || connect.CodeOf(err) != connect.CodeUnavailable || strings.Contains(err.Error(), canary) {
		t.Fatalf("unavailable reader leaked/returned a page: %v", err)
	}
}

package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/historyquery"
)

func TestNHA164RequestReservationIsSingleStoreBoundAndCannotBeReused(t *testing.T) {
	s := nha032SQLFixture(t)
	epoch := nha031Epoch(t, 171)
	nha031Open(t, s, epoch)
	trace := nha186Epoch(t, "00000000000000000000000000000171")
	if _, err := s.OpenExecutionHistoryEpoch(context.Background(), trace); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	q := nha164Query(t, historyquery.Observations, "nha164-reservation", at.Add(-time.Hour), at.Add(time.Hour), 100)
	var retained context.Context
	err := s.WithHistoryReadReservation(context.Background(), func(ctx context.Context) error {
		retained = ctx
		if len(s.historyReadSlots) != 1 {
			t.Fatal("request did not acquire exactly one slot")
		}
		raw, err := s.ReadHistoricalRecorderPageAt(ctx, q, nil, nil)
		if err != nil {
			return err
		}
		raw.Release()
		page, err := s.ReadExecutionHistoryPage(ctx, HistoryPageQuery{Epoch: trace, Since: at.Add(-time.Hour), Until: at.Add(time.Hour), Limit: 31, ClipRetention: true, HoldReservation: true})
		if err != nil {
			return err
		}
		page.Release()
		if len(s.historyReadSlots) != 1 {
			t.Fatal("page release returned its enclosing request slot")
		}
		other := &Store{pool: s.pool, historyReadSlots: make(chan struct{}, 2)}
		if _, release, err := other.acquireHistoryRead(ctx); err == nil {
			release()
			t.Fatal("reservation borrowed by another Store")
		}
		if _, err := other.ReadExecutionHistoryPage(ctx, HistoryPageQuery{Epoch: trace, Limit: 1}); !errors.Is(err, ErrHistoryInvalid) || errors.Is(err, ErrHistoryUnavailable) {
			t.Fatal("foreign trace reservation lost its invalid classification", err)
		}
		zero := context.WithValue(ctx, historyReservationKey{}, (*historyReservation)(nil))
		if _, release, err := s.acquireHistoryRead(zero); err == nil {
			release()
			t.Fatal("zero reservation admitted")
		}
		if _, err := s.ReadExecutionHistoryPage(zero, HistoryPageQuery{Epoch: trace, Limit: 1}); !errors.Is(err, ErrHistoryInvalid) || errors.Is(err, ErrHistoryUnavailable) {
			t.Fatal("zero trace reservation lost its invalid classification", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.historyReadSlots) != 0 {
		t.Fatal("request slot was not returned")
	}
	if _, release, err := s.acquireHistoryRead(retained); err == nil {
		release()
		t.Fatal("released request context remained reusable")
	}
	if _, err := s.ReadExecutionHistoryPage(retained, HistoryPageQuery{Epoch: trace, Limit: 1}); !errors.Is(err, ErrHistoryInvalid) || errors.Is(err, ErrHistoryUnavailable) {
		t.Fatal("released trace reservation lost its invalid classification", err)
	}
	sentinel := errors.New("synthetic encoding failure")
	if err := s.WithHistoryReadReservation(context.Background(), func(context.Context) error { return sentinel }); !errors.Is(err, sentinel) || len(s.historyReadSlots) != 0 {
		t.Fatal("callback error lost or leaked reservation", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	err = s.WithHistoryReadReservation(ctx, func(ctx context.Context) error { cancel(); return ctx.Err() })
	if !errors.Is(err, context.Canceled) || len(s.historyReadSlots) != 0 {
		t.Fatal("callback cancellation leaked reservation", err)
	}
	// The older persisted trace reader promises its storage error family even
	// when the shared request reservation expires before any SQL starts.
	s.historyReadSlots <- struct{}{}
	s.historyReadSlots <- struct{}{}
	deadline, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_, err = s.ReadExecutionHistoryPage(deadline, HistoryPageQuery{Epoch: trace, Limit: 1})
	stop()
	if !errors.Is(err, ErrHistoryUnavailable) || !errors.Is(err, context.DeadlineExceeded) || len(s.historyReadSlots) != 2 {
		t.Fatal("persisted trace acquisition lost unavailable/deadline or borrowed a third slot", err)
	}
	<-s.historyReadSlots
	<-s.historyReadSlots
}

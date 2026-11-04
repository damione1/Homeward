package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/apimutations"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/mediasessions"
)

type nha202MediaClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *nha202MediaClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *nha202MediaClock) advance(by time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(by)
	c.mu.Unlock()
}
func nha202SessionOwner(t *testing.T) (*nha162RPCRig, *mediasessions.Service, *nha202MediaClock, context.Context, *mediasessions.Admission, mediasessions.SourceBinding) {
	t.Helper()
	r := nha162NewRPCRig(t)
	nha162SetPolicy(t, r, r.admin, nhf016AllCameras)
	ctx := nha162ImportContext(t, r)
	clock := &nha202MediaClock{now: time.Now().UTC().Truncate(time.Microsecond)}
	owner, err := mediasessions.NewService(r.store, r.coordinator.authority, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	entity, _ := domain.ParseEntityID("synthetic-nha202-camera")
	slot, _ := authorization.NewCapabilitySlot(nhf016CameraSchema, nhf016CameraEndpoint)
	admission, err := owner.AdmitSource(ctx, entity, slot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admission.Release)
	var codec [32]byte
	codec[0] = 1
	source, err := mediasessions.NewSourceBinding(entity, slot, mediasessions.Camera, 1, "synthetic-source-epoch", codec)
	if err != nil {
		t.Fatal(err)
	}
	return r, owner, clock, ctx, admission, source
}
func TestNHA202MediaSessionSQLSingleOwnerBoundsRestartAndExpiry(t *testing.T) {
	r, owner, clock, ctx, admission, source := nha202SessionOwner(t)
	input, err := mediasessions.NewInput(source, nil, time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	var first mediasessions.Record
	for i := 0; i < mediasessions.MaxActivePerActor; i++ {
		record, err := owner.Create(ctx, admission, input)
		if err != nil {
			t.Fatal("bounded insert", err)
		}
		if i == 0 {
			first = record
		}
	}
	if _, err := owner.Create(ctx, admission, input); !mediasessions.IsCode(err, mediasessions.ActiveCapacity) {
		t.Fatal("active capacity", err)
	}
	other, err := mediasessions.NewService(r.store, r.coordinator.authority, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	reopened, err := other.AdmitSource(ctx, source.Entity(), source.Slot())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Release()
	got, err := other.Get(ctx, reopened, first.ID(), source)
	if err != nil || got.ID() != first.ID() || !got.ExpireTime().Equal(first.ExpireTime()) {
		t.Fatal("SQL sole owner reopen", err)
	}
	var codec [32]byte
	codec[0] = 1
	changed, err := mediasessions.NewSourceBinding(source.Entity(), source.Slot(), mediasessions.Camera, 2, "synthetic-source-epoch", codec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.Get(ctx, reopened, first.ID(), changed); !mediasessions.IsCode(err, mediasessions.SourceChanged) {
		t.Fatal("stale config accepted", err)
	}
	operation, err := owner.BeginOperation(ctx, first.ID())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { <-operation.Context().Done(); operation.Close(); close(done) }()
	if _, err = owner.Retire(ctx, admission, first.ID(), changed); err != nil {
		t.Fatal("retire", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retire did not cancel and join")
	}
	if _, err = owner.Get(ctx, admission, first.ID(), source); !mediasessions.IsCode(err, mediasessions.NotFound) {
		t.Fatal("retired content", err)
	}
	if _, err = owner.LookupOwned(ctx, first.ID()); err != nil {
		t.Fatal("tombstone missing before retention", err)
	}
	clock.advance(time.Minute)
	if _, err = owner.Get(ctx, admission, first.ID(), source); !mediasessions.IsCode(err, mediasessions.NotFound) {
		t.Fatal("expiry", err)
	}
	if _, err = owner.Create(ctx, admission, input); err != nil {
		t.Fatal("expired leases did not free live capacity", err)
	}
	clock.advance(mediasessions.TombstoneRetention)
	if _, err = owner.LookupOwned(ctx, first.ID()); !mediasessions.IsCode(err, mediasessions.NotFound) {
		t.Fatal("retention", err)
	}
}
func TestNHA202MediaSessionSQLDedupDoesNotRecreateOrRenew(t *testing.T) {
	_, owner, clock, ctx, admission, source := nha202SessionOwner(t)
	input, err := mediasessions.NewInput(source, nil, time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	request, err := apimutations.New(ctx, "/home.api.v2.MediaService/CreateMediaSession", "ecfdf7a2-85b7-4934-8a01-0e473ff7f7d2", []byte("exact synthetic intention"))
	if err != nil {
		t.Fatal(err)
	}
	bound, err := apimutations.WithResultEncoder(ctx, request, func(record mediasessions.Record) ([]byte, error) { return []byte(record.ID().String()), nil })
	if err != nil {
		t.Fatal(err)
	}
	first, err := owner.Create(bound, admission, input)
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Second)
	_, err = owner.Create(bound, admission, input)
	var outcome *apimutations.Error
	if !errors.As(err, &outcome) {
		t.Fatal("no exact replay", err)
	}
	original, ok := outcome.ReplayResult()
	if !ok || string(original) != first.ID().String() {
		t.Fatal("replay substituted result")
	}
	current, err := owner.Get(ctx, admission, first.ID(), source)
	if err != nil || !current.ExpireTime().Equal(first.ExpireTime()) {
		t.Fatal("replay renewed lease", err)
	}
	different, err := apimutations.New(ctx, "/home.api.v2.MediaService/CreateMediaSession", "ecfdf7a2-85b7-4934-8a01-0e473ff7f7d2", []byte("changed intention"))
	if err != nil {
		t.Fatal(err)
	}
	changed, err := apimutations.WithResultEncoder(ctx, different, func(record mediasessions.Record) ([]byte, error) { return []byte(record.ID().String()), nil })
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.Create(changed, admission, input)
	if !errors.As(err, &outcome) || outcome.Code() != apimutations.RequestIDReused {
		t.Fatal("different intention reused ID", err)
	}
}

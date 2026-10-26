package server

import (
	"context"
	"errors"
	authwire "github.com/damione1/Homeward/gen/home/auth/v1"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/view"
)

// Deterministic profile authority for isolated tests only. It has no provider
// address, implicit host timezone or coordinate field. Updates wake idle views.
type nhf010Profiles struct {
	mu          sync.Mutex
	zones       map[view.ActorID]string
	failed      bool
	reads       int
	next        uint64
	subscribers map[uint64]nhf010ProfileSubscription
}
type nhf010ProfileSubscription struct {
	actor   view.ActorID
	changes chan struct{}
}

func (p *nhf010Profiles) DisplayTimeZone(ctx context.Context, actor view.ActorID) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads++
	if p.failed {
		return "", authorization.ErrUnavailable
	}
	return p.zones[actor], nil
}
func (p *nhf010Profiles) SubscribeDisplayProfileChanges(ctx context.Context, actor view.ActorID) (<-chan struct{}, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failed {
		return nil, nil, authorization.ErrUnavailable
	}
	if p.subscribers == nil {
		p.subscribers = make(map[uint64]nhf010ProfileSubscription)
	}
	p.next++
	id := p.next
	changes := make(chan struct{}, 1)
	p.subscribers[id] = nhf010ProfileSubscription{actor, changes}
	return changes, sync.OnceFunc(func() { p.mu.Lock(); delete(p.subscribers, id); p.mu.Unlock() }), nil
}
func (p *nhf010Profiles) set(actor view.ActorID, zone string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.zones == nil {
		p.zones = make(map[view.ActorID]string)
	}
	p.zones[actor] = zone
	for _, sub := range p.subscribers {
		if sub.actor == actor {
			select {
			case sub.changes <- struct{}{}:
			default:
			}
		}
	}
}
func nhf010AwaitNotification(t *testing.T, changes <-chan struct{}, wantOpen bool) {
	t.Helper()
	select {
	case _, open := <-changes:
		if open != wantOpen {
			t.Fatalf("notification open=%v want=%v", open, wantOpen)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("idle display-profile subscription did not wake")
	}
}

func TestNHF010ProfileSourceIsReadAfterEveryGrantCacheHitAndCannotBroadenRights(t *testing.T) {
	f := nha165GrantSetup(t)
	f.rows(t, 1) // Warm the real policy/capability grant cache before decoration.
	profiles := new(nhf010Profiles)
	profiles.set(f.actor, "America/Toronto")
	f.a.profiles = profiles
	page, err := f.service.OpenView(f.ctx, f.actor, view.All(), 1)
	if err != nil || page.DisplayProfile.GetTimeZone() != "America/Toronto" {
		t.Fatal("profiled grant", page, err)
	}
	profiles.set(f.actor, "Europe/Paris")
	next, err := f.service.OpenView(f.ctx, f.actor, view.All(), 1)
	if err != nil || next.DisplayProfile.GetTimeZone() != "Europe/Paris" || len(next.Items) != 1 {
		t.Fatal("cached grant retained stale profile", next, err)
	}
	profiles.mu.Lock()
	profiles.failed = true
	profiles.mu.Unlock()
	if _, err := f.a.CurrentGrant(f.ctx, f.actor); !errors.Is(err, authorization.ErrUnavailable) {
		t.Fatal("profile failure served a cached profile", err)
	}
	profiles.mu.Lock()
	profiles.failed = false
	profiles.mu.Unlock()
	profiles.set(f.actor, "Local")
	if _, err := f.a.CurrentGrant(f.ctx, f.actor); !errors.Is(err, authorization.ErrUnavailable) {
		t.Fatal("machine fallback admitted", err)
	}
	profiles.set(f.actor, "")
	absent, err := f.service.OpenView(f.ctx, f.actor, view.All(), 1)
	if err != nil || absent.DisplayProfile != nil {
		t.Fatal("absent profile inferred", absent, err)
	}
	// Profile availability must never bypass the existing policy authority.
	profiles.set(f.actor, "UTC")
	empty, err := authorization.EmptySource(f.account)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.a.authority.Publish(nha164AuthorityState(t, empty, authwire.AccountRole_ACCOUNT_ROLE_USER, 2)); err != nil {
		t.Fatal(err)
	}
	f.rows(t, 0)
}

func TestNHF010ProfileAndPolicyNotificationsMergeAndJoinOnRelease(t *testing.T) {
	f := nha165GrantSetup(t)
	profiles := new(nhf010Profiles)
	profiles.set(f.actor, "America/Toronto")
	f.a.profiles = profiles
	changes, release, err := f.a.SubscribeGrantChanges(f.ctx, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	profiles.set(f.actor, "Europe/Paris")
	nhf010AwaitNotification(t, changes, true)
	// A profile wake is additive to ordinary rights withdrawal, never a
	// replacement notification path that could hide a policy revocation.
	if err := f.a.authority.Quarantine(f.principal); err != nil {
		t.Fatal(err)
	}
	nhf010AwaitNotification(t, changes, true)
	release()
	release()
	nhf010AwaitNotification(t, changes, false)
	profiles.mu.Lock()
	retained := len(profiles.subscribers)
	profiles.mu.Unlock()
	if retained != 0 {
		t.Fatal("released profile subscriber retained", retained)
	}

	// A different actor is not allowed to attach this principal's profile.
	foreign, _ := domain.ParseActorID("foreign-profile-actor")
	if _, _, err := f.a.SubscribeGrantChanges(f.ctx, foreign); !errors.Is(err, authorization.ErrDenied) {
		t.Fatal("foreign actor subscription", err)
	}
}

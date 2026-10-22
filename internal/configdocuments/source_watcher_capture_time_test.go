package configdocuments

import (
	"context"
	"os"
	"testing"
	"time"

	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/storage"
)

type nha055CaptureEndClock struct {
	now   time.Time
	reads int
}

func (c *nha055CaptureEndClock) Now() time.Time                            { c.reads++; return c.now }
func (c *nha055CaptureEndClock) Wait(context.Context, time.Duration) error { return nil }

// A real SQL24 metadata/filesystem capture runs with a deliberately stale
// invocation timestamp. The clock is sampled at capture completion, not before
// its potentially slow SQL/FS reads. No activation is attempted in this test.
func TestNHA055SourceWatcherStabilityStartsAfterTheActualCapture(t *testing.T) {
	service, ctx, operational, _ := nha055Documents(t)
	id, err := accounts.NewAccountID()
	if err != nil {
		t.Fatal(err)
	}
	login, err := accounts.ParseLoginName("nha055 capture timing")
	if err != nil {
		t.Fatal(err)
	}
	display, err := accounts.ParseDisplayName("NHA055 capture timing")
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := accounts.HashNewPassword(ctx, "NHA055 capture timing fixture password")
	if err != nil {
		t.Fatal(err)
	}
	record, err := service.store.BootstrapAdmin(ctx, storage.NewAccount{ID: id, Login: login, Display: display, Role: auth.AccountRole_ACCOUNT_ROLE_ADMIN, Verifier: verifier})
	if err != nil {
		t.Fatal(err)
	}
	account, err := record.ID()
	if err != nil {
		t.Fatal(err)
	}
	state, err := service.store.AuthorizationPolicy(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	source, err := state.Source()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := source.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	binding, err := storage.AuthorizationPolicyBinding(account)
	if err != nil {
		t.Fatal(err)
	}
	physical := nha055ObservedFile(t, operational, binding.ID(), binding, raw)
	clean, err := service.sourceWatchCapture(ctx)
	if err != nil || len(clean.targets) != 1 {
		t.Fatal("clean actual capture", err)
	}
	window := &sourceWatchWindow{cut: sourceWatchCutHash(clean.operational)}
	if window.policy.observe(clean.targets[0]) {
		t.Fatal("clean initial policy armed")
	}
	dirty := append([]byte("# deliberately edited after clean baseline\n"), raw...)
	if err := os.WriteFile(physical, dirty, 0600); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	clock := &nha055CaptureEndClock{now: start.Add(time.Second)}
	watcher := &Watcher{service: service, clock: clock}
	windows := sourceWatchState{windows: map[storage.ConfigDocumentID]*sourceWatchWindow{binding.ID(): window}}
	watcher.pollSourceSets(ctx, &windows, start)
	if clock.reads == 0 || !window.stable.present || !window.stable.since.Equal(clock.now) {
		t.Fatal("stability reused the pre-capture timestamp")
	}
	observation := window.stable.observation
	if window.stable.admit(observation, start.Add(1250*time.Millisecond)) || window.stable.admit(observation, start.Add(1499*time.Millisecond)) {
		t.Fatal("slow capture time counted toward500ms stable exposure")
	}
	if !window.stable.admit(observation, start.Add(1500*time.Millisecond)) {
		t.Fatal("exact500ms after completed capture was refused")
	}
	if window.stable.admit(observation, start.Add(1400*time.Millisecond)) {
		t.Fatal("clock reversal reused stability")
	}
}

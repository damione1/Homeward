package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/scope"
)

// The explicit bootstrap is provisioned before this installation has a key or
// active generation. The real Run must commit its source before lending the
// factory any composition authority, even when the workspace directory cannot
// receive an automatically generated sidecar.
func TestNHA029C5FreshReadOnlyExplicitBootstrapAdmitsSQLBeforeFactory(t *testing.T) {
	f := nha029C5NewFixture(t)
	bootstrap := filepath.Join(f.workspace, "operator-bootstrap.yaml")
	if err := os.WriteFile(bootstrap, f.raw, 0400); err != nil {
		t.Fatal(err)
	}
	choice, err := config.ExistingBootstrap(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(f.path, choice, nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	selectedBootstrap, err := startup.BootstrapSource()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(before)
	if err := os.Chmod(f.workspace, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(f.workspace, 0700); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	joined := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-joined:
		case <-time.After(12 * time.Second):
			t.Error("fresh explicit Run did not join during cleanup")
		}
	})
	factoryCalls := 0
	var admitted storage.ConfigGenerationSnapshot
	factory := func(ctx context.Context, store *storage.Store) (RuntimePlan, error) {
		factoryCalls++
		var present bool
		var readErr error
		admitted, present, readErr = store.ReadActiveConfig(ctx)
		if readErr != nil || !present || admitted.Availability() != storage.ConfigGenerationComplete || admitted.ManifestFormat() != storage.ConfigManifestCompiledV2 {
			return RuntimePlan{}, errors.New("fresh explicit factory preceded complete SQL admission")
		}
		manifest, err := registry.ParseManifest(scope.ManifestV1())
		if err != nil {
			return RuntimePlan{}, err
		}
		repository, err := registry.NewRepository(store, manifest)
		return RuntimePlan{Identity: repository}, err
	}
	go func() {
		done <- RunWithStartupFactory(ctx, startup, (&net.ListenConfig{}).Listen, func(net.Addr) { close(ready) }, factory)
		close(joined)
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("fresh explicit Run exited before Ready: %v", err)
	case <-f.ctx.Done():
		t.Fatal("fresh explicit Run did not become Ready")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-f.ctx.Done():
		t.Fatal("fresh explicit Run did not join")
	}
	if factoryCalls != 1 || admitted.Revision() != f.active(t).Revision() {
		t.Fatal("fresh explicit Run did not compose its single admitted SQL generation")
	}
	activeBootstrap, present, err := f.store.ReadActiveBootstrap(f.ctx)
	if err != nil || !present || activeBootstrap.Path() != selectedBootstrap.Path() || activeBootstrap.SHA256() != sha256.Sum256(before) {
		t.Fatalf("fresh explicit bootstrap was not the SQL launch authority: %v", err)
	}
	after, err := os.ReadFile(bootstrap)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("fresh explicit Run overwrote the provisioned bootstrap: %v", err)
	}
	clear(after)
	info, err := os.Stat(bootstrap)
	if err != nil || info.Mode().Perm() != 0400 {
		t.Fatalf("fresh explicit Run changed bootstrap permissions: %v", err)
	}
	if _, err := os.Lstat(f.path + ".bootstrap.yaml"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fresh explicit Run created an unselected default sidecar: %v", err)
	}
	if counts := f.counts(t); counts != [3]int64{1, 1, 0} || len(f.effects) != 0 {
		t.Fatalf("fresh explicit Run duplicated activation or dispatched work: %v", counts)
	}
}

// P7's bootstrap-only update has an established operational SQL authority A.
// A fresh explicit bootstrap without any operational source is separately
// refused below; it must not be made Ready by inventing a generation.
func TestNHA029C5BootstrapOnlyAndMixedConfiguredRestarts(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		name := "bootstrap_only"
		if mixed {
			name = "mixed"
		}
		t.Run(name, func(t *testing.T) {
			f := nha029C5NewFixture(t)
			initial := f.start(t, false)
			initial.ready(t, nil)
			a := f.active(t)
			bootstrapA, present, err := f.store.ReadActiveBootstrap(f.ctx)
			if err != nil || !present {
				t.Fatalf("initial bootstrap SQL missing: %v", err)
			}
			sidecar := f.path + ".bootstrap.yaml"
			info, err := os.Stat(sidecar)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("default sidecar permissions: %v", err)
			}
			base := bootstrapA.Bytes()
			defer clear(base)
			onDisk, err := os.ReadFile(sidecar)
			if err != nil || !bytes.Equal(onDisk, base) {
				t.Fatalf("default sidecar differs from SQL: %v", err)
			}
			clear(onDisk)
			initial.fireWaits(t)
			if len(f.effects) != 1 {
				t.Fatal("bootstrap hot refusal lacks its old terminal effect and real waits")
			}
			var operationalB []byte
			if mixed {
				f.edit(t, "mixed operational B must require explicit activation")
				operationalB, err = os.ReadFile(f.path)
				if err != nil {
					t.Fatal(err)
				}
				defer clear(operationalB)
			}
			changed := append(append([]byte(nil), base...), []byte("recorder:\n  budget_mib: 32\n")...)
			defer clear(changed)
			if err := os.WriteFile(sidecar, changed, 0600); err != nil {
				t.Fatal(err)
			}
			before := f.counts(t)
			initial.send(t, nha029C5Message{kind: nha029C5Activate})
			refused := initial.await(t, nha029C5Activated)
			if refused.count>>32 != 1 || configactivation.Outcome(uint32(refused.count)) != configactivation.RestartRequired {
				t.Fatal("bootstrap edit was hot activated instead of requiring restart")
			}
			visible := initial.status(t, true)
			if visible.revision != uint64(a.Revision().Number()) || visible.manifest != a.ManifestSHA256() || f.counts(t) != before || len(f.effects) != 1 {
				t.Fatal("restart-required refusal changed operational authority or emitted an action")
			}
			unchanged, present, err := f.store.ReadActiveBootstrap(f.ctx)
			if err != nil || !present || unchanged.Ref() != bootstrapA.Ref() || unchanged.SHA256() != bootstrapA.SHA256() {
				t.Fatalf("hot refusal wrote bootstrap SQL: %v", err)
			}
			// A positive explicit event reaches the very same old 040 child after
			// refusal. This is new requested work, not a replay by configuration.
			initial.send(t, nha029C5Message{kind: nha029C5ReleaseWait})
			for len(f.effects) < 2 {
				message := initial.next(t)
				switch message.kind {
				case nha029C5Ack:
				case nha029C5Effect:
					initial.recordEffect(t, message, true)
				default:
					t.Fatal("old waited action did not resume after restart-required refusal")
				}
			}
			if err := nha029C5Eventually(f.ctx, func() (bool, error) {
				var nonterminal int
				err := f.observer.QueryRow(f.ctx, `SELECT count(*) FROM public.command_intents WHERE state<>'result'`).Scan(&nonterminal)
				return nonterminal == 0, err
			}); err != nil {
				t.Fatalf("old waited action did not finish its real ledger: %v", err)
			}
			initial.stop(t)
			beforeRestart := f.counts(t)
			if mixed {
				if err := os.WriteFile(f.path, []byte("invalid: [\n"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(f.path); err != nil {
				t.Fatal(err)
			}
			// Selection must work without parsing a present operational workspace.
			startup, err := config.LoadStartupPaths(f.path, config.DefaultBootstrapChoice(), nha029TestEnv)
			if err != nil || startup.Origin() != config.BootstrapOriginDefaultSidecar {
				t.Fatalf("bootstrap locator depended on dirty workspace: %v", err)
			}
			launch, err := startup.LaunchConfig()
			if err != nil {
				t.Fatal(err)
			}
			limits, err := launch.RecorderLimits()
			if err != nil || limits.TotalBytes() != 32<<20 {
				t.Fatalf("changed bootstrap launch was not admitted: %v", err)
			}
			// This child uses the default configured production Run, no IR factory.
			configured := f.start(t, true)
			configured.ready(t, &a)
			bootstrapB, present, err := f.store.ReadActiveBootstrap(f.ctx)
			if err != nil || !present || bootstrapB.Ref().Document() != bootstrapA.Ref().Document() || bootstrapB.Ref().Revision().Number() != bootstrapA.Ref().Revision().Number()+1 || bootstrapB.SHA256() != sha256.Sum256(changed) || bootstrapB.Path() != bootstrapA.Path() {
				t.Fatalf("restart did not advance exactly the bootstrap source: %v", err)
			}
			if active := f.active(t); active.Revision() != a.Revision() || active.ManifestSHA256() != a.ManifestSHA256() || active.CandidateSHA256() != a.CandidateSHA256() {
				t.Fatal("bootstrap restart implicitly activated a dirty operational workspace")
			}
			if f.counts(t) != beforeRestart || len(f.effects) != 2 {
				t.Fatal("bootstrap restart wrote an operational generation/audit or replayed an action")
			}
			configured.stop(t)
			second := f.start(t, true)
			second.ready(t, &a)
			second.stop(t)
			stable, present, err := f.store.ReadActiveBootstrap(f.ctx)
			if err != nil || !present || stable.Ref() != bootstrapB.Ref() || stable.SHA256() != bootstrapB.SHA256() || f.counts(t) != beforeRestart {
				t.Fatalf("unchanged second restart duplicated bootstrap or operational work: %v", err)
			}
			onDisk, err = os.ReadFile(sidecar)
			if err != nil || !bytes.Equal(onDisk, changed) {
				t.Fatalf("restart overwrote the selected sidecar: %v", err)
			}
			clear(onDisk)
			// Reuse the existing trusted execution fixture only to exercise its
			// real Owner's hot port. It does not claim durable Logic1 restoration.
			current := a
			if mixed {
				if err := os.WriteFile(f.path, operationalB, 0600); err != nil {
					t.Fatal(err)
				}
			}
			ownerRun := f.start(t, false)
			ownerRun.ready(t, &a)
			if mixed {
				ownerRun.send(t, nha029C5Message{kind: nha029C5Activate})
				published := ownerRun.await(t, nha029C5Activated)
				if published.count>>32 != 0 || configactivation.Outcome(uint32(published.count)) != configactivation.Published {
					t.Fatal("mixed operational B did not require and accept explicit hot activation")
				}
				current = f.active(t)
				if current.Revision().Number() <= a.Revision().Number() {
					t.Fatal("explicit B did not advance operational revision")
				}
				matched := false
				for _, version := range current.Versions() {
					if version.SHA256() == sha256.Sum256(operationalB) {
						matched = true
					}
				}
				if !matched {
					t.Fatal("explicit B did not persist its exact workspace source")
				}
				written := f.counts(t)
				if written[0] != beforeRestart[0]+1 || written[1] != beforeRestart[1]+1 || written[2] != beforeRestart[2] {
					t.Fatal("explicit B did not add exactly one operational generation/audit")
				}
			}
			currentCounts := f.counts(t)
			changedAgain := append(append([]byte(nil), base...), []byte("recorder:\n  budget_mib: 24\n")...)
			defer clear(changedAgain)
			if err := os.WriteFile(sidecar, changedAgain, 0600); err != nil {
				t.Fatal(err)
			}
			ownerRun.send(t, nha029C5Message{kind: nha029C5Activate})
			refused = ownerRun.await(t, nha029C5Activated)
			if refused.count>>32 != 1 || configactivation.Outcome(uint32(refused.count)) != configactivation.RestartRequired {
				t.Fatal("post-restart bootstrap edit bypassed restart requirement")
			}
			visible = ownerRun.status(t, true)
			if visible.revision != uint64(current.Revision().Number()) || visible.manifest != current.ManifestSHA256() || f.counts(t) != currentCounts || len(f.effects) != 2 {
				t.Fatal("post-restart refusal changed SQL/stamp or replayed work")
			}
			stable, present, err = f.store.ReadActiveBootstrap(f.ctx)
			if err != nil || !present || stable.Ref() != bootstrapB.Ref() {
				t.Fatalf("post-restart hot request changed bootstrap SQL: %v", err)
			}
			ownerRun.stop(t)
		})
	}
}

func TestNHA029C5FreshExplicitBootstrapWithoutOperationalSourceFailsClosed(t *testing.T) {
	f := nha029C5NewFixture(t)
	bootstrap := filepath.Join(f.workspace, "operator-bootstrap.yaml")
	if err := os.WriteFile(bootstrap, f.raw, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	choice, err := config.ExistingBootstrap(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(f.path, choice, nha029TestEnv)
	if err != nil || startup.Origin() != config.BootstrapOriginExplicitSidecar {
		t.Fatalf("fresh explicit launch was not selectable: %v", err)
	}
	observed, listened, ready := false, false, false
	listen := func(context.Context, string, string) (net.Listener, error) {
		listened = true
		return nil, errors.New("unexpected listener before operational admission")
	}
	dependencies := runDependencies{observe: func(context.Context, *sequencer.Sequencer) error {
		observed = true
		return errors.New("unexpected composition without operational source")
	}}
	ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
	defer cancel()
	err = runWithDependencies(ctx, startup, listen, func(net.Addr) { ready = true }, nil, dependencies)
	if err == nil || observed || listened || ready {
		t.Fatalf("missing initial operational source reached serving: observed=%t listened=%t ready=%t err=%v", observed, listened, ready, err)
	}
	if _, present, readErr := f.store.ReadActiveConfig(f.ctx); readErr != nil || present {
		t.Fatalf("bootstrap-only refusal invented operational authority: present=%t err=%v", present, readErr)
	}
	if _, present, readErr := f.store.ReadActiveBootstrap(f.ctx); readErr != nil || present {
		t.Fatalf("bootstrap-only refusal published partial bootstrap SQL: present=%t err=%v", present, readErr)
	}
	if _, err := os.Stat(filepath.Join(f.dataDir, ".env")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing initial operational source created a key: %v", err)
	}
	if raw, readErr := os.ReadFile(bootstrap); readErr != nil || !bytes.Equal(raw, f.raw) {
		t.Fatalf("explicit read-only bootstrap was overwritten: %v", readErr)
	}
	if _, err := os.Stat(f.path + ".bootstrap.yaml"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("explicit selection wrote a default sidecar: %v", err)
	}
}

package server

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/parser"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/helpers"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/view"
	"github.com/jackc/pgx/v5"
)

// These marks deliberately retain no source bytes or Candidate. Restoration
// below must reopen A's authenticated SQL history after the workspace became B.
type nha029C5RestoreMark struct {
	revision    storage.ConfigGenerationRevision
	manifest    [sha256.Size]byte
	candidate   [sha256.Size]byte
	operational storage.ConfigVersionRef
	source      [sha256.Size]byte
	versions    map[storage.ConfigDocumentID]storage.ConfigVersionRef
}

func nha029C5RestoreMarkSQL(t *testing.T, ctx context.Context, store *storage.Store) nha029C5RestoreMark {
	t.Helper()
	snapshot, present, err := store.ReadActiveConfig(ctx)
	if err != nil || !present || snapshot.Availability() != storage.ConfigGenerationComplete || snapshot.ManifestFormat() != storage.ConfigManifestCompiledV2 {
		t.Fatalf("restore SQL authority incomplete: present=%t err=%v", present, err)
	}
	mark := nha029C5RestoreMark{revision: snapshot.Revision(), manifest: snapshot.ManifestSHA256(), candidate: snapshot.CandidateSHA256(), versions: make(map[storage.ConfigDocumentID]storage.ConfigVersionRef)}
	for _, version := range snapshot.Versions() {
		kind, present := version.Kind()
		if !present {
			t.Fatal("restore source missing")
		}
		mark.versions[version.Ref().Document()] = version.Ref()
		if kind == storage.ConfigDocumentKindOperationalYAML {
			if mark.operational.Valid() {
				t.Fatal("multiple operational sources")
			}
			mark.operational, mark.source = version.Ref(), version.SHA256()
		}
	}
	if !mark.operational.Valid() || len(mark.versions) != 2 {
		t.Fatal("restore fixture lacks its two real SQL sources")
	}
	return mark
}

func nha029C5RestoreCompile(t *testing.T, profile parser.SyntaxProfile, entry string, sources []parser.SourceFile) bundle.Candidate {
	t.Helper()
	var candidate bundle.Candidate
	var err error
	switch profile {
	case parser.SyntaxProfileDeclarative:
		candidate, err = bundle.CompileSources(entry, sources)
	case parser.SyntaxProfileDeclarative2:
		candidate, err = bundle.CompileSourcesV2(entry, sources)
	default:
		t.Fatal("unexpected restore profile")
	}
	if err != nil {
		t.Fatal(err)
	}
	return candidate
}

func nha029C5RestoreRequest(t *testing.T, candidate bundle.Candidate, bindings []storage.ConfigDocumentBinding) configactivation.Request {
	t.Helper()
	definitions, err := configactivation.ReplaceDefinitions(candidate, bindings)
	if err != nil {
		t.Fatal(err)
	}
	author, err := storage.NewConfigAuthor("nha029:c5-trusted-restore-test")
	if err != nil {
		t.Fatal(err)
	}
	source, err := storage.NewConfigSource("nha029:c5-historical-sql")
	if err != nil {
		t.Fatal(err)
	}
	request, err := configactivation.NewWorkspaceRequest(definitions, author, source, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func nha029C5RestorePublishFixture(t *testing.T, ctx context.Context, r *nha029OwnerRig, profile parser.SyntaxProfile, label string) helpers.Definition {
	t.Helper()
	r.edit(t, "# C5 ordinary operational "+label+"\n")
	raw := []byte(fmt.Sprintf("group:\n  c5_restore:\n    name: 'C5 %s'\n    entities: [input_boolean.away_mode]\ninput_boolean:\n  away_mode:\n    name: 'C5 %s'\n    initial: false\n", label, label))
	defer clear(raw)
	file, err := parser.NewSourceFile("definitions.yaml", raw)
	if err != nil {
		t.Fatal(err)
	}
	candidate := nha029C5RestoreCompile(t, profile, "definitions.yaml", []parser.SourceFile{file})
	document, err := storage.NewConfigDocumentID("nha029.c5.declarative")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := storage.NewConfigDocumentBinding(document, "definitions.yaml")
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := helpers.Definitions(candidate)
	if err != nil || len(definitions) != 1 {
		t.Fatalf("restore helper definitions: %v", err)
	}
	result, err := r.owner.Activate(ctx, nha029C5RestoreRequest(t, candidate, []storage.ConfigDocumentBinding{binding}))
	if err != nil || result.Outcome() != configactivation.Published || result.AttemptOutcome() != storage.TransactionCommitted {
		t.Fatalf("restore fixture %s publication: outcome=%v transaction=%v err=%v", label, result.Outcome(), result.AttemptOutcome(), err)
	}
	return definitions[0]
}

func nha029C5RestoreFromHistory(t *testing.T, ctx context.Context, store *storage.Store, replay *storage.ConfigReplayRepository, a nha029C5RestoreMark, path string) configactivation.Request {
	t.Helper()
	history, err := store.ReadConfigGeneration(ctx, a.revision)
	if err != nil || history.Availability() != storage.ConfigGenerationComplete || history.ManifestSHA256() != a.manifest || history.CandidateSHA256() != a.candidate {
		t.Fatalf("historical A unavailable or changed: %v", err)
	}
	var sources []parser.SourceFile
	var bindings []storage.ConfigDocumentBinding
	var profile parser.SyntaxProfile
	for _, version := range history.Versions() {
		kind, present := version.Kind()
		if !present {
			t.Fatal("historical A membership incomplete")
		}
		if kind == storage.ConfigDocumentKindOperationalYAML {
			continue
		}
		if kind != storage.ConfigDocumentKindYAMLSource {
			t.Fatal("unexpected historical source kind")
		}
		versionProfile, present := version.Profile()
		if !present || (profile != 0 && profile != versionProfile) {
			t.Fatal("historical profiles disagree")
		}
		profile = versionProfile
		raw, plain := version.Bytes()
		if !plain || sha256.Sum256(raw) != version.SHA256() {
			t.Fatal("historical declarative bytes unavailable")
		}
		file, err := parser.NewSourceFile(version.Path(), raw)
		clear(raw)
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, file)
		binding, err := storage.NewConfigDocumentBinding(version.Ref().Document(), version.Path())
		if err != nil {
			t.Fatal(err)
		}
		bindings = append(bindings, binding)
	}
	candidate := nha029C5RestoreCompile(t, profile, history.Entry(), sources)
	protected, err := replay.ReadHistoricalProtectedSource(ctx, a.revision, a.manifest, a.operational)
	if err != nil || protected.SHA256() != a.source {
		t.Fatalf("protected historical A unavailable: %v", err)
	}
	raw := protected.Bytes()
	defer clear(raw)
	if sha256.Sum256(raw) != a.source {
		t.Fatal("historical operational digest changed")
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return nha029C5RestoreRequest(t, candidate, bindings)
}

func nha029C5RestoreDigest(t *testing.T, ctx context.Context, observer *pgx.Conn, query string, args ...any) [sha256.Size]byte {
	t.Helper()
	var raw []byte
	if err := observer.QueryRow(ctx, query, args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != sha256.Size {
		t.Fatal("invalid SQL observation digest")
	}
	return [sha256.Size]byte(raw)
}

const nha029C5RestoreBusinessDigest = `SELECT sha256(convert_to(COALESCE((SELECT jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text) FROM public.helper_values x),'[]'::jsonb)::text,'UTF8'))`
const nha029C5RestoreHumanDigest = `SELECT sha256(convert_to(jsonb_build_array((SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb) FROM public.auth_accounts x),(SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb) FROM public.auth_sessions x),(SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb) FROM public.authorization_policy_active x))::text,'UTF8'))`
const nha029C5RestoreIntentDigest = `SELECT sha256(convert_to(to_jsonb(x)::text,'UTF8')) FROM public.command_intents x WHERE command_id=$1`

func nha029C5RestoreObserved(t *testing.T, sequence *sequencer.Sequencer, entity domain.EntityID, wanted bool, revision uint64) {
	t.Helper()
	nha042ServerWait(t, "restored native helper value and business revision", func() bool {
		state, present, err := sequence.State(entity)
		if err != nil || !present {
			return false
		}
		value, present := state.CurrentValue()
		if !present {
			return false
		}
		helper, current, present := value.Helper()
		actual, boolean := helper.Boolean()
		return present && boolean && actual == wanted && current == revision
	})
}

// This is the configured production Run path, with only its existing read-only
// sequencer observation seam. No factory supplies a Logic1 image at restart.
func nha029C5RestoreConfiguredRun(t *testing.T, startup config.StartupInputs) (*nha034RunHandle, *sequencer.Sequencer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	run := &nha034RunHandle{cancel: cancel, ready: make(chan struct{}), done: make(chan struct{})}
	observed := make(chan *sequencer.Sequencer, 1)
	dependencies := runDependencies{observe: func(_ context.Context, sequence *sequencer.Sequencer) error { observed <- sequence; return nil }}
	go func() {
		run.err = runWithDependencies(ctx, startup, (&net.ListenConfig{}).Listen, func(net.Addr) { close(run.ready) }, nil, dependencies)
		close(run.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-run.done:
		case <-time.After(12 * time.Second):
			t.Error("restored configured Run did not join")
		}
	})
	run.waitReady(t)
	select {
	case sequence := <-observed:
		return run, sequence
	case <-time.After(5 * time.Second):
		t.Fatal("configured Run observation absent")
		return nil, nil
	}
}

func TestNHA029C5RestoreHistoricalSQLAsNewGeneration(t *testing.T) {
	for _, profile := range []parser.SyntaxProfile{parser.SyntaxProfileDeclarative, parser.SyntaxProfileDeclarative2} {
		t.Run(fmt.Sprintf("profile_%d", profile), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			r := nha029StartOwner(t, nha029OwnerParent, nha029OwnerChild, nil)
			definition := nha029C5RestorePublishFixture(t, ctx, r, profile, "A")
			a := nha029C5RestoreMarkSQL(t, ctx, r.store)
			var entityText string
			if err := r.environment.observer.QueryRow(ctx, `SELECT id FROM public.registry_entities WHERE manifest_alias='input_boolean.away_mode' AND NOT tombstoned`).Scan(&entityText); err != nil {
				t.Fatal(err)
			}
			entity, err := domain.ParseEntityID(entityText)
			if err != nil {
				t.Fatal(err)
			}
			repository, err := helpers.NewRepository(r.store)
			if err != nil {
				t.Fatal(err)
			}
			handle, err := repository.Bind(ctx, entity, definition)
			if err != nil {
				t.Fatal(err)
			}
			initial, err := repository.Restore(ctx, handle, definition)
			if err != nil {
				t.Fatal(err)
			}
			initialRevision, present := initial.Revision().Number()
			if !present {
				t.Fatal("initial helper revision absent")
			}
			nha029C5RestoreObserved(t, r.identity.Sequence(), entity, false, initialRevision)
			changed, err := repository.CompareAndSwap(ctx, handle, definition, initial.Revision(), helpers.Boolean(true))
			if err != nil {
				t.Fatal(err)
			}
			businessRevision, present := changed.Revision().Number()
			if !present || businessRevision <= initialRevision {
				t.Fatal("business CAS did not advance")
			}
			// An old real command finishes under A before either restoration.
			r.waitBoth(t)
			r.pilot.emit(t, "nha042_release")
			nha042ServerWait(t, "A terminal simulated effect", func() bool { return len(r.driver.snapshot()) == 1 })
			oldID := r.driver.snapshot()[0]
			oldText, err := oldID.Value()
			if err != nil {
				t.Fatal(err)
			}
			nha042ServerWait(t, "A command committed terminal", func() bool {
				state, response := nha042ServerIntent(t, r.environment.observer, oldID)
				return state == "result" && response.Status == actions.ActionStatus_ACTION_STATUS_DISPATCHED
			})
			var internal, actorAbsent bool
			if err := r.environment.observer.QueryRow(ctx, `SELECT origin_kind='internal_automation',actor_id IS NULL FROM public.command_intents WHERE command_id=$1`, oldText).Scan(&internal, &actorAbsent); err != nil || !internal || !actorAbsent {
				t.Fatalf("old command internal origin: %v", err)
			}
			intentBefore := nha029C5RestoreDigest(t, ctx, r.environment.observer, nha029C5RestoreIntentDigest, oldText)
			nha029C5RestorePublishFixture(t, ctx, r, profile, "B")
			b := nha029C5RestoreMarkSQL(t, ctx, r.store)
			if b.revision.Number() <= a.revision.Number() || b.source == a.source || b.candidate == a.candidate {
				t.Fatal("B did not replace A content")
			}
			nha029C5RestoreObserved(t, r.identity.Sequence(), entity, true, businessRevision)
			businessBefore := nha029C5RestoreDigest(t, ctx, r.environment.observer, nha029C5RestoreBusinessDigest)
			humansBefore := nha029C5RestoreDigest(t, ctx, r.environment.observer, nha029C5RestoreHumanDigest)
			var attemptsBefore int
			if err := r.environment.observer.QueryRow(ctx, `SELECT count(*) FROM public.config_activation_attempts`).Scan(&attemptsBefore); err != nil {
				t.Fatal(err)
			}
			// No A source/candidate survives as a restore input. The physical file
			// is B and these marks contain only SQL identities and digests.
			clear(r.raw)
			r.raw = nil
			dataDir, err := r.environment.config.DataDir()
			if err != nil {
				t.Fatal(err)
			}
			secrets, err := r.store.BootstrapSecrets(ctx, dataDir)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := storage.NewConfigReplayRepository(r.store, secrets)
			if err != nil {
				t.Fatal(err)
			}
			request := nha029C5RestoreFromHistory(t, ctx, r.store, replay, a, r.path)
			result, err := r.owner.Activate(ctx, request)
			if err != nil || result.Outcome() != configactivation.Published || result.AttemptOutcome() != storage.TransactionCommitted {
				t.Fatalf("historical C publication: %v %v %v", result.Outcome(), result.AttemptOutcome(), err)
			}
			c := nha029C5RestoreMarkSQL(t, ctx, r.store)
			if c.revision != result.Revision() || c.revision.Number() <= b.revision.Number() || c.candidate != a.candidate || c.source != a.source || c.manifest == a.manifest {
				t.Fatal("C did not restore A content with a new authenticated generation")
			}
			for document, previous := range b.versions {
				current, present := c.versions[document]
				if !present || current.Revision().Number() <= previous.Revision().Number() {
					t.Fatal("C reused an old version reference")
				}
			}
			expected, err := storage.ExpectConfigActive(b.revision, b.manifest)
			if err != nil {
				t.Fatal(err)
			}
			attempt, present, err := r.store.ReadActivationAttempt(ctx, result.Attempt())
			if err != nil || !present || attempt.Expected() != expected || attempt.Target() != c.revision || attempt.ManifestSHA256() != c.manifest {
				t.Fatalf("C did not CAS exact B: %v", err)
			}
			protected, err := replay.ReadActiveProtectedSource(ctx, c.revision, c.manifest, c.operational)
			if err != nil || protected.SHA256() != a.source {
				t.Fatalf("C protected source mismatch: %v", err)
			}
			opened := protected.Bytes()
			if sha256.Sum256(opened) != a.source {
				t.Fatal("C opened different operational bytes")
			}
			clear(opened)
			stamp, err := r.owner.CurrentConfig(ctx)
			if err != nil {
				t.Fatal(err)
			}
			revision, manifest, err := stamp.Components()
			if err != nil || revision != uint64(c.revision.Number()) || manifest != c.manifest {
				t.Fatal("Owner disagrees with C SQL authority")
			}
			viewCtx, err := authorization.WithPrincipal(ctx, r.environment.principal)
			if err != nil {
				t.Fatal(err)
			}
			actor, err := r.environment.principal.Actor()
			if err != nil {
				t.Fatal(err)
			}
			page, err := r.views.OpenView(viewCtx, actor, view.All(), 16)
			if err != nil {
				t.Fatal(err)
			}
			_, viewStamp, _, _, err := page.Cursor.Components()
			if err != nil || viewStamp != stamp {
				t.Fatal("coherent view disagrees with C")
			}
			nha029C5RestoreObserved(t, r.identity.Sequence(), entity, true, businessRevision)
			if nha029C5RestoreDigest(t, ctx, r.environment.observer, nha029C5RestoreBusinessDigest) != businessBefore || nha029C5RestoreDigest(t, ctx, r.environment.observer, nha029C5RestoreHumanDigest) != humansBefore {
				t.Fatal("configuration restoration rewrote autonomous state")
			}
			if nha029C5RestoreDigest(t, ctx, r.environment.observer, nha029C5RestoreIntentDigest, oldText) != intentBefore || len(r.driver.snapshot()) != 1 {
				t.Fatal("configuration restoration replayed or rewrote old command")
			}
			settings, err := r.environment.config.PostgresSettings()
			if err != nil {
				t.Fatal(err)
			}
			reader, err := storage.Open(ctx, settings)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(reader.Close)
			r.close(t)
			if err := os.WriteFile(r.path, []byte("not: [valid\n"), 0600); err != nil {
				t.Fatal(err)
			}
			startup, err := config.LoadStartupPaths(r.path, config.DefaultBootstrapChoice(), nha029TestEnv)
			if err != nil {
				t.Fatal(err)
			}
			restarted, sequence := nha029C5RestoreConfiguredRun(t, startup)
			nha029C5RestoreObserved(t, sequence, entity, true, businessRevision)
			restarted.cancel()
			if err := restarted.waitDone(t); err != nil {
				t.Fatal(err)
			}
			active, present, err := reader.ReadActiveConfig(ctx)
			if err != nil || !present || active.Revision() != c.revision || active.ManifestSHA256() != c.manifest {
				t.Fatalf("configured restart changed C: %v", err)
			}
			processFixture := &nha029C5Fixture{ctx: ctx, workspace: filepath.Dir(r.path), path: r.path, dataDir: dataDir, backendPort: int(r.environment.observer.Config().Port), store: reader, observer: r.environment.observer, effects: map[string]int{oldText: 1}}
			child := processFixture.start(t, true)
			ready := child.ready(t, &active)
			if ready.digest != a.source {
				t.Fatal("child did not reopen protected C source")
			}
			child.stop(t)
			if after := nha029C5RestoreMarkSQL(t, ctx, reader); after.revision != c.revision || after.manifest != c.manifest {
				t.Fatal("child restart changed C authority")
			}
			var commands, attempts int
			if err := r.environment.observer.QueryRow(ctx, `SELECT (SELECT count(*) FROM public.command_intents),(SELECT count(*) FROM public.config_activation_attempts)`).Scan(&commands, &attempts); err != nil {
				t.Fatal(err)
			}
			if commands != 1 || attempts != attemptsBefore+1 || len(processFixture.effects) != 1 || processFixture.effects[oldText] != 1 || len(r.driver.snapshot()) != 1 {
				t.Fatalf("restart replayed work: commands=%d attempts=%d", commands, attempts)
			}
			if nha029C5RestoreDigest(t, ctx, r.environment.observer, nha029C5RestoreIntentDigest, oldText) != intentBefore || nha029C5RestoreDigest(t, ctx, r.environment.observer, nha029C5RestoreBusinessDigest) != businessBefore {
				t.Fatal("restart changed completed command or business state")
			}
		})
	}
}

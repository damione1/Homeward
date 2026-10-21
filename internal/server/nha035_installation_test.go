package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/backups"
	backuparchive "github.com/damione1/Homeward/internal/backups/archive"
	"github.com/damione1/Homeward/internal/backups/installation"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/jobs"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
	"github.com/jackc/pgx/v5"
)

func nha035Tools(t *testing.T) storage.BackupPGTools {
	t.Helper()
	bin, e := exec.LookPath("pg_dump")
	if e != nil {
		t.Fatal(e)
	}
	bin, e = filepath.Abs(bin)
	if e != nil {
		t.Fatal(e)
	}
	tools, e := storage.ParseBackupPGTools(filepath.Dir(bin))
	if e != nil {
		t.Fatal(e)
	}
	return tools
}
func nha035Recovery(t *testing.T) (backuparchive.RecoveryPublic, backuparchive.RecoveryPrivate) {
	t.Helper()
	key, e := age.GenerateX25519Identity()
	if e != nil {
		t.Fatal(e)
	}
	private, e := backuparchive.ParseRecoveryPrivate([]byte(key.String()))
	if e != nil {
		t.Fatal(e)
	}
	public, e := private.Public()
	if e != nil {
		t.Fatal(e)
	}
	return public, private
}
func nha035Export(t *testing.T, sealed *backuparchive.Sealed) []byte {
	t.Helper()
	defer sealed.Close()
	var result bytes.Buffer
	if _, e := sealed.WriteTo(&result); e != nil {
		t.Fatal(e)
	}
	return bytes.Clone(result.Bytes())
}
func nha035Bootstrap(port int, data string) []byte {
	return []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\n", data, port))
}
func nha035Settings(t *testing.T, port int) storage.Settings {
	t.Helper()
	settings, e := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, nha029TestEnv)
	if e != nil {
		t.Fatal(e)
	}
	return settings
}

func TestNHA035SnapshotConcurrentActivationUsesOneGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	r := nha029StartOwner(t, nha029OwnerParent, nha029OwnerChild, nil)
	nha029C5RestorePublishFixture(t, ctx, r, parser.SyntaxProfileDeclarative2, "A")
	before := nha029C5RestoreMarkSQL(t, ctx, r.store)
	dataDir, e := r.environment.config.DataDir()
	if e != nil {
		t.Fatal(e)
	}
	secrets, e := r.store.BootstrapSecrets(ctx, dataDir)
	if e != nil {
		t.Fatal(e)
	}
	public, private := nha035Recovery(t)
	service, e := installation.NewService(r.store, secrets, public, filepath.Join(dataDir, "installation-pending"))
	if e != nil {
		t.Fatal(e)
	}
	tools := nha035Tools(t)
	snapshot, e := r.store.BeginBackupSnapshot(ctx, secrets)
	if e != nil {
		t.Fatal(e)
	}
	defer snapshot.Close()
	if snapshot.Generation().Revision() != before.revision || snapshot.Generation().ManifestSHA256() != before.manifest {
		t.Fatal("snapshot did not establish A")
	}
	// A real Owner029 activation commits B while the exporting transaction A
	// stays alive. The later pg_dump imports that exact earlier SQL snapshot.
	activated := make(chan struct{})
	go func() {
		defer close(activated)
		nha029C5RestorePublishFixture(t, ctx, r, parser.SyntaxProfileDeclarative2, "B")
	}()
	select {
	case <-activated:
	case <-ctx.Done():
		t.Fatal("concurrent Owner activation did not finish")
	}
	after := nha029C5RestoreMarkSQL(t, ctx, r.store)
	if after.revision == before.revision {
		t.Fatal("B did not commit")
	}
	afterSnapshot, e := r.store.BeginBackupSnapshot(ctx, secrets)
	if e != nil {
		t.Fatal(e)
	}
	defer afterSnapshot.Close()
	if afterSnapshot.Generation().Revision() != after.revision || afterSnapshot.Generation().ManifestSHA256() != after.manifest {
		t.Fatal("snapshot did not establish B")
	}
	// Establish both SQL cuts before editing the workspace. The watcher may
	// publish this valid edit as C while either archive is exported; neither
	// snapshot may include those later bytes, even after they become active.
	r.edit(t, "# NHA035 DIRTY WORKSPACE MUST NOT BE ARCHIVED\n")
	for _, test := range []struct {
		name   string
		source *storage.BackupSnapshot
		want   nha029C5RestoreMark
	}{{"before", snapshot, before}, {"after", afterSnapshot, after}} {
		t.Run(test.name, func(t *testing.T) {
			id, _ := backups.NewArtifactID()
			sealed, err := service.CreateFromSnapshot(ctx, test.source, tools, id)
			if err != nil {
				t.Fatal(err)
			}
			payload := nha035Export(t, sealed)
			target := testpg.Start(t)
			parent := shortPrivateDataDir(t)
			destination := filepath.Join(parent, "restored")
			bootstrap := nha035Bootstrap(target.Port, destination)
			result, err := installation.Restore(ctx, bytes.NewReader(payload), id, private, parent, "restored", tools, bootstrap, nha029TestEnv)
			if err != nil || result.State() != installation.Restored {
				t.Fatalf("coherent restore state=%v err=%v", result.State(), err)
			}
			raw, err := os.ReadFile(filepath.Join(destination, "yaml/installation.json"))
			if err != nil {
				t.Fatal(err)
			}
			var manifest struct {
				Generation     int64  `json:"generation"`
				ManifestSHA256 string `json:"manifest_sha256"`
			}
			if err = json.Unmarshal(raw, &manifest); err != nil {
				t.Fatal(err)
			}
			if manifest.Generation != test.want.revision.Number() {
				t.Fatalf("wrong snapshot generation: got %d want %d", manifest.Generation, test.want.revision.Number())
			}
			if manifest.ManifestSHA256 != fmt.Sprintf("%x", test.want.manifest) {
				t.Fatal("archive manifest differs from snapshot SQL authority")
			}
			raw, err = os.ReadFile(filepath.Join(destination, "configuration.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(raw, []byte("DIRTY WORKSPACE")) {
				t.Fatal("dirty filesystem replaced SQL sources")
			}
			if sha256.Sum256(raw) != test.want.source {
				t.Fatal("restored YAML differs from snapshot SQL source")
			}
			restored, err := storage.Open(ctx, nha035Settings(t, target.Port))
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			active, present, err := restored.ReadActiveConfig(ctx)
			if err != nil || !present || active.Revision() != result.Generation() {
				t.Fatal("restored DB generation differs from manifest fence")
			}
			if active.ManifestSHA256() != test.want.manifest {
				t.Fatal("DB and YAML generations were spliced")
			}
		})
	}
}

type nha035BackupCounter struct{ calls atomic.Int64 }

func (c *nha035BackupCounter) ExecuteBackup(context.Context, jobs.BackupKey, jobs.IdempotencyKey) (jobs.Outcome, error) {
	c.calls.Add(1)
	return jobs.Completed, nil
}

func TestNHA035RunRestoredArchivePredatingEffectDoesNotReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	r := nha029StartOwner(t, nha029OwnerParent, nha029OwnerChild, nil)
	r.waitBoth(t)
	// The child is suspended before its only command. No command intention for
	// that future effect exists at the snapshot cut.
	var intentsBefore int
	if e := r.environment.observer.QueryRow(ctx, `SELECT count(*) FROM public.command_intents`).Scan(&intentsBefore); e != nil || intentsBefore != 0 {
		t.Fatal("archive did not precede effect intention")
	}
	dataDir, e := r.environment.config.DataDir()
	if e != nil {
		t.Fatal(e)
	}
	secrets, e := r.store.BootstrapSecrets(ctx, dataDir)
	if e != nil {
		t.Fatal(e)
	}
	public, private := nha035Recovery(t)
	service, e := installation.NewService(r.store, secrets, public, filepath.Join(dataDir, "installation-pending"))
	if e != nil {
		t.Fatal(e)
	}
	e = r.store.InTransaction(ctx, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `INSERT INTO public.scheduled_job_definitions(job_id,version,deadline_utc,timezone,expires_at_utc,late_policy,tolerance_us,work_kind,backup_key) VALUES ('old-backup',1,now()-interval '1 second','UTC',now()+interval '1 hour',1,3600000000,2,'fixture'); INSERT INTO public.scheduled_jobs(occurrence_id,job_id,definition_version,idempotency_key,state,updated_at) VALUES ('old-backup','old-backup',1,'old-backup',1,now())`)
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
	r.pilot.mu.Lock()
	_, archivedPhysical, e := r.pilot.binding.IDs()
	r.pilot.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	archivedPhysicalText, _ := archivedPhysical.Value()
	nha042ServerWait(t, "old physical observation committed to Recorder", func() bool {
		var count int
		e := r.store.InTransaction(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM public.state_history WHERE entity_id=$1`, archivedPhysicalText).Scan(&count)
		})
		return e == nil && count > 0
	})
	tools := nha035Tools(t)
	id, _ := backups.NewArtifactID()
	sealed, e := service.Create(ctx, tools, id)
	if e != nil {
		t.Fatal(e)
	}
	payload := nha035Export(t, sealed)
	r.pilot.emit(t, "nha042_release")
	nha042ServerWait(t, "post-archive actual fake effect", func() bool { return len(r.driver.snapshot()) == 1 })
	oldID := r.driver.snapshot()[0]
	nha042ServerWait(t, "post-archive effect terminal", func() bool {
		state, _ := nha042ServerIntent(t, r.environment.observer, oldID)
		return state == "result"
	})
	r.run.cancel()
	select {
	case <-r.run.done:
	case <-ctx.Done():
		t.Fatal("source Run did not join")
	}
	target := testpg.Start(t)
	parent := shortPrivateDataDir(t)
	destination := filepath.Join(parent, "restored")
	bootstrap := nha035Bootstrap(target.Port, destination)
	result, e := installation.Restore(ctx, bytes.NewReader(payload), id, private, parent, "restored", tools, bootstrap, nha029TestEnv)
	if e != nil || result.State() != installation.Restored {
		t.Fatalf("restore failed: %v", e)
	}
	raw, e := os.ReadFile(filepath.Join(destination, "configuration.yaml"))
	if e != nil {
		t.Fatal(e)
	}
	startup, e := config.NewDetachedStartup(raw, "configuration.yaml", bootstrap, nha029TestEnv)
	if e != nil {
		t.Fatal(e)
	}
	counter := &nha035BackupCounter{}
	var restoredStore *storage.Store
	var observed *sequencer.Sequencer
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	factory := func(callback context.Context, store *storage.Store) (RuntimePlan, error) {
		restoredStore = store
		manifest, err := registry.ParseManifest(scope.ManifestV1())
		if err != nil {
			return RuntimePlan{}, err
		}
		identity, err := registry.NewRepository(store, manifest)
		if err != nil {
			return RuntimePlan{}, err
		}
		composition, err := NewDurableJobsComposition(DurableJobsInput{Backup: counter, Clock: jobs.RealClock{}, Timer: jobs.RealTimer{}})
		return RuntimePlan{Identity: identity, DurableJobs: composition}, err
	}
	go func() {
		done <- runWithDependencies(runCtx, startup, (&net.ListenConfig{}).Listen, func(net.Addr) { close(ready) }, factory, runDependencies{observe: func(_ context.Context, sequence *sequencer.Sequencer) error { observed = sequence; return nil }})
	}()
	defer func() {
		runCancel()
		select {
		case e := <-done:
			if e != nil {
				t.Error(e)
			}
		case <-time.After(12 * time.Second):
			t.Error("restored Run did not join")
		}
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("restored Run did not become ready")
	}
	if restoredStore == nil || observed == nil {
		t.Fatal("real restored Run did not compose")
	}
	var archivedHistory int
	if e = restoredStore.InTransaction(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM public.state_history WHERE entity_id=$1`, archivedPhysicalText).Scan(&archivedHistory)
	}); e != nil || archivedHistory == 0 {
		t.Fatal("archive did not retain actual physical Recorder history", e)
	}
	// A direct claim through the same repository proves the old due row is
	// terminal; a missing fence would be claimed by this or the active scheduler.
	repository, e := jobs.NewRepository(restoredStore)
	if e != nil {
		t.Fatal(e)
	}
	occurrence, _ := jobs.ParseOccurrenceID("old-backup")
	record, present, e := repository.Get(ctx, occurrence)
	if e != nil || !present || record.State() != jobs.Skipped {
		t.Fatal("old pending job was not terminal before scheduler")
	}
	if counter.calls.Load() != 0 || len(r.driver.snapshot()) != 1 {
		t.Fatal("restoration replayed archived work")
	}
	// The old physical device is not configured in this offline restored run.
	// Recorder rows and the source pilot's old observation cannot become fresh.
	r.pilot.mu.Lock()
	_, oldEntity, e := r.pilot.binding.IDs()
	r.pilot.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	if state, present, e := observed.State(oldEntity); e != nil {
		t.Fatal(e)
	} else if present {
		if _, current := state.CurrentValue(); current {
			t.Fatal("old physical observation became current after restore")
		}
	}
}

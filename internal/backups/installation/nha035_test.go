package installation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/backups"
	backuparchive "github.com/damione1/Homeward/internal/backups/archive"
	"github.com/damione1/Homeward/internal/backups/local"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/helpers"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/jobs"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

const fixtureLocalYAML = "input_boolean:\n  away_mode: {initial: false}\n  default_dashboard: {initial: true}\n  guests: {}\ncounter:\n  bathroom_heating_used_today: {minimum: 0, step: 1, initial: 0, restore: true}\n"
const fixtureSecret = "NHA035_INDEPENDENT_RECOVERY_FAKE_ONLY"

type fixture struct {
	ctx         context.Context
	cluster     testpg.Cluster
	store       *storage.Store
	secrets     *storage.SecretRepository
	data        string
	service     *Service
	tools       storage.BackupPGTools
	private     backuparchive.RecoveryPrivate
	public      backuparchive.RecoveryPublic
	definitions []helpers.Definition
	ids         map[string]domain.EntityID
	registry    registry.Manifest
	key         storage.SecretHandle
	lastCommit  time.Time
}

func fixtureLookup(name string) (string, bool) {
	return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD"
}
func fixtureBootstrap(port int, data string) []byte {
	return []byte(fmt.Sprintf("listen: '127.0.0.1:0'\ndata_dir: %q\npostgres:\n  host: '127.0.0.1'\n  port: %d\n  database: postgres\n  user: homeward_test\n  password_env: HOMEWARD_TEST_PASSWORD\n  sslmode: disable\n", data, port))
}
func fixtureSettings(t *testing.T, port int) storage.Settings {
	t.Helper()
	s, e := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"}, fixtureLookup)
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func fixtureTools(t *testing.T) storage.BackupPGTools {
	t.Helper()
	tool, e := exec.LookPath("pg_dump")
	if e != nil {
		t.Fatal(e)
	}
	tool, e = filepath.Abs(tool)
	if e != nil {
		t.Fatal(e)
	}
	tools, e := storage.ParseBackupPGTools(filepath.Dir(tool))
	if e != nil {
		t.Fatal(e)
	}
	return tools
}
func fixturePrivate(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "private")
	if e := os.Mkdir(p, 0700); e != nil {
		t.Fatal(e)
	}
	return p
}
func setupFixture(t *testing.T) *fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	t.Cleanup(cancel)
	cluster := testpg.Start(t)
	store, e := storage.Open(ctx, fixtureSettings(t, cluster.Port))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(store.Close)
	if _, e = store.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	data := filepath.Join(t.TempDir(), "source-data")
	secrets, e := store.BootstrapSecrets(ctx, data)
	if e != nil {
		t.Fatal(e)
	}
	source, e := parser.NewSourceFile("helpers.yaml", []byte(fixtureLocalYAML))
	if e != nil {
		t.Fatal(e)
	}
	candidate, e := bundle.CompileSourcesV2("helpers.yaml", []parser.SourceFile{source})
	if e != nil {
		t.Fatal(e)
	}
	document, _ := storage.NewConfigDocumentID("nha035.helpers")
	binding, _ := storage.NewConfigDocumentBinding(document, "helpers.yaml")
	author, _ := storage.NewConfigAuthor("fixture")
	origin, _ := storage.NewConfigSource("nha035")
	metadata, _ := storage.NewConfigVersionMetadata(author, origin)
	outcome, e := store.InConfigTransactionOutcome(ctx, func(tx *storage.ConfigTx) error {
		refs, e := tx.SaveCandidate(candidate, []storage.ConfigDocumentBinding{binding}, metadata)
		if e != nil {
			return e
		}
		generation, e := tx.RegisterGeneration(candidate, refs)
		if e != nil {
			return e
		}
		return tx.ActivateGeneration(storage.ExpectNoConfigActive(), generation)
	})
	if e != nil || outcome != storage.TransactionCommitted {
		t.Fatal("seed real legacy generation", e)
	}
	startup, e := config.NewDetachedStartup(fixtureBootstrap(cluster.Port, data), "configuration.yaml", nil, fixtureLookup)
	if e != nil {
		t.Fatal(e)
	}
	if _, _, e = configactivation.ReconcileStartup(ctx, store, startup); e != nil {
		t.Fatal(e)
	}
	manifestRaw, e := os.ReadFile("../../../scope/manifest.json")
	if e != nil {
		t.Fatal(e)
	}
	manifest, e := registry.ParseManifest(manifestRaw)
	if e != nil {
		t.Fatal(e)
	}
	identities, e := registry.NewRepository(store, manifest)
	if e != nil {
		t.Fatal(e)
	}
	definitions, e := helpers.Definitions(candidate)
	if e != nil {
		t.Fatal(e)
	}
	values, e := helpers.NewRepository(store)
	if e != nil {
		t.Fatal(e)
	}
	ids := make(map[string]domain.EntityID)
	instance, _ := domain.ParseIntegrationInstanceID("homeward-helpers-v1")
	for _, definition := range definitions {
		alias, e := definition.Alias()
		if e != nil {
			t.Fatal(e)
		}
		text, _ := alias.Value()
		key, e := helpers.ConfiguredSourceKey(alias)
		if e != nil {
			t.Fatal(e)
		}
		identity, e := identities.ConfigureBinding(ctx, instance, key, text)
		if e != nil {
			t.Fatal(e)
		}
		ids[text] = identity.Entity
		handle, e := values.Bind(ctx, identity.Entity, definition)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = values.Restore(ctx, handle, definition); e != nil {
			t.Fatal(e)
		}
	}
	var decoded struct {
		Entities []struct {
			ID string `json:"entity_id"`
		} `json:"entities"`
	}
	if e = json.Unmarshal(manifestRaw, &decoded); e != nil {
		t.Fatal(e)
	}
	for _, item := range decoded.Entities {
		if len(ids) == 10 {
			break
		}
		if _, ok := ids[item.ID]; ok {
			continue
		}
		key, _ := integrations.NewSourceKey("fixture", item.ID)
		identity, e := identities.ConfigureBinding(ctx, instance, key, item.ID)
		if e != nil {
			t.Fatal(e)
		}
		ids[item.ID] = identity.Entity
	}
	if len(ids) != 10 {
		t.Fatal("fixture must contain ten real SQL bindings")
	}
	var calendar string
	for _, entity := range ids {
		calendar, _ = entity.Value()
		break
	}
	e = store.InTransaction(ctx, func(tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, `INSERT INTO public.homeward_lists(list_id,origin,name,lifecycle,revision) VALUES ('nha035-list','native','Synthetic grocery',1,3)`); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, `INSERT INTO public.homeward_list_items(list_id,item_uid,origin,summary,completed,position,lifecycle,revision) VALUES ('nha035-list','milk','native','Fixture milk',false,0,1,7)`); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, `INSERT INTO public.local_calendars(entity_id,origin,name,lifecycle,revision,updated_at) VALUES ($1,1,'Synthetic calendar',1,4,now())`, calendar); e != nil {
			return e
		}
		_, e := tx.Exec(ctx, `INSERT INTO public.calendar_events(calendar_id,uid,lifecycle,revision,updated_at,summary,description,location,temporal_kind,start_date,end_date) VALUES ($1,'fixture-event',1,9,now(),'Synthetic event','','',1,DATE '2026-10-21',DATE '2026-10-22')`, calendar)
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
	e = store.InTransaction(ctx, func(tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, `INSERT INTO public.auth_accounts(id,login_name,login_key,display_name,role,status,password_profile,password_salt,password_digest) VALUES ('nha035-human','fixture','fixture','Synthetic',1,1,1,decode(repeat('01',16),'hex'),decode(repeat('02',32),'hex'))`); e != nil {
			return e
		}
		_, e := tx.Exec(ctx, `INSERT INTO public.auth_sessions(id,account_id,account_revision,created_at,expires_at,current_generation) VALUES ('nha035-expired','nha035-human',1,TIMESTAMPTZ '2001-01-01 00:00:00+00',TIMESTAMPTZ '2001-01-02 00:00:00+00',1),('nha035-valid','nha035-human',1,TIMESTAMPTZ '2001-01-01 00:00:00+00',TIMESTAMPTZ '2099-01-01 00:00:00+00',1)`)
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
	secretInstance, _ := domain.ParseIntegrationInstanceID("nha035-fixture")
	secretName, _ := storage.ParseSecretName("credential")
	secretHandle, _ := storage.NewSecretHandle(secretInstance, secretName)
	value, _ := storage.NewSecretValue([]byte(fixtureSecret))
	if e = secrets.RotateCredential(ctx, secretHandle, value); e != nil {
		t.Fatal(e)
	}
	identity, e := age.GenerateX25519Identity()
	if e != nil {
		t.Fatal(e)
	}
	private, e := backuparchive.ParseRecoveryPrivate([]byte(identity.String()))
	if e != nil {
		t.Fatal(e)
	}
	public, e := private.Public()
	if e != nil {
		t.Fatal(e)
	}
	service, e := NewService(store, secrets, public, filepath.Join(data, "archives-pending"))
	if e != nil {
		t.Fatal(e)
	}
	return &fixture{ctx, cluster, store, secrets, data, service, fixtureTools(t), private, public, definitions, ids, manifest, secretHandle, time.Now()}
}
func (f *fixture) archive(t *testing.T) ([]byte, backups.ArtifactID) {
	t.Helper()
	id, _ := backups.NewArtifactID()
	sealed, e := f.service.Create(f.ctx, f.tools, id)
	if e != nil {
		t.Fatal("create coherent installation", e)
	}
	defer sealed.Close()
	instance, _ := backups.ParseInstanceID("local")
	settings, e := backups.NewLocalSettings(instance, true, filepath.Join(t.TempDir(), "local-provider"), backups.DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	provider, e := (local.Factory{}).Open(f.ctx, settings)
	if e != nil {
		t.Fatal(e)
	}
	defer provider.Close()
	result, e := sealed.Publish(f.ctx, provider)
	if e != nil || result.State() != backups.Published {
		t.Fatal("publish154", e)
	}
	reader, e := provider.Open(f.ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := io.ReadAll(reader)
	closeErr := reader.Close()
	if e != nil || closeErr != nil {
		t.Fatal("read verified provider", e, closeErr)
	}
	return raw, id
}
func TestNHA035RestoreAfterDatabaseAndEnvLoss(t *testing.T) {
	f := setupFixture(t)
	// All nonterminal states must be fenced even when the old archive lacks
	// the command intention of an effect that happened after its cut.
	// Use SQL only to arrange exact crash states; actual recovery and scheduler
	// paths are exercised below, and no alternate production ledger is created.
	e := f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
		for i := 1; i <= 3; i++ {
			id := fmt.Sprintf("old-%d", i)
			if _, e := tx.Exec(f.ctx, `INSERT INTO public.scheduled_job_definitions(job_id,version,deadline_utc,timezone,expires_at_utc,late_policy,tolerance_us,work_kind,backup_key) VALUES ($1,1,now()-interval '1 second','UTC',now()+interval '1 hour',2,0,2,'fixture')`, id); e != nil {
				return e
			}
			var lease any
			token := 0
			if i > 1 {
				lease = time.Now().Add(time.Hour)
				token = 1
			}
			if _, e := tx.Exec(f.ctx, `INSERT INTO public.scheduled_jobs(occurrence_id,job_id,definition_version,idempotency_key,state,claim_token,lease_until,updated_at) VALUES ($1,$1,1,$1,$2,$3,$4,now())`, id, i, token, lease); e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	f.lastCommit = time.Now()
	payload, id := f.archive(t)
	// The real post-archive physical-effect journal is exercised by the server
	// Run test; this fixture destroys source data and checks restored business rows.
	if e = f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
		_, e := tx.Exec(f.ctx, `UPDATE public.helper_values SET boolean_value=NOT boolean_value,revision=revision+1 WHERE kind=1`)
		return e
	}); e != nil {
		t.Fatal(e)
	}
	var originalDatabase string
	if e = f.store.InTransaction(f.ctx, func(tx pgx.Tx) error { return tx.QueryRow(f.ctx, `SHOW data_directory`).Scan(&originalDatabase) }); e != nil {
		t.Fatal(e)
	}
	if filepath.Base(originalDatabase) != "data" || !strings.Contains(originalDatabase, "TestNHA035RestoreAfterDatabaseAndEnvLoss") {
		t.Fatal("refuse unsafe fixture cleanup")
	}
	failureAt := time.Now()
	f.store.Close()
	if e = f.cluster.Stop(); e != nil {
		t.Fatal(e)
	}
	if e = os.RemoveAll(originalDatabase); e != nil {
		t.Fatal(e)
	}
	if e = os.RemoveAll(f.data); e != nil {
		t.Fatal(e)
	}
	f.store = nil
	f.secrets = nil
	f.service = nil
	if _, e = os.Stat(filepath.Join(f.data, ".env")); !os.IsNotExist(e) {
		t.Fatal("original key survived loss")
	}
	if _, e = os.Stat(originalDatabase); !os.IsNotExist(e) {
		t.Fatal("original database survived loss")
	}
	target := testpg.Start(t)
	parent := fixturePrivate(t)
	destination := filepath.Join(parent, "recovered")
	bootstrap := fixtureBootstrap(target.Port, destination)
	result, e := Restore(f.ctx, bytes.NewReader(payload), id, f.private, parent, "recovered", f.tools, bootstrap, fixtureLookup)
	if e != nil || result.State() != Restored {
		t.Fatalf("restore state=%d err=%v retained=%s", result.State(), e, result.RetainedStage())
	}
	if !result.Measurement().WithinTargets() {
		t.Fatalf("synthetic RPO/RTO targets not met: %+v", result.Measurement())
	}
	actualRPO := failureAt.Sub(f.lastCommit)
	if actualRPO < 0 || actualRPO > time.Hour {
		t.Fatalf("measured operational loss window %s", actualRPO)
	}
	t.Logf("MEASURED_SYNTHETIC cut=%s intervention=%s completed=%s operational_RPO_observed=%s restoration=%s bytes=%d", result.Measurement().CutUTC.Format(time.RFC3339Nano), result.Measurement().InterventionUTC.Format(time.RFC3339Nano), result.Measurement().CompletedUTC.Format(time.RFC3339Nano), actualRPO, result.Measurement().RestoreDuration, len(payload))
	restored, e := storage.Open(f.ctx, fixtureSettings(t, target.Port))
	if e != nil {
		t.Fatal(e)
	}
	defer restored.Close()
	secrets, e := restored.BootstrapSecretsForStartup(f.ctx, destination, storage.RequireEstablishedSecretStartup())
	if e != nil {
		t.Fatal(e)
	}
	secret, e := secrets.Read(f.ctx, f.key)
	if e != nil {
		t.Fatal(e)
	}
	secretRaw, e := secret.Bytes()
	if e != nil || string(secretRaw) != fixtureSecret {
		t.Fatal("independent key failed to decrypt restored credential")
	}
	clear(secretRaw)
	identities, e := registry.NewRepository(restored, f.registry)
	if e != nil {
		t.Fatal(e)
	}
	_ = identities
	var bindings, lists, events, pending int
	if e = restored.InTransaction(f.ctx, func(tx pgx.Tx) error {
		for query, dst := range map[string]*int{`SELECT count(*) FROM public.registry_source_bindings`: &bindings, `SELECT count(*) FROM public.homeward_list_items WHERE summary='Fixture milk' AND revision=7 AND NOT completed`: &lists, `SELECT count(*) FROM public.calendar_events WHERE uid='fixture-event' AND revision=9`: &events, `SELECT count(*) FROM public.scheduled_jobs WHERE state IN (1,2,3)`: &pending} {
			if e := tx.QueryRow(f.ctx, query).Scan(dst); e != nil {
				return e
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if bindings != 10 || lists != 1 || events != 1 || pending != 0 {
		t.Fatalf("restored operational data bindings=%d lists=%d events=%d pending=%d", bindings, lists, events, pending)
	}
	// Changed YAML initials cannot overwrite restored committed business values.
	changedYAML := strings.ReplaceAll(fixtureLocalYAML, "initial: false", "initial: true")
	changedYAML = strings.Replace(changedYAML, "default_dashboard: {initial: true}", "default_dashboard: {initial: false}", 1)
	changedYAML = strings.Replace(changedYAML, "initial: 0", "initial: 99", 1)
	changedSource, e := parser.NewSourceFile("helpers.yaml", []byte(changedYAML))
	if e != nil {
		t.Fatal(e)
	}
	changedCandidate, e := bundle.CompileSourcesV2("helpers.yaml", []parser.SourceFile{changedSource})
	if e != nil {
		t.Fatal(e)
	}
	f.definitions, e = helpers.Definitions(changedCandidate)
	if e != nil {
		t.Fatal(e)
	}
	var expired, valid, revoked bool
	if e = restored.InTransaction(f.ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(f.ctx, `SELECT (SELECT expires_at=TIMESTAMPTZ '2001-01-02 00:00:00+00' AND expires_at<now() FROM public.auth_sessions WHERE id='nha035-expired'),(SELECT expires_at=TIMESTAMPTZ '2099-01-01 00:00:00+00' AND expires_at>now() FROM public.auth_sessions WHERE id='nha035-valid'),EXISTS(SELECT 1 FROM public.auth_sessions WHERE revoked_at IS NOT NULL)`).Scan(&expired, &valid, &revoked)
	}); e != nil || !expired || !valid || revoked {
		t.Fatal("restoration extended or revoked saved human sessions", e)
	}
	values, e := helpers.NewRepository(restored)
	if e != nil {
		t.Fatal(e)
	}
	for _, definition := range f.definitions {
		alias, _ := definition.Alias()
		name, _ := alias.Value()
		handle, e := values.Bind(f.ctx, f.ids[name], definition)
		if e != nil {
			t.Fatal(e)
		}
		value, e := values.Restore(f.ctx, handle, definition)
		if e != nil {
			t.Fatal(e)
		}
		revision, valid := value.Revision().Number()
		wantRevision := uint64(1)
		if name == "input_boolean.guests" {
			wantRevision = 0
		}
		if !valid || revision != wantRevision {
			t.Fatalf("%s revision changed: %d", name, revision)
		}
		v, present := value.Value()
		switch name {
		case "input_boolean.guests":
			if present {
				t.Fatal("absent acquired value")
			}
		case "input_boolean.away_mode":
			b, ok := v.Boolean()
			if !present || !ok || b {
				t.Fatal("false changed")
			}
		case "input_boolean.default_dashboard":
			b, ok := v.Boolean()
			if !present || !ok || !b {
				t.Fatal("nonzero business value changed")
			}
		case "counter.bathroom_heating_used_today":
			n, ok := v.Counter()
			if !present || !ok || n != 0 {
				t.Fatal("zero counter changed")
			}
		}
	}
	restoredJobs, e := jobs.NewRepository(restored)
	if e != nil {
		t.Fatal(e)
	}
	_, claim, outcome, e := restoredJobs.Claim(f.ctx, time.Now().UTC())
	if e != nil || outcome != storage.TransactionCommitted || claim != jobs.NoWork {
		t.Fatalf("restored old job was executable: claim=%v outcome=%v err=%v", claim, outcome, e)
	}
}

func TestNHA035StrictManifestRejectsUnknownDuplicateAndFuture(t *testing.T) {
	f := setupFixture(t)
	snapshot, e := f.store.BeginBackupSnapshot(f.ctx, f.secrets)
	if e != nil {
		t.Fatal(e)
	}
	defer snapshot.Close()
	m, e := newManifest(snapshot)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = decodeManifest(raw); e != nil {
		t.Fatal(e)
	}
	cases := map[string][]byte{"unknown": append([]byte(`{"unknown":true,`), raw[1:]...), "duplicate": append([]byte(`{"version":1,`), raw[1:]...), "future": bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":2`), 1), "trailing": append(raw, 'x'), "wrong-source": bytes.Replace(raw, []byte(`sources/000000.yaml`), []byte(`../escaped.yaml`), 1)}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, e := decodeManifest(raw); e == nil {
				t.Fatal("untrusted manifest admitted")
			}
		})
	}
	hash := sha256.Sum256([]byte("witness"))
	if _, e = storage.NewBackupRestoreExpectation(storage.ConfigGenerationRevision{}, hash, snapshot.Tables()); e == nil {
		t.Fatal("zero generation admitted")
	}
}

func TestNHA035RestoreRejectsInvalidInputsClosed(t *testing.T) {
	f := setupFixture(t)
	payload, id := f.archive(t)
	other, e := age.GenerateX25519Identity()
	if e != nil {
		t.Fatal(e)
	}
	wrong, e := backuparchive.ParseRecoveryPrivate([]byte(other.String()))
	if e != nil {
		t.Fatal(e)
	}
	target := testpg.Start(t)
	parent := fixturePrivate(t)
	corrupt := bytes.Clone(payload)
	corrupt[len(corrupt)/2] ^= 1
	var typedNil *bytes.Reader
	cases := []struct {
		name   string
		source io.Reader
		key    backuparchive.RecoveryPrivate
		port   int
		cancel bool
	}{
		{"missing-key", bytes.NewReader(payload), backuparchive.RecoveryPrivate{}, target.Port, false},
		{"wrong-key", bytes.NewReader(payload), wrong, target.Port, false},
		{"altered", bytes.NewReader(corrupt), f.private, target.Port, false},
		{"truncated", bytes.NewReader(payload[:len(payload)-1]), f.private, target.Port, false},
		{"typed-nil", typedNil, f.private, target.Port, false},
		{"nonempty-target", bytes.NewReader(payload), f.private, f.cluster.Port, false},
		{"cancelled", bytes.NewReader(payload), f.private, target.Port, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			destination := filepath.Join(parent, tc.name)
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			if tc.cancel {
				cancel()
			}
			result, e := Restore(ctx, tc.source, id, tc.key, parent, tc.name, f.tools, fixtureBootstrap(tc.port, destination), fixtureLookup)
			if e == nil || result.State() != NotPublished {
				t.Fatalf("invalid input published or touched DB state=%d error=%v", result.State(), e)
			}
			if strings.Contains(fmt.Sprintf("%+v %#v", e, e), fixtureSecret) {
				t.Fatal("credential leaked")
			}
			if _, e = os.Lstat(destination); !os.IsNotExist(e) {
				t.Fatal("failed operation published installation")
			}
		})
	}
}

func rearchiveFixture(t *testing.T, f *fixture, payload []byte, id backups.ArtifactID, change func(*manifest), omit int) []byte {
	t.Helper()
	parent := fixturePrivate(t)
	state, e := backuparchive.Recover(f.ctx, bytes.NewReader(payload), id, f.private, parent, "extracted", backuparchive.DefaultLimits())
	if e != nil || state.State() != backuparchive.Restored {
		t.Fatal(e)
	}
	directory := filepath.Join(parent, "extracted")
	raw, e := os.ReadFile(filepath.Join(directory, "yaml", manifestPath))
	if e != nil {
		t.Fatal(e)
	}
	m, e := decodeManifest(raw)
	if e != nil {
		t.Fatal(e)
	}
	if change != nil {
		change(&m)
	}
	raw, e = json.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	first, e := backuparchive.NewYAMLSource(manifestPath, int64(len(raw)), bytes.NewReader(raw))
	if e != nil {
		t.Fatal(e)
	}
	yaml := []backuparchive.YAMLSource{first}
	for index, witness := range m.Sources {
		if index == omit {
			continue
		}
		raw, e := os.ReadFile(filepath.Join(directory, "yaml", witness.Archive))
		if e != nil {
			t.Fatal(e)
		}
		item, e := backuparchive.NewYAMLSource(witness.Archive, int64(len(raw)), bytes.NewReader(raw))
		if e != nil {
			t.Fatal(e)
		}
		yaml = append(yaml, item)
	}
	keyRaw, e := os.ReadFile(filepath.Join(directory, ".env"))
	if e != nil {
		t.Fatal(e)
	}
	key, e := storage.ParseBackupServerKey(keyRaw)
	clear(keyRaw)
	if e != nil {
		t.Fatal(e)
	}
	dump, e := os.Open(filepath.Join(directory, "database.dump"))
	if e != nil {
		t.Fatal(e)
	}
	defer dump.Close()
	info, e := dump.Stat()
	if e != nil {
		t.Fatal(e)
	}
	input, e := backuparchive.NewInput(dump, info.Size(), yaml, key)
	if e != nil {
		t.Fatal(e)
	}
	sealed, e := backuparchive.Seal(f.ctx, fixturePrivate(t), id, f.public, input, backuparchive.DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	defer sealed.Close()
	var ciphertext bytes.Buffer
	if _, e = sealed.WriteTo(&ciphertext); e != nil {
		t.Fatal(e)
	}
	return bytes.Clone(ciphertext.Bytes())
}
func TestNHA035RestoreRejectsMissingRequiredReferences(t *testing.T) {
	f := setupFixture(t)
	payload, id := f.archive(t)
	for _, tc := range []struct {
		name   string
		change func(*manifest)
		omit   int
		state  RestoreState
	}{{"yaml", nil, 0, NotPublished}, {"sql-reference", func(m *manifest) { m.Sources[0].Document = "missing.required.document" }, -1, Indeterminate}} {
		t.Run(tc.name, func(t *testing.T) {
			altered := rearchiveFixture(t, f, payload, id, tc.change, tc.omit)
			target := testpg.Start(t)
			parent := fixturePrivate(t)
			destination := filepath.Join(parent, "restored")
			result, e := Restore(f.ctx, bytes.NewReader(altered), id, f.private, parent, "restored", f.tools, fixtureBootstrap(target.Port, destination), fixtureLookup)
			if e == nil || result.State() != tc.state {
				t.Fatalf("missing reference state=%d err=%v", result.State(), e)
			}
			if _, e = os.Lstat(destination); !os.IsNotExist(e) {
				t.Fatal("missing reference published")
			}
			if tc.state == Indeterminate {
				if info, e := os.Stat(result.RetainedStage()); e != nil || !info.IsDir() {
					t.Fatal("post-import failure lost its private staging evidence")
				}
			}
		})
	}
}
func TestNHA035RestorePostRenameFailuresRemainIndeterminate(t *testing.T) {
	f := setupFixture(t)
	payload, id := f.archive(t)
	for _, phase := range []string{"rename", "parent-sync"} {
		t.Run(phase, func(t *testing.T) {
			target := testpg.Start(t)
			parent := fixturePrivate(t)
			destination := filepath.Join(parent, "restored")
			operations := restoreIO{rename: renameExclusive, sync: syncDirectory}
			if phase == "rename" {
				operations.rename = func(root *os.Root, from, to string) error {
					if e := renameExclusive(root, from, to); e != nil {
						return e
					}
					return io.ErrClosedPipe
				}
			} else {
				operations.sync = func(root *os.Root, name string) error {
					if e := syncDirectory(root, name); e != nil {
						return e
					}
					if info, e := root.Stat("restored"); e == nil && info.IsDir() {
						return io.ErrClosedPipe
					}
					return nil
				}
			}
			result, e := restoreWithIO(f.ctx, bytes.NewReader(payload), id, f.private, parent, "restored", f.tools, fixtureBootstrap(target.Port, destination), fixtureLookup, operations)
			if e == nil || result.State() != Indeterminate || result.RetainedStage() != destination {
				t.Fatalf("post-effect result state=%d retained=%q error=%v", result.State(), result.RetainedStage(), e)
			}
			if _, e = os.Stat(filepath.Join(destination, ".env")); e != nil {
				t.Fatal("published recovery lost key after post-effect error")
			}
		})
	}
}

func TestNHA035ServiceRejectsMismatchedStoreAndSecretsBeforeIO(t *testing.T) {
	a := setupFixture(t)
	b := setupFixture(t)
	parent := fixturePrivate(t)
	wrongDirectory := filepath.Join(parent, "wrong-store")
	service, err := NewService(a.store, b.secrets, a.public, wrongDirectory)
	admitted, typed := err.(*Error)
	if service != nil || !typed || admitted.Kind() != Invalid {
		t.Fatal("mismatched Store and secrets were admitted")
	}
	if _, err := os.Lstat(wrongDirectory); !os.IsNotExist(err) {
		t.Fatal("mismatched Store created a directory or artifact")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatal("rejected constructor performed filesystem writes")
	}
	// This is the direct-snapshot path from Astra F1. A correctly admitted
	// pair must borrow A's opaque snapshot and put A's key in the archive.
	snapshot, err := a.store.BeginBackupSnapshot(a.ctx, a.secrets)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	valid, err := NewService(a.store, a.secrets, a.public, filepath.Join(parent, "valid-store"))
	if err != nil || valid == nil {
		t.Fatal("same Store and secrets were refused", err)
	}
	id, err := backups.NewArtifactID()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := valid.CreateFromSnapshot(a.ctx, snapshot, a.tools, id)
	if err != nil || sealed == nil || sealed.Size() <= 0 {
		t.Fatal("valid Store snapshot did not seal", err)
	}
	defer sealed.Close()
	var ciphertext bytes.Buffer
	if _, err := sealed.WriteTo(&ciphertext); err != nil {
		t.Fatal(err)
	}
	recoveryParent := fixturePrivate(t)
	result, err := backuparchive.Recover(a.ctx, &ciphertext, id, a.private, recoveryParent, "recovered", backuparchive.DefaultLimits())
	if err != nil || result.State() != backuparchive.Restored {
		t.Fatal("valid Store archive did not recover", err)
	}
	recoveredKey, err := os.ReadFile(filepath.Join(recoveryParent, "recovered", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	defer clear(recoveredKey)
	var expectedA, unrelatedB bytes.Buffer
	keyA, err := a.secrets.BackupKey()
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := b.secrets.BackupKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keyA.WriteTo(&expectedA); err != nil {
		t.Fatal(err)
	}
	defer clear(expectedA.Bytes())
	if _, err := keyB.WriteTo(&unrelatedB); err != nil {
		t.Fatal(err)
	}
	defer clear(unrelatedB.Bytes())
	if !bytes.Equal(recoveredKey, expectedA.Bytes()) || bytes.Equal(recoveredKey, unrelatedB.Bytes()) {
		t.Fatal("valid snapshot archived the wrong Store key")
	}
}

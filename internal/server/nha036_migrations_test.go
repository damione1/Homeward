package server

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/helpers"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
)

type nha036Artifact struct {
	Test           string `json:"test"`
	Homed          string `json:"homed"`
	TestSHA256     string `json:"test_sha256"`
	HomedSHA256    string `json:"homed_sha256"`
	SourcesSHA256  string `json:"sources_sha256"`
	SourceManifest string `json:"source_manifest"`
	Schema         uint32 `json:"schema"`
	Release        string `json:"release"`
	PlanSHA256     string `json:"plan_sha256"`
	MinimumReader  string `json:"minimum_reader"`
	MinimumWriter  string `json:"minimum_writer"`
}
type nha036Artifacts struct {
	Format    int                       `json:"format"`
	Race      bool                      `json:"race"`
	Go        string                    `json:"go"`
	Source    string                    `json:"source"`
	Profile   nha036SchemaProfile       `json:"profile"`
	Artifacts map[string]nha036Artifact `json:"artifacts"`
}

type nha036SchemaProfile struct {
	BaselineSchema   uint32 `json:"baseline_schema"`
	SuccessorSchema  uint32 `json:"successor_schema"`
	BaselineRelease  string `json:"baseline_release"`
	SuccessorRelease string `json:"successor_release"`
}

func nha036CompiledProfile() nha036SchemaProfile {
	profile := storage.CurrentSchemaBuild().Profile()
	return nha036SchemaProfile{profile.Baseline().Number(), profile.Successor().Number(), profile.BaselineRelease(), profile.SuccessorRelease()}
}

type nha036BuildIdentity struct {
	Release       string              `json:"release"`
	Schema        uint32              `json:"schema"`
	Plan          string              `json:"plan"`
	Profile       nha036SchemaProfile `json:"profile"`
	MinimumReader string              `json:"minimum_reader"`
	MinimumWriter string              `json:"minimum_writer"`
}

func nha036CurrentIdentity() nha036BuildIdentity {
	build := storage.CurrentSchemaBuild()
	plan := build.PlanSHA256()
	return nha036BuildIdentity{build.Release(), build.Version().Number(), hex.EncodeToString(plan[:]), nha036CompiledProfile(), build.MinimumReader(), build.MinimumWriter()}
}

func nha036FileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
func nha036FileDigest(t *testing.T, path string) string {
	t.Helper()
	digest, err := nha036FileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// These are the exact names excluded by stage.py's copy and inventory walks,
// at every depth and for both files and directories. Other symlinks/special
// files fail closed rather than silently selecting a different source tree.
func nha036ExcludedSourceName(name string) bool {
	switch name {
	case ".git", ".tools", "target", "__pycache__":
		return true
	default:
		return false
	}
}
func nha036SourceInventory(root string) (map[string]string, error) {
	inventory := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkError error) error {
		if walkError != nil {
			return walkError
		}
		if path == root {
			return nil
		}
		if nha036ExcludedSourceName(entry.Name()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("NHA036 unsupported source node: %s", path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		digest, err := nha036FileSHA256(path)
		if err != nil {
			return err
		}
		inventory[filepath.ToSlash(relative)] = digest
		return nil
	})
	return inventory, err
}
func nha036VerifySourceInventory(root string, expected map[string]string) error {
	if len(expected) == 0 {
		return errors.New("NHA036 source inventory absent")
	}
	for relative := range expected {
		local := filepath.FromSlash(relative)
		if !filepath.IsLocal(local) || filepath.ToSlash(local) != relative {
			return errors.New("NHA036 invalid source inventory path")
		}
	}
	actual, err := nha036SourceInventory(root)
	if err != nil {
		return err
	}
	if !maps.Equal(actual, expected) {
		return errors.New("NHA036 complete source path set or hashes changed")
	}
	return nil
}

func nha036LoadArtifacts(t *testing.T) nha036Artifacts {
	t.Helper()
	path := os.Getenv("HOMEWARD_NHA036_ARTIFACTS")
	if !filepath.IsAbs(path) {
		t.Fatal("NHA036 requires frozen artifacts: run qualification/migrations/stage.py under the build slot and export HOMEWARD_NHA036_ARTIFACTS")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest nha036Artifacts
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Format != 1 || manifest.Race != nha036Race || !strings.HasPrefix(manifest.Go, "go version go1.27.2 ") || len(manifest.Artifacts) != 4 {
		t.Fatal("NHA036 artifact format, toolchain, race mode or roles mismatch")
	}
	if !storage.CurrentSchemaBuild().Valid() || manifest.Profile != nha036CompiledProfile() {
		t.Fatal("NHA036 invalid or mismatched compiled profile")
	}
	profile := manifest.Profile
	seen := make(map[string]bool)
	plans := make(map[string]bool)
	for _, role := range []string{"baseline", "additive", "destructive", "unknown-kind"} {
		artifact, present := manifest.Artifacts[role]
		expectedSchema, expectedRelease := profile.SuccessorSchema, profile.SuccessorRelease
		if role == "baseline" {
			expectedSchema, expectedRelease = profile.BaselineSchema, profile.BaselineRelease
		}
		if !present || artifact.Schema != expectedSchema || artifact.Release != expectedRelease || artifact.MinimumReader != profile.BaselineRelease || artifact.MinimumWriter != profile.BaselineRelease || len(artifact.PlanSHA256) != 64 || artifact.PlanSHA256 == strings.Repeat("0", 64) {
			t.Fatalf("NHA036 invalid compiled role %s", role)
		}
		for path, expected := range map[string]string{artifact.Test: artifact.TestSHA256, artifact.Homed: artifact.HomedSHA256, artifact.SourceManifest: artifact.SourcesSHA256} {
			if !filepath.IsAbs(path) || nha036FileDigest(t, path) != expected {
				t.Fatalf("NHA036 artifact changed for %s", role)
			}
		}
		if seen[artifact.TestSHA256] {
			t.Fatal("NHA036 executables are identical")
		}
		seen[artifact.TestSHA256] = true
		if plans[artifact.PlanSHA256] {
			t.Fatal("NHA036 migration plans are identical")
		}
		plans[artifact.PlanSHA256] = true
		sourcesRaw, err := os.ReadFile(artifact.SourceManifest)
		if err != nil {
			t.Fatal(err)
		}
		var sources map[string]string
		if err := json.Unmarshal(sourcesRaw, &sources); err != nil || len(sources) == 0 {
			t.Fatal("NHA036 source inventory absent")
		}
		tree := filepath.Join(filepath.Dir(artifact.SourceManifest), "source")
		if err := nha036VerifySourceInventory(tree, sources); err != nil {
			t.Fatalf("NHA036 frozen source inventory changed: %v", err)
		}
		// Compare the COMPLETE current baseline, including paths absent at
		// stage time. An added implementation or oracle invalidates the pair.
		if role == "baseline" {
			root, err := filepath.Abs(filepath.Join("..", ".."))
			if err != nil {
				t.Fatal(err)
			}
			if err := nha036VerifySourceInventory(root, sources); err != nil {
				t.Fatalf("NHA036 stale baseline source inventory: %v", err)
			}
		}
		t.Logf("NHA036 artifact role=%s schema=%d release=%s sources=%s executable=%s homed=%s plan=%s race=%t", role, artifact.Schema, artifact.Release, artifact.SourcesSHA256, artifact.TestSHA256, artifact.HomedSHA256, artifact.PlanSHA256, manifest.Race)
	}
	current := storage.CurrentSchemaBuild()
	digest := current.PlanSHA256()
	if current.Release() != manifest.Artifacts["baseline"].Release || hex.EncodeToString(digest[:]) != manifest.Artifacts["baseline"].PlanSHA256 {
		t.Fatal("NHA036 current migrator differs from baseline")
	}
	return manifest
}

func nha036Ready(t *testing.T, f *nha029C5Fixture, artifact nha036Artifact, version uint32) *nha029C5Process {
	t.Helper()
	child := nha036Start(t, f, artifact.Test, false)
	ready := child.ready(t, nil)
	var reported nha036BuildIdentity
	expected := nha036BuildIdentity{artifact.Release, artifact.Schema, artifact.PlanSHA256, nha036CompiledProfile(), artifact.MinimumReader, artifact.MinimumWriter}
	if json.Unmarshal([]byte(ready.value), &reported) != nil || reported != expected || ready.count != uint64(version) {
		t.Fatalf("NHA036 child identity/schema mismatch: %s schema=%d", ready.value, ready.count)
	}
	return child
}
func nha036Operate(t *testing.T, child *nha029C5Process, operation string, a *nha029C5RestoreMark) {
	t.Helper()
	message := nha029C5Message{kind: nha029C5ActivateProjection, value: operation}
	if a != nil {
		message.revision, message.manifest = uint64(a.revision.Number()), a.manifest
	}
	child.send(t, message)
	if report := child.await(t, nha029C5Projection); report.value != operation {
		t.Fatal("NHA036 required operation absent")
	}
}

const nha036HistoryDigest = `SELECT sha256(convert_to((SELECT jsonb_agg(to_jsonb(x) ORDER BY version) FROM public.homeward_schema_migrations x WHERE version<=$1)::text,'UTF8'))`
const nha036WindowDigest = `SELECT sha256(convert_to(to_jsonb(x)::text,'UTF8')) FROM public.homeward_schema_window x WHERE singleton`

const nha036RegistryDigest = `SELECT sha256(convert_to(jsonb_build_array(
    (SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM public.registry_devices x),
    (SELECT jsonb_agg(to_jsonb(x) ORDER BY instance_id,source_device) FROM public.registry_device_sources x),
    (SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM public.registry_entities x),
    (SELECT jsonb_agg(to_jsonb(x) ORDER BY instance_id,source_device,source_entity) FROM public.registry_source_bindings x),
    (SELECT jsonb_agg(to_jsonb(x) ORDER BY alias) FROM public.registry_aliases x))::text,'UTF8'))`

func nha036ObserveBusiness(t *testing.T, f *nha029C5Fixture, boolean bool, revision int64, extension bool) {
	t.Helper()
	var actual bool
	var current int64
	var nulls bool
	if err := f.observer.QueryRow(f.ctx, `SELECT v.boolean_value,v.revision,v.number_value IS NULL AND v.counter_value IS NULL AND v.text_value IS NULL FROM public.helper_values v JOIN public.registry_entities e ON e.id=v.entity_id WHERE e.manifest_alias='input_boolean.away_mode'`).Scan(&actual, &current, &nulls); err != nil || actual != boolean || current != revision || !nulls {
		t.Fatalf("NHA036 SQL boolean sentinel drift: value=%t revision=%d err=%v", actual, current, err)
	}
	var zero, empty, absent int
	if err := f.observer.QueryRow(f.ctx, `SELECT
        (SELECT count(*) FROM public.helper_values v JOIN public.registry_entities e ON e.id=v.entity_id WHERE e.manifest_alias='counter.bathroom_heating_used_today' AND v.counter_value=0 AND v.revision=1 AND v.boolean_value IS NULL AND v.text_value IS NULL),
        (SELECT count(*) FROM public.helper_values v JOIN public.registry_entities e ON e.id=v.entity_id WHERE e.manifest_alias='input_text.guest_bedroom_remote_last_event' AND v.text_value='' AND v.revision=1 AND v.counter_value IS NULL AND v.boolean_value IS NULL),
        (SELECT count(*) FROM public.helper_values v JOIN public.registry_entities e ON e.id=v.entity_id WHERE e.manifest_alias='input_boolean.guests')`).Scan(&zero, &empty, &absent); err != nil || zero != 1 || empty != 1 || absent != 0 {
		t.Fatalf("NHA036 zero/empty/absent sentinel drift: %d/%d/%d err=%v", zero, empty, absent, err)
	}
	if extension {
		var note *string
		var nullNote bool
		if err := f.observer.QueryRow(f.ctx, `SELECT nha036_successor_note FROM public.helper_values v JOIN public.registry_entities e ON e.id=v.entity_id WHERE e.manifest_alias='input_boolean.away_mode'`).Scan(&note); err != nil || note == nil || *note != "successor-owned" {
			t.Fatal("NHA036 successor field lost")
		}
		if err := f.observer.QueryRow(f.ctx, `SELECT nha036_successor_note IS NULL FROM public.helper_values v JOIN public.registry_entities e ON e.id=v.entity_id WHERE e.manifest_alias='counter.bathroom_heating_used_today'`).Scan(&nullNote); err != nil || !nullNote {
			t.Fatal("NHA036 successor NULL changed")
		}
	}
}

// The real public homed command is built independently in EACH frozen tree.
// It must pass its own Migrate and configured SQL cold start before ready.
func nha036Homed(t *testing.T, f *nha029C5Fixture, artifact nha036Artifact, admitted bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, artifact.Homed, "-config", f.path)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOMEWARD_TEST_PASSWORD=" + testpg.Password(), "TMPDIR=" + f.workspace}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	diagnostics := &nha029C5ChildLog{}
	command.Stderr = diagnostics
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		diagnostic, total := diagnostics.snapshot()
		exitCode := -1
		if command.ProcessState != nil {
			exitCode = command.ProcessState.ExitCode()
		}
		t.Logf("NHA036 real-homed executable=%s release=%s schema=%d expected_ready=%t exit_code=%d context_expired=%t diagnostics_total=%d diagnostics=%q", artifact.HomedSHA256, artifact.Release, artifact.Schema, admitted, exitCode, ctx.Err() != nil, total, diagnostic)
	}()
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), 4096)
	if !admitted {
		for scanner.Scan() {
			// Continue the parent negative scenario so BOTH exact homed
			// artifacts are attempted, but never turn a real READY into PASS.
			t.Errorf("NHA036 required homed refusal emitted stdout before terminal: %s", scanner.Text())
			return
		}
		readError := scanner.Err()
		err := command.Wait()
		diagnostic, _ := diagnostics.snapshot()
		var exit *exec.ExitError
		var event struct {
			Code      string `json:"code"`
			Level     string `json:"level"`
			Component string `json:"component"`
		}
		decoded := json.Unmarshal([]byte(diagnostic), &event)
		if readError != nil || !errors.As(err, &exit) || exit.ExitCode() != 1 || ctx.Err() != nil || decoded != nil || event.Code != "LOG_CODE_PROCESS_RUNTIME_START_FAILED" || event.Level != "LOG_LEVEL_ERROR" || event.Component != "LOG_COMPONENT_PROCESS" {
			t.Errorf("NHA036 required homed refusal lacks a terminal runtime failure before ready: exit=%v read=%v", err, readError)
			return
		}
		t.Logf("NHA036 homed-refused-before-ready executable=%s release=%s schema=%d exit_code=1", artifact.HomedSHA256, artifact.Release, artifact.Schema)
		return
	}
	if !scanner.Scan() || !strings.Contains(scanner.Text(), "LOG_CODE_PROCESS_READY") {
		diagnostic, _ := diagnostics.snapshot()
		t.Fatalf("NHA036 real homed did not reach ready: %s", diagnostic)
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	for scanner.Scan() {
		if !strings.Contains(scanner.Text(), "LOG_CODE_PROCESS_STOPPED") {
			t.Fatal("NHA036 unexpected process event")
		}
	}
	if err := command.Wait(); err != nil || ctx.Err() != nil {
		t.Fatalf("NHA036 real homed did not stop cleanly: %v", err)
	}
}

func TestNHA036MigrationWindow(t *testing.T) {
	artifacts := nha036LoadArtifacts(t)
	profile := artifacts.Profile
	baseline, next := artifacts.Artifacts["baseline"], artifacts.Artifacts["additive"]
	f := nha029C5NewFixture(t)
	old := nha036Ready(t, f, baseline, profile.BaselineSchema)
	nha036Operate(t, old, "seed", nil)
	a := nha029C5RestoreMarkSQL(t, f.ctx, f.store)
	nha036ObserveBusiness(t, f, false, 1, false)
	identities := nha029C5RestoreDigest(t, f.ctx, f.observer, nha036RegistryDigest)
	historyBefore := nha029C5RestoreDigest(t, f.ctx, f.observer, nha036HistoryDigest, profile.BaselineSchema)
	humans := nha029C5RestoreDigest(t, f.ctx, f.observer, nha029C5RestoreHumanDigest)
	old.fireWaits(t)
	// Parent allows one fake effect only after C5 independently verifies the
	// committed Host/024 internal-automation intention in this same database.
	nha042ServerWait(t, "036 one terminal fake effect", func() bool {
		var terminal int
		if err := f.observer.QueryRow(f.ctx, `SELECT count(*) FROM public.command_intents WHERE state='result'`).Scan(&terminal); err != nil {
			t.Fatal(err)
		}
		return len(f.effects) == 1 && terminal == 1
	})
	var commandID string
	for id := range f.effects {
		commandID = id
	}
	intent := nha029C5RestoreDigest(t, f.ctx, f.observer, nha029C5RestoreIntentDigest, commandID)
	old.stop(t)
	nha036Homed(t, f, baseline, true)
	newer := nha036Ready(t, f, next, profile.SuccessorSchema)
	nha036Operate(t, newer, "successor", nil)
	b := nha029C5RestoreMarkSQL(t, f.ctx, f.store)
	if b.revision.Number() <= a.revision.Number() || b.candidate == a.candidate || b.source == a.source {
		t.Fatal("NHA036 successor did not publish B")
	}
	nha036ObserveBusiness(t, f, true, 2, true)
	declared := nha029C5RestoreDigest(t, f.ctx, f.observer, nha036WindowDigest)
	newer.stop(t)
	nha036Homed(t, f, next, true)
	// This is the SAME exact baseline executable and database after N's DDL.
	returned := nha036Ready(t, f, baseline, profile.SuccessorSchema)
	nha036Operate(t, returned, "restore-a", &a)
	c := nha029C5RestoreMarkSQL(t, f.ctx, f.store)
	if c.revision.Number() <= b.revision.Number() || c.candidate != a.candidate || c.source != a.source || c.manifest == a.manifest {
		t.Fatal("NHA036 C did not restore A as a new generation")
	}
	nha036ObserveBusiness(t, f, false, 3, true)
	returned.stop(t)
	nha036Homed(t, f, baseline, true)
	verifier := nha036Ready(t, f, next, profile.SuccessorSchema)
	nha036Operate(t, verifier, "verify", nil)
	verifier.stop(t)
	nha036Homed(t, f, next, true)
	if nha029C5RestoreDigest(t, f.ctx, f.observer, nha036RegistryDigest) != identities || nha029C5RestoreDigest(t, f.ctx, f.observer, nha029C5RestoreIntentDigest, commandID) != intent || len(f.effects) != 1 || f.effects[commandID] != 1 {
		t.Fatal("NHA036 identity or terminal command changed/replayed")
	}
	var commands, history int
	if err := f.observer.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM public.command_intents),(SELECT count(*) FROM public.homeward_schema_migrations)`).Scan(&commands, &history); err != nil || commands != 1 || history != int(profile.SuccessorSchema) {
		t.Fatal("NHA036 cold start replay or history rewrite")
	}
	nha036ObserveBusiness(t, f, false, 3, true)
	if nha029C5RestoreDigest(t, f.ctx, f.observer, nha036HistoryDigest, profile.BaselineSchema) != historyBefore || nha029C5RestoreDigest(t, f.ctx, f.observer, nha036WindowDigest) != declared || nha029C5RestoreDigest(t, f.ctx, f.observer, nha029C5RestoreHumanDigest) != humans {
		t.Fatal("NHA036 return rewrote history, successor declaration or human state")
	}
	final := nha029C5RestoreMarkSQL(t, f.ctx, f.store)
	if final.revision != c.revision || final.manifest != c.manifest {
		t.Fatal("NHA036 restart changed C")
	}
	nha036Record(t, "additive", next, true, "old-reader-writer-and-Owner-restart")
}

func nha036Record(t *testing.T, role string, artifact nha036Artifact, qualified bool, phase string) {
	t.Helper()
	receipt := struct {
		Role          string              `json:"role"`
		Executable    string              `json:"executable_sha256"`
		Plan          string              `json:"plan_sha256"`
		Qualified     bool                `json:"qualified"`
		Phase         string              `json:"phase"`
		Profile       nha036SchemaProfile `json:"profile"`
		Release       string              `json:"release"`
		Schema        uint32              `json:"schema"`
		MinimumReader string              `json:"minimum_reader"`
		MinimumWriter string              `json:"minimum_writer"`
		Homed         string              `json:"homed_sha256"`
	}{Role: role, Executable: artifact.TestSHA256, Plan: artifact.PlanSHA256, Qualified: qualified, Phase: phase, Profile: nha036CompiledProfile(), Release: artifact.Release, Schema: artifact.Schema, MinimumReader: artifact.MinimumReader, MinimumWriter: artifact.MinimumWriter, Homed: artifact.HomedSHA256}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("NHA036 qualification %s", raw)
	directory := filepath.Join(filepath.Dir(os.Getenv("HOMEWARD_NHA036_ARTIFACTS")), "results")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, fmt.Sprintf("%s-%d.json", role, time.Now().UnixNano())), append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestNHA036BuildIdentityHelper(t *testing.T) {
	if os.Getenv("NHA036_BUILD_IDENTITY") != "1" {
		return
	}
	if !storage.CurrentSchemaBuild().Valid() {
		t.Fatal("NHA036 compiled identity invalid")
	}
	raw, err := json.Marshal(nha036CurrentIdentity())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("NHA036_BUILD %s\n", raw)
}

// A one-shot OLD executable probe, after the candidate has committed real DDL.
// It calls the actual old Store.Migrate, Bind and Restore. Its nonzero exit is
// a failed compatible qualification; a missing executable or readiness timeout
// cannot masquerade as an expected old-reader failure.
func TestNHA036RequiredRepositoryHelper(t *testing.T) {
	path := os.Getenv("NHA036_REPOSITORY_SOURCE")
	if path == "" {
		return
	}
	startup, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(os.Getenv("NHA036_REPOSITORY_PORT"))
	if err != nil || port < 1 || port > 65535 {
		t.Fatal("invalid owned probe port")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	child := &nha029C5Child{ctx: ctx, startup: startup, wire: &nha029C5Wire{writer: io.Discard}}
	child.driver = &nha029C5Driver{observer: nha030ServerObserver(t, port), pending: make(map[string]chan struct{})}
	expectedCode := helpers.ErrorInvalid
	switch os.Getenv("NHA036_REPOSITORY_CASE") {
	case "destructive":
		expectedCode = helpers.Unavailable
	case "unknown-kind":
		expectedCode = helpers.Corrupt
	default:
		t.Fatal("unknown old repository qualification case")
	}
	reachedRepository, ready := false, false
	runError := RunWithStartupFactory(ctx, startup, (&net.ListenConfig{}).Listen, func(net.Addr) { ready = true }, func(ctx context.Context, store *storage.Store) (RuntimePlan, error) {
		plan, err := child.factory(ctx, store)
		if err != nil {
			return RuntimePlan{}, err
		}
		var version int
		if err := child.driver.observer.QueryRow(ctx, `SELECT max(version) FROM public.homeward_schema_migrations`).Scan(&version); err != nil || version != int(storage.CurrentSchemaBuild().Profile().Successor().Number()) {
			return RuntimePlan{}, errors.New("probe did not admit actual future schema")
		}
		definitions, err := helpers.Definitions(nha036Candidate(t, "A"))
		if err != nil {
			return RuntimePlan{}, err
		}
		repository, err := helpers.NewRepository(store)
		if err != nil {
			return RuntimePlan{}, err
		}
		var entityText string
		if err := child.driver.observer.QueryRow(ctx, `SELECT id FROM public.registry_entities WHERE manifest_alias='input_boolean.away_mode'`).Scan(&entityText); err != nil {
			return RuntimePlan{}, err
		}
		entity, err := domain.ParseEntityID(entityText)
		if err != nil {
			return RuntimePlan{}, err
		}
		for _, definition := range definitions {
			alias, err := definition.Alias()
			if err != nil {
				return RuntimePlan{}, err
			}
			name, err := alias.Value()
			if err != nil {
				return RuntimePlan{}, err
			}
			if name != "input_boolean.away_mode" {
				continue
			}
			handle, err := repository.Bind(ctx, entity, definition)
			if err != nil {
				return RuntimePlan{}, err
			}
			if _, err := repository.Restore(ctx, handle, definition); err != nil {
				var classified *helpers.Error
				if !errors.As(err, &classified) || classified.Code() != expectedCode {
					return RuntimePlan{}, errors.New("unexpected old repository failure classification")
				}
				reachedRepository = true
				return RuntimePlan{}, err
			}
			// A successful required operation would allow the candidate to
			// continue; cancellation bounds this deliberately unexpected path.
			cancel()
			return plan, nil
		}
		return RuntimePlan{}, errors.New("required fixture definition missing")
	})
	if reachedRepository && runError != nil && !ready && ctx.Err() == nil {
		fmt.Println("NHA036_REQUIRED_REPOSITORY_REFUSED")
		t.Fatal("NHA036 compatible candidate failed required old-reader operation before ready")
	}
	if runError != nil {
		t.Fatal("NHA036 old runtime failed outside the required operation")
	}
}

func nha036CandidateMigration(t *testing.T, f *nha029C5Fixture, artifact nha036Artifact) {
	t.Helper()
	command := exec.CommandContext(f.ctx, artifact.Test, "-test.run=^TestNHA036MigrationOnlyHelper$", "-test.count=1", "-test.v")
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + f.workspace, "NHA036_MIGRATION_SOURCE=" + f.path}
	diagnostics := &nha029C5ChildLog{}
	command.Stdout, command.Stderr = diagnostics, diagnostics
	err := command.Run()
	output, total := diagnostics.snapshot()
	if err != nil || !strings.Contains(output, "NHA036_CANDIDATE_MIGRATION_COMMITTED") {
		t.Fatalf("NHA036 candidate did not commit its own real Migrate: exit=%v output_total=%d output=%s", err, total, output)
	}
}
func TestNHA036MigrationOnlyHelper(t *testing.T) {
	path := os.Getenv("NHA036_MIGRATION_SOURCE")
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := config.ParseWithEnv(raw, nha029TestEnv)
	clear(raw)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := parsed.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	version, err := store.Migrate(ctx)
	if err != nil || version.Number() != storage.CurrentSchemaBuild().Profile().Successor().Number() {
		t.Fatal("NHA036 candidate migration failed")
	}
	fmt.Println("NHA036_CANDIDATE_MIGRATION_COMMITTED")
}

func TestNHA036IncompatibleCandidatesCannotPromote(t *testing.T) {
	artifacts := nha036LoadArtifacts(t)
	profile := artifacts.Profile
	for _, role := range []string{"destructive", "unknown-kind"} {
		t.Run(role, func(t *testing.T) {
			f := nha029C5NewFixture(t)
			baseline, candidate := artifacts.Artifacts["baseline"], artifacts.Artifacts[role]
			child := nha036Ready(t, f, baseline, profile.BaselineSchema)
			nha036Operate(t, child, "seed", nil)
			child.stop(t)
			nha036ObserveBusiness(t, f, false, 1, false)
			nha036CandidateMigration(t, f, candidate)
			var column bool
			var history int
			if err := f.observer.QueryRow(f.ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='helper_values' AND column_name='boolean_value'),(SELECT count(*) FROM public.homeward_schema_migrations)`).Scan(&column, &history); err != nil || history != int(profile.SuccessorSchema) || column != (role != "destructive") {
				t.Fatal("NHA036 candidate DDL/history not observed")
			}
			if role == "unknown-kind" {
				var kind int16
				var preserved bool
				if err := f.observer.QueryRow(f.ctx, `SELECT kind,boolean_value=false FROM public.helper_values v JOIN public.registry_entities e ON e.id=v.entity_id WHERE e.manifest_alias='input_boolean.away_mode'`).Scan(&kind, &preserved); err != nil || kind != 32767 || !preserved {
					t.Fatal("NHA036 unknown kind fixture did not preserve its physical value")
				}
			}
			// The declaration intentionally claims the baseline may write.
			// Its guard must admit the exact compiled successor before the old operation fails.
			version, err := f.store.Migrate(f.ctx)
			if err != nil || version.Number() != storage.CurrentSchemaBuild().Profile().Successor().Number() {
				t.Fatal("negative candidate refused before required old operation")
			}
			command := exec.CommandContext(f.ctx, baseline.Test, "-test.run=^TestNHA036RequiredRepositoryHelper$", "-test.count=1", "-test.v")
			command.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + f.workspace, "NHA036_REPOSITORY_SOURCE=" + f.path, "NHA036_REPOSITORY_PORT=" + strconv.Itoa(f.backendPort), "NHA036_REPOSITORY_CASE=" + role}
			diagnostics := &nha029C5ChildLog{}
			command.Stdout, command.Stderr = diagnostics, diagnostics
			probeError := command.Run()
			output, total := diagnostics.snapshot()
			var exit *exec.ExitError
			if !errors.As(probeError, &exit) || exit.ExitCode() != 1 || !strings.Contains(output, "NHA036_REQUIRED_REPOSITORY_REFUSED") {
				t.Fatalf("NHA036 candidate did not fail the actual old repository: exit=%v output_total=%d output=%s", probeError, total, output)
			}
			t.Logf("NHA036 negative candidate=%s old_reader_exit=%d phase=required-repository output_total=%d output=%s", role, exit.ExitCode(), total, output)
			// Independently exercise the actual public command of BOTH
			// frozen releases on this same incompatible DB. The typed old
			// repository probe above remains mandatory and discriminating.
			nha036Homed(t, f, baseline, false)
			nha036Homed(t, f, candidate, false)
			// This exact artifact receives no compatible/promotable result.
			nha036Record(t, role, candidate, false, "required-old-repository")
		})
	}
}

func TestNHA036FutureRefusalsBeforeReady(t *testing.T) {
	artifacts := nha036LoadArtifacts(t)
	profile := artifacts.Profile
	for _, name := range []string{"unknown-future", "absent-contract", "unknown-format", "unknown-release", "writer-incompatible", "reader-incompatible", "window-exceeded", "wrong-baseline-plan"} {
		t.Run(name, func(t *testing.T) {
			f := nha029C5NewFixture(t)
			child := nha036Ready(t, f, artifacts.Artifacts["baseline"], profile.BaselineSchema)
			nha036Operate(t, child, "seed", nil)
			child.stop(t)
			if err := f.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
				if _, err := tx.Exec(f.ctx, `INSERT INTO public.homeward_schema_migrations(version) VALUES ($1)`, profile.SuccessorSchema); err != nil {
					return err
				}
				switch name {
				case "unknown-future":
					return nil
				case "absent-contract":
					_, err := tx.Exec(f.ctx, `DELETE FROM public.homeward_schema_window`)
					return err
				}
				version := profile.SuccessorSchema
				if name == "window-exceeded" {
					version++ // a negative probe OUTSIDE the explicitly compiled pair
					if _, err := tx.Exec(f.ctx, `INSERT INTO public.homeward_schema_migrations(version) VALUES ($1)`, version); err != nil {
						return err
					}
				}
				if _, err := tx.Exec(f.ctx, `UPDATE public.homeward_schema_window SET schema_version=$1,release_id=$2,maximum_release=$2,migration_plan=decode(repeat('ab',32),'hex')`, version, profile.SuccessorRelease); err != nil {
					return err
				}
				var err error
				switch name {
				case "unknown-format":
					_, err = tx.Exec(f.ctx, `UPDATE public.homeward_schema_window SET format=2`)
				case "unknown-release":
					_, err = tx.Exec(f.ctx, `UPDATE public.homeward_schema_window SET release_id='!unknown-release',maximum_release='!unknown-release'`)
				case "writer-incompatible":
					_, err = tx.Exec(f.ctx, `UPDATE public.homeward_schema_window SET minimum_writer=$1`, profile.SuccessorRelease)
				case "reader-incompatible":
					_, err = tx.Exec(f.ctx, `UPDATE public.homeward_schema_window SET minimum_reader=$1`, profile.SuccessorRelease)
				case "window-exceeded":
				case "wrong-baseline-plan":
					_, err = tx.Exec(f.ctx, `UPDATE public.homeward_schema_window SET baseline_plan=decode(repeat('cd',32),'hex')`)
				default:
					return errors.New("unknown future refusal fixture")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			stateBefore := nha029C5RestoreDigest(t, f.ctx, f.observer, nha029C5RestoreBusinessDigest)
			configBefore := nha029C5RestoreMarkSQL(t, f.ctx, f.store)
			var classified *storage.Error
			if _, err := f.store.Migrate(f.ctx); !errors.As(err, &classified) || classified.Kind() != storage.FutureSchema {
				t.Fatalf("NHA036 future guard failed: %v", err)
			}
			nha036Homed(t, f, artifacts.Artifacts["baseline"], false)
			configAfter := nha029C5RestoreMarkSQL(t, f.ctx, f.store)
			if configAfter.revision != configBefore.revision || configAfter.manifest != configBefore.manifest || nha029C5RestoreDigest(t, f.ctx, f.observer, nha029C5RestoreBusinessDigest) != stateBefore {
				t.Fatal("NHA036 rejected future changed state before ready")
			}
		})
	}
}

// The manifest is captured before each mutation, exactly as after a stage.
// In particular, adding a new compilable implementation or oracle must reject
// the old pair even though every previously inventoried file is unchanged.
func TestNHA036SourceInventoryFreshness(t *testing.T) {
	for _, test := range []struct {
		name     string
		side     string
		mutation string
	}{
		{"new-current-oracle", "current", "add"},
		{"new-frozen-implementation", "frozen", "add"},
		{"removed-current-source", "current", "remove"},
		{"removed-frozen-source", "frozen", "remove"},
		{"modified-current-source", "current", "modify"},
		{"modified-frozen-source", "frozen", "modify"},
		{"identical-exclusions", "both", "excluded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			current, frozen := t.TempDir(), t.TempDir()
			write := func(root, relative, raw string) {
				path := filepath.Join(root, filepath.FromSlash(relative))
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for _, root := range []string{current, frozen} {
				write(root, "internal/server/source.go", "package server\nfunc baseline() {}\n")
				write(root, "internal/server/oracle_test.go", "package server\n")
			}
			expected, err := nha036SourceInventory(frozen)
			if err != nil {
				t.Fatal(err)
			}
			if err := nha036VerifySourceInventory(current, expected); err != nil {
				t.Fatal(err)
			}
			target := current
			if test.side == "frozen" {
				target = frozen
			}
			switch test.mutation {
			case "add":
				write(target, "internal/server/new_oracle_test.go", "package server\nfunc newOracle() {}\n")
			case "remove":
				if err := os.Remove(filepath.Join(target, "internal/server/source.go")); err != nil {
					t.Fatal(err)
				}
			case "modify":
				write(target, "internal/server/source.go", "package server\nfunc changedImplementation() {}\n")
			case "excluded":
				for _, root := range []string{current, frozen} {
					for _, relative := range []string{".git/index", ".tools/bin/go", "target/artifact", "internal/server/__pycache__/cache.pyc", "internal/target/ignored.go"} {
						write(root, relative, root+relative)
					}
				}
			default:
				t.Fatal("unknown inventory mutation")
			}
			for _, item := range []struct{ side, root string }{{"current", current}, {"frozen", frozen}} {
				err := nha036VerifySourceInventory(item.root, expected)
				rejected := test.mutation != "excluded" && item.side == test.side
				if (err != nil) != rejected {
					t.Fatalf("source set freshness result: side=%s mutation=%s err=%v", item.side, test.mutation, err)
				}
			}
		})
	}
}

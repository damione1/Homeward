package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/helpers"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
)

// Uses C5's owned nonce pipes, bounded logs, real Run, Owner, Host and ledger.
// The executable path is explicit and attested by the parent before launch.
func nha036Start(t *testing.T, f *nha029C5Fixture, executable string, configured bool) *nha029C5Process {
	t.Helper()
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	inputR, inputW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outputR, outputW, err := os.Pipe()
	if err != nil {
		_ = inputR.Close()
		_ = inputW.Close()
		t.Fatal(err)
	}
	p := &nha029C5Process{fixture: f, control: inputW, reports: outputR, messages: make(chan nha029C5Message, 16),
		readerDone: make(chan struct{}), readerStop: make(chan struct{}), done: make(chan struct{}), journalBefore: len(f.effects)}
	p.wire = &nha029C5Wire{writer: inputW, nonce: nonce}
	mode := "ir_fixture"
	if configured {
		mode = "configured_sql"
	}
	p.command = exec.Command(executable, "-test.run=^TestNHA036ProcessHelper$", "-test.count=1", "-test.timeout=120s", "-test.v")
	p.command.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + f.workspace,
		"NHA029_C5_CHILD=" + hex.EncodeToString(nonce[:]), "NHA029_C5_SOURCE=" + f.path,
		"NHA029_C5_BOOTSTRAP=" + f.bootstrap, "NHA029_C5_BACKEND=" + strconv.Itoa(f.backendPort), "NHA029_C5_MODE=" + mode}
	p.command.ExtraFiles = []*os.File{inputR, outputW}
	// The protocol is on owned extra pipes, never stdout. Test output is kept
	// bounded and is not a transport for source/credential data.
	p.diagnostics = &nha029C5ChildLog{}
	p.command.Stdout, p.command.Stderr = p.diagnostics, p.diagnostics
	if err := p.command.Start(); err != nil {
		_ = inputR.Close()
		_ = inputW.Close()
		_ = outputR.Close()
		_ = outputW.Close()
		t.Fatal(err)
	}
	_ = inputR.Close()
	_ = outputW.Close()
	go func() { p.err = p.command.Wait(); close(p.done) }()
	go func() {
		defer close(p.readerDone)
		defer close(p.messages)
		var next uint64
		for {
			message, err := nha029C5Read(outputR, nonce, &next)
			if err != nil {
				p.readErr = err
				return
			}
			select {
			case p.messages <- message:
			case <-p.readerStop:
				return
			}
		}
	}()
	t.Cleanup(func() { p.dispose(t) })
	return p
}
func TestNHA036ProcessHelper(t *testing.T) {
	nonceText := os.Getenv("NHA029_C5_CHILD")
	if nonceText == "" {
		return
	}
	decoded, err := hex.DecodeString(nonceText)
	if err != nil || len(decoded) != 32 {
		t.Fatal("invalid owned child nonce")
	}
	var nonce [32]byte
	copy(nonce[:], decoded)
	if nonce == ([32]byte{}) {
		t.Fatal("zero owned child nonce")
	}
	path := os.Getenv("NHA029_C5_SOURCE")
	bootstrap := os.Getenv("NHA029_C5_BOOTSTRAP")
	mode := os.Getenv("NHA029_C5_MODE")
	port, err := strconv.Atoi(os.Getenv("NHA029_C5_BACKEND"))
	if err != nil || port < 1 || port > 65535 || !filepath.IsAbs(path) || filepath.Base(path) != "configuration.yaml" || (mode != "ir_fixture" && mode != "configured_sql") {
		t.Fatal("invalid owned child configuration")
	}
	if bootstrap != "" && (!filepath.IsAbs(bootstrap) || filepath.Dir(bootstrap) != filepath.Dir(path)) {
		t.Fatal("invalid explicit child bootstrap")
	}
	input := os.NewFile(3, "nha029-c5-control")
	output := os.NewFile(4, "nha029-c5-reports")
	if input == nil || output == nil {
		t.Fatal("owned child pipes absent")
	}
	defer input.Close()
	defer output.Close()
	wire := &nha029C5Wire{writer: output, nonce: nonce}
	choice := config.DefaultBootstrapChoice()
	if bootstrap != "" {
		choice, err = config.ExistingBootstrap(bootstrap)
		if err != nil {
			t.Fatal(err)
		}
	}
	startup, err := config.LoadStartupPaths(path, choice, nha029TestEnv)
	if err != nil {
		_ = wire.send(nha029C5Message{kind: nha029C5Failure, value: "startup_source"})
		t.Fatal("owned child source refused")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	child := &nha029C5Child{ctx: ctx, startup: startup, wire: wire}
	child.driver = &nha029C5Driver{wire: wire, observer: nha030ServerObserver(t, port), pending: make(map[string]chan struct{})}
	defer child.driver.releaseAll()
	done := make(chan error, 1)
	ready := make(chan struct{})
	if mode == "configured_sql" {
		// This is the default configured Run path with its existing read-only
		// consumer seam; no factory/IR chooses a replacement program at restart.
		dependencies := runDependencies{observe: func(observeCtx context.Context, sequence *sequencer.Sequencer) error {
			child.sequence = sequence
			var revision int64
			var manifest, source []byte
			err := child.driver.observer.QueryRow(observeCtx, `SELECT g.revision,g.manifest_hash,v.source_hash FROM public.config_active a JOIN public.config_generations g ON g.revision=a.generation_revision JOIN public.config_generation_entries e ON e.generation_revision=g.revision JOIN public.config_documents d ON d.id=e.document_id JOIN public.config_versions v ON v.document_id=e.document_id AND v.revision=e.version_revision WHERE a.singleton AND d.kind='operational_yaml'`).Scan(&revision, &manifest, &source)
			if err != nil || len(manifest) != 32 || len(source) != 32 {
				return errors.New("C5 configured Run had no admitted SQL source")
			}
			message := nha029C5Message{kind: nha029C5Admitted, revision: uint64(revision)}
			copy(message.manifest[:], manifest)
			copy(message.digest[:], source)
			return wire.send(message)
		}}
		go func() {
			done <- runWithDependencies(ctx, startup, (&net.ListenConfig{}).Listen, func(net.Addr) { close(ready) }, nil, dependencies)
		}()
	} else {
		go func() {
			done <- RunWithStartupFactory(ctx, startup, (&net.ListenConfig{}).Listen, func(net.Addr) { close(ready) }, child.factory)
		}()
	}
	select {
	case <-ready:
	case err := <-done:
		_ = wire.send(nha029C5Message{kind: nha029C5Failure, value: "Run_before_ready"})
		t.Fatalf("owned Run refused: %v", err)
	case <-ctx.Done():
		t.Fatal("owned Run readiness deadline")
	}
	var readyMessage nha029C5Message
	if mode == "ir_fixture" {
		select {
		case <-child.pilot.ready:
		case <-ctx.Done():
			t.Fatal("owned fake pilot did not start")
		}
		readyMessage = nha029C5Message{kind: nha029C5Ready, revision: uint64(child.admitted.Revision().Number()), manifest: child.admitted.ManifestSHA256(), digest: child.admitted.SourceSHA256()}
	} else {
		var revision int64
		var manifest, source []byte
		if err := child.driver.observer.QueryRow(ctx, `SELECT g.revision,g.manifest_hash,v.source_hash FROM public.config_active a JOIN public.config_generations g ON g.revision=a.generation_revision JOIN public.config_generation_entries e ON e.generation_revision=g.revision JOIN public.config_documents d ON d.id=e.document_id JOIN public.config_versions v ON v.document_id=e.document_id AND v.revision=e.version_revision WHERE a.singleton AND d.kind='operational_yaml'`).Scan(&revision, &manifest, &source); err != nil {
			t.Fatal(err)
		}
		readyMessage = nha029C5Message{kind: nha029C5Ready, revision: uint64(revision)}
		copy(readyMessage.manifest[:], manifest)
		copy(readyMessage.digest[:], source)
	}
	if !storage.CurrentSchemaBuild().Valid() {
		t.Fatal("NHA036 compiled child identity invalid")
	}
	identity, err := json.Marshal(nha036CurrentIdentity())
	if err != nil {
		t.Fatal(err)
	}
	readyMessage.value = string(identity)
	var admittedVersion int64
	if err := child.driver.observer.QueryRow(ctx, `SELECT max(version) FROM public.homeward_schema_migrations`).Scan(&admittedVersion); err != nil {
		t.Fatal(err)
	}
	readyMessage.count = uint64(admittedVersion)
	if err := wire.send(readyMessage); err != nil {
		t.Fatal("owned ready report failed")
	}
	var next uint64
	for {
		message, err := nha029C5Read(input, nonce, &next)
		if err != nil {
			cancel()
			t.Fatal("owned control channel ended")
		}
		if message.kind == nha029C5Stop {
			break
		}
		if message.kind == nha029C5ActivateProjection {
			nha036ChildOperation(t, child, path, message)
			continue
		}
		if err := child.command(message); err != nil {
			_ = wire.send(nha029C5Message{kind: nha029C5Failure, value: "control_phase"})
			cancel()
			t.Fatal("owned child control refused")
		}
	}
	child.driver.releaseAll()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("owned Run close failed: %v", err)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("owned Run did not join")
	}
	child.tasks.Wait()
	if child.owner != nil {
		select {
		case <-child.owner.Done():
		default:
			t.Fatal("owned Run ended before Owner.Done")
		}
		nha034WaitRunStoreClosed(t, child.store)
	}
	if err := wire.send(nha029C5Message{kind: nha029C5Stopped}); err != nil {
		t.Fatal("owned final report failed")
	}
}

const nha036Definitions = `input_boolean:
  away_mode: {name: 'NHA036 %s', initial: false}
  guests: {}
counter:
  bathroom_heating_used_today: {initial: 0, minimum: 0, maximum: 100, step: 1}
input_text:
  guest_bedroom_remote_last_event: {initial: '', max: 100}
`

func nha036Candidate(t *testing.T, label string) bundle.Candidate {
	t.Helper()
	file, err := parser.NewSourceFile("definitions.yaml", []byte(fmt.Sprintf(nha036Definitions, label)))
	if err != nil {
		t.Fatal(err)
	}
	return nha029C5RestoreCompile(t, parser.SyntaxProfileDeclarative2, "definitions.yaml", []parser.SourceFile{file})
}

func nha036Publish(t *testing.T, c *nha029C5Child, path, label string) {
	t.Helper()
	source, err := c.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	raw := source.Bytes()
	defer clear(raw)
	if err := os.WriteFile(path, append(raw, []byte("# NHA036 publication "+label+"\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	document, err := storage.NewConfigDocumentID("nha029.c5.declarative")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := storage.NewConfigDocumentBinding(document, "definitions.yaml")
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.owner.Activate(c.ctx, nha029C5RestoreRequest(t, nha036Candidate(t, label), []storage.ConfigDocumentBinding{binding}))
	if err != nil || result.Outcome() != configactivation.Published || result.AttemptOutcome() != storage.TransactionCommitted {
		t.Fatalf("NHA036 real Owner publication %s refused: %v", label, err)
	}
}

// Every phase reads the same real repository and checks values, including
// present false, present zero, present empty text, and an absent SQL row.
func nha036Business(t *testing.T, c *nha029C5Child, stage string) {
	t.Helper()
	definitions, err := helpers.Definitions(nha036Candidate(t, "A"))
	if err != nil || len(definitions) != 4 {
		t.Fatalf("NHA036 definitions: %v count=%d", err, len(definitions))
	}
	repository, err := helpers.NewRepository(c.store)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := domain.NewHelperText("")
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range definitions {
		alias, err := definition.Alias()
		if err != nil {
			t.Fatal(err)
		}
		name, err := alias.Value()
		if err != nil {
			t.Fatal(err)
		}
		var entityText string
		if err := c.driver.observer.QueryRow(c.ctx, `SELECT id FROM public.registry_entities WHERE manifest_alias=$1 AND NOT tombstoned`, name).Scan(&entityText); err != nil {
			t.Fatal(err)
		}
		entity, err := domain.ParseEntityID(entityText)
		if err != nil {
			t.Fatal(err)
		}
		handle, err := repository.Bind(c.ctx, entity, definition)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := repository.Restore(c.ctx, handle, definition)
		if err != nil {
			t.Fatalf("NHA036 required old repository read failed: %v", err)
		}
		value, present := snapshot.Value()
		revision, valid := snapshot.Revision().Number()
		if !valid {
			t.Fatal("invalid helper revision")
		}
		switch name {
		case "input_boolean.guests":
			if present || revision != 0 {
				t.Fatal("absent helper became present")
			}
		case "counter.bathroom_heating_used_today":
			if !present || value != helpers.Counter(0) || revision != 1 {
				t.Fatal("present zero lost")
			}
		case "input_text.guest_bedroom_remote_last_event":
			if !present || value != empty || revision != 1 {
				t.Fatal("present empty text lost")
			}
		case "input_boolean.away_mode":
			wanted, wantedRevision := false, uint64(1)
			switch stage {
			case "seed":
			case "successor":
			case "rollback":
				wanted, wantedRevision = true, 2
			case "verify":
				wantedRevision = 3
			default:
				t.Fatal("unknown business phase")
			}
			if !present || value != helpers.Boolean(wanted) || revision != wantedRevision {
				t.Fatalf("boolean/revision lost: stage=%s revision=%d wanted=%d", stage, revision, wantedRevision)
			}
			if stage == "successor" || stage == "rollback" {
				committed, err := repository.CompareAndSwap(c.ctx, handle, definition, snapshot.Revision(), helpers.Boolean(!wanted))
				if err != nil {
					t.Fatalf("NHA036 required old repository CAS failed: %v", err)
				}
				next, valid := committed.Revision().Number()
				if !valid || next != revision+1 {
					t.Fatal("business revision did not advance")
				}
			}
		default:
			t.Fatal("unknown fixture helper")
		}
	}
}

func nha036HistoricalMark(t *testing.T, c *nha029C5Child, message nha029C5Message) nha029C5RestoreMark {
	t.Helper()
	revision, err := storage.NewConfigGenerationRevision(int64(message.revision))
	if err != nil {
		t.Fatal(err)
	}
	history, err := c.store.ReadConfigGeneration(c.ctx, revision)
	if err != nil || history.Availability() != storage.ConfigGenerationComplete || history.ManifestSHA256() != message.manifest {
		t.Fatal("NHA036 historical A unavailable")
	}
	mark := nha029C5RestoreMark{revision: revision, manifest: history.ManifestSHA256(), candidate: history.CandidateSHA256(), versions: make(map[storage.ConfigDocumentID]storage.ConfigVersionRef)}
	for _, version := range history.Versions() {
		mark.versions[version.Ref().Document()] = version.Ref()
		if kind, present := version.Kind(); present && kind == storage.ConfigDocumentKindOperationalYAML {
			mark.operational, mark.source = version.Ref(), version.SHA256()
		}
	}
	if !mark.operational.Valid() || len(mark.versions) != 2 {
		t.Fatal("NHA036 incomplete historical A")
	}
	return mark
}

func nha036ChildOperation(t *testing.T, c *nha029C5Child, path string, message nha029C5Message) {
	t.Helper()
	if c.owner == nil || c.store == nil {
		t.Fatal("NHA036 missing real runtime")
	}
	switch message.value {
	case "seed":
		nha036Publish(t, c, path, "A")
		nha036Business(t, c, "seed")
	case "successor":
		nha036Business(t, c, "successor")
		// This extension exists only in the successor's frozen qualification
		// sources. Baseline repository writes never mention this column.
		if err := c.store.InTransaction(c.ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(c.ctx, `UPDATE public.helper_values SET nha036_successor_note='successor-owned' WHERE entity_id=(SELECT id FROM public.registry_entities WHERE manifest_alias='input_boolean.away_mode')`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		nha036Publish(t, c, path, "B")
	case "restore-a":
		nha036Business(t, c, "rollback")
		mark := nha036HistoricalMark(t, c, message)
		// Launch-only storage paths are absent from OperationalConfig. Use
		// the secret authority already admitted by the real startup path.
		secrets, err := c.admitted.Secrets()
		if err != nil {
			t.Fatal(err)
		}
		replay, err := storage.NewConfigReplayRepository(c.store, secrets)
		if err != nil {
			t.Fatal(err)
		}
		before := nha029C5RestoreMarkSQL(t, c.ctx, c.store)
		result, err := c.owner.Activate(c.ctx, nha029C5RestoreFromHistory(t, c.ctx, c.store, replay, mark, path))
		if err != nil || result.Outcome() != configactivation.Published || result.AttemptOutcome() != storage.TransactionCommitted {
			t.Fatalf("NHA036 real Owner restore: %v", err)
		}
		after := nha029C5RestoreMarkSQL(t, c.ctx, c.store)
		if after.revision != result.Revision() || after.revision.Number() <= before.revision.Number() || after.source != mark.source || after.candidate != mark.candidate || after.manifest == mark.manifest {
			t.Fatal("NHA036 restored content without new generation")
		}
		for document, previous := range before.versions {
			current, present := after.versions[document]
			if !present || current.Revision().Number() <= previous.Revision().Number() {
				t.Fatal("NHA036 document revision rewound")
			}
		}
		expected, err := storage.ExpectConfigActive(before.revision, before.manifest)
		if err != nil {
			t.Fatal(err)
		}
		attempt, present, err := c.store.ReadActivationAttempt(c.ctx, result.Attempt())
		if err != nil || !present || attempt.Expected() != expected || attempt.Target() != after.revision || attempt.ManifestSHA256() != after.manifest {
			t.Fatal("NHA036 restore bypassed exact Owner CAS")
		}
	case "verify":
		nha036Business(t, c, "verify")
		var note *string
		if err := c.driver.observer.QueryRow(c.ctx, `SELECT nha036_successor_note FROM public.helper_values WHERE entity_id=(SELECT id FROM public.registry_entities WHERE manifest_alias='input_boolean.away_mode')`).Scan(&note); err != nil || note == nil || *note != "successor-owned" {
			t.Fatal("baseline write lost successor extension")
		}
	default:
		t.Fatal("unknown NHA036 fixture operation")
	}
	if err := c.wire.send(nha029C5Message{kind: nha029C5Projection, value: message.value}); err != nil {
		t.Fatal(err)
	}
}

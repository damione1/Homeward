package server

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

type nha166ProcessEvidence struct {
	Epoch         string `json:"epoch"`
	Guard         bool   `json:"guard"`
	Armed         bool   `json:"armed"`
	DriverEntries uint64 `json:"driver_entries"`
}

func TestNHA166ServerFatalProcessHelper(t *testing.T) {
	path := os.Getenv("HOMEWARD_NHA166_DOUBLE_CONFIG")
	if path == "" {
		return
	}
	startup, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029TestEnv)
	if err != nil {
		fmt.Fprintln(os.Stdout, "NHA166_FAILURE "+nha166ChildFailure(err))
		t.Fatal("double startup source rejected")
	}
	listener := &nha055DormantListener{closed: make(chan struct{})}
	var actual *observationRuntime
	err = runWithDependencies(context.Background(), startup.WithMQTTReadOnlyObservation(), func(context.Context, string, string) (net.Listener, error) { return listener, nil }, func(net.Addr) {
		if actual == nil {
			t.Fatal("no double runtime")
		}
		epoch, _ := actual.epoch.Wire()
		raw, _ := json.Marshal(nha166ProcessEvidence{hex.EncodeToString(epoch.Value), actual.host.MQTTReadOnlyObservation(), actual.runs != nil, actual.host.CommandDriverEntries()})
		fmt.Fprintln(os.Stdout, "NHA166_READY "+string(raw))
	}, nil, runDependencies{observeRuntime: func(runtime *observationRuntime) { actual = runtime }})
	if err != nil {
		fmt.Fprintln(os.Stdout, "NHA166_FAILURE "+nha166ChildFailure(err))
		t.Fatal("double server returned unexpectedly")
	}
}
func nha166Process(t *testing.T, path string) (*exec.Cmd, nha166ProcessEvidence) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestNHA166ServerFatalProcessHelper$", "-test.count=1", "-test.timeout=60s")
	command.Env = append(os.Environ(), "HOMEWARD_NHA166_DOUBLE_CONFIG="+path)
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// Keep child diagnostics private and bounded by this synthetic test. They
	// are not used as evidence of a successful server start.
	diagnostics := &nha166ChildDiagnostics{}
	command.Stderr = diagnostics
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	type childMessage struct {
		Evidence nha166ProcessEvidence
		Failure  string
	}
	result := make(chan childMessage, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 4096), 64<<10)
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "NHA166_READY ") {
				var evidence nha166ProcessEvidence
				if json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "NHA166_READY ")), &evidence) == nil {
					result <- childMessage{Evidence: evidence}
				}
				return
			}
			if strings.HasPrefix(scanner.Text(), "NHA166_FAILURE ") {
				failure := strings.TrimPrefix(scanner.Text(), "NHA166_FAILURE ")
				if len(failure) > 64 || strings.ContainsAny(failure, " \t\r\n") {
					failure = "invalid_failure_marker"
				}
				result <- childMessage{Failure: failure}
				return
			}
		}
		failure := "stdout_eof"
		if scanner.Err() != nil {
			failure = "stdout_scan_failed"
		}
		result <- childMessage{Failure: failure}
	}()
	select {
	case message := <-result:
		if message.Failure != "" {
			_ = command.Process.Kill()
			waitErr := command.Wait()
			t.Fatalf("fatal/restart child failed before ready: class=%s exit=%v stderr=%s", message.Failure, waitErr, diagnostics.summary())
		}
		return command, message.Evidence
	case <-time.After(20 * time.Second):
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("fatal/restart double did not become ready: stderr=%s", diagnostics.summary())
		return nil, nha166ProcessEvidence{}
	}
}
func TestNHA166FatalRestartKeepsGuardAndRecoversWithoutActionReplay(t *testing.T) {
	broker := testbroker.Start(t)
	cluster := testpg.Start(t)
	cfg := nha166Config(t, cluster.Port, broker.Address)
	source, err := cfg.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "configuration.yaml")
	if err := os.WriteFile(path, source.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	physical, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029TestEnv)
	if err != nil {
		t.Fatal("physical crash fixture startup source", err)
	}
	cfg.startup = physical
	nha166Provision(t, cfg)
	// Detached provisioning cannot establish this physical restart locator.
	bootstrapPath, present := physical.BootstrapPath()
	if !present {
		t.Fatal("physical crash fixture omitted bootstrap locator")
	}
	if info, err := os.Stat(bootstrapPath); err != nil || !info.Mode().IsRegular() {
		t.Fatal("physical crash fixture did not publish its restart sidecar")
	}
	if err := physical.ConfirmBootstrapSource(); err != nil {
		t.Fatal("published crash fixture sidecar differs from initial startup")
	}
	settings, err := cfg.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	id, _ := domain.ParseCommandID("nha166-crash-dispatching-double")
	actor, _ := domain.ParseActorID("nha166-double")
	_, created, outcome, err := store.AcceptCommandIntent(context.Background(), id, actor, "nha166-old-process", []byte("nha166-double-intent"))
	if err != nil || !created || outcome != storage.TransactionCommitted {
		t.Fatal("could not seed durable crash double", err)
	}
	moved, outcome, err := store.MoveCommandIntent(context.Background(), id, storage.IntentAccepted, storage.IntentDispatching, nil, storage.IntentNoError)
	if err != nil || !moved || outcome != storage.TransactionCommitted {
		t.Fatal("could not seed dispatched crash double", err)
	}
	previous := ""
	for incarnation := 0; incarnation < 2; incarnation++ {
		process, evidence := nha166Process(t, path)
		if evidence.Epoch == "" || evidence.Epoch == previous || !evidence.Guard || evidence.Armed || evidence.DriverEntries != 0 {
			_ = process.Process.Kill()
			_ = process.Wait()
			t.Fatal("fatal recovery lost guard, replayed or reused process epoch", evidence)
		}
		previous = evidence.Epoch
		if err := process.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		if err := process.Wait(); err == nil {
			t.Fatal("double was not a real fatal process termination")
		}
	}
	record, found, err := store.CommandIntent(context.Background(), id)
	if err != nil || !found || record.State() != storage.IntentIndeterminate {
		t.Fatal("dispatched intent did not recover conservatively", err)
	}
}

// Only finite error classifications cross the child protocol. Provider,
// credential and filesystem error text never enters the parent test report.
func nha166ChildFailure(err error) string {
	switch {
	case errors.Is(err, config.ErrStartupSourceChanged):
		return "bootstrap_source_changed_or_missing"
	case errors.Is(err, config.ErrStartupSource):
		return "startup_source_unavailable"
	case errors.Is(err, configactivation.ErrStartupIntegrity):
		return "startup_integrity"
	case errors.Is(err, configactivation.ErrStartupConflict):
		return "startup_conflict"
	}
	var stored *storage.Error
	if errors.As(err, &stored) {
		return fmt.Sprintf("storage_kind_%d", stored.Kind())
	}
	return "runtime_startup_failed"
}

type nha166ChildDiagnostics struct {
	mu       sync.Mutex
	retained []byte
	total    int
}

func (d *nha166ChildDiagnostics) Write(data []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.total += len(data)
	remaining := 8192 - len(d.retained)
	if remaining > len(data) {
		remaining = len(data)
	}
	if remaining > 0 {
		d.retained = append(d.retained, data[:remaining]...)
	}
	return len(data), nil
}
func (d *nha166ChildDiagnostics) summary() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return fmt.Sprintf("bytes=%d retained=%d truncated=%t data_race=%t", d.total, len(d.retained), d.total > len(d.retained), strings.Contains(string(d.retained), "WARNING: DATA RACE"))
}

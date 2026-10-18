package worker_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/internal/worker"
	"github.com/damione1/Homeward/sdk/conformance"
)

type casetaFixtureInfo struct {
	Settings   string `json:"settings"`
	CA         string `json:"ca"`
	ClientCert string `json:"client_cert"`
	ClientKey  string `json:"client_key"`
	Control    string `json:"control"`
}
type casetaSource struct {
	Device, Entity, Zone string
}
type casetaPending struct {
	command    conformance.AdmittedCommand
	commandURL string
	wantCount  int
	ack        chan error
	report     chan *heldCasetaReport
	held       *heldCasetaReport
}
type heldCasetaReport struct {
	binding     conformance.Binding
	observation conformance.StateObservation
	host        conformance.SessionHost
	release     chan error
	done        chan error
	finished    chan struct{}
}
type casetaAdapter struct {
	mu            sync.Mutex
	config        worker.Config
	functions     map[string]casetaSource
	control       string
	fixture       *exec.Cmd
	fixtureCancel context.CancelFunc
	fixtureOutput *bytes.Buffer
	fixtureDir    string
	secrets       *storage.SecretRepository
	ready         chan struct{}
	pending       map[conformance.CommandID]*casetaPending
	current       *casetaPilot
	liveReports   map[*heldCasetaReport]struct{}
	closed        bool
	events        []string
}
type casetaPilot struct {
	inner   conformance.Pilot
	adapter *casetaAdapter
	host    *interceptCasetaHost
}
type interceptCasetaHost struct {
	base    conformance.SessionHost
	adapter *casetaAdapter
}

func casetaControlResult(socket string, command any) (map[string]json.RawMessage, error) {
	connection, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
	if err := json.NewEncoder(connection).Encode(command); err != nil {
		return nil, err
	}
	var answer map[string]json.RawMessage
	if err := json.NewDecoder(bufio.NewReader(connection)).Decode(&answer); err != nil {
		return nil, err
	}
	if len(answer) == 0 {
		return nil, errors.New("empty fake bridge control response")
	}
	return answer, nil
}

func casetaControl(socket string, command any) error {
	answer, err := casetaControlResult(socket, command)
	if err != nil {
		return err
	}
	if !bytes.Equal(answer["ok"], []byte("true")) {
		return fmt.Errorf("fake bridge control rejected operation: %s", answer["error"])
	}
	return nil
}

func casetaCommands(socket string) (map[string]int, error) {
	answer, err := casetaControlResult(socket, map[string]any{"op": "snapshot"})
	if err != nil {
		return nil, err
	}
	var commands map[string]int
	if err := json.Unmarshal(answer["commands"], &commands); err != nil || commands == nil {
		return nil, fmt.Errorf("invalid fake bridge command snapshot: %w", err)
	}
	return commands, nil
}

func startCasetaAdapter(root, python string, secrets *storage.SecretRepository) (*casetaAdapter, error) {
	folder, err := os.MkdirTemp("/tmp", "nha077-sdk-")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	fixture := exec.CommandContext(ctx, python, filepath.Join(root, "testworker/caseta_test_server.py"),
		"--root", folder, "--reconcile-seconds", "30")
	fixture.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	output := new(bytes.Buffer)
	fixture.Stdout, fixture.Stderr = output, output
	if err := fixture.Start(); err != nil {
		cancel()
		_ = os.RemoveAll(folder)
		return nil, err
	}
	readyFile := filepath.Join(folder, "ready.json")
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	data, err := os.ReadFile(readyFile)
	if err != nil {
		cancel()
		_ = fixture.Wait()
		_ = os.RemoveAll(folder)
		return nil, fmt.Errorf("fake bridge startup: %w: %s", err, output.String())
	}
	var info casetaFixtureInfo
	if err := json.Unmarshal(data, &info); err != nil {
		cancel()
		_ = fixture.Wait()
		_ = os.RemoveAll(folder)
		return nil, err
	}
	settingsData, err := os.ReadFile(info.Settings)
	if err != nil {
		cancel()
		_ = fixture.Wait()
		_ = os.RemoveAll(folder)
		return nil, err
	}
	var settings struct {
		Functions map[string]struct {
			Device string `json:"source_device"`
			Entity string `json:"source_entity"`
			Zone   string `json:"zone_id"`
		} `json:"functions"`
	}
	if err := json.Unmarshal(settingsData, &settings); err != nil {
		cancel()
		_ = fixture.Wait()
		_ = os.RemoveAll(folder)
		return nil, err
	}
	functions := make(map[string]casetaSource, len(settings.Functions))
	for alias, source := range settings.Functions {
		functions[alias] = casetaSource{source.Device, source.Entity, source.Zone}
	}
	if len(functions) != 13 {
		cancel()
		_ = fixture.Wait()
		_ = os.RemoveAll(folder)
		return nil, errors.New("fake bridge has incomplete inventory")
	}
	if err := casetaControl(info.Control, map[string]any{"op": "level", "zone": functions["light.living_room_lights"].Zone, "value": 50}); err != nil {
		cancel()
		_ = fixture.Wait()
		_ = os.RemoveAll(folder)
		return nil, err
	}
	instance, err := domain.ParseIntegrationInstanceID("nha095-synthetic")
	if err != nil {
		cancel()
		_ = fixture.Wait()
		_ = os.RemoveAll(folder)
		return nil, err
	}
	for name, path := range map[string]string{"fake-ca": info.CA, "fake-cert": info.ClientCert, "fake-key": info.ClientKey} {
		material, err := os.ReadFile(path)
		if err != nil {
			cancel()
			_ = fixture.Wait()
			_ = os.RemoveAll(folder)
			return nil, err
		}
		value, err := storage.NewSecretValue(material)
		if err != nil {
			cancel()
			_ = fixture.Wait()
			_ = os.RemoveAll(folder)
			return nil, err
		}
		secretName, err := storage.ParseSecretName(name)
		if err != nil {
			cancel()
			_ = fixture.Wait()
			_ = os.RemoveAll(folder)
			return nil, err
		}
		handle, err := storage.NewSecretHandle(instance, secretName)
		if err != nil {
			cancel()
			_ = fixture.Wait()
			_ = os.RemoveAll(folder)
			return nil, err
		}
		if err := secrets.RotateCredential(context.Background(), handle, value); err != nil {
			cancel()
			_ = fixture.Wait()
			_ = os.RemoveAll(folder)
			return nil, err
		}
	}
	config, err := worker.NewConfig("nha095-synthetic", python,
		[]string{filepath.Join(root, "drivers/caseta/worker.py"), "--config", info.Settings},
		[]string{"fake-ca", "fake-cert", "fake-key"})
	if err != nil {
		cancel()
		_ = fixture.Wait()
		_ = os.RemoveAll(folder)
		return nil, err
	}
	return &casetaAdapter{config: config, functions: functions, control: info.Control,
		fixture: fixture, fixtureCancel: cancel, fixtureOutput: output, fixtureDir: folder,
		secrets: secrets, ready: make(chan struct{}, 3),
		pending:     make(map[conformance.CommandID]*casetaPending),
		liveReports: make(map[*heldCasetaReport]struct{})}, nil
}

func (a *casetaAdapter) PrepareFixture(ctx context.Context, admission conformance.FixtureAdmission) error {
	aliases := make([]string, 0, 11)
	for alias := range a.functions {
		if alias != "light.living_room_lights" && alias != "switch.gazebo_lights" {
			aliases = append(aliases, alias)
		}
	}
	sort.Strings(aliases)
	bindings := make([]conformance.SupplementalBinding, 0, len(aliases))
	for _, alias := range aliases {
		f := a.functions[alias]
		binding, err := conformance.NewSupplementalBinding(f.Device, f.Entity, alias)
		if err != nil {
			return err
		}
		bindings = append(bindings, binding)
	}
	if len(bindings) != 11 {
		return errors.New("supplemental source count")
	}
	if err := admission.AddSources(ctx, bindings); err != nil {
		return err
	}
	return admission.AttachSecrets(a.secrets)
}

func (a *casetaAdapter) Spec(instance conformance.IntegrationInstanceID) (conformance.PilotSpec, error) {
	return conformance.NewPilotSpec(instance, func() conformance.Pilot {
		inner, err := worker.NewPilot(a.config)
		if err != nil {
			panic(err)
		}
		pilot := &casetaPilot{inner: inner, adapter: a}
		a.mu.Lock()
		a.current = pilot
		a.mu.Unlock()
		return pilot
	})
}
func (a *casetaAdapter) Completion() conformance.CompletionSemantics {
	return conformance.CompletionAcknowledged
}
func (a *casetaAdapter) Ready(ctx context.Context) error {
	select {
	case <-a.ready:
		return nil
	case <-ctx.Done():
		a.mu.Lock()
		defer a.mu.Unlock()
		return fmt.Errorf("%w; adapter events=%v; fake output=%s", ctx.Err(), a.events, a.fixtureOutput.String())
	}
}
func (a *casetaAdapter) Delivered(ctx context.Context, id conformance.CommandID) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		a.mu.Lock()
		entry := a.pending[id]
		a.mu.Unlock()
		if entry != nil {
			commands, err := casetaCommands(a.control)
			if err != nil {
				return err
			}
			if commands[entry.commandURL] >= entry.wantCount {
				return nil
			}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
func (a *casetaAdapter) Deliveries() int {
	commands, err := casetaCommands(a.control)
	if err != nil {
		a.mu.Lock()
		a.events = append(a.events, "command-snapshot-error:"+err.Error())
		a.mu.Unlock()
		return -1
	}
	total := 0
	for _, count := range commands {
		total += count
	}
	return total
}
func (a *casetaAdapter) Crash(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return casetaControl(a.control, map[string]any{"op": "close"})
}
func (a *casetaAdapter) Close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	reports := make([]*heldCasetaReport, 0, len(a.liveReports))
	for report := range a.liveReports {
		reports = append(reports, report)
	}
	a.mu.Unlock()
	_, probeErr := casetaCommands(a.control)
	for _, report := range reports {
		select {
		case report.release <- nil:
		default:
		}
	}
	var drainErr error
	for _, report := range reports {
		select {
		case <-report.finished:
		case <-time.After(2 * time.Second):
			drainErr = errors.New("intercepted worker Publish did not drain")
		}
	}
	a.fixtureCancel()
	waitErr := a.fixture.Wait()
	cleanupErr := os.RemoveAll(a.fixtureDir)
	if waitErr != nil && !errors.Is(waitErr, context.Canceled) {
		var exit *exec.ExitError
		status, ok := syscall.WaitStatus(0), false
		if errors.As(waitErr, &exit) {
			status, ok = exit.ProcessState.Sys().(syscall.WaitStatus)
		}
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
			return waitErr
		}
	}
	if probeErr != nil {
		return fmt.Errorf("fake bridge exited before teardown: %w", probeErr)
	}
	if drainErr != nil {
		return drainErr
	}
	return cleanupErr
}

func (p *casetaPilot) Start(ctx context.Context, host conformance.SessionHost, tasks conformance.Tasks) error {
	p.host = &interceptCasetaHost{base: host, adapter: p.adapter}
	p.adapter.mu.Lock()
	p.adapter.events = append(p.adapter.events, "start")
	p.adapter.mu.Unlock()
	err := p.inner.Start(ctx, p.host, tasks)
	if err != nil {
		p.adapter.mu.Lock()
		p.adapter.events = append(p.adapter.events, "start-error:"+err.Error())
		p.adapter.mu.Unlock()
	}
	return err
}
func (p *casetaPilot) Stop(ctx context.Context) error           { return p.inner.Stop(ctx) }
func (p *casetaPilot) CommandDriver() conformance.CommandDriver { return p }
func (p *casetaPilot) Dispatch(ctx context.Context, command conformance.AdmittedCommand) error {
	id, _, _, err := command.Identity()
	if err != nil {
		return err
	}
	request, err := command.Request()
	if err != nil {
		return err
	}
	alias := "switch.gazebo_lights"
	if request.CapabilitySchemaId == contracts.LightSchema {
		alias = "light.living_room_lights"
	}
	zone := p.adapter.functions[alias].Zone
	readURL := "/zone/" + zone + "/status"
	if err := casetaControl(p.adapter.control, map[string]any{"op": "arm_after_command",
		"command_url": "/zone/" + zone + "/commandprocessor", "read_url": readURL}); err != nil {
		return err
	}
	commands, err := casetaCommands(p.adapter.control)
	if err != nil {
		return err
	}
	commandURL := "/zone/" + zone + "/commandprocessor"
	entry := &casetaPending{command: command, commandURL: commandURL, wantCount: commands[commandURL] + 1,
		ack: make(chan error, 1), report: make(chan *heldCasetaReport, 1)}
	p.adapter.mu.Lock()
	p.adapter.pending[id] = entry
	p.adapter.mu.Unlock()
	driver := p.inner.(conformance.SupervisedCommandPilot).CommandDriver()
	err = driver.Dispatch(ctx, command)
	entry.ack <- err
	return err
}

func (h *interceptCasetaHost) Discover(ctx context.Context, c []conformance.Candidate) ([]conformance.Binding, error) {
	result, err := h.base.Discover(ctx, c)
	h.adapter.mu.Lock()
	h.adapter.events = append(h.adapter.events, fmt.Sprintf("discover:%d:%v", len(c), err))
	h.adapter.mu.Unlock()
	return result, err
}
func (h *interceptCasetaHost) Emit(ctx context.Context, b conformance.Binding, e conformance.DeviceEvent) error {
	return h.base.Emit(ctx, b, e)
}
func (h *interceptCasetaHost) Secrets() (integrations.SecretProvider, error) { return h.base.Secrets() }
func (h *interceptCasetaHost) ReportHealth(ctx context.Context, r conformance.HealthReport) error {
	status, _, _ := r.Components()
	if err := h.base.ReportHealth(ctx, r); err != nil {
		h.adapter.mu.Lock()
		h.adapter.events = append(h.adapter.events, fmt.Sprintf("health-error:%d:%v", status, err))
		h.adapter.mu.Unlock()
		return err
	}
	h.adapter.mu.Lock()
	h.adapter.events = append(h.adapter.events, fmt.Sprintf("health:%d", status))
	h.adapter.mu.Unlock()
	_, _, err := r.Components()
	if err == nil && status == integrations.HealthReady {
		select {
		case h.adapter.ready <- struct{}{}:
		default:
		}
	}
	return nil
}
func (h *interceptCasetaHost) Publish(ctx context.Context, b conformance.Binding, o conformance.StateObservation) error {
	_, _, _, _, _, _, cause, err := o.Components()
	if err != nil {
		return err
	}
	_, correlation, err := cause.Components()
	if err != nil {
		return err
	}
	if correlation == nil {
		return h.base.Publish(ctx, b, o)
	}
	h.adapter.mu.Lock()
	var entry *casetaPending
	for _, pending := range h.adapter.pending {
		pendingCause, err := pending.command.Causality()
		if err != nil {
			continue
		}
		_, want, err := pendingCause.Components()
		if err == nil && want != nil && *want == *correlation {
			entry = pending
			break
		}
	}
	h.adapter.mu.Unlock()
	if entry == nil {
		return errors.New("unowned correlated worker report")
	}
	held := &heldCasetaReport{binding: b, observation: o, host: h.base,
		release: make(chan error, 1), done: make(chan error, 1), finished: make(chan struct{})}
	h.adapter.mu.Lock()
	h.adapter.liveReports[held] = struct{}{}
	h.adapter.mu.Unlock()
	defer func() {
		h.adapter.mu.Lock()
		delete(h.adapter.liveReports, held)
		h.adapter.mu.Unlock()
		close(held.finished)
	}()
	select {
	case entry.report <- held:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-held.release:
		err := h.base.Publish(ctx, b, o)
		held.done <- err
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *casetaAdapter) Apply(ctx context.Context, id conformance.CommandID, stimulus conformance.Stimulus) error {
	a.mu.Lock()
	pending := a.pending[id]
	a.mu.Unlock()
	if pending == nil {
		return errors.New("unknown conformance command")
	}
	if stimulus == conformance.StimulusAcknowledge {
		// Delivery is witnessed at the fake bridge before its LEAP response.
		// Only this stimulus lets the actual worker receive that response.
		if err := casetaControl(a.control, map[string]any{"op": "release_command_response",
			"command_url": pending.commandURL}); err != nil {
			return err
		}
		select {
		case err := <-pending.ack:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	held := pending.held
	if held == nil {
		request, err := pending.command.Request()
		if err != nil {
			return err
		}
		alias := "switch.gazebo_lights"
		level := 0.0
		if request.CapabilitySchemaId == contracts.LightSchema {
			alias = "light.living_room_lights"
			level = request.Arguments.GetSetLight().GetBrightnessPercent()
		} else if request.Arguments.GetSetSwitch().On {
			level = 100
		}
		zone := a.functions[alias].Zone
		if err := casetaControl(a.control, map[string]any{"op": "level", "zone": zone, "value": level}); err != nil {
			return err
		}
		if err := casetaControl(a.control, map[string]any{"op": "release", "operation": "ReadRequest", "url": "/zone/" + zone + "/status"}); err != nil {
			return err
		}
		select {
		case held = <-pending.report:
			pending.held = held
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if held == nil {
		return errors.New("real worker has not produced the post-ACK report")
	}
	// The held report was decoded by the actual worker and crossed its UDS RPC.
	// Fault copies are injected only at the SessionHost transport boundary.
	if stimulus == conformance.StimulusMatch {
		held.release <- nil
		select {
		case err := <-held.done:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	endpoint, quality, value, observed, _, _, _, err := held.observation.Components()
	if err != nil {
		return err
	}
	if stimulus == conformance.StimulusStale {
		quality, err = conformance.StaleQuality()
		if err != nil {
			return err
		}
	}
	if stimulus == conformance.StimulusPartialLight {
		if value == nil {
			return errors.New("missing real light value")
		}
		light, ok := value.Light()
		if !ok {
			return errors.New("partial light requested on switch")
		}
		on, _, _, err := light.Components()
		if err != nil {
			return err
		}
		partial, err := domain.NewLightValue(on, nil, nil)
		if err != nil {
			return err
		}
		v, err := domain.NewLightStateValue(partial)
		if err != nil {
			return err
		}
		value = &v
	}
	now := time.Now()
	copyReport, err := integrations.NewStateObservation(endpoint, quality, value, observed, now, nil)
	if err != nil {
		return err
	}
	if stimulus == conformance.StimulusWrongCorrelation {
		wrong, err := domain.ParseCorrelationID("nha095-foreign-correlation")
		if err != nil {
			return err
		}
		cause, err := domain.NewCausality(nil, &wrong)
		if err != nil {
			return err
		}
		copyReport, err = copyReport.WithCausality(cause)
		if err != nil {
			return err
		}
	}
	err = held.host.Publish(ctx, held.binding, copyReport)
	if stimulus == conformance.StimulusWrongCorrelation && errors.Is(err, integrations.ErrForeignBinding) {
		return nil
	}
	return err
}

func TestNHA077RealCasetaWorkerNHA095BrightnessOnly(t *testing.T) {
	cluster := testpg.Start(t)
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	path, err := os.ReadFile(filepath.Join(root, ".tools/worker-python/python-path"))
	if err != nil {
		t.Fatal(err)
	}
	python := strings.TrimSpace(string(path))
	settings, err := storage.ParseSettings(storage.ConnectionInput{Host: "127.0.0.1", Port: cluster.Port, Database: "postgres", User: "homeward_test", PasswordEnv: "HOMEWARD_TEST_PASSWORD", SSLMode: "disable"},
		func(name string) (string, bool) { return testpg.Password(), name == "HOMEWARD_TEST_PASSWORD" })
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	secrets, err := store.BootstrapSecrets(context.Background(), filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	factory := func(_ conformance.Case, _ string) (conformance.Adapter, error) {
		return startCasetaAdapter(root, python, secrets)
	}
	switchSource := conformance.FixtureSource{Device: "fake-device-30", Entity: "fake-zone-30", Endpoint: "relay", Schema: contracts.SwitchSchema}
	lightSource := conformance.FixtureSource{Device: "fake-device-9", Entity: "fake-zone-9", Endpoint: "level", Schema: contracts.LightSchema}
	profile, err := conformance.NewFixtureProfile(switchSource, lightSource, conformance.LightBrightnessOnly)
	if err != nil {
		t.Fatal(err)
	}
	failures := conformance.Check(t.Context(), factory, conformance.Options{WorkDir: t.TempDir(), Timeout: 20 * time.Second, Profile: &profile})
	for _, failure := range failures {
		t.Error(failure)
	}
}

func TestNHA077CasetaControlRejectsNegativeReply(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "nha077-control-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "control.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer connection.Close()
		_, err = bufio.NewReader(connection).ReadBytes('\n')
		if err == nil {
			_, err = connection.Write([]byte("{\"ok\":false,\"error\":\"refused\"}\n"))
		}
		done <- err
	}()
	if err := casetaControl(path, map[string]string{"op": "release"}); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("negative control reply admitted: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNHA077CasetaAdapterCloseReportsIndependentCrash(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "nha077-crash-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", "exit 7")
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond) // Allow the independent exit before teardown starts.
	adapter := &casetaAdapter{fixture: cmd, fixtureCancel: cancel,
		fixtureDir: dir, control: filepath.Join(dir, "missing.sock")}
	if err := adapter.Close(); err == nil || !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("independent fixture crash hidden by Close: %v", err)
	}
}

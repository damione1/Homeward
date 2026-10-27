package server

// Explicit isolated qualification: the ordinary Run/session/registry/Recorder/
// maintenance/HistoryService composition, synthetic measurements and a fresh PG.
// No command driver or external device endpoint is configured.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/recordermaintenance"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
)

func TestNHF013HistoryServer(t *testing.T) {
	out := os.Getenv("HOMEWARD_NHF013_FIXTURE")
	if out == "" {
		t.Skip("explicit isolated NHF013 fixture only")
	}
	if os.Getenv("HOMEWARD_NHF013_SYNTHETIC_ONLY") != "1" || !filepath.IsAbs(out) || filepath.Clean(out) != out {
		t.Fatal("fresh absolute synthetic fixture required")
	}
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	sourcePath := os.Getenv("HOMEWARD_NHF013_SOURCE")
	if !filepath.IsAbs(sourcePath) {
		t.Fatal("explicit synthetic source configuration required")
	}
	input, err := os.ReadFile(sourcePath)
	if err != nil || len(input) > 4096 {
		t.Fatal("bounded synthetic source configuration required", err)
	}
	var fixtureSource struct {
		SyntheticOnly bool   `json:"synthetic_only"`
		Alias         string `json:"alias"`
		CostsMock     bool   `json:"costs_mock"`
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&fixtureSource); err != nil {
		t.Fatal(err)
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) || !fixtureSource.SyntheticOnly {
		t.Fatal("synthetic source configuration required")
	}
	if _, err = domain.ParseHAAlias(fixtureSource.Alias); err != nil {
		t.Fatal(err)
	}
	ctx, signals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer signals()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	settings := nha049Config(t, cluster.Port, data)
	manifest, err := registry.LoadManifest(scope.BaselineV1())
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nhf013-synthetic-only")
	key, _ := integrations.NewSourceKey("synthetic-meter", "energy")
	at := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	clock := &nha164NativeClock{at: at}
	maintenanceClock := nha032Clock()
	maintenanceClock.releaseStop()
	var maintenance *recordermaintenance.Worker
	var host *integrations.Manager
	var actual *observationRuntime
	var sqlStore *storage.Store
	factory := RuntimeFactory(func(factoryCtx context.Context, store *storage.Store) (RuntimePlan, error) {
		identities, err := registry.NewRepository(store, manifest)
		if err != nil {
			return RuntimePlan{}, err
		}
		if _, err = identities.ConfigureBinding(factoryCtx, instance, key, fixtureSource.Alias); err != nil {
			return RuntimePlan{}, err
		}
		public, err := NewPublicPlan(nil, nil, accountTransportAudit{store: store})
		if err != nil {
			return RuntimePlan{}, err
		}
		return RuntimePlan{Identity: identities, Public: public,
			ConfigureActivation: func(_ context.Context, owner *configactivation.Owner) error {
				if owner == nil {
					return errors.New("actual configuration owner required")
				}
				return nil
			},
			ConfigureAuthorization: func(context.Context, AuthorizationComposition) error { return nil },
			Configure: func(_ context.Context, manager *integrations.Manager, store *storage.Store) error {
				host = manager
				sqlStore = store
				return nil
			},
		}, nil
	})
	runCtx, stop := context.WithCancel(ctx)
	done, ready := make(chan error, 1), make(chan net.Addr, 1)
	go func() {
		done <- runWithDependencies(runCtx, settings.startup, func(listenCtx context.Context, kind, _ string) (net.Listener, error) {
			return (&net.ListenConfig{}).Listen(listenCtx, kind, "127.0.0.1:0")
		}, func(address net.Addr) { ready <- address }, factory,
			runDependencies{sequenceClock: clock, maintenanceClock: maintenanceClock, observeRuntime: func(runtime *observationRuntime) { actual = runtime }, observeRecorderMaintenance: func(worker *recordermaintenance.Worker) { maintenance = worker }})
	}()
	joined := false
	defer func() {
		stop()
		if !joined {
			select {
			case err := <-done:
				joined = true
				if err != nil {
					t.Error(err)
				}
			case <-time.After(20 * time.Second):
				t.Error("history Run did not join")
			}
		}
		entries := uint64(0)
		if host != nil {
			entries = host.CommandDriverEntries()
		}
		nhf003WritePrivate(t, out, "result.json", map[string]any{"server_joined": joined, "command_driver_entries": entries, "real_device_writes": 0})
		if entries != 0 {
			t.Error("read-only history fixture entered a command driver")
		}
	}()
	var address net.Addr
	select {
	case address = <-ready:
	case err := <-done:
		joined = true
		t.Fatal("history startup", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	source, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := integrations.NewCandidate(key, []*registrywire.CapabilityDescription{{SchemaId: contracts.MeasurementSchema, Major: 1, DisplayName: "Compteur synthétique", Endpoint: "energy", Constraints: &registrywire.CapabilityDescription_Measurement{Measurement: &registrywire.MeasurementConstraints{Unit: "kWh", Precision: 3, Semantic: registrywire.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL, Aggregation: registrywire.MeasurementAggregation_MEASUREMENT_AGGREGATION_SUM}}}})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := source.Discover(ctx, []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		t.Fatal("synthetic history discovery", err)
	}
	_, entity, err := bindings[0].IDs()
	if err != nil {
		t.Fatal(err)
	}
	name, _ := entity.Value()
	unit, err := domain.NewUnitCatalog().Parse("kWh")
	if err != nil {
		t.Fatal(err)
	}
	const pointCount = 20000
	for i := 0; i < pointCount; i++ {
		stamp := at.Add(time.Duration(i) * 350 * time.Millisecond)
		clock.set(stamp)
		freshness, provenance := domain.FreshnessFresh, domain.ProvenanceObserved
		quality, err := domain.NewQuality(domain.AvailabilityAvailable, freshness, provenance)
		if err != nil {
			t.Fatal(err)
		}
		// A synthetic precomputed server counter, never integrated by Rust.
		measurement, err := domain.NewMeasurement(float64(i)/1000, unit, 3, domain.SemanticTotal, domain.AggregationSum)
		if err != nil {
			t.Fatal(err)
		}
		value, err := domain.NewMeasurementValue(measurement)
		if err != nil {
			t.Fatal(err)
		}
		observed := stamp.Add(-time.Hour)
		observation, err := integrations.NewStateObservation("energy", quality, &value, &observed, stamp, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = source.Publish(ctx, bindings[0], observation); err != nil {
			t.Fatal("synthetic history admission", err)
		}
		if i%256 == 255 || i == pointCount-1 {
			capture, err := actual.sequence.CaptureEntityCatalogue(entity, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			_, through, err := capture.Cursor().Components()
			if err != nil {
				t.Fatal(err)
			}
			nha042ServerWait(t, "history durable prefix", func() bool {
				status, err := actual.recording.Status()
				return err == nil && status.Checkpoint >= through
			})
		}
	}
	// Ordinary maintenance owns the aggregate arithmetic. Await a real result,
	// not a client reconstruction or a direct aggregate-table SQL fixture.
	for cycle := 0; cycle < 64; cycle++ {
		before := maintenance.Status().Cycles
		maintenanceClock.advance()
		nha042ServerWait(t, "history maintenance cycle", func() bool { status := maintenance.Status(); return status.Cycles > before && !status.Active })
		status := maintenance.Status()
		if status.LastError != nil {
			t.Fatal("history maintenance", status.LastError)
		}
		if !status.Result.More {
			break
		}
		if cycle == 63 {
			t.Fatal("history maintenance did not reach its bounded prefix")
		}
	}
	nha042ServerWait(t, "history hourly aggregate", func() bool {
		rows, err := sqlStore.ReadRecorderHourlyAggregates(ctx, name, at, at.Add(2*time.Hour))
		return err == nil && len(rows) > 0
	})
	const adminName, adminPassword = "NHF013 isolated admin", "NHF013 synthetic admin password"
	_ = nha162RunBootstrap(t, ctx, data, adminName, adminPassword)
	client := nha181Client(roots)
	defer client.CloseIdleConnections()
	base := "https://" + address.String()
	authAPI := authv1connect.NewAuthServiceClient(client, base)
	admin := nha162RunLogin(t, ctx, authAPI, adminName, adminPassword)
	accounts := authv1connect.NewAccountServiceClient(client, base)
	const readerName, readerPassword = "NHF013 Reader", "NHF013 synthetic reader password"
	created, err := accounts.CreateAccount(ctx, nha162Bearer(&auth.CreateAccountRequest{LoginName: readerName, DisplayName: readerName, InitialPassword: readerPassword, Role: auth.AccountRole_ACCOUNT_ROLE_USER}, admin.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	policies := authv1connect.NewAuthorizationPolicyServiceClient(client, base)
	setHistory := func(allowed bool) {
		grants := `  - scope: {family: state_registry, operation: read}
    objects: {entities: {all: {capabilities: {all: {}}}}}
  - scope: {family: dashboards, operation: read}
    objects: {dashboards: {all: {}}}
`
		if allowed {
			grants += `  - scope: {family: history, operation: read}
    objects: {entities: {all: {capabilities: {all: {}}}}}
`
		}
		nha162RunPolicy(t, ctx, policies, created.Msg.Account.Id, admin.AccessToken, grants)
	}
	setHistory(true)
	certificate, err := os.ReadFile(filepath.Join(data, nha181CertificateName))
	if err != nil {
		t.Fatal(err)
	}
	if fixtureSource.CostsMock {
		// Only this opt-in fixture substitutes monetary wire values, after
		// ordinary History authorization. Production costs remain unqualified.
		base = nhf013CostMock(t, base, client, data, at, at.Add((pointCount-1)*350*time.Millisecond))
	}
	nhf003WritePrivate(t, out, "ready.json", map[string]any{"mode": "synthetic-only", "origin": base, "ca_pem": string(certificate), "entity_id": name, "point_count": pointCount, "start": at.Format(time.RFC3339Nano), "end": at.Add(2 * time.Hour).Format(time.RFC3339Nano), "reader": map[string]string{"login_name": readerName, "password": readerPassword}, "costs_mock": fixtureSource.CostsMock})
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var controlSequence uint64
	for {
		select {
		case err := <-done:
			joined = true
			if err != nil {
				t.Fatal(err)
			}
			return
		case <-ctx.Done():
			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatal(ctx.Err())
			}
			return
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(out, "stop")); err == nil {
				return
			}
			command, err := nhf013ReadControl(out, controlSequence)
			if err != nil {
				t.Fatal(err)
			}
			if command == nil {
				continue
			}
			switch command.Action {
			case nhf013Revoke:
				setHistory(false)
			case nhf013Allow:
				setHistory(true)
			case nhf013Stale:
				stamp := at.Add(2*time.Hour - time.Second)
				clock.set(stamp)
				quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessStale, domain.ProvenanceRestored)
				if err != nil {
					t.Fatal(err)
				}
				measurement, err := domain.NewMeasurement(19.999, unit, 3, domain.SemanticTotal, domain.AggregationSum)
				if err != nil {
					t.Fatal(err)
				}
				value, err := domain.NewMeasurementValue(measurement)
				if err != nil {
					t.Fatal(err)
				}
				observed := stamp.Add(-time.Hour)
				observation, err := integrations.NewStateObservation("energy", quality, &value, &observed, stamp, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err = source.Publish(ctx, bindings[0], observation); err != nil {
					t.Fatal(err)
				}
				capture, err := actual.sequence.CaptureEntityCatalogue(entity, 1<<20)
				if err != nil {
					t.Fatal(err)
				}
				_, through, err := capture.Cursor().Components()
				if err != nil {
					t.Fatal(err)
				}
				nha042ServerWait(t, "stale history durable prefix", func() bool {
					status, err := actual.recording.Status()
					return err == nil && status.Checkpoint >= through
				})
			default:
				t.Fatal("unadmitted history control")
			}
			controlSequence = command.Sequence
			nhf003WritePrivate(t, out, fmt.Sprintf("ack-%02d.json", controlSequence), command)
		}
	}
}

type nhf013ControlAction string

const (
	nhf013Revoke nhf013ControlAction = "revoke-history"
	nhf013Allow  nhf013ControlAction = "allow-history"
	nhf013Stale  nhf013ControlAction = "publish-stale"
)

func (a *nhf013ControlAction) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	switch value := nhf013ControlAction(text); value {
	case nhf013Revoke, nhf013Allow, nhf013Stale:
		*a = value
		return nil
	default:
		return errors.New("unknown history fixture control")
	}
}

type nhf013Control struct {
	Sequence uint64              `json:"sequence"`
	Action   nhf013ControlAction `json:"action"`
}

func nhf013ReadControl(root string, after uint64) (*nhf013Control, error) {
	path := filepath.Join(root, "command.json")
	stat, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Size() > 4096 {
		return nil, errors.New("history control must be a bounded regular file")
	}
	input, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	var command nhf013Control
	if err = decoder.Decode(&command); err != nil {
		return nil, err
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing history control")
	}
	if command.Sequence == 0 || command.Action == "" {
		return nil, errors.New("incomplete history control")
	}
	if command.Sequence <= after {
		return nil, nil
	}
	if command.Sequence != after+1 {
		return nil, errors.New("out-of-order history control")
	}
	return &command, nil
}

func TestNHF013HistoryFixtureControls(t *testing.T) {
	root := t.TempDir()
	for _, text := range []string{`{}`, `{"sequence":0,"action":"publish-stale"}`, `{"sequence":2,"action":"publish-stale"}`, `{"sequence":1,"action":"unknown"}`, `{"sequence":1,"action":"publish-stale","extra":true}`, `{"sequence":1,"action":"publish-stale"} {}`} {
		if err := os.WriteFile(filepath.Join(root, "command.json"), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := nhf013ReadControl(root, 0); err == nil {
			t.Fatal("invalid history control admitted")
		}
	}
	for _, action := range []nhf013ControlAction{nhf013Revoke, nhf013Allow, nhf013Stale} {
		input, err := json.Marshal(nhf013Control{Sequence: 1, Action: action})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(root, "command.json"), input, 0600); err != nil {
			t.Fatal(err)
		}
		command, err := nhf013ReadControl(root, 0)
		if err != nil || command == nil || command.Action != action {
			t.Fatal("valid history control refused", err)
		}
		command, err = nhf013ReadControl(root, 1)
		if err != nil || command != nil {
			t.Fatal("history control replayed", err)
		}
	}
}

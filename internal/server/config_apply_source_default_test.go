package server

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

// The actual Run owns this listener through its existing ListenFunc. It never
// accepts a connection or opens a serving socket; PostgreSQL is the only real
// loopback service used by this fixture.
type nha055DormantListener struct {
	closed chan struct{}
	once   sync.Once
}

func (l *nha055DormantListener) Accept() (net.Conn, error) { <-l.closed; return nil, net.ErrClosed }
func (l *nha055DormantListener) Close() error              { l.once.Do(func() { close(l.closed) }); return nil }
func (*nha055DormantListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}
}

func TestNHA055DefaultRunInstallsSourceFactoryAndRealDormantServicesWithoutActions(t *testing.T) {
	cluster := testpg.Start(t)
	cfg := nha030ServerConfig(t, cluster.Port)
	observer := nha030ServerObserver(t, cluster.Port)
	ctx, cancel := context.WithCancel(context.Background())
	done, ready := make(chan error, 1), make(chan struct{})
	owners, observations := make(chan *configactivation.Owner, 1), make(chan *observationRuntime, 1)
	listener := &nha055DormantListener{closed: make(chan struct{})}
	joined := false
	t.Cleanup(func() {
		cancel()
		if joined {
			return
		}
		select {
		case err := <-done:
			joined = true
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("actual default Run did not join dormant services")
		}
	})
	go func() {
		done <- runWithDependencies(ctx, cfg.startup, func(context.Context, string, string) (net.Listener, error) { return listener, nil }, func(net.Addr) { close(ready) }, nil, runDependencies{
			observeRemoteActivation: func(owner *configactivation.Owner) { owners <- owner },
			observeRuntime:          func(runtime *observationRuntime) { observations <- runtime },
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		joined = true
		t.Fatalf("actual default Run failed before ready: %v", err)
	case <-time.After(12 * time.Second):
		t.Fatal("actual default Run did not reach ready")
	}
	var owner *configactivation.Owner
	var actual *observationRuntime
	select {
	case owner = <-owners:
	case <-time.After(time.Second):
		t.Fatal("existing activation observer did not receive the actual Owner")
	}
	select {
	case actual = <-observations:
	case <-time.After(time.Second):
		t.Fatal("existing runtime observer did not receive the actual services")
	}
	if owner == nil || actual == nil || actual.runs == nil || actual.host == nil || actual.sequence == nil || actual.store == nil {
		t.Fatal("default assembly omitted a real runtime participant")
	}
	triggerPlan, err := actual.runs.CloneTriggerPlan()
	if err != nil || !triggerPlan.Valid() || triggerPlan.Count() != 0 || len(triggerPlan.AutomationIDs()) != 0 {
		t.Fatal("real dormant controller did not retain the actual empty trigger service", err)
	}
	if traces, evicted := actual.runs.Traces(); len(traces) != 0 || evicted != 0 {
		t.Fatal("default dormant assembly executed or replayed logic")
	}
	readCtx, finishRead := context.WithTimeout(context.Background(), 5*time.Second)
	defer finishRead()
	active, present, err := actual.store.ReadActiveConfig(readCtx)
	if err != nil || !present || active.Availability() != storage.ConfigGenerationComplete || len(active.Versions()) != 1 {
		t.Fatal("real SQL24 startup did not admit exactly its operational source", err)
	}
	kind, present := active.Versions()[0].Kind()
	if !present || kind != storage.ConfigDocumentKindOperationalYAML {
		t.Fatal("fixture silently introduced a SourceGroup or projected SQL26")
	}
	var beforeGenerations, beforeVersions, beforeCommands int64
	if err := observer.QueryRow(readCtx, `SELECT (SELECT count(*) FROM public.config_generations),(SELECT count(*) FROM public.config_versions),(SELECT count(*) FROM public.command_intents)`).Scan(&beforeGenerations, &beforeVersions, &beforeCommands); err != nil {
		t.Fatal(err)
	}
	if beforeCommands != 0 {
		t.Fatal("dormant Run dispatched a command")
	}
	// Exercise the SourceFactory actually attached to the real default Owner.
	// Semantic preparation alone must neither stage files nor write SQL26.
	descriptor, err := storage.DashboardSourceDescriptor("dashboard.nha055_default", "dashboard.yaml")
	if err != nil {
		t.Fatal(err)
	}
	id, err := storage.NewConfigDocumentID("source.dashboard.nha055_default")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := storage.NewConfigDocumentBinding(id, "dashboard.yaml")
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.NewSourceFile("dashboard.yaml", []byte("views: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	group, err := configactivation.NewSourceGroup(descriptor, []parser.SourceFile{file}, []storage.ConfigDocumentBinding{binding})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := owner.CompileEditableSourceGroup(readCtx, group)
	if err != nil || !compiled.Valid() || len(compiled.Groups()) != 1 {
		t.Fatal("actual default Owner has no working source factory", err)
	}
	dashboard, err := authorization.ParseDashboardID("dashboard.nha055_default")
	if err != nil {
		t.Fatal(err)
	}
	if _, present := compiled.Dashboard(dashboard); !present {
		t.Fatal("default source factory lost the real dashboard compiler result")
	}
	var afterGenerations, afterVersions, afterCommands int64
	if err := observer.QueryRow(readCtx, `SELECT (SELECT count(*) FROM public.config_generations),(SELECT count(*) FROM public.config_versions),(SELECT count(*) FROM public.command_intents)`).Scan(&afterGenerations, &afterVersions, &afterCommands); err != nil {
		t.Fatal(err)
	}
	if beforeGenerations != afterGenerations || beforeVersions != afterVersions || afterCommands != 0 {
		t.Fatal("source preparation mutated generation history or dispatched actions")
	}
	if traces, evicted := actual.runs.Traces(); len(traces) != 0 || evicted != 0 {
		t.Fatal("source preparation armed or replayed dormant definitions")
	}
}

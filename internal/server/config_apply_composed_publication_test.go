package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	configuration "github.com/damione1/Homeward/gen/home/configuration/v1"
	"github.com/damione1/Homeward/gen/home/configuration/v1/configurationv1connect"
	wire "github.com/damione1/Homeward/gen/home/configurationapply/v1"
	"github.com/damione1/Homeward/gen/home/configurationapply/v1/configurationapplyv1connect"
	planwire "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	"github.com/damione1/Homeward/gen/home/configurationplan/v1/configurationplanv1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Only observers differ from the existing default-Run fixture. There is no
// runtime factory, fake activation owner, inline DDL or physical driver.
type nha055PublicationFixture struct {
	nha055TLSFixture
	startup config.StartupInputs
	roots   *x509.CertPool
	actual  *observationRuntime
	owner   *configactivation.Owner
}

func nha055PublicationStart(t *testing.T) *nha055PublicationFixture {
	t.Helper()
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	cfg := nha181Config(t, cluster.Port, data, "127.0.0.1:0", nha181TLSYAML())
	source, err := cfg.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "configuration.yaml")
	if err := os.WriteFile(path, source.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	f := &nha055PublicationFixture{startup: startup, roots: roots}
	f.path, f.raw, f.ctx = path, source.Bytes(), ctx
	f.startRun(t)
	nha162RunBootstrap(t, ctx, data, "NHA055 publication admin", "NHA055 publication isolated password")
	f.login(t)
	return f
}

func (f *nha055PublicationFixture) startRun(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	run := &nha162Running{stop: cancel, done: make(chan error, 1)}
	ready := make(chan net.Addr, 1)
	actual := make(chan *observationRuntime, 1)
	owners := make(chan *configactivation.Owner, 1)
	go func() {
		run.done <- runWithDependencies(ctx, f.startup, (&net.ListenConfig{}).Listen, func(address net.Addr) { ready <- address }, nil, runDependencies{
			observeRuntime:          func(runtime *observationRuntime) { actual <- runtime },
			observeRemoteActivation: func(owner *configactivation.Owner) { owners <- owner },
		})
	}()
	t.Cleanup(func() {
		if !run.joined {
			if err := run.join(t); err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		}
	})
	select {
	case run.address = <-ready:
	case err := <-run.done:
		run.joined = true
		t.Fatal("composed default Run exited before READY", err)
	case <-time.After(12 * time.Second):
		t.Fatal("composed default Run did not reach READY")
	}
	select {
	case f.actual = <-actual:
	default:
		t.Fatal("READY without actual runtime")
	}
	select {
	case f.owner = <-owners:
	default:
		t.Fatal("READY without actual owner")
	}
	f.run = run
	client := nha162RunClient(t, f.roots, run.address)
	base := "https://" + run.address.String()
	f.authentication = authv1connect.NewAuthServiceClient(client, base)
	f.api = configurationv1connect.NewConfigurationServiceClient(client, base)
	f.plans = configurationplanv1connect.NewConfigurationPlanServiceClient(client, base)
	f.policies = authv1connect.NewAuthorizationPolicyServiceClient(client, base)
	f.apply = configurationapplyv1connect.NewConfigurationApplyServiceClient(client, base)
}

func (f *nha055PublicationFixture) login(t *testing.T) {
	t.Helper()
	f.tokens = nha162RunLogin(t, f.ctx, f.authentication, "NHA055 publication admin", "NHA055 publication isolated password")
	f.token = f.tokens.AccessToken
}

func nha055PublicationExpected(id, path string, previous *configuration.DocumentVersion, raw []byte) *wire.SourcePrecondition {
	value := &wire.SourcePrecondition{Id: &configuration.DocumentId{Value: id}, LogicalPath: path}
	if previous == nil {
		value.SqlState = &wire.SourcePrecondition_SqlAbsent{SqlAbsent: &emptypb.Empty{}}
		value.WorkspaceState = &wire.SourcePrecondition_WorkspaceAbsent{WorkspaceAbsent: &emptypb.Empty{}}
	} else {
		value.SqlState = &wire.SourcePrecondition_Existing{Existing: proto.Clone(previous).(*configuration.DocumentVersion)}
		hash := sha256.Sum256(raw)
		value.WorkspaceState = &wire.SourcePrecondition_WorkspaceSha256{WorkspaceSha256: hash[:]}
	}
	return value
}

// These seven missing publication surfaces complement the existing operational
// and authorization Apply/Rollback TLS tests. A and B use different exact bytes,
// and every rollback creates C rather than rewinding A's revision. Restart uses
// the same real PG/data directory, after all services from the first Run join.
func TestNHA055ActualRunTLSComposedSourceApplyRollbackAndRestart(t *testing.T) {
	// The retained source identity and the HA YAML runtime id are distinct.
	const automationRuntimeID = "nha055_publication"
	cases := []struct {
		name            string
		family          planwire.SourceFamily
		identity, entry string
		raw             []byte
	}{
		{"declarative1", planwire.SourceFamily_SOURCE_FAMILY_DECLARATIVE_V1, "nha055.publication.definitions", "definitions.yaml", []byte("input_boolean:\n  away_mode: {name: 'NHA055 isolated helper', initial: false}\n")},
		{"declarative2", planwire.SourceFamily_SOURCE_FAMILY_DECLARATIVE_V2, "nha055.publication.definitions", "definitions.yaml", []byte("input_boolean:\n  away_mode: {name: 'NHA055 isolated helper', initial: false}\n")},
		{"dashboard", planwire.SourceFamily_SOURCE_FAMILY_DASHBOARD, "dashboard.nha055_publication", "dashboard.yaml", []byte("views: []\n")},
		{"automation", planwire.SourceFamily_SOURCE_FAMILY_AUTOMATION, "automation.turn_on_lights_at_sunset", "automation.yaml", []byte("id: " + automationRuntimeID + "\ntriggers:\n - trigger: time\n   at: '18:00:00'\nactions:\n - stop: complete\n")},
		{"script", planwire.SourceFamily_SOURCE_FAMILY_SCRIPT, "script.nha055_publication", "script.yaml", []byte("sequence:\n - stop: complete\n")},
		{"scene", planwire.SourceFamily_SOURCE_FAMILY_SCENE, "scene.ceilling_fan_night", "scene.yaml", []byte("id: fixture\nname: NHA055 isolated scene\nentities:\n switch.arc_soundbar_crossfade: 'off'\n")},
		{"ownership", planwire.SourceFamily_SOURCE_FAMILY_OWNERSHIP, "heating.ownership", "ownership.json", scope.HeatingOwnershipV1()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := nha055PublicationStart(t)
			id := "nha055.publication." + c.name
			before := f.read(t)
			candidate := &planwire.Candidate{Family: c.family, Identity: c.identity, Entry: c.entry, Files: []*planwire.SourceFile{{LogicalPath: c.entry, ExactSource: bytes.Clone(c.raw)}}}
			apply := func(base *configuration.ReadDocumentResponse, raw []byte, previous *configuration.DocumentVersion, previousRaw []byte) *wire.ApplyResponse {
				t.Helper()
				next := proto.Clone(candidate).(*planwire.Candidate)
				next.Files[0].ExactSource = bytes.Clone(raw)
				plan := f.build(t, next)
				request := nha055Request(base, next, &plan.Receipt)
				request.Sources = []*wire.SourcePrecondition{nha055PublicationExpected(id, c.entry, previous, previousRaw)}
				// The journal can already have a documentary Refresh batch
				// queued before activation. It is valid history, not the
				// coalesced publication signal; keep reading until real resync.
				watchCtx, stopWatch := context.WithTimeout(f.ctx, 10*time.Second)
				defer stopWatch()
				stream, err := f.api.WatchConfiguration(watchCtx, nha056SessionRequest(&configuration.WatchConfigurationRequest{ResumeToken: base.ResumeToken}, f.token))
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Close()
				if !stream.Receive() || stream.Msg().GetReady() == nil {
					t.Fatal("publication watch not ready", stream.Err())
				}
				response, err := f.apply.Apply(f.ctx, nha056SessionRequest(request, f.token))
				if err != nil {
					t.Fatal("composed source Apply", err)
				}
				if response.Msg.Activation.Outcome != configuration.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED || response.Msg.Current.ActiveGeneration.Revision != base.ActiveGeneration.Revision+1 {
					t.Fatal("source Apply did not publish exactly one new generation")
				}
				resynchronized := false
				for stream.Receive() {
					frame := stream.Msg()
					if frame.GetResyncRequired() != nil {
						resynchronized = true
						break
					}
					batch := frame.GetChanges()
					if batch == nil || len(batch.Changes) == 0 || batch.ResumeToken == "" {
						t.Fatal("unexpected frame before publication resync")
					}
					t.Logf("observed documentary batch before publication resync: %d change(s)", len(batch.Changes))
				}
				if !resynchronized {
					t.Fatal("source publication did not notify actual public consumer", stream.Err())
				}
				return response.Msg
			}
			readSource := func() *configuration.DocumentVersion {
				t.Helper()
				value, err := f.api.ReadDocument(f.ctx, nha056SessionRequest(&configuration.ReadDocumentRequest{Id: &configuration.DocumentId{Value: id}}, f.token))
				if err != nil || value.Msg.Document == nil || value.Msg.Document.Version == nil {
					t.Fatal("published source not readable through TLS", err)
				}
				return value.Msg.Document.Version
			}
			first := apply(before, c.raw, nil, nil)
			firstVersion := readSource()
			secondRaw := append(bytes.Clone(c.raw), []byte("\n")...)
			second := apply(first.Current, secondRaw, firstVersion, c.raw)
			secondVersion := readSource()
			if secondVersion.Revision != firstVersion.Revision+1 {
				t.Fatal("source Apply did not create a new source revision")
			}
			restored, err := f.apply.Rollback(f.ctx, nha056SessionRequest(&wire.RollbackRequest{
				Id: second.Current.Document.Id, Expected: second.Current.Document.Version, ExpectedWorkspaceSha256: second.Current.Document.GetWorkspace().SourceSha256,
				Historical: first.Current.ActiveGeneration, Family: c.family, Identity: c.identity,
				Sources: []*wire.SourcePrecondition{nha055PublicationExpected(id, c.entry, secondVersion, secondRaw)},
			}, f.token))
			if err != nil {
				t.Fatal("composed source rollback", err)
			}
			if restored.Msg.Activation.Outcome != configuration.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED || restored.Msg.Current.ActiveGeneration.Revision != second.Current.ActiveGeneration.Revision+1 {
				t.Fatal("rollback did not create a new published generation")
			}
			thirdVersion := readSource()
			if thirdVersion.Revision != secondVersion.Revision+1 || !bytes.Equal(thirdVersion.SourceSha256, firstVersion.SourceSha256) {
				t.Fatal("rollback rewound revision or substituted historical bytes")
			}
			check := func() {
				t.Helper()
				active, present, err := f.actual.store.ReadActiveConfig(f.ctx)
				if err != nil || !present || active.Revision().Number() != restored.Msg.Current.ActiveGeneration.Revision {
					t.Fatal("actual SQL active generation differs", err)
				}
				stamp, err := f.owner.CurrentConfig(f.ctx)
				if err != nil {
					t.Fatal(err)
				}
				revision, manifest, err := stamp.Components()
				if err != nil || int64(revision) != active.Revision().Number() || manifest != active.ManifestSHA256() {
					t.Fatal("actual runtime owner did not publish the SQL generation", err)
				}
				found := false
				for _, version := range active.Versions() {
					if version.Ref().Document().String() == id {
						raw, present := version.Bytes()
						if !present || !bytes.Equal(raw, c.raw) {
							t.Fatal("active generation did not retain exact restored source")
						}
						found = true
					}
				}
				if !found {
					t.Fatal("source absent from the actual active generation")
				}
				if f.actual.runs == nil {
					t.Fatal("actual runtime controller missing")
				}
				if traces, evicted := f.actual.runs.Traces(); len(traces) != 0 || evicted != 0 {
					t.Fatal("publication or restart executed/replayed automation")
				}
				if _, commands := nha150Counts(t, f.actual.store); commands != 0 {
					t.Fatal("publication/rollback/restart dispatched a physical command")
				}

				if c.family == planwire.SourceFamily_SOURCE_FAMILY_DASHBOARD {
					dashboard, err := authorization.ParseDashboardID(c.identity)
					if err != nil {
						t.Fatal(err)
					}
					definition, present, err := f.owner.CurrentDashboard(f.ctx, dashboard)
					if err != nil || !present || len(definition.Views()) != 0 {
						t.Fatal("published dashboard consumer lost the exact compiled source", err)
					}
				}
				if c.family == planwire.SourceFamily_SOURCE_FAMILY_OWNERSHIP {
					decision, present, err := f.owner.CurrentOwnership(f.ctx)
					if err != nil || !present || len(decision.Pairs()) == 0 {
						t.Fatal("published ownership consumer lost the admitted decision", err)
					}
				}
				if c.family == planwire.SourceFamily_SOURCE_FAMILY_DECLARATIVE_V1 || c.family == planwire.SourceFamily_SOURCE_FAMILY_DECLARATIVE_V2 {
					projection, err := f.actual.sequence.Capture()
					if err != nil {
						t.Fatal(err)
					}
					helper := false
					for entity, record := range projection.Registry {
						original, _, _, err := record.Aliases()
						if err != nil {
							t.Fatal(err)
						}
						name, err := original.Value()
						if err != nil {
							t.Fatal(err)
						}
						if name == "input_boolean.away_mode" && !record.Retired() {
							_, helper = projection.States[entity]
						}
					}
					if !helper {
						t.Fatal("published registry/helper consumer did not receive declarative definition")
					}
				}
				if c.family == planwire.SourceFamily_SOURCE_FAMILY_AUTOMATION {
					plan, err := f.actual.runs.CloneTriggerPlan()
					ids := plan.AutomationIDs()
					if err != nil || !plan.Valid() || len(ids) != 1 || ids[0] != automationRuntimeID {
						t.Fatalf("actual run consumer automation IDs = %v, want [%s]: %v", ids, automationRuntimeID, err)
					}
				}
				if c.family == planwire.SourceFamily_SOURCE_FAMILY_SCRIPT {
					script, err := ir.ParseScriptID(c.identity)
					if err != nil {
						t.Fatal(err)
					}
					if err := f.actual.runs.StopScript(f.ctx, script); err != nil {
						t.Fatal("actual run consumer did not receive the script", err)
					}
				}
			}
			check()
			group, err := storage.NewConfigDocumentID(c.identity)
			if err != nil {
				t.Fatal(err)
			}
			if c.family != planwire.SourceFamily_SOURCE_FAMILY_DECLARATIVE_V1 && c.family != planwire.SourceFamily_SOURCE_FAMILY_DECLARATIVE_V2 {
				var descriptor storage.SourceDescriptor
				switch c.family {
				case planwire.SourceFamily_SOURCE_FAMILY_DASHBOARD:
					descriptor, err = storage.DashboardSourceDescriptor(c.identity, c.entry)
				case planwire.SourceFamily_SOURCE_FAMILY_AUTOMATION:
					descriptor, err = storage.AutomationSourceDescriptor(c.identity, c.entry)
				case planwire.SourceFamily_SOURCE_FAMILY_SCRIPT:
					descriptor, err = storage.ScriptSourceDescriptor(c.identity, c.entry)
				case planwire.SourceFamily_SOURCE_FAMILY_SCENE:
					descriptor, err = storage.SceneSourceDescriptor(c.identity, c.entry)
				case planwire.SourceFamily_SOURCE_FAMILY_OWNERSHIP:
					descriptor, err = storage.OwnershipSourceDescriptor(c.identity, c.entry)
				}
				if err != nil {
					t.Fatal(err)
				}
				group, err = configdocuments.SourceWorkspaceGroup(descriptor)
				if err != nil {
					t.Fatal(err)
				}
			}
			groupHash := sha256.Sum256([]byte(group.String()))
			physical := filepath.Join(filepath.Dir(f.path), "configuration.d", hex.EncodeToString(groupHash[:]), c.entry)
			exact, err := os.ReadFile(physical)
			if err != nil || !bytes.Equal(exact, c.raw) {
				t.Fatal("rollback did not restore the actual selected workspace file", err)
			}
			// Poison only the editable source after shutdown. The cold default Run must
			// rebuild from the retained SQL source rather than this newer invalid file.
			if err := f.run.join(t); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if err := os.WriteFile(physical, []byte("invalid: [ workspace\n"), 0600); err != nil {
				t.Fatal(err)
			}
			f.startRun(t)
			f.login(t)
			check()
			afterRestart := readSource()
			if !proto.Equal(afterRestart, thirdVersion) {
				t.Fatal("cold restart invented a source revision or replayed the workspace")
			}
		})
	}
}

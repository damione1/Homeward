package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"connectrpc.com/connect"
	cfg "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	configurationv1connect "github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"go.yaml.in/yaml/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

type nha148TLSFixture struct {
	ctx                context.Context
	data, path         string
	roots              *x509.CertPool
	run                *nha162Running
	runtime            *observationRuntime
	integrationHandler *integrationHandler
	snapshotsMu        sync.Mutex
	snapshots          map[string]map[string]*cfg.IntegrationInstance
	api                configurationv1connect.IntegrationServiceClient
	documents          configurationv1connect.ConfigurationServiceClient
	authentication     apiv2connect.AuthServiceClient
	accounts           apiv2connect.AccountServiceClient
	token              string
	tokens             *cfg.SessionTokens
}

func nha148StartTLS(t *testing.T) *nha148TLSFixture {
	t.Helper()
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	settings := nha181Config(t, cluster.Port, data, "127.0.0.1:0", nha181TLSYAML())
	captured, err := settings.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "configuration.yaml")
	if err := os.WriteFile(path, captured.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	t.Cleanup(cancel)
	f := &nha148TLSFixture{ctx: ctx, data: data, path: path, roots: roots}
	f.start(t)
	nha162RunBootstrap(t, ctx, data, "NHA148 admin", "NHA148 synthetic isolated password")
	f.login(t)
	return f
}
func (f *nha148TLSFixture) start(t *testing.T) {
	t.Helper()
	startup, err := config.LoadStartupPaths(f.path, config.DefaultBootstrapChoice(), nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	runctx, cancel := context.WithCancel(context.Background())
	run := &nha162Running{stop: cancel, done: make(chan error, 1)}
	ready := make(chan net.Addr, 1)
	owners := make(chan nha162ObservedRun, 1)
	runtime := make(chan *observationRuntime, 1)
	dependencies := runDependencies{observeLists: func(lists *listComposition, sessions *sessionCoordinator) {
		owners <- nha162ObservedRun{lists, sessions}
	}, observeRuntime: func(actual *observationRuntime) { runtime <- actual }, observeIntegrationAdministration: func(handler *integrationHandler) { f.integrationHandler = handler }}
	go func() {
		run.done <- runWithDependencies(runctx, startup, (&net.ListenConfig{}).Listen, func(address net.Addr) { ready <- address }, nil, dependencies)
	}()
	t.Cleanup(func() {
		if !run.joined {
			if err := run.join(t); err != nil {
				t.Error(err)
			}
		}
	})
	select {
	case run.address = <-ready:
	case err := <-run.done:
		run.joined = true
		t.Fatal("actual default Run", err)
	case <-time.After(12 * time.Second):
		t.Fatal("Run readiness deadline")
	}
	run.observed = <-owners
	f.runtime = <-runtime
	f.run = run
	client := nha162RunClient(t, f.roots, run.address)
	base := "https://" + run.address.String()
	f.api = configurationv1connect.NewIntegrationServiceClient(client, base)
	f.documents = configurationv1connect.NewConfigurationServiceClient(client, base)
	f.authentication = apiv2connect.NewAuthServiceClient(client, base)
	f.accounts = apiv2connect.NewAccountServiceClient(client, base)
}
func (f *nha148TLSFixture) login(t *testing.T) {
	t.Helper()
	f.tokens = nha162RunLogin(t, f.ctx, f.authentication, "NHA148 admin", "NHA148 synthetic isolated password")
	f.token = f.tokens.AccessToken
}
func (f *nha148TLSFixture) read(t *testing.T) *cfg.ConfigurationDocument {
	t.Helper()
	r, err := f.documents.GetConfigurationDocument(f.ctx, nha056SessionRequest(&cfg.GetConfigurationDocumentRequest{Name: nha203DocumentName(t, f.ctx, f.run, "operational.main")}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	instances := make(map[string]*cfg.IntegrationInstance)
	pageToken := ""
	for {
		page, err := f.api.ListIntegrationInstances(f.ctx, nha056SessionRequest(&cfg.ListIntegrationInstancesRequest{Parent: nha203HomeName(t, f.ctx, f.run), PageToken: pageToken}, f.token))
		if err != nil {
			t.Fatal("capture instance etags through v2", err)
		}
		for _, item := range page.Msg.IntegrationInstances {
			instances[item.InstanceKey] = proto.Clone(item).(*cfg.IntegrationInstance)
		}
		pageToken = page.Msg.NextPageToken
		if pageToken == "" {
			break
		}
	}
	f.snapshotsMu.Lock()
	if f.snapshots == nil {
		f.snapshots = make(map[string]map[string]*cfg.IntegrationInstance)
	}
	f.snapshots[nha148SnapshotKey(t, r.Msg)] = instances
	f.snapshotsMu.Unlock()
	return r.Msg
}

func nha148SnapshotKey(t *testing.T, document *cfg.ConfigurationDocument) string {
	t.Helper()
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	return fmt.Sprintf("%x", digest)
}
func (f *nha148TLSFixture) captured(t *testing.T, document *cfg.ConfigurationDocument, id string) *cfg.IntegrationInstance {
	t.Helper()
	key := nha148SnapshotKey(t, document)
	f.snapshotsMu.Lock()
	defer f.snapshotsMu.Unlock()
	value := f.snapshots[key][id]
	if value == nil {
		t.Fatal("v2 fixture lacks original instance capture", id)
	}
	return proto.Clone(value).(*cfg.IntegrationInstance)
}
func (f *nha148TLSFixture) typeName(t *testing.T, key string) string {
	t.Helper()
	home, err := apitypes.ParseHomeName(nha203HomeName(t, f.ctx, f.run))
	if err != nil {
		t.Fatal(err)
	}
	name, err := integrationTypeName(home, key)
	if err != nil {
		t.Fatal(err)
	}
	return name.String()
}
func (f *nha148TLSFixture) instanceName(t *testing.T, key string) string {
	t.Helper()
	home, ok := any(f.run.observed.lists.registry).(administrationHomeIdentity)
	if !ok {
		t.Fatal("home owner")
	}
	name, err := (administrationNames{home: home, store: f.runtime.store}).name(f.ctx, storage.APIIntegrationInstances, key)
	if err != nil {
		t.Fatal(err)
	}
	return name
}
func (f *nha148TLSFixture) createRequest(t *testing.T, typ, id string, state cfg.IntegrationDesiredState, configuration *cfg.IntegrationConfiguration) *cfg.CreateIntegrationInstanceRequest {
	t.Helper()
	return &cfg.CreateIntegrationInstanceRequest{Parent: nha203HomeName(t, f.ctx, f.run), IntegrationInstance: &cfg.IntegrationInstance{InstanceKey: id, IntegrationType: f.typeName(t, typ), DesiredState: state, Configuration: &cfg.IntegrationInstance_Editable{Editable: configuration}}}
}
func (f *nha148TLSFixture) updateRequest(t *testing.T, before *cfg.ConfigurationDocument, id string, configuration *cfg.IntegrationConfiguration) *cfg.UpdateIntegrationInstanceRequest {
	t.Helper()
	value := f.captured(t, before, id)
	value.Configuration = &cfg.IntegrationInstance_Editable{Editable: configuration}
	return &cfg.UpdateIntegrationInstanceRequest{IntegrationInstance: value, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"editable"}}}
}
func (f *nha148TLSFixture) desiredRequest(t *testing.T, before *cfg.ConfigurationDocument, id string, state cfg.IntegrationDesiredState) *cfg.UpdateIntegrationInstanceRequest {
	t.Helper()
	value := f.captured(t, before, id)
	value.DesiredState = state
	return &cfg.UpdateIntegrationInstanceRequest{IntegrationInstance: value, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"desired_state"}}}
}
func (f *nha148TLSFixture) deleteRequest(t *testing.T, before *cfg.ConfigurationDocument, id string) *cfg.DeleteIntegrationInstanceRequest {
	t.Helper()
	value := f.captured(t, before, id)
	return &cfg.DeleteIntegrationInstanceRequest{Name: value.Name, Etag: value.Etag}
}
func nha148Published(operation *longrunningpb.Operation) bool {
	if operation == nil || !operation.Done || operation.GetResponse() == nil {
		return false
	}
	metadata := new(cfg.IntegrationOperationMetadata)
	return anypb.UnmarshalTo(operation.Metadata, metadata, proto.UnmarshalOptions{}) == nil && metadata.Phase == cfg.IntegrationOperationPhase_INTEGRATION_OPERATION_PHASE_PUBLISHED
}

func nha148OperationFailure(operation *longrunningpb.Operation) error {
	if operation == nil || operation.GetError() == nil {
		return fmt.Errorf("expected a completed native operation")
	}
	fault, err := apierrors.AdmitStatus(operation.GetError())
	if err != nil {
		return err
	}
	return apiRPCError(context.Background(), fault)
}

func nha148Workday(zone string) *cfg.IntegrationConfiguration {
	return &cfg.IntegrationConfiguration{SchemaVersion: "homeward.integration/workday/1", YamlSource: []byte("timezone: " + zone + "\n")}
}
func (f *nha148TLSFixture) createWorkday(t *testing.T) {
	t.Helper()
	r, err := f.api.CreateIntegrationInstance(f.ctx, nha056SessionRequest(f.createRequest(t, "workday", "homeward-workday-v1", cfg.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_DISABLED, nha148Workday("America/Toronto")), f.token))
	if err != nil || !nha148Published(r.Msg) {
		t.Fatal("actual CRUD create", err)
	}
}
func (f *nha148TLSFixture) get(t *testing.T, id string) *cfg.IntegrationInstance {
	t.Helper()
	r, err := f.api.GetIntegrationInstance(f.ctx, nha056SessionRequest(&cfg.GetIntegrationInstanceRequest{Name: f.instanceName(t, id)}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	return r.Msg
}
func (f *nha148TLSFixture) set(t *testing.T, id string, state cfg.IntegrationDesiredState) {
	t.Helper()
	r, err := f.api.UpdateIntegrationInstance(f.ctx, nha056SessionRequest(f.desiredRequest(t, f.read(t), id, state), f.token))
	if err != nil || !nha148Published(r.Msg) {
		t.Fatal("actual desired state publication", err)
	}
}
func (f *nha148TLSFixture) unchanged(t *testing.T, before *cfg.ConfigurationDocument, raw []byte) {
	t.Helper()
	after := f.read(t)
	actual, err := os.ReadFile(f.path)
	if err != nil || !bytes.Equal(raw, actual) || !proto.Equal(before.Version, after.Version) || !proto.Equal(before.ActiveGeneration, after.ActiveGeneration) {
		t.Fatal("refused mutation changed source or generation", err)
	}
}

func TestNHA148ActualRunTLSCatalogueIsDeclaredAndUnknownTypeHasNoFallback(t *testing.T) {
	f := nha148StartTLS(t)
	r, err := f.api.ListIntegrationTypes(f.ctx, nha056SessionRequest(&cfg.ListIntegrationTypesRequest{Parent: nha203HomeName(t, f.ctx, f.run)}, f.token))
	if err != nil || len(r.Msg.IntegrationTypes) != 10 {
		t.Fatal("catalogue", err)
	}
	expected := map[string]bool{"android_tv": true, "sonos": true, "ecobee": true, "mqtt": true, "neviweb": true, "hilo": true, "solar": true, "workday": true, "energy": true, "derived_power": true}
	for _, typ := range r.Msg.IntegrationTypes {
		digest := sha256.Sum256(typ.JsonSchema)
		if !expected[typ.TypeKey] || !bytes.Equal(digest[:], typ.SchemaSha256) || typ.ManifestVersion == "" || typ.SchemaVersion == "" || len(typ.CapabilitySchemaIds) == 0 {
			t.Fatal("manifest was invented or incomplete")
		}
		delete(expected, typ.TypeKey)
	}
	for _, unknown := range []string{"foreign-nha136", "calendar", "arbitrary-executable"} {
		_, err := f.api.GetIntegrationType(f.ctx, nha056SessionRequest(&cfg.GetIntegrationTypeRequest{Name: f.typeName(t, unknown)}, f.token))
		if connect.CodeOf(err) != connect.CodeNotFound {
			t.Fatal("unknown type fallback", err)
		}
	}
	before := f.read(t)
	raw, _ := os.ReadFile(f.path)
	_, err = f.api.CreateIntegrationInstance(f.ctx, nha056SessionRequest(f.createRequest(t, "foreign-nha136", "nha148-unknown", cfg.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_DISABLED, nha148Workday("America/Toronto")), f.token))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatal(err)
	}
	f.unchanged(t, before, raw)
}
func TestNHA148ActualRunTLSCRUDCASDirtyAndInvalidKeepActive(t *testing.T) {
	f := nha148StartTLS(t)
	f.createWorkday(t)
	before := f.read(t)
	start := make(chan struct{})
	answers := make(chan error, 2)
	var group sync.WaitGroup
	for _, zone := range []string{"America/Montreal", "Europe/Paris"} {
		group.Add(1)
		go func(zone string) {
			defer group.Done()
			<-start
			r, err := f.api.UpdateIntegrationInstance(f.ctx, nha056SessionRequest(f.updateRequest(t, before, "homeward-workday-v1", nha148Workday(zone)), f.token))
			if err == nil && !nha148Published(r.Msg) {
				err = nha148OperationFailure(r.Msg)
			}
			answers <- err
		}(zone)
	}
	close(start)
	group.Wait()
	close(answers)
	wins, conflicts := 0, 0
	for err := range answers {
		if err == nil {
			wins++
		} else if connect.CodeOf(err) == connect.CodeAborted {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatal("CAS did not have exactly one winner")
	}
	before = f.read(t)
	raw, _ := os.ReadFile(f.path)
	_, err := f.api.UpdateIntegrationInstance(f.ctx, nha056SessionRequest(f.updateRequest(t, before, "homeward-workday-v1", nha148Workday("invalid/timezone")), f.token))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatal("invalid schema not rejected", err)
	}
	f.unchanged(t, before, raw)
	dirty := bytes.Replace(raw, []byte("127.0.0.1:0"), []byte("127.0.0.1:1"), 1)
	if bytes.Equal(dirty, raw) {
		t.Fatal("fixture did not edit unrelated launch field")
	}
	if os.WriteFile(f.path, dirty, 0600) != nil {
		t.Fatal("edit")
	}
	baseDirty := f.read(t)
	_, err = f.api.UpdateIntegrationInstance(f.ctx, nha056SessionRequest(f.updateRequest(t, baseDirty, "homeward-workday-v1", nha148Workday("America/Toronto")), f.token))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatal("domain update published an unrelated dirty edit", err)
	}
	preserved, _ := os.ReadFile(f.path)
	if !bytes.Equal(preserved, dirty) {
		t.Fatal("refused domain update changed unrelated workspace edit")
	}
	before = f.read(t)
	bad := []byte("workday: [\n")
	if os.WriteFile(f.path, bad, 0600) != nil {
		t.Fatal("edit")
	}
	badBase := f.read(t)
	_, err = f.api.UpdateIntegrationInstance(f.ctx, nha056SessionRequest(f.desiredRequest(t, badBase, "homeward-workday-v1", cfg.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_ENABLED), f.token))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatal("invalid dirty source not diagnosed", err)
	}
	f.unchanged(t, before, bad)
	fixed, err := f.documents.PublishConfigurationDocument(f.ctx, nha056SessionRequest(&cfg.PublishConfigurationDocumentRequest{Name: badBase.Name, Expected: badBase.Version, ExpectedWorkspaceSha256: badBase.GetWorkspace().SourceSha256, ExactYamlSource: preserved}, f.token))
	if err != nil || !fixed.Msg.Done {
		t.Fatal("full149 correction", err)
	}
	removed, err := f.api.DeleteIntegrationInstance(f.ctx, nha056SessionRequest(f.deleteRequest(t, f.read(t), "homeward-workday-v1"), f.token))
	if err != nil || !nha148Published(removed.Msg) {
		t.Fatal("CRUD delete", err)
	}
	_, err = f.api.GetIntegrationInstance(f.ctx, nha056SessionRequest(&cfg.GetIntegrationInstanceRequest{Name: f.instanceName(t, "homeward-workday-v1")}, f.token))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatal("deleted instance survived", err)
	}
	final, _ := os.ReadFile(f.path)
	if bytes.Contains(final, []byte("disabled_integrations")) {
		t.Fatal("delete retained orphan desired state")
	}
}
func TestNHA148ActualRunTLS149And148ShareOneCAS(t *testing.T) {
	f := nha148StartTLS(t)
	f.createWorkday(t)
	before := f.read(t)
	raw, _ := os.ReadFile(f.path)
	start := make(chan struct{})
	answers := make(chan error, 2)
	go func() {
		<-start
		_, err := f.documents.PublishConfigurationDocument(f.ctx, nha056SessionRequest(&cfg.PublishConfigurationDocumentRequest{Name: before.Name, Expected: before.Version, ExpectedWorkspaceSha256: before.GetWorkspace().SourceSha256, ExactYamlSource: append(raw, []byte("# concurrent149\n")...)}, f.token))
		answers <- err
	}()
	go func() {
		<-start
		response, err := f.api.UpdateIntegrationInstance(f.ctx, nha056SessionRequest(f.updateRequest(t, before, "homeward-workday-v1", nha148Workday("America/Montreal")), f.token))
		if err == nil && !nha148Published(response.Msg) {
			err = nha148OperationFailure(response.Msg)
		}
		answers <- err
	}()
	close(start)
	wins, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		err := <-answers
		if err == nil {
			wins++
		} else if connect.CodeOf(err) == connect.CodeAborted {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatal("148 and149 did not serialize on one authority")
	}
}
func nha148MQTTConfiguration(address string) *cfg.IntegrationConfiguration {
	return &cfg.IntegrationConfiguration{SchemaVersion: "homeward.integration/mqtt/1", YamlSource: []byte(fmt.Sprintf("profile: '5'\nbroker: mqtt://%s\ninstance: nha148-mqtt\nmode: passive\nsubscriptions: [homeassistant/#]\n", address)), Secrets: []*cfg.IntegrationSecretReference{{Field: "username_secret", InstanceKey: "nha148-mqtt", SecretKey: "user-ref"}, {Field: "password_secret", InstanceKey: "nha148-mqtt", SecretKey: "password-ref"}}}
}
func (f *nha148TLSFixture) seed(t *testing.T, name, value string) {
	t.Helper()
	repo, err := f.runtime.store.BootstrapSecrets(f.ctx, f.data)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := storage.NewSecretValue([]byte(value))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RotateCredential(f.ctx, nha051ServerHandle(t, "nha148-mqtt", name), secret); err != nil {
		t.Fatal(err)
	}
}
func TestNHA148ActualRunTLSDisabledRestartReadyFailureAndReactivation(t *testing.T) {
	broker := testbroker.Start(t)
	f := nha148StartTLS(t)
	f.seed(t, "user-ref", testbroker.PilotUser)
	f.seed(t, "password-ref", testbroker.PilotPassword)
	created, err := f.api.CreateIntegrationInstance(f.ctx, nha056SessionRequest(f.createRequest(t, "mqtt", "nha148-mqtt", cfg.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_DISABLED, nha148MQTTConfiguration(broker.Address)), f.token))
	if err != nil || !nha148Published(created.Msg) {
		t.Fatal(err)
	}
	instance := f.get(t, "nha148-mqtt")
	if instance.DesiredState != cfg.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_DISABLED || instance.GetNotSupervised() == nil || instance.GetNoHealthReport() == nil {
		t.Fatal("disabled configuration started a pilot")
	}
	if err := f.run.join(t); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	f.login(t)
	if f.get(t, "nha148-mqtt").DesiredState != cfg.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_DISABLED {
		t.Fatal("restart lost persisted desired state")
	}
	f.set(t, "nha148-mqtt", cfg.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_ENABLED)
	nha042ServerWait(t, "actual MQTT ready report", func() bool {
		i := f.get(t, "nha148-mqtt")
		return i.GetSupervision() != nil && i.GetSupervision().Lifecycle == cfg.IntegrationLifecycle_INTEGRATION_LIFECYCLE_RUNNING && i.GetHealthReport() != nil && i.GetHealthReport().Status == cfg.IntegrationHealthStatus_INTEGRATION_HEALTH_STATUS_READY
	})
	f.set(t, "nha148-mqtt", cfg.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_DISABLED)
	if i := f.get(t, "nha148-mqtt"); i.GetSupervision() != nil && i.GetSupervision().Lifecycle == cfg.IntegrationLifecycle_INTEGRATION_LIFECYCLE_RUNNING {
		t.Fatal("disabled pilot still running")
	}
	f.seed(t, "password-ref", "NHA148_SYNTHETIC_WRONG_PASSWORD")
	f.set(t, "nha148-mqtt", cfg.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_ENABLED)
	nha042ServerWait(t, "failed pilot typed degradation", func() bool {
		i := f.get(t, "nha148-mqtt")
		return i.GetSupervision() != nil && i.GetSupervision().Lifecycle == cfg.IntegrationLifecycle_INTEGRATION_LIFECYCLE_DEGRADED
	})
	failed := f.get(t, "nha148-mqtt")
	if failed.GetHealthReport() != nil && failed.GetHealthReport().Status == cfg.IntegrationHealthStatus_INTEGRATION_HEALTH_STATUS_READY {
		t.Fatal("replacement reused previous session Ready")
	}
	f.set(t, "nha148-mqtt", cfg.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_DISABLED)
	f.seed(t, "password-ref", testbroker.PilotPassword)
	f.set(t, "nha148-mqtt", cfg.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_ENABLED)
	nha042ServerWait(t, "new session ready", func() bool {
		i := f.get(t, "nha148-mqtt")
		return i.GetHealthReport() != nil && i.GetHealthReport().Status == cfg.IntegrationHealthStatus_INTEGRATION_HEALTH_STATUS_READY
	})
	digest := sha256.Sum256([]byte("nha148-mqtt"))
	clientID := fmt.Sprintf("homeward-%x", digest[:12])
	if broker.PublishCount(clientID) != 0 {
		t.Fatal("activation or reconnect produced application action output")
	}
}
func TestNHA148ActualRunTLSReferencesOnlyAndHostProviderDeniesForeign(t *testing.T) {
	f := nha148StartTLS(t)
	before := f.read(t)
	raw, _ := os.ReadFile(f.path)
	for _, configChange := range []func(*cfg.IntegrationConfiguration){func(c *cfg.IntegrationConfiguration) {
		c.YamlSource = append(c.YamlSource, []byte("password: NHA148_PLAINTEXT_CANARY\n")...)
	}, func(c *cfg.IntegrationConfiguration) { c.Secrets[1].InstanceKey = "foreign" }, func(c *cfg.IntegrationConfiguration) {
		c.YamlSource = append(c.YamlSource, []byte("password_secret: NHA148_PLAINTEXT_CANARY\n")...)
	}} {
		configuration := nha148MQTTConfiguration("127.0.0.1:1")
		configChange(configuration)
		_, err := f.api.CreateIntegrationInstance(f.ctx, nha056SessionRequest(f.createRequest(t, "mqtt", "nha148-mqtt", cfg.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_DISABLED, configuration), f.token))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || strings.Contains(err.Error(), "NHA148_PLAINTEXT_CANARY") {
			t.Fatal("plaintext or foreign ref admitted/leaked", err)
		}
		f.unchanged(t, before, raw)
	}
	f.seed(t, "user-ref", "NHA148_SYNTHETIC_PROVIDER_VALUE")
	instance, _ := domain.ParseIntegrationInstanceID("nha148-secret-reader")
	session, err := f.runtime.host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := session.Secrets()
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Read(f.ctx, nha051ServerHandle(t, "nha148-mqtt", "user-ref"))
	if err != integrations.ErrForeignSecretHandle {
		t.Fatal("actual composed provider crossed instance boundary", err)
	}
	if err := f.runtime.host.Revoke(session); err != nil {
		t.Fatal(err)
	}
	_, err = provider.Read(f.ctx, nha051ServerHandle(t, "nha148-secret-reader", "absent"))
	if err != integrations.ErrInactiveSession {
		t.Fatal("retired provider still admitted", err)
	}
	var denied int
	err = f.runtime.store.InTransaction(f.ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(f.ctx, `SELECT count(*) FROM public.secret_access_audit WHERE requesting_instance=$1 AND reason='foreign_handle'`, "nha148-secret-reader").Scan(&denied)
	})
	if err != nil || denied != 1 {
		t.Fatal("actual foreign read denial lost audit", err)
	}
}
func TestNHA148ActualRunTLSUserOpaqueBearerAndRevokedSessionCannotMutate(t *testing.T) {
	f := nha148StartTLS(t)
	f.createWorkday(t)
	created, err := f.accounts.CreateAccount(f.ctx, nha056SessionRequest(&cfg.CreateAccountRequest{Parent: nha203SessionAccount(t, f.tokens.Session).Home().String(), Account: &cfg.Account{LoginName: "NHA148 user", DisplayName: "NHA148 user", Role: cfg.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: "NHA148 synthetic user password"}}, f.token))
	if err != nil || created.Msg == nil {
		t.Fatal(err)
	}
	user := nha162RunLogin(t, f.ctx, f.authentication, "NHA148 user", "NHA148 synthetic user password").AccessToken
	//145 has not delivered a MCP issuer or validation repository on this base.
	// This controlled value is deliberately NOT claimed to be an issued key.
	// Native Connect must refuse a non-session bearer; the shared-service test
	// separately proves the admitted scoped principal's exact ceiling and role.
	const opaqueBearer = "NHA148_CONTROLLED_NON_NATIVE_OPAQUE_BEARER"
	before := f.read(t)
	raw, _ := os.ReadFile(f.path)
	for _, test := range []struct {
		token string
		code  connect.Code
	}{{user, connect.CodePermissionDenied}, {opaqueBearer, connect.CodeUnauthenticated}, {"invalid-bearer", connect.CodeUnauthenticated}} {
		_, err := f.api.UpdateIntegrationInstance(f.ctx, nha056SessionRequest(f.desiredRequest(t, before, "homeward-workday-v1", cfg.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_ENABLED), test.token))
		if connect.CodeOf(err) != test.code {
			t.Fatal("wrong Connect refusal", err)
		}
		f.unchanged(t, before, raw)
	}
	if _, err := f.authentication.RevokeSession(f.ctx, nha056SessionRequest(&cfg.RevokeSessionRequest{Name: f.tokens.Session.Name}, f.token)); err != nil {
		t.Fatal(err)
	}
	_, err = f.api.DeleteIntegrationInstance(f.ctx, nha056SessionRequest(f.deleteRequest(t, before, "homeward-workday-v1"), f.token))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("revoked credential mutated", err)
	}
	actual, _ := os.ReadFile(f.path)
	if !bytes.Equal(raw, actual) {
		t.Fatal("revoked mutation changed source")
	}
}

func TestNHA148ActualRunTLSOpaqueWorkerKeepsProtectedArgumentsAndNoManifest(t *testing.T) {
	f := nha148StartTLS(t)
	before := f.read(t)
	raw, _ := os.ReadFile(f.path)
	raw = append(raw, []byte("external_workers:\n  - instance: nha148-opaque\n    executable: /usr/bin/true\n    args: [NHA148_PRIVATE_OPAQUE_ARGUMENT]\n    secret_names: [opaque-ref]\ndisabled_integrations: [nha148-opaque]\n")...)
	written, err := f.documents.PublishConfigurationDocument(f.ctx, nha056SessionRequest(&cfg.PublishConfigurationDocumentRequest{Name: before.Name, Expected: before.Version, ExpectedWorkspaceSha256: before.GetWorkspace().SourceSha256, ExactYamlSource: raw}, f.token))
	if err != nil || !written.Msg.Done {
		t.Fatal("existing149 declared opaque config", err)
	}
	item := f.get(t, "nha148-opaque")
	encoded, err := proto.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if item.IntegrationType != "" || item.GetOpaque() == nil || len(item.GetOpaque().Secrets) != 1 || bytes.Contains(encoded, []byte("NHA148_PRIVATE_OPAQUE_ARGUMENT")) || bytes.Contains(encoded, []byte("/usr/bin/true")) {
		t.Fatal("opaque worker exposed private args or invented a manifest")
	}
	deleted, err := f.api.DeleteIntegrationInstance(f.ctx, nha056SessionRequest(f.deleteRequest(t, f.read(t), "nha148-opaque"), f.token))
	if err != nil || !nha148Published(deleted.Msg) {
		t.Fatal("targeted opaque delete", err)
	}
}

// Reopen the real authenticated active source, separately from the physical
// workspace. Public List/Get must leave both exact representations unchanged.
func (f *nha148TLSFixture) protectedSource(t *testing.T) []byte {
	t.Helper()
	active, present, err := f.runtime.store.ReadActiveConfig(f.ctx)
	if err != nil || !present {
		t.Fatal("active SQL configuration", err)
	}
	secrets, err := f.runtime.store.BootstrapSecrets(f.ctx, f.data)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := storage.NewConfigReplayRepository(f.runtime.store, secrets)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range active.Versions() {
		kind, known := version.Kind()
		if known && kind == storage.ConfigDocumentKindOperationalYAML {
			source, err := replay.ReadActiveProtectedSource(f.ctx, active.Revision(), active.ManifestSHA256(), version.Ref())
			if err != nil {
				t.Fatal("authenticated active operational source", err)
			}
			return source.Bytes()
		}
	}
	t.Fatal("active operational source missing")
	return nil
}

func (f *nha148TLSFixture) writeExactOperational(t *testing.T, raw []byte) {
	t.Helper()
	if _, err := config.ParseOperationalWithEnv(raw, func(string) (string, bool) { return "", false }); err != nil {
		t.Fatal("fixture must pass the shared operational parser", err)
	}
	before := f.read(t)
	written, err := f.documents.PublishConfigurationDocument(f.ctx, nha056SessionRequest(&cfg.PublishConfigurationDocumentRequest{Name: before.Name, Expected: before.Version, ExpectedWorkspaceSha256: before.GetWorkspace().SourceSha256, ExactYamlSource: raw}, f.token))
	if err != nil || !written.Msg.Done {
		t.Fatal("real149 exact source publication", err)
	}
	f.exactOperational(t, raw)
}

func (f *nha148TLSFixture) exactOperational(t *testing.T, expected []byte) {
	t.Helper()
	physical, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal("read physical operational source", err)
	}
	for _, source := range []struct {
		name string
		raw  []byte
	}{{"physical", physical}, {"authenticated protected", f.protectedSource(t)}} {
		if !bytes.Equal(source.raw, expected) {
			first := 0
			for first < len(expected) && first < len(source.raw) && expected[first] == source.raw[first] {
				first++
			}
			t.Fatalf("%s source differs: first_delta=%d expected_len=%d actual_len=%d expected_sha256=%x actual_sha256=%x", source.name, first, len(expected), len(source.raw), sha256.Sum256(expected), sha256.Sum256(source.raw))
		}
	}
	digest := sha256.Sum256(expected)
	current := f.read(t)
	if !bytes.Equal(current.Version.SourceSha256, digest[:]) || !bytes.Equal(current.GetWorkspace().SourceSha256, digest[:]) || current.GetWorkspace().Dirty != cfg.DirtyState_DIRTY_STATE_CLEAN {
		t.Fatal("source, workspace hash or clean state differs from exact expected bytes")
	}
}

func TestNHA148ActualRunTLSNullSlicesReadAndCASMatchSharedParser(t *testing.T) {
	f := nha148StartTLS(t)
	initial, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, nulls := range []string{
		"external_workers: null\n",
		"disabled_integrations: null\n",
		"external_workers: ~\ndisabled_integrations: !!null null\n",
		"external_workers:\ndisabled_integrations: ~\n",
	} {
		raw := append(bytes.Clone(initial), []byte("workday: {timezone: America/Toronto}\n"+nulls)...)
		f.writeExactOperational(t, raw)
		before := f.read(t)
		listed, err := f.api.ListIntegrationInstances(f.ctx, nha056SessionRequest(&cfg.ListIntegrationInstancesRequest{Parent: nha203HomeName(t, f.ctx, f.run)}, f.token))
		if err != nil || len(listed.Msg.IntegrationInstances) != 1 || listed.Msg.IntegrationInstances[0].InstanceKey != "homeward-workday-v1" || listed.Msg.IntegrationInstances[0].DesiredState != cfg.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_ENABLED {
			t.Fatal("List rejected admitted null slices or invented an opaque worker", err)
		}
		got := f.get(t, "homeward-workday-v1")
		if !proto.Equal(got.GetEditable(), listed.Msg.IntegrationInstances[0].GetEditable()) || got.DesiredState != listed.Msg.IntegrationInstances[0].DesiredState {
			t.Fatal("Get/List did not preserve the admitted null document snapshot")
		}
		f.unchanged(t, before, raw)
		f.exactOperational(t, raw)
		var expectedDocument yaml.Node
		if err := yaml.Unmarshal(raw, &expectedDocument); err != nil {
			t.Fatal(err)
		}
		// UpdateInstance replaces its targeted subtree with the supplied public
		// fragment. Its declared block representation is intentional; the old
		// flow style belongs only to the replaced subtree, not unrelated data.
		var requestedWorkday yaml.Node
		if err := yaml.Unmarshal([]byte("timezone: America/Montreal\n"), &requestedWorkday); err != nil {
			t.Fatal(err)
		}
		expectedRoot := expectedDocument.Content[0]
		for i := 0; i < len(expectedRoot.Content); i += 2 {
			switch expectedRoot.Content[i].Value {
			case "workday":
				expectedRoot.Content[i+1] = requestedWorkday.Content[0]
			case "disabled_integrations":
				// An empty desired-state selection has the existing canonical
				// omission form. Every unrelated node, including worker null,
				// must remain in the exact expected candidate.
				expectedRoot.Content = append(expectedRoot.Content[:i], expectedRoot.Content[i+2:]...)
				i -= 2
			}
		}
		expected, err := yaml.Marshal(&expectedDocument)
		if err != nil {
			t.Fatal(err)
		}
		result, err := f.api.UpdateIntegrationInstance(f.ctx, nha056SessionRequest(f.updateRequest(t, before, "homeward-workday-v1", nha148Workday("America/Montreal")), f.token))
		if err != nil || !nha148Published(result.Msg) || f.read(t).Version.Revision <= before.Version.Revision {
			t.Fatal("targeted148 CAS rejected a document admitted by149", err)
		}
		f.exactOperational(t, expected)
		after := f.read(t)
		mutated, err := os.ReadFile(f.path)
		if err != nil || !bytes.Contains(f.get(t, "homeward-workday-v1").GetEditable().YamlSource, []byte("America/Montreal")) {
			t.Fatal("actual148 update did not publish the requested timezone", err)
		}
		_, err = f.api.UpdateIntegrationInstance(f.ctx, nha056SessionRequest(f.updateRequest(t, before, "homeward-workday-v1", nha148Workday("America/Toronto")), f.token))
		if connect.CodeOf(err) != connect.CodeAborted {
			t.Fatal("null-compatible mutation lost the shared149 stale CAS refusal", err)
		}
		f.unchanged(t, after, mutated)
		f.exactOperational(t, mutated)
	}
}

func TestNHA148ActualRunTLSPrivateCommentsNeverProjectAndCASPreservesSource(t *testing.T) {
	f := nha148StartTLS(t)
	initial, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	// All destinations are isolated or disabled. The nested subscriptions and
	// sibling scalars exercise recursive public projection, not only its root.
	fragment := `mqtt:
    # NHA148_PRIVATE_CANARY_SIBLING_HEAD
    profile: '5' # NHA148_PRIVATE_CANARY_SIBLING_LINE
    broker: mqtt://127.0.0.1:1 # NHA148_PRIVATE_CANARY_BROKER_LINE
    instance: nha148-mqtt
    mode: passive
    username_secret: user-ref
    password_secret: password-ref
    subscriptions:
        # NHA148_PRIVATE_CANARY_CHILD_HEAD
        - homeassistant/# # NHA148_PRIVATE_CANARY_CHILD_LINE

        # NHA148_PRIVATE_CANARY_CHILD_FOOT

workday:
    # NHA148_PRIVATE_CANARY_WORKDAY_HEAD
    timezone: America/Toronto # password=NHA148_PRIVATE_CANARY_WORKDAY_LINE

    # NHA148_PRIVATE_CANARY_WORKDAY_FOOT

disabled_integrations:
    - nha148-mqtt
`
	var document yaml.Node
	if err := yaml.Unmarshal(append(bytes.Clone(initial), []byte(fragment)...), &document); err != nil {
		t.Fatal(err)
	}
	// Prove this fixture actually contains all three comment slots, including
	// nested sequence children. The eventual oracle uses real TLS List/Get.
	seen := map[string]bool{}
	deepComment := false
	var inspect func(*yaml.Node, int)
	inspect = func(node *yaml.Node, depth int) {
		for kind, comment := range map[string]string{"head": node.HeadComment, "line": node.LineComment, "foot": node.FootComment} {
			if strings.Contains(comment, "NHA148_PRIVATE_CANARY_") {
				seen[kind] = true
				deepComment = deepComment || depth >= 3
			}
		}
		for _, child := range node.Content {
			inspect(child, depth+1)
		}
	}
	inspect(document.Content[0], 0)
	if len(seen) != 3 || !deepComment {
		t.Fatal("fixture must cover HeadComment, LineComment, FootComment and nested children")
	}
	raw, err := yaml.Marshal(&document)
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"SIBLING_HEAD", "SIBLING_LINE", "BROKER_LINE", "CHILD_HEAD", "CHILD_LINE", "CHILD_FOOT", "WORKDAY_HEAD", "WORKDAY_LINE", "WORKDAY_FOOT"} {
		if !bytes.Contains(raw, []byte("NHA148_PRIVATE_CANARY_"+suffix)) {
			t.Fatal("fixture did not preserve a protected comment canary")
		}
	}
	f.writeExactOperational(t, raw)
	before := f.read(t)
	checkPublic := func() {
		t.Helper()
		listed, err := f.api.ListIntegrationInstances(f.ctx, nha056SessionRequest(&cfg.ListIntegrationInstancesRequest{Parent: nha203HomeName(t, f.ctx, f.run)}, f.token))
		if err != nil || len(listed.Msg.IntegrationInstances) != 2 {
			t.Fatal("real public list", err)
		}
		messages := []proto.Message{listed.Msg}
		for _, item := range listed.Msg.IntegrationInstances {
			got := f.get(t, item.InstanceKey)
			if !proto.Equal(item.GetEditable(), got.GetEditable()) || item.DesiredState != got.DesiredState {
				t.Fatal("List/Get public projections differ")
			}
			messages = append(messages, got)
		}
		for _, message := range messages {
			encoded, err := proto.Marshal(message)
			if err != nil || bytes.Contains(encoded, []byte("NHA148_PRIVATE_CANARY_")) {
				t.Fatal("real List/Get response leaked a protected YAML comment", err)
			}
		}
	}
	checkPublic()
	f.unchanged(t, before, raw)
	f.exactOperational(t, raw)
	oldSelection := []byte("disabled_integrations:\n    - nha148-mqtt\n")
	newSelection := []byte("disabled_integrations:\n    - homeward-workday-v1\n    - nha148-mqtt\n")
	if bytes.Count(raw, oldSelection) != 1 {
		t.Fatal("fixture's exact desired-state replacement is not unique")
	}
	expected := bytes.Replace(raw, oldSelection, newSelection, 1)
	result, err := f.api.UpdateIntegrationInstance(f.ctx, nha056SessionRequest(f.desiredRequest(t, before, "homeward-workday-v1", cfg.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_DISABLED), f.token))
	if err != nil || !nha148Published(result.Msg) || f.read(t).Version.Revision <= before.Version.Revision {
		t.Fatal("actual targeted desired-state CAS publication", err)
	}
	f.exactOperational(t, expected)
	if f.get(t, "homeward-workday-v1").DesiredState != cfg.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_DISABLED {
		t.Fatal("published desired state missing")
	}
	checkPublic()
	after := f.read(t)
	_, err = f.api.UpdateIntegrationInstance(f.ctx, nha056SessionRequest(f.desiredRequest(t, before, "homeward-workday-v1", cfg.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_ENABLED), f.token))
	if connect.CodeOf(err) != connect.CodeAborted {
		t.Fatal("stale comment-bearing mutation bypassed149 CAS", err)
	}
	f.unchanged(t, after, expected)
	f.exactOperational(t, expected)
}

func TestNHA148ActualRunTLSOpaqueWorkerNullSecretNamesMatchSharedParser(t *testing.T) {
	f := nha148StartTLS(t)
	initial, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	private := t.TempDir()
	marker := filepath.Join(private, "unexpected-execution")
	executable := filepath.Join(private, "must-not-run")
	// This local sentinel has no network or device capability. Operational
	// admission may stat it; a disabled worker must never launch it.
	script := []byte(fmt.Sprintf("#!/bin/sh\n: > %q\nexit 1\n", marker))
	if err := os.WriteFile(executable, script, 0700); err != nil {
		t.Fatal(err)
	}
	assertNeverStarted := func() {
		t.Helper()
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatal("disabled opaque worker executable was launched or marker cannot be checked", err)
		}
	}
	for _, names := range []string{"", "    secret_names: []\n", "    secret_names: null\n", "    secret_names: ~\n"} {
		fragment := fmt.Sprintf("external_workers:\n  - instance: nha148-opaque-null\n    executable: %q\n    args: [NHA148_PRIVATE_NULL_WORKER_ARGUMENT]\n%sdisabled_integrations: [nha148-opaque-null]\n", executable, names)
		raw := append(bytes.Clone(initial), []byte(fragment)...)
		f.writeExactOperational(t, raw)
		before := f.read(t)
		listed, err := f.api.ListIntegrationInstances(f.ctx, nha056SessionRequest(&cfg.ListIntegrationInstancesRequest{Parent: nha203HomeName(t, f.ctx, f.run)}, f.token))
		if err != nil || len(listed.Msg.IntegrationInstances) != 1 {
			t.Fatal("List rejected an admitted empty opaque secret-name slice", err)
		}
		item := listed.Msg.IntegrationInstances[0]
		got := f.get(t, "nha148-opaque-null")
		if item.InstanceKey != "nha148-opaque-null" || item.IntegrationType != "" || item.GetOpaque() == nil || len(item.GetOpaque().Secrets) != 0 || item.DesiredState != cfg.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_DISABLED || item.GetNotSupervised() == nil || item.GetNoHealthReport() == nil || !proto.Equal(item.GetOpaque(), got.GetOpaque()) {
			t.Fatal("empty opaque secrets acquired references, a manifest, runtime or a different snapshot")
		}
		for _, message := range []proto.Message{listed.Msg, got} {
			encoded, err := proto.Marshal(message)
			if err != nil || bytes.Contains(encoded, []byte(executable)) || bytes.Contains(encoded, []byte("NHA148_PRIVATE_NULL_WORKER_ARGUMENT")) {
				t.Fatal("opaque worker response exposed protected executable or arguments", err)
			}
		}
		f.unchanged(t, before, raw)
		f.exactOperational(t, raw)
		assertNeverStarted()
		// Mutate an unrelated integration through the same149 CAS. The opaque
		// subtree, including its exact null/empty representation, is retained.
		var expectedDocument yaml.Node
		if err := yaml.Unmarshal(raw, &expectedDocument); err != nil {
			t.Fatal(err)
		}
		expectedRoot := expectedDocument.Content[0]
		for i := 0; i < len(expectedRoot.Content); i += 2 {
			if expectedRoot.Content[i].Value == "disabled_integrations" {
				expectedRoot.Content[i+1] = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!str", Value: "homeward-workday-v1"}, {Kind: yaml.ScalarNode, Tag: "!!str", Value: "nha148-opaque-null"}}}
			}
		}
		var workday yaml.Node
		if err := yaml.Unmarshal([]byte("timezone: America/Toronto\n"), &workday); err != nil {
			t.Fatal(err)
		}
		expectedRoot.Content = append(expectedRoot.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "workday"}, workday.Content[0])
		expected, err := yaml.Marshal(&expectedDocument)
		if err != nil {
			t.Fatal(err)
		}
		created, err := f.api.CreateIntegrationInstance(f.ctx, nha056SessionRequest(f.createRequest(t, "workday", "homeward-workday-v1", cfg.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_DISABLED, nha148Workday("America/Toronto")), f.token))
		if err != nil || !nha148Published(created.Msg) || f.read(t).Version.Revision <= before.Version.Revision {
			t.Fatal("opaque null secret names blocked unrelated targeted CAS", err)
		}
		f.exactOperational(t, expected)
		after := f.read(t)
		_, err = f.api.DeleteIntegrationInstance(f.ctx, nha056SessionRequest(f.deleteRequest(t, before, "nha148-opaque-null"), f.token))
		if connect.CodeOf(err) != connect.CodeAborted {
			t.Fatal("opaque null secret names bypassed stale149 CAS", err)
		}
		f.unchanged(t, after, expected)
		f.exactOperational(t, expected)
		assertNeverStarted()
	}
	if err := f.run.join(t); err != nil {
		t.Fatal(err)
	}
	assertNeverStarted()
}

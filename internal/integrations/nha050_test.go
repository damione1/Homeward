package integrations_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/logicselection"
	"github.com/damione1/Homeward/internal/scenes"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/scope"
	"github.com/jackc/pgx/v5"
)

type nha050Rig struct {
	host       *integrations.Manager
	fixture    *fake.Fixture
	authority  *authorization.Authority
	ctx        context.Context
	actor      domain.ActorID
	account    contracts.AccountID
	instance   domain.IntegrationInstanceID
	keys       []integrations.SourceKey
	identities []integrations.ResolvedIdentity
	session    *integrations.Session
	bindings   []integrations.Binding
	gen        *nha022Generation
}

func nha050Policy(t *testing.T, account contracts.AccountID, read, write []domain.EntityID) authorization.PolicySource {
	t.Helper()
	id, _ := account.Value()
	var data strings.Builder
	fmt.Fprintf(&data, "schema: homeward.authorization/1\naccount_id: %q\ngrants:\n", id)
	for _, rule := range []struct {
		family, op string
		ids        []domain.EntityID
	}{{"state_registry", "read", read}, {"actions", "read", read}, {"actions", "write", write}} {
		fmt.Fprintf(&data, "  - scope: {family: %s, operation: %s}\n    objects: {entities: {explicit: [", rule.family, rule.op)
		for i, entity := range rule.ids {
			if i > 0 {
				data.WriteString(", ")
			}
			text, _ := entity.Value()
			fmt.Fprintf(&data, "{entity_id: %q, capabilities: {all: {}}}", text)
		}
		data.WriteString("]}}\n")
	}
	source, err := authorization.AdmitPolicySource(account, []byte(data.String()))
	if err != nil {
		t.Fatal(err)
	}
	return source
}
func nha050State(t *testing.T, source authorization.PolicySource, revision uint64) authorization.AccountState {
	t.Helper()
	hash, _ := source.SHA256()
	version, err := authorization.NewVersion(revision, hash)
	if err != nil {
		t.Fatal(err)
	}
	state, err := authorization.NewAccountState(source, authv1.AccountRole_ACCOUNT_ROLE_USER, authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE, 1, version, revision)
	if err != nil {
		t.Fatal(err)
	}
	return state
}
func (f *nha050Rig) policy(t *testing.T, read, write []domain.EntityID, revision uint64) {
	t.Helper()
	if err := f.authority.Publish(nha050State(t, nha050Policy(t, f.account, read, write), revision)); err != nil {
		t.Fatal(err)
	}
}
func nha050Setup(t *testing.T, driver integrations.CommandDriver) *nha050Rig {
	t.Helper()
	ctx := context.Background()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	account, _ := contracts.ParseAccountID("nha050-account")
	sessionID, _ := contracts.ParseSessionID("nha050-session")
	principal, err := authorization.NativePrincipal(account, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = authorization.WithPrincipal(ctx, principal)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := principal.Actor()
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha050-relay")
	first, _ := integrations.NewSourceKey("first", "relay")
	second, _ := integrations.NewSourceKey("second", "relay")
	keys := []integrations.SourceKey{first, second}
	ids, err := fixture.Resolve(ctx, instance, keys)
	if err != nil {
		t.Fatal(err)
	}
	authority := authorization.NewAuthority()
	all := []domain.EntityID{ids[0].Entity, ids[1].Entity}
	if err := authority.Hydrate([]authorization.AccountState{nha050State(t, nha050Policy(t, account, all, all), 1)}); err != nil {
		t.Fatal(err)
	}
	if err := host.AttachAuthorization(authority); err != nil {
		t.Fatal(err)
	}
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{nha022Record(t, ids[0], instance, keys[0], "switch.nha050_a", "switch.nha050_target", false), nha022Record(t, ids[1], instance, keys[1], "switch.nha050_b", "switch.nha050_b", false)}); err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	candidates := make([]integrations.Candidate, 0, 2)
	for _, key := range keys {
		candidate, err := integrations.NewCandidate(key, []*registry.CapabilityDescription{{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "relay", DisplayName: "NHA050 synthetic relay", Constraints: &registry.CapabilityDescription_Switch{Switch: &registry.SwitchConstraints{SetOn: true}}}})
		if err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, candidate)
	}
	bindings, err := session.Discover(ctx, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.AttachCommandDriver(session, driver); err != nil {
		t.Fatal(err)
	}
	f := &nha050Rig{host, fixture, authority, ctx, actor, account, instance, keys, ids, session, bindings, &nha022Generation{revision: 7}}
	f.publish(t, 0, false)
	f.publish(t, 1, false)
	return f
}
func (f *nha050Rig) publish(t *testing.T, index int, on bool) {
	t.Helper()
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	value := domain.NewSwitchValue(on)
	at := time.Now()
	observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.session.Publish(context.Background(), f.bindings[index], observation); err != nil {
		t.Fatal(err)
	}
}
func (f *nha050Rig) reassignAlias(t *testing.T) {
	t.Helper()
	if err := f.fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{nha022Record(t, f.identities[0], f.instance, f.keys[0], "switch.nha050_a", "switch.nha050_a", false), nha022Record(t, f.identities[1], f.instance, f.keys[1], "switch.nha050_b", "switch.nha050_target", false)}); err != nil {
		t.Fatal(err)
	}
}
func (f *nha050Rig) prepared(t *testing.T, service interface {
	CaptureDirect(context.Context, domain.HAAlias, integrations.DirectKind) (integrations.DirectCapture, error)
}, id string) integrations.PreparedDirect {
	t.Helper()
	alias, _ := domain.ParseHAAlias("switch.nha050_target")
	capture, err := service.CaptureDirect(f.ctx, alias, integrations.DirectSwitch)
	if err != nil {
		t.Fatal(err)
	}
	commandID, _ := domain.ParseCommandID(id)
	prepared, err := capture.PrepareSwitch(f.actor, commandID, time.Now().Add(time.Minute), true)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func TestNHA050VolatileStoredTargetSurvivesAliasReassignment(t *testing.T) {
	driver := &nha022Driver{}
	f := nha050Setup(t, driver)
	service, err := integrations.NewVolatileCommandService(f.host, f.gen, integrations.DefaultVolatileCommandLimits())
	if err != nil {
		t.Fatal(err)
	}
	prepared := f.prepared(t, service, "nha050-volatile-alias")
	first, err := service.DispatchPreparedDirect(f.ctx, prepared)
	if err != nil || first.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatal(first, err)
	}
	f.reassignAlias(t)
	a, b := f.identities[0].Entity, f.identities[1].Entity
	f.policy(t, []domain.EntityID{b}, []domain.EntityID{b}, 2)
	if result, found, err := service.RecordedPreparedDirect(f.ctx, prepared); result != nil || found || !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
		t.Fatal("old A result authorized through current B alias", result, found, err)
	}
	oversized, err := prepared.Request()
	if err != nil {
		t.Fatal(err)
	}
	oversized.ProtoReflect().SetUnknown(bytes.Repeat([]byte{0xa0, 0x06, 0x01}, 23000))
	for _, id := range []string{oversized.CommandId, "nha050-unknown-oversized-id"} {
		oversized.CommandId = id
		if result, err := service.Dispatch(f.ctx, f.actor, oversized); result != nil || !errors.Is(err, integrations.ErrCommandIntentTooLarge) {
			t.Fatal("oversized envelope revealed old forbidden ID", result, err)
		}
	}
	f.policy(t, []domain.EntityID{a}, []domain.EntityID{a}, 3)
	if result, found, err := service.RecordedPreparedDirect(f.ctx, prepared); err != nil || !found || result.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatal("permitted A result resolved current B", result, found, err)
	}
	if _, err := service.DispatchPreparedDirect(f.ctx, prepared); !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
		t.Fatal("prepared A was allowed to become B", err)
	}
	if driver.count() != 1 {
		t.Fatal("retained consultation resent a command", driver.count())
	}
}
func TestNHA050DurableStoredTargetRestartAndUnknownCommit(t *testing.T) {
	store, _ := nha024Store(t)
	driver := &nha022Driver{}
	f := nha050Setup(t, driver)
	service, err := integrations.NewDurableCommandService(f.ctx, f.host, f.gen, store)
	if err != nil {
		t.Fatal(err)
	}
	prepared := f.prepared(t, service, "nha050-durable-alias")
	first, err := service.DispatchPreparedDirect(f.ctx, prepared)
	if err != nil || first.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatal(first, err)
	}
	commandID, _ := domain.ParseCommandID("nha050-durable-alias")
	record, found, err := store.CommandIntent(f.ctx, commandID)
	if err != nil || !found {
		t.Fatal(err)
	}
	target, present, err := record.AuthorizationTarget()
	entity, _, targetErr := target.Values()
	if err != nil || !present || targetErr != nil || entity != f.identities[0].Entity {
		t.Fatal("stored target was not admitted A", err, targetErr)
	}
	f.reassignAlias(t)
	a, b := f.identities[0].Entity, f.identities[1].Entity
	f.policy(t, []domain.EntityID{b}, []domain.EntityID{b}, 2)
	if status, found, err := service.Status(f.ctx, commandID); status.Response != nil || found || !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
		t.Fatal("durable A authorized through B", status, found, err)
	}
	f.policy(t, []domain.EntityID{a}, []domain.EntityID{a}, 3)
	if _, found, err := service.Status(f.ctx, commandID); err != nil || !found {
		t.Fatal("permitted historical A status refused", found, err)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := service.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	// A separate Host/runtime reopens the same durable ledger. Only its native
	// current policy and the persisted target are consulted, without an alias.
	otherDriver := &nha022Driver{}
	restarted := nha050Setup(t, otherDriver)
	restarted.policy(t, []domain.EntityID{a}, []domain.EntityID{a}, 2)
	second, err := integrations.NewDurableCommandService(restarted.ctx, restarted.host, restarted.gen, store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = second.Close(ctx)
	})
	if _, found, err := second.Status(restarted.ctx, commandID); err != nil || !found {
		t.Fatal("restart lost stored target proof", found, err)
	}
	if driver.count() != 1 || otherDriver.count() != 0 {
		t.Fatal("restart or status resent command")
	}
	if err := store.InTransaction(f.ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(f.ctx, `CREATE FUNCTION public.nha050_unknown_intent_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic commit failure'; RETURN NEW; END $$; CREATE CONSTRAINT TRIGGER nha050_unknown_intent_commit AFTER INSERT ON public.command_intents DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha050_unknown_intent_commit()`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	unknown := restarted.prepared(t, second, "nha050-unknown-target")
	if result, err := second.DispatchPreparedDirect(restarted.ctx, unknown); result != nil || !errors.Is(err, integrations.ErrDurableCommitUnknown) {
		t.Fatal("unknown target commit admitted dispatch", result, err)
	}
	if otherDriver.count() != 0 {
		t.Fatal("unknown target transaction emitted command")
	}
}

func TestNHA050AliasChangeAfterTargetRetentionRefusesFinalAdmission(t *testing.T) {
	for _, durable := range []bool{false, true} {
		name := "volatile"
		if durable {
			name = "durable"
		}
		t.Run(name, func(t *testing.T) {
			driver := &nha022Driver{}
			f := nha050Setup(t, driver)
			generation := &nha022PausingGeneration{entered: make(chan struct{}), release: make(chan struct{})}
			var service interface {
				Dispatch(context.Context, domain.ActorID, *actions.CommandRequest) (*actions.CommandResponse, error)
			}
			var storedTarget func() authorization.CommandTarget
			idText := "nha050-inter-admission-" + name
			id, _ := domain.ParseCommandID(idText)
			if durable {
				store, _ := nha024Store(t)
				durableService, err := integrations.NewDurableCommandService(f.ctx, f.host, generation, store)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if err := durableService.Close(ctx); err != nil {
						t.Error(err)
					}
				})
				service = durableService
				storedTarget = func() authorization.CommandTarget {
					record, found, err := store.CommandIntent(f.ctx, id)
					if err != nil || !found {
						t.Fatal("target not committed before generation fence", found, err)
					}
					target, present, err := record.AuthorizationTarget()
					if err != nil || !present {
						t.Fatal("target proof absent", err)
					}
					return target
				}
			} else {
				var err error
				service, err = integrations.NewVolatileCommandService(f.host, generation, integrations.DefaultVolatileCommandLimits())
				if err != nil {
					t.Fatal(err)
				}
			}
			epoch, err := f.fixture.Sequencer().CurrentAdmissionEpoch()
			if err != nil {
				t.Fatal(err)
			}
			request := nha022Request(t, f.identities[0].Entity, epoch, contracts.SwitchSchema, "relay", nha022Switch())
			request.CommandId = idText
			request.Target = &registry.EntityRef{Reference: &registry.EntityRef_HaAlias{HaAlias: "switch.nha050_target"}}
			type result struct {
				response *actions.CommandResponse
				err      error
			}
			done := make(chan result, 1)
			go func() {
				response, err := service.Dispatch(f.ctx, f.actor, request)
				done <- result{response, err}
			}()
			select {
			case <-generation.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("final generation admission was not reached")
			}
			// Both A and B are permitted. Only the exact admitted target fence
			// can stop an alias reassignment here; a permission recheck alone
			// would allow B under a record whose authorization proof names A.
			if storedTarget != nil {
				entity, _, err := storedTarget().Values()
				if err != nil || entity != f.identities[0].Entity {
					t.Fatal("retained target was not A", err)
				}
			}
			f.reassignAlias(t)
			close(generation.release)
			select {
			case result := <-done:
				if result.err != nil || result.response == nil || result.response.Status != actions.ActionStatus_ACTION_STATUS_FAILED || result.response.Error.GetCode() != actions.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
					t.Fatal("changed alias did not fail at the target fence", result.response, result.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("changed alias dispatch did not settle")
			}
			if driver.count() != 0 {
				t.Fatal("target A metadata allowed an emission to B", driver.count())
			}
			if storedTarget != nil {
				entity, _, err := storedTarget().Values()
				if err != nil || entity != f.identities[0].Entity {
					t.Fatal("settlement rewrote authorization target", err)
				}
			}
		})
	}
}

type nha050QueuedDriver struct {
	entered chan struct{}
	release chan struct{}
	emitted atomic.Int32
}

func (d *nha050QueuedDriver) Dispatch(_ context.Context, command integrations.AdmittedCommand) error {
	close(d.entered)
	<-d.release
	if !command.ReadyForTransport() {
		return integrations.ErrCommandAuthorizationDenied
	}
	d.emitted.Add(1)
	return nil
}
func TestNHA050PreparedAndQueuedCommandsObserveCurrentRights(t *testing.T) {
	driver := &nha050QueuedDriver{entered: make(chan struct{}), release: make(chan struct{})}
	f := nha050Setup(t, driver)
	service, err := integrations.NewVolatileCommandService(f.host, f.gen, integrations.DefaultVolatileCommandLimits())
	if err != nil {
		t.Fatal(err)
	}
	prepared := f.prepared(t, service, "nha050-prepared-revoke")
	f.policy(t, nil, nil, 2)
	if result, err := service.DispatchPreparedDirect(f.ctx, prepared); result != nil || !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
		t.Fatal("revoked preparation reached dispatch", result, err)
	}
	select {
	case <-driver.entered:
		t.Fatal("revocation winning before dispatch called driver")
	default:
	}
	a := f.identities[0].Entity
	f.policy(t, []domain.EntityID{a}, []domain.EntityID{a}, 3)
	queued := f.prepared(t, service, "nha050-queued-revoke")
	done := make(chan error, 1)
	go func() { _, err := service.DispatchPreparedDirect(f.ctx, queued); done <- err }()
	select {
	case <-driver.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("driver queue not reached")
	}
	f.policy(t, nil, nil, 4)
	close(driver.release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("queued command did not finish")
	}
	if driver.emitted.Load() != 0 {
		t.Fatal("deferred transport ignored revoked permit")
	}
	if err := prepared.AuthorizeRead(f.ctx); !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
		t.Fatal("old no-op/status remained readable", err)
	}
	alias, _ := domain.ParseHAAlias("switch.nha050_target")
	if _, err := service.CaptureDirect(f.ctx, alias, integrations.DirectSwitch); !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
		t.Fatal("forbidden capture exposed route", err)
	}
}

type nha050SceneCapture struct {
	source       *sequencer.Sequencer
	entered      chan struct{}
	release      chan struct{}
	block        atomic.Bool
	omitRegistry bool
}

func (c *nha050SceneCapture) Capture() (sequencer.Snapshot, error) {
	snapshot, err := c.source.Capture()
	if c.omitRegistry {
		snapshot.Registry = nil
	}
	if c.block.CompareAndSwap(true, false) {
		close(c.entered)
		<-c.release
	}
	return snapshot, err
}

type nha050ZeroSceneAccessPort struct {
	*integrations.VolatileCommandService
}

func (p nha050ZeroSceneAccessPort) WithSceneRead(_ context.Context, fn func(integrations.SceneReadAccess) error) error {
	return fn(integrations.SceneReadAccess{})
}

func TestNHA050SceneCaptureRefusesMissingNativeIdentityProof(t *testing.T) {
	f := nha050Setup(t, &nha022Driver{})
	commands, err := integrations.NewVolatileCommandService(f.host, f.gen, integrations.DefaultVolatileCommandLimits())
	if err != nil {
		t.Fatal(err)
	}
	selection, err := logicselection.LoadV1()
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := scenes.CompileSelected(selection, scope.LogicMVPSourcesV1())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		port scenes.CommandPort
	}{{"native", commands}, {"zero-is-not-legacy", nha050ZeroSceneAccessPort{commands}}} {
		t.Run(test.name, func(t *testing.T) {
			capture := &nha050SceneCapture{source: f.fixture.Sequencer(), omitRegistry: true}
			service, err := scenes.NewService(catalogue, capture, f.gen, test.port, sequencer.RealClock{})
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			alias, _ := domain.ParseHAAlias("switch.nha050_target")
			id, _ := domain.ParseHAAlias("scene.nha050_missing_native_proof")
			ref, omissions, err := service.CreateTemporary(f.ctx, id, nil, []domain.HAAlias{alias}, time.Hour, nil)
			if !errors.Is(err, integrations.ErrCommandAuthorizationDenied) || len(omissions) != 0 || ref.Version() != 0 {
				t.Fatal("incomplete native/zero proof became trusted legacy capture", ref, omissions, err)
			}
		})
	}
}
func (c *nha050SceneCapture) CurrentAdmissionEpoch() (string, error) {
	return c.source.CurrentAdmissionEpoch()
}
func TestNHA050SceneCaptureOrderingAndLatchedProofConfidentiality(t *testing.T) {
	driver := &nha022Driver{}
	f := nha050Setup(t, driver)
	commands, err := integrations.NewVolatileCommandService(f.host, f.gen, integrations.DefaultVolatileCommandLimits())
	if err != nil {
		t.Fatal(err)
	}
	selection, err := logicselection.LoadV1()
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := scenes.CompileSelected(selection, scope.LogicMVPSourcesV1())
	if err != nil {
		t.Fatal(err)
	}
	capture := &nha050SceneCapture{source: f.fixture.Sequencer(), entered: make(chan struct{}), release: make(chan struct{})}
	capture.block.Store(true)
	service, err := scenes.NewService(catalogue, capture, f.gen, commands, &nha179Clock{now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	id, _ := domain.ParseHAAlias("scene.nha050_capture")
	alias, _ := domain.ParseHAAlias("switch.nha050_target")
	type result struct {
		ref     scenes.SceneRef
		omitted []scenes.Omission
		err     error
	}
	captured := make(chan result, 1)
	go func() {
		ref, omitted, err := service.CreateTemporary(f.ctx, id, nil, []domain.HAAlias{alias}, time.Hour, nil)
		captured <- result{ref, omitted, err}
	}()
	select {
	case <-capture.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("native capture did not enter shared snapshot")
	}
	writeStarted := make(chan struct{})
	writeDone := make(chan error, 1)
	emptyState := nha050State(t, nha050Policy(t, f.account, nil, nil), 2)
	go func() {
		close(writeStarted)
		mutation, err := f.authority.BeginMutation(f.account)
		if err != nil {
			writeDone <- err
			return
		}
		defer mutation.Release()
		writeDone <- f.authority.Publish(emptyState)
	}()
	<-writeStarted
	runtime.Gosched()
	close(capture.release)
	var saved result
	select {
	case saved = <-captured:
		if saved.err != nil || len(saved.omitted) != 0 {
			t.Fatal("capture owning the read order failed", saved.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("capture reacquired authority with writer waiting")
	}
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("policy writer did not follow capture")
	}
	otherID, _ := domain.ParseHAAlias("scene.nha050_denied_capture")
	_, omitted, denied := service.CreateTemporary(f.ctx, otherID, nil, []domain.HAAlias{alias}, time.Hour, nil)
	missing, _ := domain.ParseHAAlias("switch.nha050_missing")
	_, absentOmissions, absent := service.CreateTemporary(f.ctx, otherID, nil, []domain.HAAlias{missing}, time.Hour, nil)
	if !errors.Is(denied, integrations.ErrCommandAuthorizationDenied) || !errors.Is(absent, integrations.ErrCommandAuthorizationDenied) || len(omitted) != 0 || len(absentOmissions) != 0 || denied.Error() != absent.Error() {
		t.Fatal("forbidden capture revealed value/quality/presence", omitted, absentOmissions, denied, absent)
	}
	a := f.identities[0].Entity
	f.policy(t, []domain.EntityID{a}, []domain.EntityID{a}, 3)
	invocation, _ := domain.ParseCommandID("nha050-noop-scene")
	slot, err := scenes.NewPreparationSlot(service, saved.ref, f.actor, invocation, nil, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := slot.PrepareOnce(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.ApplyPrepared(f.ctx, prepared)
	if err != nil || first.Members()[0].Outcome() != scenes.OutcomeAlreadySatisfied || driver.count() != 0 {
		t.Fatal("positive no-op proof failed", first, err)
	}
	f.policy(t, nil, nil, 4)
	if _, err := slot.PrepareOnce(f.ctx); !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
		t.Fatal("cached preparation exposed withdrawn source", err)
	}
	if result, err := service.ApplyPrepared(f.ctx, prepared); !errors.Is(err, integrations.ErrCommandAuthorizationDenied) || len(result.Members()) != 0 {
		t.Fatal("latched proof exposed forbidden alias/revision", result, err)
	}
	if driver.count() != 0 {
		t.Fatal("proof/consultation dispatched an effect")
	}
}

type nha050RefusedCapturePort struct {
	*integrations.VolatileCommandService
	cause error
}

func (p *nha050RefusedCapturePort) CaptureDirect(context.Context, domain.HAAlias, integrations.DirectKind) (integrations.DirectCapture, error) {
	return integrations.DirectCapture{}, p.cause
}
func TestNHA050SceneAuthorizationUsesCauseRatherThanDiagnosticText(t *testing.T) {
	for _, test := range []struct {
		name   string
		cause  error
		denied bool
	}{{"wrapped", fmt.Errorf("redacted route: %w", integrations.ErrCommandAuthorizationDenied), true}, {"same-text", errors.New(integrations.ErrCommandAuthorizationDenied.Error()), false}} {
		t.Run(test.name, func(t *testing.T) {
			f := nha050Setup(t, &nha022Driver{})
			commands, err := integrations.NewVolatileCommandService(f.host, f.gen, integrations.DefaultVolatileCommandLimits())
			if err != nil {
				t.Fatal(err)
			}
			selection, err := logicselection.LoadV1()
			if err != nil {
				t.Fatal(err)
			}
			catalogue, err := scenes.CompileSelected(selection, scope.LogicMVPSourcesV1())
			if err != nil {
				t.Fatal(err)
			}
			service, err := scenes.NewService(catalogue, f.fixture.Sequencer(), f.gen, &nha050RefusedCapturePort{commands, test.cause}, &nha179Clock{now: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			id, _ := domain.ParseHAAlias("scene.nha050_typed_refusal")
			alias, _ := domain.ParseHAAlias("switch.nha050_target")
			ref, _, err := service.CreateTemporary(f.ctx, id, nil, []domain.HAAlias{alias}, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			invocation, _ := domain.ParseCommandID("nha050-refused-capture")
			slot, err := scenes.NewPreparationSlot(service, ref, f.actor, invocation, nil, time.Now().Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := slot.PrepareOnce(f.ctx)
			if errors.Is(err, integrations.ErrCommandAuthorizationDenied) != test.denied {
				t.Fatal("display text became authorization authority", err)
			}
			if !test.denied && (err != nil || len(prepared.Members()) != 1 || prepared.Members()[0].Outcome() != scenes.OutcomeRefused) {
				t.Fatal("ordinary route diagnostic was reclassified", prepared.Members(), err)
			}
		})
	}
}

package server

import (
	"context"
	"errors"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/recorder"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/remotecalendar"
	"github.com/damione1/Homeward/internal/storage"
	"sync"
	"time"
)

const remoteCalendarAlias = "calendar.famille"

type remoteCalendarRuntime struct {
	mu        sync.Mutex
	store     *storage.Store
	identity  *registry.Repository
	service   *calendars.Service
	clock     automationtriggers.Clock
	policy    *integrations.CloudPolicy
	bridge    *remoteCalendarPublisher
	active    map[*configuredRemoteCalendarPilot]struct{}
	closing   bool
	closeOnce sync.Once
	done      chan struct{}
}
type remoteCalendarCloudTimer struct{ clock automationtriggers.Clock }

func (t remoteCalendarCloudTimer) NewTimer(d time.Duration) integrations.CloudWait {
	return t.clock.NewTimer(d)
}

func newRemoteCalendarRuntime(store *storage.Store, identity *registry.Repository, manifest registry.Manifest, clock automationtriggers.Clock) (*remoteCalendarRuntime, error) {
	if store == nil || identity == nil || nilReactiveInterface(clock) {
		return nil, ErrInvalidTriggerComposition
	}
	actual, err := identity.ScopeManifest()
	if err != nil {
		return nil, err
	}
	a, aok := actual.Digest()
	b, bok := manifest.Digest()
	ar, arv := actual.Revision()
	br, brv := manifest.Revision()
	if !aok || !bok || !arv || !brv || a != b || ar != br {
		return nil, registry.ErrOutsideManifest
	}
	repository, err := calendars.NewRepositoryWithClock(store, manifest, clock)
	if err != nil {
		return nil, err
	}
	service, err := calendars.NewService(repository)
	if err != nil {
		return nil, err
	}
	policy, err := integrations.NewCloudPolicy(integrations.DefaultCloudLimits(), nil, clock, remoteCalendarCloudTimer{clock}, integrations.RandomJitter{})
	if err != nil {
		service.Close()
		return nil, err
	}
	source, err := automationtriggers.ParseCalendarSourceID(remoteCalendarAlias)
	if err != nil {
		service.Close()
		return nil, err
	}
	return &remoteCalendarRuntime{store: store, identity: identity, service: service, clock: clock, policy: policy, bridge: &remoteCalendarPublisher{source: source}, active: make(map[*configuredRemoteCalendarPilot]struct{}), done: make(chan struct{})}, nil
}
func (r *remoteCalendarRuntime) Done() <-chan struct{} {
	if r == nil {
		return nil
	}
	return r.done
}
func (r *remoteCalendarRuntime) closeAdmissions() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.closing = true
	r.mu.Unlock()
	r.bridge.close()
}
func (r *remoteCalendarRuntime) beginClose() <-chan struct{} {
	if r == nil {
		return nil
	}
	r.closeAdmissions()
	r.closeOnce.Do(func() {
		go func() {
			r.mu.Lock()
			active := make([]*configuredRemoteCalendarPilot, 0, len(r.active))
			for pilot := range r.active {
				active = append(active, pilot)
			}
			r.mu.Unlock()
			for _, pilot := range active {
				_ = pilot.Stop(context.Background())
			}
			r.service.Close()
			close(r.done)
		}()
	})
	return r.done
}
func (r *remoteCalendarRuntime) close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	select {
	case <-r.beginClose():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (r *remoteCalendarRuntime) attach(service *automationtriggers.Service) error {
	if r == nil {
		return nil
	}
	return r.bridge.attach(service)
}
func (r *remoteCalendarRuntime) factory(settings remotecalendar.Settings) (integrations.PilotFactory, error) {
	if r == nil || !settings.Valid() {
		return nil, ErrInvalidTriggerComposition
	}
	return func() integrations.Pilot {
		return &configuredRemoteCalendarPilot{owner: r, settings: settings, startDone: make(chan struct{})}
	}, nil
}
func (r *remoteCalendarRuntime) add(plan *RuntimePlan, settings remotecalendar.Settings) error {
	factory, err := r.factory(settings)
	if err != nil {
		return err
	}
	instance, key, err := settings.Identity()
	if err != nil {
		return err
	}
	spec, err := integrations.NewPilotSpec(instance, factory)
	if err != nil {
		return err
	}
	previous := plan.Configure
	plan.Configure = func(ctx context.Context, host *integrations.Manager, store *storage.Store) error {
		if _, err := r.identity.ConfigureBinding(ctx, instance, key, remoteCalendarAlias); err != nil {
			return err
		}
		if previous != nil {
			return previous(ctx, host, store)
		}
		return nil
	}
	plan.Pilots = append(plan.Pilots, spec)
	return nil
}

type remoteCalendarWriter struct {
	service *calendars.Service
	id      calendars.CalendarID
}

func (w remoteCalendarWriter) Open(ctx context.Context, source calendars.RemoteSource) (calendars.RemoteWriter, error) {
	return w.service.ConfigureRemoteSource(ctx, w.id, source)
}

type remoteCalendarReader struct{ service *calendars.Service }

func (r remoteCalendarReader) CollectRemoteWindow(ctx context.Context, id calendars.CalendarID, request calendars.AutomationWindowRequest) (calendars.AutomationWindow, error) {
	return r.service.CollectRemoteWindow(ctx, id, request)
}
func (r remoteCalendarReader) CheckRemoteWindow(ctx context.Context, window calendars.AutomationWindow) error {
	return r.service.CheckRemoteWindow(ctx, window)
}

type remoteCalendarPublisher struct {
	mu               sync.Mutex
	source           automationtriggers.CalendarSourceID
	engine           *automationtriggers.Service
	attached, closed bool
	beforeReplace    func()
	afterReplace     func(error)
}

func (p *remoteCalendarPublisher) attach(engine *automationtriggers.Service) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.attached || p.closed {
		return ErrInvalidTriggerComposition
	}
	p.engine, p.attached = engine, true
	return nil
}
func (p *remoteCalendarPublisher) publication() (remotecalendar.Publication, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.attached || p.closed {
		return remotecalendar.Publication{}, ErrInvalidTriggerComposition
	}
	if p.engine == nil {
		return remotecalendar.WithoutAutomationPublication(), nil
	}
	return remotecalendar.WithAutomationPublication(p)
}
func (p *remoteCalendarPublisher) Replace(ctx context.Context, publication remotecalendar.PublicationWindow) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.attached || p.closed || p.engine == nil {
		return ErrInvalidTriggerComposition
	}
	wall, until, mono, untilMono, ok := publication.Deadline()
	if !ok {
		return ErrInvalidTriggerComposition
	}
	deadline, err := automationtriggers.NewRemoteCalendarDeadline(wall, until, mono, untilMono)
	if err != nil {
		return err
	}
	if p.beforeReplace != nil {
		p.beforeReplace()
	}
	err = p.engine.ReplaceRemoteCalendarWindow(ctx, p.source, publication.Window(), deadline)
	if p.afterReplace != nil {
		p.afterReplace(err)
	}
	return err
}
func (p *remoteCalendarPublisher) Degrade(ctx context.Context, _ calendars.RemoteReason) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.engine == nil {
		return nil
	}
	return p.engine.DegradeCalendarSource(ctx, p.source)
}
func (p *remoteCalendarPublisher) close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.engine != nil {
		_ = p.engine.DegradeCalendarSource(context.Background(), p.source)
	}
}

// The factory is prepared without I/O. Binding and source ownership begin
// only after the generation is committed and Supervisor starts this pilot.
type configuredRemoteCalendarPilot struct {
	mu               sync.Mutex
	owner            *remoteCalendarRuntime
	settings         remotecalendar.Settings
	inner            *remotecalendar.Pilot
	started, stopped bool
	cancel           context.CancelFunc
	startDone        chan struct{}
}

func (p *configuredRemoteCalendarPilot) Start(ctx context.Context, host integrations.SessionHost, tasks integrations.Tasks) (returned error) {
	if p == nil || p.owner == nil || !p.settings.Valid() || nilReactiveInterface(ctx) || nilReactiveInterface(host) || nilReactiveInterface(tasks) {
		return integrations.ErrInvalidInput
	}
	p.mu.Lock()
	if p.started || p.stopped {
		p.mu.Unlock()
		return integrations.ErrSupervisorState
	}
	p.started = true
	bounded, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.mu.Unlock()
	defer close(p.startDone)
	p.owner.mu.Lock()
	if p.owner.closing {
		p.owner.mu.Unlock()
		cancel()
		return integrations.ErrAdmissionsClosed
	}
	p.owner.active[p] = struct{}{}
	p.owner.mu.Unlock()
	defer func() {
		if returned != nil {
			cancel()
		}
	}()
	instance, key, err := p.settings.Identity()
	if err != nil {
		return err
	}
	identities, err := p.owner.identity.Resolve(bounded, instance, []integrations.SourceKey{key})
	if err != nil || len(identities) != 1 {
		return ErrIdentityNotConfigured
	}
	id, err := p.owner.service.Bind(bounded, identities[0].Entity)
	if err != nil {
		return err
	}
	publication, err := p.owner.bridge.publication()
	if err != nil {
		return err
	}
	source, err := remotecalendar.NewSource(p.settings, id, remoteCalendarWriter{p.owner.service, id}, remoteCalendarReader{p.owner.service}, publication, p.owner.policy)
	if err != nil {
		return err
	}
	inner, err := remotecalendar.NewPilot(source, p.owner.clock)
	if err != nil {
		return err
	}
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return integrations.ErrAdmissionsClosed
	}
	p.inner = inner
	p.mu.Unlock()
	if err := inner.Start(bounded, host, tasks); err != nil {
		return err
	}
	return nil
}
func (p *configuredRemoteCalendarPilot) Stop(ctx context.Context) error {
	p.mu.Lock()
	p.stopped = true
	cancel, started := p.cancel, p.started
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if !started {
		return nil
	}
	select {
	case <-p.startDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	p.mu.Lock()
	inner := p.inner
	p.mu.Unlock()
	var err error
	if inner != nil {
		err = inner.Stop(ctx)
	}
	if err == nil {
		p.owner.mu.Lock()
		delete(p.owner.active, p)
		p.owner.mu.Unlock()
	}
	return err
}
func (p *configuredRemoteCalendarPilot) Refresh(ctx context.Context) error {
	p.mu.Lock()
	inner, stopped := p.inner, p.stopped
	p.mu.Unlock()
	if inner == nil || stopped {
		return integrations.ErrAdmissionsClosed
	}
	return inner.Refresh(ctx)
}

// Explicit nil-safe drain helpers are shared by prepared-error and serving
// exits. Their true Done remains in Store retention after a timeout.
func closeRemoteStoreWithRecorder(remote *remoteCalendarRuntime, store *storage.Store, recording *recorder.Recorder, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	done := remote.beginClose()
	remoteErr := remote.close(ctx)
	recorderErr := drainRecorder(ctx, recording)
	retainStoreUntilDrained(store, done, recording.Done())
	return errors.Join(cause, remoteErr, recorderErr)
}
func closeRemotePreparedRuntime(remote *remoteCalendarRuntime, host *integrations.Manager, authorization *cloudAuthorizationCoordinator, store *storage.Store, cause error, templates *reactiveTemplateRuntime, recording *recorder.Recorder, jobs *durableJobsRuntime, mfa *mfaRuntime) error {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	return closeRemotePreparedRuntimeWithContext(remote, ctx, host, authorization, store, cause, templates, recording, jobs, mfa)
}
func closeRemotePreparedRuntimeWithContext(remote *remoteCalendarRuntime, ctx context.Context, host *integrations.Manager, authorization *cloudAuthorizationCoordinator, store *storage.Store, cause error, templates *reactiveTemplateRuntime, recording *recorder.Recorder, jobs *durableJobsRuntime, mfa *mfaRuntime, extra ...<-chan struct{}) error {
	done := remote.beginClose()
	remoteErr := remote.close(ctx)
	extra = append(extra, done)
	return closePreparedRuntimeWithContext(ctx, host, authorization, store, errors.Join(cause, remoteErr), templates, recording, jobs, mfa, extra...)
}
func closeRemoteTriggerPreparedRuntime(remote *remoteCalendarRuntime, host *integrations.Manager, service *automationtriggers.Service, runs *automationruns.Controller, composition *AutomationRunsComposition, other func(context.Context, <-chan struct{}) error) error {
	remote.closeAdmissions()
	return closeTriggerPreparedRuntime(host, service, runs, composition, other)
}

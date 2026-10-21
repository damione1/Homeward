package server

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
)

var ErrCloudAuthorizationClosed = errors.New("server: cloud authorization admission closed")
var ErrCloudAuthorizationBusy = errors.New("server: cloud authorization capacity exhausted")
var ErrCloudAuthorizationJoinTimeout = errors.New("server: cloud authorization did not join before shutdown deadline")
var ErrCloudAuthorizationProviderFailed = errors.New("server: cloud authorization provider failed")

// Composition size is independent of the 16 simultaneous authorization slots.
const maxCloudAuthorizationRegistrations = 1024

type CloudAuthorizationProvider interface {
	Authorize(context.Context, integrations.CloudAuthorizationExchanger, integrations.CloudAuthorizationInput) (integrations.CloudAuthorizedCredential, error)
}

type CloudAuthorizationRegistration struct {
	instance     domain.IntegrationInstanceID
	handle       storage.SecretHandle
	profile      integrations.CloudAuthorizationProfileID
	policy       *integrations.CloudPolicy
	provider     CloudAuthorizationProvider
	admission    integrations.CloudAttemptAdmission
	hasAdmission bool
}

func nilCloudAuthorizationInterface(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return r.IsNil()
	default:
		return false
	}
}

// Optional admission validates a configured durable hook without requiring
// one for every provider.
func NewCloudAuthorizationRegistration(instance domain.IntegrationInstanceID, handle storage.SecretHandle, profile integrations.CloudAuthorizationProfileID, policy *integrations.CloudPolicy, provider CloudAuthorizationProvider, admission ...integrations.CloudAttemptAdmission) (CloudAuthorizationRegistration, error) {
	if _, err := instance.Value(); err != nil || !profile.Valid() || policy == nil || nilCloudAuthorizationInterface(provider) || len(admission) > 1 {
		return CloudAuthorizationRegistration{}, integrations.ErrInvalidInput
	}
	target, _, err := handle.Components()
	if err != nil || target != instance {
		return CloudAuthorizationRegistration{}, integrations.ErrInvalidInput
	}
	r := CloudAuthorizationRegistration{instance: instance, handle: handle, profile: profile, policy: policy, provider: provider}
	if len(admission) == 1 {
		if nilCloudAuthorizationInterface(admission[0]) {
			return CloudAuthorizationRegistration{}, integrations.ErrInvalidInput
		}
		r.admission, r.hasAdmission = admission[0], true
	}
	return r, nil
}
func (r CloudAuthorizationRegistration) String() string {
	return "[REDACTED CLOUD AUTHORIZATION REGISTRATION]"
}
func (r CloudAuthorizationRegistration) GoString() string { return r.String() }
func (r CloudAuthorizationRegistration) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.String())
}

type CloudAuthorizationComposition interface {
	Register(CloudAuthorizationRegistration) (CloudAuthorizationBinder, error)
	Invoker() CloudAuthorizationInvoker
}
type CloudAuthorizationBinder interface {
	Bind(*integrations.CloudClient) error
	Unbind(*integrations.CloudClient)
}
type CloudAuthorizationInvoker interface {
	Authorize(context.Context, domain.IntegrationInstanceID, integrations.CloudAuthorizationInput) (CloudAuthorizationReceipt, error)
}

type CloudAuthorizationReceipt struct {
	attempt   integrations.CloudAuthorizationAttemptID
	published bool
}

func (r CloudAuthorizationReceipt) AttemptID() integrations.CloudAuthorizationAttemptID {
	return r.attempt
}
func (r CloudAuthorizationReceipt) Published() bool              { return r.published && r.attempt.Valid() }
func (r CloudAuthorizationReceipt) String() string               { return "[REDACTED CLOUD AUTHORIZATION RECEIPT]" }
func (r CloudAuthorizationReceipt) GoString() string             { return r.String() }
func (r CloudAuthorizationReceipt) MarshalJSON() ([]byte, error) { return json.Marshal(r.String()) }

type cloudAuthorizationBinding struct {
	registration CloudAuthorizationRegistration
	client       *integrations.CloudClient
	owner        *integrations.CloudAuthorizationOwner
	generation   uint64
}
type cloudAuthorizationInvocation struct {
	done       chan struct{}
	client     *integrations.CloudClient
	generation uint64
	cancel     context.CancelFunc
	attempt    *integrations.CloudAuthorizationAttempt
}
type cloudAuthorizationCoordinator struct {
	pendingReplacement *cloudRegistrationReplacement
	host               *integrations.Manager
	limits             integrations.CloudAuthorizationLimits
	ctx                context.Context
	cancel             context.CancelFunc
	mu                 sync.Mutex
	registrations      map[domain.IntegrationInstanceID]*cloudAuthorizationBinding
	active             map[storage.SecretHandle]*cloudAuthorizationInvocation
	frozen             bool
	open               bool
	closed             bool
	done               chan struct{}
}

func newCloudAuthorizationCoordinator(ctx context.Context, host *integrations.Manager, limits integrations.CloudAuthorizationLimits) (*cloudAuthorizationCoordinator, error) {
	if nilCloudAuthorizationInterface(ctx) || host == nil || !limits.Valid() {
		return nil, integrations.ErrInvalidInput
	}
	lifecycle, cancel := context.WithCancel(context.WithoutCancel(ctx))
	return &cloudAuthorizationCoordinator{
		host: host, limits: limits, ctx: lifecycle, cancel: cancel,
		registrations: make(map[domain.IntegrationInstanceID]*cloudAuthorizationBinding),
		active:        make(map[storage.SecretHandle]*cloudAuthorizationInvocation),
		done:          make(chan struct{}),
	}, nil
}

type cloudAuthorizationCompositionView struct {
	coordinator *cloudAuthorizationCoordinator
}

func (v cloudAuthorizationCompositionView) Register(r CloudAuthorizationRegistration) (CloudAuthorizationBinder, error) {
	if v.coordinator == nil {
		return nil, integrations.ErrInvalidInput
	}
	return v.coordinator.register(r)
}
func (v cloudAuthorizationCompositionView) Invoker() CloudAuthorizationInvoker {
	if v.coordinator == nil {
		return nil
	}
	return cloudAuthorizationInvokerView{coordinator: v.coordinator}
}

type cloudAuthorizationBinderView struct {
	coordinator *cloudAuthorizationCoordinator
	instance    domain.IntegrationInstanceID
}

func (v cloudAuthorizationBinderView) Bind(client *integrations.CloudClient) error {
	if v.coordinator == nil {
		return integrations.ErrInvalidInput
	}
	return v.coordinator.bind(v.instance, client)
}
func (v cloudAuthorizationBinderView) Unbind(client *integrations.CloudClient) {
	if v.coordinator != nil {
		v.coordinator.unbind(v.instance, client)
	}
}

type cloudAuthorizationInvokerView struct {
	coordinator *cloudAuthorizationCoordinator
}

func (v cloudAuthorizationInvokerView) Authorize(ctx context.Context, instance domain.IntegrationInstanceID, input integrations.CloudAuthorizationInput) (CloudAuthorizationReceipt, error) {
	if v.coordinator == nil {
		return CloudAuthorizationReceipt{}, integrations.ErrInvalidInput
	}
	return v.coordinator.authorize(ctx, instance, input)
}
func (c *cloudAuthorizationCoordinator) composition() CloudAuthorizationComposition {
	return cloudAuthorizationCompositionView{coordinator: c}
}

func (c *cloudAuthorizationCoordinator) register(r CloudAuthorizationRegistration) (CloudAuthorizationBinder, error) {
	if c == nil || r.policy == nil || nilCloudAuthorizationInterface(r.provider) || !r.profile.Valid() {
		return nil, integrations.ErrInvalidInput
	}
	target, _, err := r.handle.Components()
	if err != nil || target != r.instance {
		return nil, integrations.ErrInvalidInput
	}
	if _, err := r.instance.Value(); err != nil {
		return nil, integrations.ErrInvalidInput
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.frozen {
		return nil, ErrCloudAuthorizationClosed
	}
	if len(c.registrations) >= maxCloudAuthorizationRegistrations {
		return nil, ErrCloudAuthorizationBusy
	}
	if c.registrations[r.instance] != nil {
		return nil, integrations.ErrInvalidInput
	}
	c.registrations[r.instance] = &cloudAuthorizationBinding{registration: r}
	return cloudAuthorizationBinderView{coordinator: c, instance: r.instance}, nil
}
func (c *cloudAuthorizationCoordinator) freeze() {
	c.mu.Lock()
	c.frozen = true
	c.mu.Unlock()
}
func (c *cloudAuthorizationCoordinator) openAdmissions() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || !c.frozen || c.open {
		return ErrCloudAuthorizationClosed
	}
	c.open = true
	return nil
}

func (c *cloudAuthorizationCoordinator) bind(instance domain.IntegrationInstanceID, client *integrations.CloudClient) error {
	if c == nil || client == nil {
		return integrations.ErrInvalidInput
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	binding := c.registrations[instance]
	if binding == nil || c.closed || !c.frozen {
		return ErrCloudAuthorizationClosed
	}
	if binding.client == client {
		return integrations.ErrInvalidInput
	}
	r := binding.registration
	var owner *integrations.CloudAuthorizationOwner
	var err error
	if r.hasAdmission {
		owner, err = integrations.NewCloudAuthorizationOwner(c.host, client, r.policy, r.handle, r.profile, c.limits, r.admission)
	} else {
		owner, err = integrations.NewCloudAuthorizationOwner(c.host, client, r.policy, r.handle, r.profile, c.limits)
	}
	if err != nil {
		return err
	}
	if binding.client != nil {
		if active := c.active[r.handle]; active != nil {
			active.cancel()
		}
	}
	binding.client, binding.owner = client, owner
	binding.generation++
	return nil
}
func (c *cloudAuthorizationCoordinator) unbind(instance domain.IntegrationInstanceID, client *integrations.CloudClient) {
	if c == nil || client == nil {
		return
	}
	c.mu.Lock()
	binding := c.registrations[instance]
	var cancel context.CancelFunc
	if binding != nil && binding.client == client {
		binding.client, binding.owner = nil, nil
		binding.generation++
		if active := c.active[binding.registration.handle]; active != nil && active.client == client {
			cancel = active.cancel
		}
	}
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *cloudAuthorizationCoordinator) authorize(ctx context.Context, instance domain.IntegrationInstanceID, input integrations.CloudAuthorizationInput) (CloudAuthorizationReceipt, error) {
	if c == nil || nilCloudAuthorizationInterface(ctx) {
		return CloudAuthorizationReceipt{}, integrations.ErrInvalidInput
	}
	if ctx.Err() != nil {
		return CloudAuthorizationReceipt{}, ctx.Err()
	}
	if _, err := input.Secret(); err != nil {
		return CloudAuthorizationReceipt{}, integrations.ErrInvalidInput
	}
	c.mu.Lock()
	binding := c.registrations[instance]
	if !c.open || c.closed {
		c.mu.Unlock()
		return CloudAuthorizationReceipt{}, ErrCloudAuthorizationClosed
	}
	if binding == nil || binding.client == nil || binding.owner == nil {
		c.mu.Unlock()
		return CloudAuthorizationReceipt{}, integrations.ErrInactiveSession
	}
	if !input.Profile().Equal(binding.registration.profile) {
		c.mu.Unlock()
		return CloudAuthorizationReceipt{}, integrations.ErrInvalidInput
	}
	handle := binding.registration.handle
	if len(c.active) >= c.limits.Input().MaxActive || c.active[handle] != nil {
		c.mu.Unlock()
		return CloudAuthorizationReceipt{}, ErrCloudAuthorizationBusy
	}
	invokeCtx, cancel := context.WithTimeout(ctx, c.limits.Input().TotalTimeout)
	if c.pendingReplacement != nil && c.pendingReplacement.quiescing && c.pendingReplacement.touches(instance) {
		c.mu.Unlock()
		cancel()
		return CloudAuthorizationReceipt{}, ErrCloudAuthorizationClosed
	}
	active := &cloudAuthorizationInvocation{client: binding.client, generation: binding.generation, cancel: cancel, done: make(chan struct{})}
	c.active[handle] = active
	owner, provider := binding.owner, binding.registration.provider
	c.mu.Unlock()
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-c.ctx.Done():
			cancel()
		case <-invokeCtx.Done():
		}
	}()
	defer func() {
		cancel()
		<-watcherDone
		c.mu.Lock()
		if c.active[handle] == active {
			delete(c.active, handle)
		}
		if c.closed && len(c.active) == 0 {
			c.finishLocked()
		}
		c.mu.Unlock()
		close(active.done)
	}()
	attempt, err := owner.Begin(invokeCtx)
	if err != nil {
		return CloudAuthorizationReceipt{}, err
	}
	c.mu.Lock()
	active.attempt = attempt
	stillCurrent := c.open && !c.closed && binding.client == active.client && binding.generation == active.generation
	c.mu.Unlock()
	if !stillCurrent {
		attempt.Cancel()
	}
	defer attempt.Close()
	if !stillCurrent {
		return CloudAuthorizationReceipt{}, ErrCloudAuthorizationClosed
	}
	result, err := provider.Authorize(invokeCtx, attempt.Exchanger(), input)
	if invokeCtx.Err() != nil {
		return CloudAuthorizationReceipt{}, invokeCtx.Err()
	}
	if err != nil {
		if cloudErr, ok := err.(*integrations.CloudError); ok {
			return CloudAuthorizationReceipt{}, cloudErr
		}
		return CloudAuthorizationReceipt{}, ErrCloudAuthorizationProviderFailed
	}
	if !result.Profile().Equal(binding.registration.profile) {
		return CloudAuthorizationReceipt{}, integrations.ErrInvalidInput
	}
	if err := attempt.Publish(invokeCtx, result); err != nil {
		return CloudAuthorizationReceipt{}, err
	}
	c.mu.Lock()
	deliver := c.open && !c.closed && binding.client == active.client && binding.generation == active.generation && ctx.Err() == nil
	c.mu.Unlock()
	if !deliver {
		return CloudAuthorizationReceipt{}, integrations.ErrCredentialCommittedUnobserved
	}
	return CloudAuthorizationReceipt{attempt: attempt.ID(), published: true}, nil
}
func (c *cloudAuthorizationCoordinator) finishLocked() {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
}
func (c *cloudAuthorizationCoordinator) CloseAdmissions() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed, c.open, c.frozen = true, false, true
	cancels := make([]context.CancelFunc, 0, len(c.active))
	for _, active := range c.active {
		cancels = append(cancels, active.cancel)
	}
	if len(c.active) == 0 {
		c.finishLocked()
	}
	c.mu.Unlock()
	c.cancel()
	for _, cancel := range cancels {
		cancel()
	}
}
func (c *cloudAuthorizationCoordinator) Close(ctx context.Context) error {
	if c == nil || nilCloudAuthorizationInterface(ctx) {
		return integrations.ErrInvalidInput
	}
	c.CloseAdmissions()
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return errors.Join(ErrCloudAuthorizationJoinTimeout, ctx.Err())
	}
}
func (c *cloudAuthorizationCoordinator) Done() <-chan struct{} {
	if c == nil {
		return nil
	}
	return c.done
}

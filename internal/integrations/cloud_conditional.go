package integrations

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"hash"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
)

type ConditionalFailure uint8

const (
	ConditionalInvalid ConditionalFailure = iota + 1
	ConditionalDestinationDenied
	ConditionalSessionInactive
	ConditionalUnmatchedNotModified
	ConditionalValidatorMismatch
	ConditionalCachePolicy
	ConditionalInvalidResponse
	ConditionalExpired
)

type ConditionalError struct{ kind ConditionalFailure }

func conditionalFailure(k ConditionalFailure) error { return &ConditionalError{kind: k} }
func (e *ConditionalError) Kind() ConditionalFailure {
	if e == nil {
		return 0
	}
	return e.kind
}
func (e *ConditionalError) Error() string {
	switch e.Kind() {
	case ConditionalInvalid:
		return "conditional calendar: invalid admission"
	case ConditionalDestinationDenied:
		return "conditional calendar: destination refused"
	case ConditionalSessionInactive:
		return "conditional calendar: session or observation inactive"
	case ConditionalUnmatchedNotModified:
		return "conditional calendar: no matching stored representation"
	case ConditionalValidatorMismatch:
		return "conditional calendar: validator mismatch"
	case ConditionalCachePolicy:
		return "conditional calendar: representation cache policy refused"
	case ConditionalInvalidResponse:
		return "conditional calendar: invalid HTTP representation metadata"
	case ConditionalExpired:
		return "conditional calendar: validation already expired"
	default:
		return "conditional calendar: invalid failure"
	}
}
func (e *ConditionalError) GoString() string { return e.Error() }
func (e *ConditionalError) Disposition() storage.RemoteCalendarConditionalDisposition {
	switch e.Kind() {
	case ConditionalUnmatchedNotModified, ConditionalValidatorMismatch, ConditionalCachePolicy:
		return storage.RequireFullRepresentation
	default:
		return storage.PreserveAdmittedValidators
	}
}

type conditionalResourceKind uint8

const (
	conditionalPublicResource conditionalResourceKind = iota + 1
	conditionalSecretResource
)

type ConditionalResource struct {
	kind   conditionalResourceKind
	public string
	handle storage.SecretHandle
}

func PublicConditionalResource(raw string) (ConditionalResource, error) {
	u, e := parseConditionalURL(raw)
	if e != nil {
		return ConditionalResource{}, e
	}
	return ConditionalResource{kind: conditionalPublicResource, public: u.String()}, nil
}
func SecretConditionalResource(handle storage.SecretHandle) (ConditionalResource, error) {
	if _, _, e := handle.Components(); e != nil {
		return ConditionalResource{}, conditionalFailure(ConditionalInvalid)
	}
	return ConditionalResource{kind: conditionalSecretResource, handle: handle}, nil
}
func (r ConditionalResource) valid() bool {
	switch r.kind {
	case conditionalPublicResource:
		return r.public != "" && r.handle == (storage.SecretHandle{})
	case conditionalSecretResource:
		_, _, e := r.handle.Components()
		return e == nil && r.public == ""
	default:
		return false
	}
}
func (r ConditionalResource) String() string               { return "conditional resource [redacted]" }
func (r ConditionalResource) GoString() string             { return r.String() }
func (r ConditionalResource) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

type conditionalAuthKind uint8

const (
	conditionalAuthNone conditionalAuthKind = iota + 1
	conditionalAuthBasic
	conditionalAuthBearer
)

type ConditionalAuth struct {
	kind    conditionalAuthKind
	handles [2]storage.SecretHandle
}

func NoConditionalAuth() ConditionalAuth { return ConditionalAuth{kind: conditionalAuthNone} }
func BasicConditionalAuth(username, password storage.SecretHandle) (ConditionalAuth, error) {
	a := ConditionalAuth{kind: conditionalAuthBasic, handles: [2]storage.SecretHandle{username, password}}
	if !a.valid() {
		return ConditionalAuth{}, conditionalFailure(ConditionalInvalid)
	}
	return a, nil
}
func BearerConditionalAuth(token storage.SecretHandle) (ConditionalAuth, error) {
	a := ConditionalAuth{kind: conditionalAuthBearer, handles: [2]storage.SecretHandle{token}}
	if !a.valid() {
		return ConditionalAuth{}, conditionalFailure(ConditionalInvalid)
	}
	return a, nil
}
func (a ConditionalAuth) valid() bool {
	empty := storage.SecretHandle{}
	switch a.kind {
	case conditionalAuthNone:
		return a.handles == ([2]storage.SecretHandle{})
	case conditionalAuthBasic:
		_, _, x := a.handles[0].Components()
		_, _, y := a.handles[1].Components()
		return x == nil && y == nil && a.handles[0] != a.handles[1]
	case conditionalAuthBearer:
		_, _, e := a.handles[0].Components()
		return e == nil && a.handles[1] == empty
	default:
		return false
	}
}
func (a ConditionalAuth) String() string               { return "conditional authentication [redacted]" }
func (a ConditionalAuth) GoString() string             { return a.String() }
func (a ConditionalAuth) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

type ConditionalSettings struct {
	source      SourceKey
	resource    ConditionalResource
	auth        ConditionalAuth
	destination ConditionalDestination
	profile     [32]byte
}

// profile identifies the admitted parser/projection configuration, excluding
// the display label. YAML admission will live in remotecalendar (T2).
func NewConditionalSettings(source SourceKey, resource ConditionalResource, auth ConditionalAuth, destination ConditionalDestination, profile [32]byte) (ConditionalSettings, error) {
	s := ConditionalSettings{source: source, resource: resource, auth: auth, destination: destination, profile: profile}
	if !s.valid() {
		return ConditionalSettings{}, conditionalFailure(ConditionalInvalid)
	}
	if resource.kind == conditionalPublicResource {
		if _, e := destination.admitURL(resource.public); e != nil {
			return ConditionalSettings{}, e
		}
	}
	if resource.kind == conditionalSecretResource {
		for _, handle := range auth.handles {
			if handle == resource.handle {
				return ConditionalSettings{}, conditionalFailure(ConditionalInvalid)
			}
		}
	}
	return s, nil
}
func (s ConditionalSettings) valid() bool {
	_, _, e := s.source.Components()
	return e == nil && s.resource.valid() && s.auth.valid() && s.destination.valid() && s.profile != ([32]byte{})
}
func (s ConditionalSettings) String() string               { return "conditional settings [redacted]" }
func (s ConditionalSettings) GoString() string             { return s.String() }
func (s ConditionalSettings) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

type ConditionalFactory struct {
	settings ConditionalSettings
	policy   *CloudPolicy
	network  conditionalNetwork
}

func NewConditionalFactory(settings ConditionalSettings, policy *CloudPolicy) (ConditionalFactory, error) {
	if !settings.valid() || policy == nil || policy.states == nil {
		return ConditionalFactory{}, conditionalFailure(ConditionalInvalid)
	}
	return ConditionalFactory{settings: settings, policy: policy}, nil
}

type ConditionalHTTP interface {
	Prepare(context.Context, domain.EntityID) (ConditionalAttempt, error)
	ObserveConditional(context.Context, ConditionalAttempt, storage.RemoteCalendarSelection) (ConditionalObservation, error)
	BeginConditionalPublication(context.Context, ConditionalObservation) error
	ConfirmConditionalAdmitted(context.Context, ConditionalObservation, storage.RemoteCalendarCommit) error
	RejectConditionalObservation(context.Context, ConditionalObservation, ConditionalRejection) error
	Invalidate(context.Context, ConditionalRejection) error
	Close()
}

func (f ConditionalFactory) Open(ctx context.Context, host SessionHost) (ConditionalHTTP, error) {
	if nilInterface(ctx) || ctx.Err() != nil || !f.settings.valid() || f.policy == nil || nilInterface(host) {
		return nil, conditionalFailure(ConditionalInvalid)
	}
	transport, e := newConditionalTransport(f.settings.destination, nil, f.network)
	if e != nil {
		return nil, e
	}
	c, e := NewCloudClient(host, f.policy, transport)
	if e != nil {
		return nil, e
	}
	c.degrade(CloudTransportFailed)
	if !c.live() {
		c.Close()
		return nil, conditionalFailure(ConditionalSessionInactive)
	}
	owned, cancel := context.WithCancel(ctx)
	return &conditionalHTTP{client: c, settings: f.settings, ctx: owned, cancel: cancel}, nil
}
func (f ConditionalFactory) String() string               { return "conditional factory [redacted]" }
func (f ConditionalFactory) GoString() string             { return f.String() }
func (f ConditionalFactory) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

type ConditionalRejection uint8

const (
	ConditionalRejectTransport ConditionalRejection = iota + 1
	ConditionalRejectContent
	ConditionalRejectCachePolicy
	ConditionalRejectExpired
	ConditionalRejectReconfigured
	ConditionalRejectCommitUnknown
	ConditionalRejectPublication
)

func (r ConditionalRejection) valid() bool {
	return r >= ConditionalRejectTransport && r <= ConditionalRejectPublication
}

type conditionalHTTP struct {
	mu         sync.Mutex
	calls      sync.WaitGroup
	ctx        context.Context
	cancel     context.CancelFunc
	preparing  bool
	client     *CloudClient
	settings   ConditionalSettings
	closed     bool
	current    *conditionalAttempt
	generation uint64
}
type conditionalAttempt struct {
	owner       *conditionalHTTP
	entity      domain.EntityID
	scope       storage.RemoteCalendarScope
	request     CloudRequest
	id          [32]byte
	generation  uint64
	cancel      context.CancelFunc
	used        bool
	observation *conditionalObservation
}
type ConditionalAttempt struct{ value *conditionalAttempt }

func (a ConditionalAttempt) Scope() (storage.RemoteCalendarScope, error) {
	if a.value == nil || a.value.owner == nil {
		return storage.RemoteCalendarScope{}, conditionalFailure(ConditionalInvalid)
	}
	c := a.value.owner
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.current != a.value || !c.client.live() {
		return storage.RemoteCalendarScope{}, conditionalFailure(ConditionalSessionInactive)
	}
	return a.value.scope, nil
}
func (a ConditionalAttempt) String() string               { return "conditional attempt [redacted]" }
func (a ConditionalAttempt) GoString() string             { return a.String() }
func (a ConditionalAttempt) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

type ConditionalObservationKind uint8

const (
	ConditionalRepresentation200 ConditionalObservationKind = iota + 1
	ConditionalNotModified304
)

type conditionalBodyState uint8

const (
	conditionalBodyAvailable conditionalBodyState = iota + 1
	conditionalBodyTaken
	conditionalBodyNotApplicable
)

type conditionalObservation struct {
	bodyState conditionalBodyState
	owner     *conditionalHTTP
	attempt   *conditionalAttempt
	kind      ConditionalObservationKind
	store     storage.RemoteCalendarObservation
	body      []byte
	selected  [32]byte
	epoch     uint64
	at, until time.Time
	consumed  bool
}
type ConditionalObservation struct{ value *conditionalObservation }

func (o ConditionalObservation) Kind() ConditionalObservationKind {
	if o.value == nil {
		return 0
	}
	return o.value.kind
}
func (o ConditionalObservation) StoreObservation() (storage.RemoteCalendarObservation, error) {
	if o.value == nil || o.value.owner == nil {
		return storage.RemoteCalendarObservation{}, conditionalFailure(ConditionalInvalid)
	}
	c := o.value.owner
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.currentObservation(o.value) {
		return storage.RemoteCalendarObservation{}, conditionalFailure(ConditionalSessionInactive)
	}
	return o.value.store, nil
}
func (o ConditionalObservation) TakeRepresentationBytes() ([]byte, error) {
	if o.value == nil || o.value.owner == nil {
		return nil, conditionalFailure(ConditionalInvalid)
	}
	c := o.value.owner
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.currentObservation(o.value) || o.value.kind != ConditionalRepresentation200 || o.value.bodyState != conditionalBodyAvailable {
		return nil, conditionalFailure(ConditionalInvalid)
	}
	return takeConditionalBody(o.value)
}

// Called only under the current owner's admission lock. This private helper
// owns the transfer state machine; it does not admit a transport observation.
func takeConditionalBody(o *conditionalObservation) ([]byte, error) {
	if o == nil || o.consumed || o.kind != ConditionalRepresentation200 || o.bodyState != conditionalBodyAvailable {
		return nil, conditionalFailure(ConditionalInvalid)
	}
	body := o.body
	o.body = nil
	o.bodyState = conditionalBodyTaken
	return body, nil
}
func (o ConditionalObservation) ObservationTime() (time.Time, error) {
	if o.value == nil || o.value.owner == nil {
		return time.Time{}, conditionalFailure(ConditionalInvalid)
	}
	c := o.value.owner
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.currentObservation(o.value) {
		return time.Time{}, conditionalFailure(ConditionalSessionInactive)
	}
	return o.value.at, nil
}
func conditionalObservationTimeAdmitted(now time.Time, o *conditionalObservation) bool {
	return o != nil && !o.at.IsZero() && !now.Before(o.at) && now.Before(o.until)
}
func (o ConditionalObservation) FreshUntil() (time.Time, error) {
	if o.value == nil || o.value.owner == nil {
		return time.Time{}, conditionalFailure(ConditionalInvalid)
	}
	c := o.value.owner
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.currentObservation(o.value) {
		return time.Time{}, conditionalFailure(ConditionalSessionInactive)
	}
	return o.value.until, nil
}
func (o ConditionalObservation) String() string               { return "conditional observation [redacted]" }
func (o ConditionalObservation) GoString() string             { return o.String() }
func (o ConditionalObservation) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func (c *conditionalHTTP) String() string                     { return "conditional HTTP [redacted]" }
func (c *conditionalHTTP) GoString() string                   { return c.String() }
func (c *conditionalHTTP) MarshalJSON() ([]byte, error)       { return []byte(`"[redacted]"`), nil }
func (c *conditionalHTTP) begin(ctx context.Context) (context.Context, func(), error) {
	c.mu.Lock()
	if c.closed || !c.client.live() {
		c.mu.Unlock()
		return nil, nil, conditionalFailure(ConditionalSessionInactive)
	}
	c.calls.Add(1)
	c.mu.Unlock()
	bounded, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	return bounded, func() { stop(); cancel(); c.calls.Done() }, nil
}
func (c *conditionalHTTP) currentObservation(o *conditionalObservation) bool {
	return c != nil && !c.closed && o != nil && o.owner == c && !o.consumed && c.current == o.attempt && o.attempt.observation == o && c.client.live()
}
func conditionalHashPart(h hash.Hash, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	h.Write(size[:])
	h.Write([]byte(value))
}
func conditionalHandlePart(h hash.Hash, handle storage.SecretHandle) error {
	instance, name, e := handle.Components()
	if e != nil {
		return conditionalFailure(ConditionalInvalid)
	}
	i, _ := instance.Value()
	n, _ := name.Value()
	conditionalHashPart(h, i)
	conditionalHashPart(h, n)
	return nil
}
func (c *conditionalHTTP) Prepare(ctx context.Context, entity domain.EntityID) (ConditionalAttempt, error) {
	if c == nil || nilInterface(ctx) || ctx.Err() != nil {
		return ConditionalAttempt{}, conditionalFailure(ConditionalInvalid)
	}
	id, e := entity.Value()
	if e != nil {
		return ConditionalAttempt{}, conditionalFailure(ConditionalInvalid)
	}
	bounded, done, e := c.begin(ctx)
	if e != nil {
		return ConditionalAttempt{}, e
	}
	defer done()
	ctx = bounded
	c.mu.Lock()
	if c.closed || c.preparing || c.current != nil || !c.client.live() {
		c.mu.Unlock()
		return ConditionalAttempt{}, conditionalFailure(ConditionalSessionInactive)
	}
	generation := c.generation
	c.preparing = true
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.preparing = false; c.mu.Unlock() }()
	settings := c.settings
	credentials := make([]storage.SecretRevision, 0, 3)
	read := func(handle storage.SecretHandle, maximum int) ([]byte, error) {
		value, revision, e := c.client.secrets.readVersioned(ctx, handle)
		if e != nil {
			c.client.degrade(CloudCredentialFailed)
			return nil, conditionalFailure(ConditionalSessionInactive)
		}
		raw, e := value.Bytes()
		if e != nil || len(raw) == 0 || len(raw) > maximum || strings.ContainsAny(string(raw), "\x00\r\n") {
			c.client.degrade(CloudCredentialFailed)
			return nil, conditionalFailure(ConditionalInvalid)
		}
		credentials = append(credentials, revision)
		return raw, nil
	}
	resource := settings.resource.public
	if settings.resource.kind == conditionalSecretResource {
		raw, e := read(settings.resource.handle, 8192)
		if e != nil {
			return ConditionalAttempt{}, e
		}
		resource = string(raw)
	}
	u, e := settings.destination.admitURL(resource)
	if e != nil {
		return ConditionalAttempt{}, e
	}
	header := http.Header{"Accept": []string{"text/calendar"}, "User-Agent": []string{"Homeward-RemoteCalendar/1"}}
	switch settings.auth.kind {
	case conditionalAuthNone:
	case conditionalAuthBasic:
		user, e := read(settings.auth.handles[0], 4096)
		if e != nil {
			return ConditionalAttempt{}, e
		}
		pass, e := read(settings.auth.handles[1], 4096)
		if e != nil {
			return ConditionalAttempt{}, e
		}
		if strings.Contains(string(user), ":") {
			return ConditionalAttempt{}, conditionalFailure(ConditionalInvalid)
		}
		header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString(append(append(append([]byte(nil), user...), ':'), pass...)))
	case conditionalAuthBearer:
		token, e := read(settings.auth.handles[0], 8192)
		if e != nil {
			return ConditionalAttempt{}, e
		}
		for _, b := range token {
			if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.ContainsRune("-._~+/=", rune(b))) {
				return ConditionalAttempt{}, conditionalFailure(ConditionalInvalid)
			}
		}
		header.Set("Authorization", "Bearer "+string(token))
	default:
		return ConditionalAttempt{}, conditionalFailure(ConditionalInvalid)
	}
	h := sha256.New()
	h.Write([]byte("homeward/conditional-calendar/1"))
	conditionalHashPart(h, id)
	instance, _ := c.client.session.instance.Value()
	conditionalHashPart(h, instance)
	device, source, _ := settings.source.Components()
	conditionalHashPart(h, device)
	conditionalHashPart(h, source)
	if settings.resource.kind == conditionalPublicResource {
		conditionalHashPart(h, u.String())
	} else {
		if e := conditionalHandlePart(h, settings.resource.handle); e != nil {
			return ConditionalAttempt{}, e
		}
	}
	h.Write([]byte{byte(settings.resource.kind), byte(settings.auth.kind), byte(settings.destination.kind)})
	for _, handle := range settings.auth.handles {
		if handle != (storage.SecretHandle{}) {
			if e := conditionalHandlePart(h, handle); e != nil {
				return ConditionalAttempt{}, e
			}
		}
	}
	conditionalHashPart(h, conditionalDestinationRegistryIdentity)
	conditionalHashPart(h, settings.destination.origin)
	for _, p := range settings.destination.prefixes {
		conditionalHashPart(h, p.String())
	}
	h.Write(settings.profile[:])
	profile := conditionalRepresentationProfile()
	h.Write(profile[:])
	var key [32]byte
	copy(key[:], h.Sum(nil))
	scope, e := storage.NewRemoteCalendarScope(key, credentials)
	if e != nil {
		return ConditionalAttempt{}, conditionalFailure(ConditionalInvalid)
	}
	request, e := NewCloudRequest(http.MethodGet, u.String(), header, nil)
	if e != nil {
		return ConditionalAttempt{}, e
	}
	attempt := &conditionalAttempt{owner: c, entity: entity, scope: scope, request: request, generation: generation}
	if _, e := rand.Read(attempt.id[:]); e != nil {
		return ConditionalAttempt{}, conditionalFailure(ConditionalInvalid)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.current != nil || c.generation != generation || !c.client.live() || ctx.Err() != nil {
		return ConditionalAttempt{}, conditionalFailure(ConditionalSessionInactive)
	}
	c.current = attempt
	return ConditionalAttempt{value: attempt}, nil
}
func conditionalRepresentationProfile() [32]byte {
	return sha256.Sum256([]byte("GET|Accept:text/calendar|User-Agent:Homeward-RemoteCalendar/1|Accept-Encoding:gzip|Authorization:authenticated-scope|v1"))
}

func (c *conditionalHTTP) ObserveConditional(ctx context.Context, attempt ConditionalAttempt, selection storage.RemoteCalendarSelection) (ConditionalObservation, error) {
	if c == nil || nilInterface(ctx) || ctx.Err() != nil {
		return ConditionalObservation{}, conditionalFailure(ConditionalInvalid)
	}
	bounded, done, e := c.begin(ctx)
	if e != nil {
		return ConditionalObservation{}, e
	}
	defer done()
	ctx = bounded
	a := attempt.value
	c.mu.Lock()
	if c.closed || a == nil || a.owner != c || c.current != a || a.used || !selection.Matches(a.entity, a.scope) {
		c.mu.Unlock()
		return ConditionalObservation{}, conditionalFailure(ConditionalInvalid)
	}
	instance, device, source, ok := selection.SourceTuple()
	i, _ := c.client.session.instance.Value()
	d, s, _ := c.settings.source.Components()
	if !ok || instance != i || device != d || source != s {
		c.mu.Unlock()
		return ConditionalObservation{}, conditionalFailure(ConditionalInvalid)
	}
	a.used = true
	operation, cancel := context.WithCancel(ctx)
	defer cancel()
	a.cancel = cancel
	ctx = operation
	request := a.request
	request.header = request.header.Clone()
	sent, _ := storage.NewRemoteCalendarValidators(storage.RemoteValidatorsNone, "", time.Time{})
	conditional := selection.Conditional(a.scope)
	profile := conditionalRepresentationProfile()
	if p, present := selection.Metadata().Profile(); !present || p != profile {
		conditional = false
	}
	if conditional {
		sent = selection.Validators()
		if tag, ok := sent.ETag(); ok {
			request.header.Set("If-None-Match", tag)
		}
		if modified, ok := sent.LastModified(); ok {
			request.header.Set("If-Modified-Since", modified.Format(http.TimeFormat))
		}
	}
	c.mu.Unlock()
	started := c.client.policy.clock.Now()
	response, e := c.client.observe(ctx, request, cloudConditionalResponse)
	if e != nil {
		c.mu.Lock()
		if c.current == a {
			a.request = CloudRequest{}
			c.current = nil
		}
		c.mu.Unlock()
		if failure, ok := e.(*CloudError); ok {
			return ConditionalObservation{}, &CloudError{kind: failure.kind, retryAt: failure.retryAt, retryable: failure.retryable}
		}
		return ConditionalObservation{}, e
	}
	now := c.client.policy.clock.Now()
	delay := now.Sub(started)
	if delay < 0 {
		delay = 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.current != a || c.generation != a.generation || !c.client.live() {
		return ConditionalObservation{}, conditionalFailure(ConditionalSessionInactive)
	}
	reject := func(e error) (ConditionalObservation, error) {
		a.request = CloudRequest{}
		c.current = nil
		c.client.degrade(CloudHTTPFailed)
		return ConditionalObservation{}, e
	}
	kind := ConditionalRepresentation200
	previous := storage.NoRemoteCalendarHTTPMetadata()
	var selected [32]byte
	if response.status == http.StatusNotModified {
		kind = ConditionalNotModified304
		if !conditional {
			return reject(conditionalFailure(ConditionalUnmatchedNotModified))
		}
		if e := selectConditional304(sent, response.header); e != nil {
			return reject(e)
		}
		selected, ok = selection.Digest()
		if !ok {
			return reject(conditionalFailure(ConditionalUnmatchedNotModified))
		}
		previous = selection.Metadata()
	}
	metadata, until, e := parseConditionalMetadata(response.header, previous, now, delay, profile)
	if e != nil {
		return reject(e)
	}
	validators, e := conditionalResponseValidators(response.header, sent, kind)
	if e != nil {
		return reject(e)
	}
	admission := storage.RemoteCalendarHTTPAdmission{}
	if kind == ConditionalRepresentation200 {
		admission, e = storage.RemoteCalendarHTTPRepresentation(sha256.Sum256(response.body))
	} else {
		admission, e = storage.RemoteCalendarHTTPRevalidation(selected)
	}
	if e != nil {
		return reject(conditionalFailure(ConditionalInvalid))
	}
	observation, e := storage.NewRemoteCalendarHTTPObservation(a.scope, a.id, now, until, validators, metadata, admission)
	if e != nil {
		return reject(conditionalFailure(ConditionalInvalid))
	}
	o := &conditionalObservation{owner: c, attempt: a, kind: kind, store: observation, body: response.body, selected: selected, epoch: response.healthEpoch, at: now, until: until}
	if kind == ConditionalRepresentation200 {
		o.bodyState = conditionalBodyAvailable
	} else {
		o.bodyState = conditionalBodyNotApplicable
	}
	a.request = CloudRequest{}
	a.observation = o
	return ConditionalObservation{value: o}, nil
}

// BeginConditionalPublication is bound to the current private observation.
// It closes Ready without retiring the epoch/receipt needed after Replace040.
func (c *conditionalHTTP) BeginConditionalPublication(ctx context.Context, observation ConditionalObservation) error {
	if c == nil || nilInterface(ctx) || ctx.Err() != nil {
		return conditionalFailure(ConditionalInvalid)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	o := observation.value
	if !c.currentObservation(o) {
		return conditionalFailure(ConditionalSessionInactive)
	}
	if o.kind == ConditionalRepresentation200 && o.bodyState != conditionalBodyTaken || o.kind == ConditionalNotModified304 && o.bodyState != conditionalBodyNotApplicable {
		return conditionalFailure(ConditionalInvalid)
	}
	if !conditionalObservationTimeAdmitted(c.client.policy.clock.Now(), o) {
		return conditionalFailure(ConditionalExpired)
	}
	return c.client.pendingObservationEpoch(ctx, o.epoch)
}
func (c *conditionalHTTP) ConfirmConditionalAdmitted(ctx context.Context, observation ConditionalObservation, commit storage.RemoteCalendarCommit) error {
	if c == nil || nilInterface(ctx) || ctx.Err() != nil {
		return conditionalFailure(ConditionalInvalid)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	o := observation.value
	if !c.currentObservation(o) {
		return conditionalFailure(ConditionalSessionInactive)
	}
	if o.kind == ConditionalRepresentation200 && o.bodyState != conditionalBodyTaken || o.kind == ConditionalNotModified304 && o.bodyState != conditionalBodyNotApplicable {
		return conditionalFailure(ConditionalInvalid)
	}
	key, _ := o.attempt.scope.Key()
	id, _ := o.store.ID()
	if !commit.Matches(o.attempt.entity, key, id) {
		return conditionalFailure(ConditionalInvalid)
	}
	digest, ok := commit.Digest()
	if !ok || o.kind == ConditionalNotModified304 && digest != o.selected {
		return conditionalFailure(ConditionalInvalid)
	}
	now := c.client.policy.clock.Now()
	if !conditionalObservationTimeAdmitted(now, o) {
		o.consumed = true
		o.body = nil
		c.current = nil
		c.client.degrade(CloudHTTPFailed)
		return conditionalFailure(ConditionalExpired)
	}
	e := c.client.confirmObservationEpoch(ctx, o.epoch)
	o.consumed = true
	o.body = nil
	c.current = nil
	return e
}
func (c *conditionalHTTP) RejectConditionalObservation(ctx context.Context, observation ConditionalObservation, reason ConditionalRejection) error {
	if c == nil || c.client == nil || nilInterface(ctx) || !reason.valid() {
		return conditionalFailure(ConditionalInvalid)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.currentObservation(observation.value) {
		return conditionalFailure(ConditionalSessionInactive)
	}
	observation.value.consumed = true
	observation.value.body = nil
	c.current = nil
	c.generation++
	c.client.degrade(CloudHTTPFailed)
	return nil
}
func (c *conditionalHTTP) Invalidate(ctx context.Context, reason ConditionalRejection) error {
	if c == nil || c.client == nil || nilInterface(ctx) || !reason.valid() {
		return conditionalFailure(ConditionalInvalid)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return conditionalFailure(ConditionalSessionInactive)
	}
	if c.current != nil {
		if c.current.cancel != nil {
			c.current.cancel()
		}
		c.current.request = CloudRequest{}
		if o := c.current.observation; o != nil {
			o.consumed = true
			o.body = nil
		}
		c.current = nil
	}
	c.generation++
	c.client.degrade(CloudHTTPFailed)
	return nil
}
func (c *conditionalHTTP) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.generation++
	if c.current != nil {
		if c.current.cancel != nil {
			c.current.cancel()
		}
		c.current.request = CloudRequest{}
		if o := c.current.observation; o != nil {
			o.consumed = true
			o.body = nil
		}
		c.current = nil
	}
	if c.cancel != nil {
		c.cancel()
	}
	c.client.degrade(CloudHTTPFailed)
	c.mu.Unlock()
	c.calls.Wait()
	c.client.Close()
}

var conditionalFieldNames = [...]string{"Cache-Control", "Vary", "Date", "Age", "Expires"}

func conditionalResponseHeaders(header http.Header) http.Header {
	out := make(http.Header, 7)
	for _, name := range append(conditionalFieldNames[:], "Etag", "Last-Modified") {
		if values, ok := header[name]; ok {
			out[name] = append([]string(nil), values...)
		}
	}
	return out
}
func conditionalSingle(header http.Header, name string) (string, bool, error) {
	values, present := header[name]
	if !present {
		return "", false, nil
	}
	if len(values) != 1 {
		return "", false, conditionalFailure(ConditionalInvalidResponse)
	}
	return values[0], true, nil
}
func conditionalResponseValidators(header http.Header, sent storage.RemoteCalendarValidators, kind ConditionalObservationKind) (storage.RemoteCalendarValidators, error) {
	tag, present, e := conditionalSingle(header, "Etag")
	if e != nil {
		return storage.RemoteCalendarValidators{}, e
	}
	modified, hasModified, e := conditionalSingle(header, "Last-Modified")
	if e != nil {
		return storage.RemoteCalendarValidators{}, e
	}
	var stamp time.Time
	if hasModified {
		stamp, e = http.ParseTime(modified)
		if e != nil {
			return storage.RemoteCalendarValidators{}, conditionalFailure(ConditionalInvalidResponse)
		}
	}
	if kind == ConditionalNotModified304 {
		if !present {
			tag, present = sent.ETag()
		}
		if !hasModified {
			stamp, hasModified = sent.LastModified()
		}
	}
	k := storage.RemoteValidatorsNone
	if present {
		k = storage.RemoteValidatorsETag
	}
	if hasModified {
		if present {
			k = storage.RemoteValidatorsBoth
		} else {
			k = storage.RemoteValidatorsLastModified
		}
	}
	validators, e := storage.NewRemoteCalendarValidators(k, tag, stamp)
	if e != nil {
		return storage.RemoteCalendarValidators{}, conditionalFailure(ConditionalInvalidResponse)
	}
	return validators, nil
}
func selectConditional304(sent storage.RemoteCalendarValidators, header http.Header) error {
	if !sent.Valid() || sent.Kind() == storage.RemoteValidatorsNone {
		return conditionalFailure(ConditionalUnmatchedNotModified)
	}
	tag, present, e := conditionalSingle(header, "Etag")
	if e != nil {
		return conditionalFailure(ConditionalValidatorMismatch)
	}
	selectedByTag := false
	if present {
		returned, e := storage.NewRemoteCalendarValidators(storage.RemoteValidatorsETag, tag, time.Time{})
		if e != nil {
			return conditionalFailure(ConditionalValidatorMismatch)
		}
		stored, ok := sent.ETag()
		if !ok {
			return conditionalFailure(ConditionalValidatorMismatch)
		}
		incoming, _ := returned.ETag()
		if strings.HasPrefix(incoming, "W/") {
			if strings.TrimPrefix(stored, "W/") != strings.TrimPrefix(incoming, "W/") {
				return conditionalFailure(ConditionalValidatorMismatch)
			}
		} else if incoming != stored {
			return conditionalFailure(ConditionalValidatorMismatch)
		}
		selectedByTag = true
	}
	raw, present, e := conditionalSingle(header, "Last-Modified")
	if e != nil {
		return conditionalFailure(ConditionalValidatorMismatch)
	}
	if present {
		stamp, e := http.ParseTime(raw)
		stored, ok := sent.LastModified()
		if e != nil || !selectedByTag && (!ok || !stored.Equal(stamp)) {
			return conditionalFailure(ConditionalValidatorMismatch)
		}
	}
	return nil
}
func conditionalDigits(raw string) bool {
	if raw == "" {
		return false
	}
	for _, b := range raw {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}
func conditionalDirectives(raw string) ([]string, error) {
	if len(raw) > storage.RemoteCalendarHTTPMetadataLimit {
		return nil, conditionalFailure(ConditionalInvalidResponse)
	}
	var out []string
	start := 0
	quoted := false
	escaped := false
	for i, c := range raw {
		if c < 0x20 && c != '\t' || c == 0x7f {
			return nil, conditionalFailure(ConditionalInvalidResponse)
		}
		if escaped {
			escaped = false
			continue
		}
		if quoted && c == '\\' {
			escaped = true
			continue
		}
		if c == '"' {
			quoted = !quoted
		}
		if c == ',' && !quoted {
			out = append(out, strings.TrimSpace(raw[start:i]))
			start = i + 1
		}
	}
	if quoted || escaped {
		return nil, conditionalFailure(ConditionalInvalidResponse)
	}
	out = append(out, strings.TrimSpace(raw[start:]))
	return out, nil
}
func parseConditionalMetadata(header http.Header, previous storage.RemoteCalendarHTTPMetadata, now time.Time, delay time.Duration, profile [32]byte) (storage.RemoteCalendarHTTPMetadata, time.Time, error) {
	invalid := func() (storage.RemoteCalendarHTTPMetadata, time.Time, error) {
		return storage.RemoteCalendarHTTPMetadata{}, time.Time{}, conditionalFailure(ConditionalInvalidResponse)
	}
	if !previous.Valid() || now.IsZero() || profile == ([32]byte{}) || delay < 0 {
		return invalid()
	}
	if prior, present := previous.Profile(); present && prior != profile {
		return invalid()
	}
	policyRefused := func() (storage.RemoteCalendarHTTPMetadata, time.Time, error) {
		return storage.RemoteCalendarHTTPMetadata{}, time.Time{}, conditionalFailure(ConditionalCachePolicy)
	}
	fields := make([]storage.RemoteHTTPField, 0, 5)
	size := 54
	for i, name := range conditionalFieldNames {
		values, present := header[name]
		if !present && previous.Present() {
			values, present = previous.Field(storage.RemoteHTTPFieldKind(i + 1)).Values()
		}
		presence := storage.RemoteHTTPAbsent
		if present {
			presence = storage.RemoteHTTPPresent
		}
		size += 3
		for _, v := range values {
			size += len(v) + 2
			if size > storage.RemoteCalendarHTTPMetadataLimit {
				return invalid()
			}
		}
		f, e := storage.NewRemoteHTTPField(storage.RemoteHTTPFieldKind(i+1), presence, values)
		if e != nil {
			return invalid()
		}
		fields = append(fields, f)
	}
	get := func(kind storage.RemoteHTTPFieldKind) ([]string, bool) { return fields[int(kind)-1].Values() }
	// The nominal daily poll is the maximum freshness without an explicit
	// duration. HTTP policy can only shorten that promise; the pilot keeps
	// NextPoll separate from this deadline (no-cache is never a retry loop).
	lifetime := 24 * time.Hour
	noCache := false
	hasMaxAge := false
	var maxAge uint64
	controls, _ := get(storage.RemoteHTTPCacheControl)
	for _, raw := range controls {
		directives, e := conditionalDirectives(raw)
		if e != nil {
			return invalid()
		}
		for _, directive := range directives {
			name, value, hasValue := strings.Cut(directive, "=")
			name = strings.ToLower(strings.TrimSpace(name))
			value = strings.TrimSpace(value)
			switch name {
			case "no-store":
				return policyRefused()
			case "no-cache":
				noCache = true
			case "max-age":
				if !hasValue {
					return invalid()
				}
				if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
					value = value[1 : len(value)-1]
				}
				if !conditionalDigits(value) {
					return invalid()
				}
				n, e := strconv.ParseUint(value, 10, 64)
				if e != nil || hasMaxAge && n != maxAge {
					return invalid()
				}
				hasMaxAge = true
				maxAge = n
			}
		}
	}
	vary, _ := get(storage.RemoteHTTPVary)
	for _, raw := range vary {
		for _, name := range strings.Split(raw, ",") {
			switch strings.ToLower(strings.TrimSpace(name)) {
			case "", "accept", "accept-encoding", "user-agent", "authorization":
			default:
				return policyRefused()
			}
		}
	}
	var date, expires time.Time
	dates, present := get(storage.RemoteHTTPDate)
	if present {
		if len(dates) != 1 {
			return invalid()
		}
		var e error
		date, e = http.ParseTime(dates[0])
		if e != nil || date.IsZero() {
			return invalid()
		}
	}
	exp, present := get(storage.RemoteHTTPExpires)
	if present {
		if len(exp) != 1 {
			return invalid()
		}
		var e error
		expires, e = http.ParseTime(exp[0])
		if e != nil || expires.IsZero() {
			return invalid()
		}
	}
	age := time.Duration(0)
	ages, present := get(storage.RemoteHTTPAge)
	if present {
		if len(ages) != 1 {
			return invalid()
		}
		if !conditionalDigits(ages[0]) {
			return invalid()
		}
		n, e := strconv.ParseUint(ages[0], 10, 64)
		if e != nil {
			return invalid()
		}
		if n >= 86400 {
			age = 24 * time.Hour
		} else {
			age = time.Duration(n) * time.Second
		}
	}
	if delay > 24*time.Hour-age {
		age = 24 * time.Hour
	} else {
		age += delay
	}
	if !date.IsZero() && now.After(date) {
		apparent := now.Sub(date)
		if apparent > age {
			age = apparent
		}
		if age > 24*time.Hour {
			age = 24 * time.Hour
		}
	}
	anchor := now
	_, newDate := header["Date"]
	_, newAge := header["Age"]
	if previous.Present() && !newDate && !newAge {
		var ok bool
		anchor, age, ok = previous.AgeAnchor()
		if !ok {
			return invalid()
		}
	}
	if hasMaxAge {
		if maxAge < 86400 {
			lifetime = time.Duration(maxAge) * time.Second
		}
	} else if !expires.IsZero() {
		base := date
		if base.IsZero() {
			base = anchor
		}
		candidate := expires.Sub(base)
		if candidate < 0 {
			candidate = 0
		}
		if candidate < lifetime {
			lifetime = candidate
		}
	}
	if noCache {
		lifetime = 0
	}
	elapsed := now.Sub(anchor)
	if elapsed < 0 {
		elapsed = 0
	}
	remaining := lifetime - age
	if elapsed >= remaining {
		remaining = 0
	} else {
		remaining -= elapsed
	}
	metadata, e := storage.NewRemoteCalendarHTTPMetadata(profile, fields, anchor, age)
	if e != nil {
		return invalid()
	}
	return metadata, now.Add(remaining), nil
}

var _ ConditionalHTTP = (*conditionalHTTP)(nil)

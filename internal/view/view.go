// Package view serves bounded, immutable pages from one sequencer capture.
// Authentication and configuration authorities are injected by composition.
package view

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"reflect"
	"sort"
	"sync"
	"time"

	environmentwire "github.com/damione1/Homeward/gen/home/environment/v1"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	statewire "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/apiresources"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/sequencer"
	"google.golang.org/protobuf/proto"
)

var (
	ErrAlreadyExists     = errors.New("entity view already exists")
	ErrInvalidInput      = errors.New("invalid view input")
	ErrViewChanged       = errors.New("view authority or configuration changed; open a new view")
	ErrSnapshotExpired   = errors.New("snapshot expired; open a new view")
	ErrSnapshotUnknown   = errors.New("unknown snapshot token")
	ErrPageToken         = errors.New("unexpected page token")
	ErrResourceExhausted = errors.New("snapshot memory limit reached")
)

const MaxPageSize = 500
const MaxSnapshots = 16
const MaxSnapshotBytes = 16 << 20
const MaxTotalBytes = 128 << 20
const MaxPreparations = 2
const SnapshotTTL = 60 * time.Second

type ActorID = domain.ActorID

func ParseActorID(value string) (ActorID, error) {
	actor, err := domain.ParseActorID(value)
	if err != nil {
		return ActorID{}, ErrInvalidInput
	}
	return actor, nil
}

// ConfigStamp must come from the trusted active-configuration authority.
type ConfigStamp struct {
	revision uint64
	hash     [32]byte
}

func NewConfigStamp(revision uint64, hash [32]byte) (ConfigStamp, error) {
	if revision == 0 || hash == ([32]byte{}) {
		return ConfigStamp{}, ErrInvalidInput
	}
	return ConfigStamp{revision, hash}, nil
}
func (c ConfigStamp) Components() (uint64, [32]byte, error) {
	if c.revision == 0 || c.hash == ([32]byte{}) {
		return 0, [32]byte{}, ErrInvalidInput
	}
	return c.revision, c.hash, nil
}

type Grant struct {
	actor             ActorID
	revision          uint64
	allowed           map[domain.EntityID]struct{}
	digest            [32]byte
	slots             map[domain.EntityID]map[authorization.CapabilitySlot]struct{}
	complete          map[domain.EntityID]bool
	actions           map[domain.EntityID]map[authorization.CapabilitySlot]statewire.ActionAccess
	displayTimeZone   string
	profileBaseDigest [32]byte
}

func NewGrant(actor ActorID, revision uint64, allowed []domain.EntityID) (Grant, error) {
	if _, err := actor.Value(); err != nil || revision == 0 {
		return Grant{}, ErrInvalidInput
	}
	set := make(map[domain.EntityID]struct{}, len(allowed))
	for _, id := range allowed {
		if _, err := id.Value(); err != nil {
			return Grant{}, ErrInvalidInput
		}
		set[id] = struct{}{}
	}
	return Grant{actor: actor, revision: revision, allowed: set, digest: hashIDs(1, set)}, nil
}
func (g Grant) validFor(actor ActorID) bool {
	// NewGrant copied the private set and established its digest. No getter
	// exposes the map, so rehashing it on every streamed frame is unnecessary.
	return g.actor == actor && g.revision > 0 && g.allowed != nil && g.digest != ([32]byte{})
}

// Authorizer receives only the actor from the trusted server auth boundary.
// No client-supplied list of rights is accepted by Service.
type Authorizer interface {
	CurrentGrant(context.Context, ActorID) (Grant, error)
}
type Configuration interface {
	CurrentConfig(context.Context) (ConfigStamp, error)
}

type filterKind uint8

const (
	filterUnspecified filterKind = iota
	filterAll
	filterOnly
)

type Filter struct {
	kind filterKind
	ids  map[domain.EntityID]struct{}
	hash [32]byte
}

func None() Filter {
	return Filter{kind: filterOnly, ids: map[domain.EntityID]struct{}{}, hash: hashIDs(3, nil)}
}

func All() Filter { return Filter{kind: filterAll, hash: hashIDs(2, nil)} }
func Only(ids []domain.EntityID) (Filter, error) {
	if len(ids) == 0 || len(ids) > sequencer.MaxProjectedEntities {
		return Filter{}, ErrInvalidInput
	}
	set := make(map[domain.EntityID]struct{}, len(ids))
	for _, id := range ids {
		if _, err := id.Value(); err != nil {
			return Filter{}, ErrInvalidInput
		}
		set[id] = struct{}{}
	}
	return Filter{kind: filterOnly, ids: set, hash: hashIDs(3, set)}, nil
}
func (f Filter) valid() bool {
	switch f.kind {
	case filterAll:
		return f.ids == nil && f.hash == hashIDs(2, nil)
	case filterOnly:
		return f.ids != nil && f.hash == hashIDs(3, f.ids)
	case filterUnspecified:
		return false
	default:
		return false
	}
}
func (f Filter) Digest() ([32]byte, error) {
	if !f.valid() {
		return [32]byte{}, ErrInvalidInput
	}
	return f.hash, nil
}
func (f Filter) permits(id domain.EntityID) bool {
	if f.kind == filterAll {
		return true
	}
	_, ok := f.ids[id]
	return ok
}

// hashIDs uses length-prefixed UTF-8 in sorted order. Embedded newlines and
// separators therefore cannot give distinct filters the same encoding.
func hashIDs(tag byte, ids map[domain.EntityID]struct{}) [32]byte {
	parts := make([]string, 0, len(ids))
	for id := range ids {
		value, _ := id.Value()
		parts = append(parts, value)
	}
	sort.Strings(parts)
	h := sha256.New()
	h.Write([]byte{tag})
	var length [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		h.Write(length[:])
		h.Write([]byte(part))
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

type Cursor struct {
	sequence              sequencer.Cursor
	catalogue             sequencer.Cursor
	config                ConfigStamp
	authorizationRevision uint64
	filterHash            [32]byte
	grantDigest           [32]byte
	proof                 [40]byte
}

// CataloguePosition is the registry/capability admission fence captured during
// preparation. It is independent of state sequence and grants no authority.
func (c Cursor) CataloguePosition() (sequencer.Cursor, error) {
	if _, _, err := c.catalogue.Components(); err != nil {
		return sequencer.Cursor{}, ErrInvalidInput
	}
	return c.catalogue, nil
}

func (c Cursor) Components() (sequencer.Cursor, ConfigStamp, uint64, [32]byte, error) {
	if _, _, err := c.sequence.Components(); err != nil {
		return sequencer.Cursor{}, ConfigStamp{}, 0, [32]byte{}, err
	}
	if _, _, err := c.config.Components(); err != nil || c.authorizationRevision == 0 || c.filterHash == ([32]byte{}) {
		return sequencer.Cursor{}, ConfigStamp{}, 0, [32]byte{}, ErrInvalidInput
	}
	return c.sequence, c.config, c.authorizationRevision, c.filterHash, nil
}

type Item struct {
	record        integrations.RegistryRecord
	state         domain.ObservedState
	hasState      bool
	capabilities  []*wireregistry.CapabilityDescription
	actions       []*statewire.CapabilityActionAccess
	projected     []apiresources.Capability
	hasProjection bool
}

// ProjectedCapabilities returns the immutable prepared image. The boolean
// distinguishes a configured empty image from a native view without a projector.
func (i Item) ProjectedCapabilities() ([]apiresources.Capability, bool) {
	return append([]apiresources.Capability(nil), i.projected...), i.hasProjection
}

// CapabilityProjector materializes only already filtered native items. It runs
// once per item during capture preparation, outside view/sequencer locks. It
// must read bounded trusted owner caches, perform no device I/O, and preserve
// the supplied actor's exact readable slots and private media permissions.
// The cursor contains capture stamps; its admission proof is finalized later.
type CapabilityProjector func(context.Context, ActorID, Item, Cursor) ([]apiresources.Capability, error)

func (i Item) Registry() integrations.RegistryRecord { return i.record }
func (i Item) State() (domain.ObservedState, bool)   { return i.state, i.hasState }
func (i Item) Capabilities() []*wireregistry.CapabilityDescription {
	out := make([]*wireregistry.CapabilityDescription, len(i.capabilities))
	for n, d := range i.capabilities {
		out[n] = proto.Clone(d).(*wireregistry.CapabilityDescription)
	}
	return out
}
func (i Item) ActionAccess() []*statewire.CapabilityActionAccess {
	out := make([]*statewire.CapabilityActionAccess, len(i.actions))
	for n, permission := range i.actions {
		out[n] = proto.CloneOf(permission)
	}
	return out
}

type Page struct {
	SnapshotToken  string
	Cursor         Cursor
	Items          []Item
	NextPageToken  string
	ExpiresAt      time.Time
	DisplayProfile *environmentwire.DisplayProfile
}

type capture struct {
	name            apitypes.EntityViewName
	selection       *entitySelection
	catalogue       map[domain.EntityID]Item
	created         time.Time
	lifetime        context.Context
	cancel          context.CancelFunc
	retired         bool
	watchers        int
	actor           ActorID
	grantRevision   uint64
	grantDigest     [32]byte
	config          ConfigStamp
	cursor          Cursor
	items           []Item
	pageSize        int
	expires         time.Time
	bytes           int
	displayTimeZone string
}
type pageSpan struct {
	start, end int
	next       string
}
type Service struct {
	mu          sync.Mutex
	source      *sequencer.Sequencer
	auth        Authorizer
	config      Configuration
	clock       sequencer.Clock
	captures    map[string]*capture
	used        int
	preparing   chan struct{}
	replaySlots chan struct{}
	proofKey    [32]byte
	projector   CapabilityProjector
}

func NewService(source *sequencer.Sequencer, auth Authorizer, config Configuration, clock sequencer.Clock, projectors ...CapabilityProjector) (*Service, error) {
	if source == nil || nilInterface(auth) || nilInterface(config) || nilInterface(clock) || len(projectors) > 1 || len(projectors) == 1 && projectors[0] == nil {
		return nil, ErrInvalidInput
	}
	if err := source.CheckCaptureAvailable(); err != nil {
		return nil, err
	}
	s := &Service{source: source, auth: auth, config: config, clock: clock, captures: make(map[string]*capture), preparing: make(chan struct{}, MaxPreparations), replaySlots: make(chan struct{}, MaxConcurrentReplays)}
	if len(projectors) == 1 {
		s.projector = projectors[0]
	}
	if _, err := rand.Read(s.proofKey[:]); err != nil {
		return nil, err
	}
	if s.proofKey == ([32]byte{}) {
		return nil, ErrInvalidInput
	}
	return s, nil
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func (s *Service) ready() bool {
	return s != nil && s.source != nil && !nilInterface(s.auth) && !nilInterface(s.config) && !nilInterface(s.clock) && s.captures != nil && s.preparing != nil && s.replaySlots != nil && s.proofKey != ([32]byte{})
}

func randomToken() (string, error) {
	var bytes [32]byte
	for {
		if _, err := rand.Read(bytes[:]); err != nil {
			return "", err
		}
		if bytes != ([32]byte{}) {
			return hex.EncodeToString(bytes[:]), nil
		}
	}
}

func (s *Service) authority(ctx context.Context, actor ActorID) (Grant, ConfigStamp, error) {
	grant, err := s.auth.CurrentGrant(ctx, actor)
	if err != nil {
		return Grant{}, ConfigStamp{}, err
	}
	if !grant.validFor(actor) {
		return Grant{}, ConfigStamp{}, ErrInvalidInput
	}
	config, err := s.config.CurrentConfig(ctx)
	if err != nil {
		return Grant{}, ConfigStamp{}, err
	}
	if _, _, err := config.Components(); err != nil {
		return Grant{}, ConfigStamp{}, err
	}
	return grant, config, nil
}

func (s *Service) OpenView(ctx context.Context, actor ActorID, filter Filter, pageSize int) (Page, error) {
	return s.openView(ctx, actor, filter, pageSize, nil)
}

func (s *Service) openView(ctx context.Context, actor ActorID, filter Filter, pageSize int, selection *entitySelection) (Page, error) {
	if !s.ready() || ctx == nil || pageSize < 1 || pageSize > MaxPageSize || !filter.valid() {
		return Page{}, ErrInvalidInput
	}
	if _, err := actor.Value(); err != nil {
		return Page{}, err
	}
	select {
	case s.preparing <- struct{}{}:
		defer func() { <-s.preparing }()
	default:
		return Page{}, ErrResourceExhausted
	}
	firstGrant, firstConfig, err := s.authority(ctx, actor)
	if err != nil {
		return Page{}, err
	}
	firstCatalogue, err := s.source.CataloguePosition()
	if err != nil {
		return Page{}, err
	}
	snapshot, err := s.source.CaptureBounded(MaxSnapshotBytes)
	if err != nil {
		if errors.Is(err, sequencer.ErrSnapshotBudget) {
			return Page{}, ErrResourceExhausted
		}
		return Page{}, err
	}
	if !snapshot.RegistryReady {
		return Page{}, sequencer.ErrRegistryUnavailable
	}
	lastGrant, lastConfig, err := s.authority(ctx, actor)
	if err != nil {
		return Page{}, err
	}
	if firstGrant.revision != lastGrant.revision || firstGrant.digest != lastGrant.digest || firstConfig != lastConfig {
		return Page{}, ErrViewChanged
	}
	filterHash := filter.hash
	if selection != nil {
		filterHash = selection.hash(filterHash)
	}
	cursor := Cursor{sequence: snapshot.Cursor, catalogue: firstCatalogue, config: lastConfig, authorizationRevision: lastGrant.revision, filterHash: filterHash, grantDigest: lastGrant.digest}
	ids := make([]domain.EntityID, 0, len(snapshot.Registry))
	for id, record := range snapshot.Registry {
		if record.Retired() || !filter.permits(id) {
			continue
		}
		if _, allowed := lastGrant.allowed[id]; allowed {
			ids = append(ids, id)
		}
	}
	// API resource IDs are the first192 bits of this registry identity hash.
	// Canonical ordering is independent of mutable labels and native prefixes.
	sort.Slice(ids, func(i, j int) bool {
		left, _ := ids[i].Value()
		right, _ := ids[j].Value()
		a, b := sha256.Sum256([]byte(left)), sha256.Sum256([]byte(right))
		return bytes.Compare(a[:24], b[:24]) < 0
	})
	items := make([]Item, 0, len(ids))
	var catalogue map[domain.EntityID]Item
	if selection != nil && selection.exposed {
		catalogue = make(map[domain.EntityID]Item, len(ids))
	}
	bytes := 0
	for _, id := range ids {
		record := snapshot.Registry[id]
		state, hasState := snapshot.States[id]
		descriptions := snapshot.Capabilities[id]
		descriptions, complete := lastGrant.filterCapabilities(id, descriptions)
		if lastGrant.slots != nil && len(descriptions) == 0 {
			continue
		}
		if !complete {
			state, hasState = domain.ObservedState{}, false
		}
		device, entity, _ := record.IDs()
		entityText, _ := entity.Value()
		deviceText, _ := device.Value()
		instance, sourceKey, _ := record.Source()
		instanceText, _ := instance.Value()
		sourceDevice, sourceEntity, _ := sourceKey.Components()
		itemBytes := 512 + len(entityText) + len(deviceText) + len(instanceText) + len(sourceDevice) + len(sourceEntity)
		_, _, aliases, _ := record.Aliases()
		for _, alias := range aliases {
			value, _ := alias.Value()
			itemBytes += len(value) + 32
		}
		for _, description := range descriptions {
			encoded := proto.Size(description)
			if encoded > MaxSnapshotBytes/4 {
				return Page{}, ErrResourceExhausted
			}
			itemBytes += encoded * 4
		}
		actions := lastGrant.actionAccess(id, descriptions)
		for _, permission := range actions {
			itemBytes += 4*proto.Size(permission) + 64
		}
		if hasState {
			stateBytes, err := sequencer.EstimateTriggerStateBytes(id, state)
			if err != nil || stateBytes > MaxSnapshotBytes-itemBytes {
				return Page{}, ErrResourceExhausted
			}
			itemBytes += stateBytes
		}
		item := Item{record: record, state: state, hasState: hasState, capabilities: descriptions, actions: actions}
		if s.projector != nil {
			projected, err := s.projector(ctx, actor, item, cursor)
			if err != nil {
				return Page{}, err
			}
			retained, err := validateProjection(id, descriptions, projected)
			if err != nil {
				return Page{}, err
			}
			if retained > MaxSnapshotBytes-itemBytes {
				return Page{}, ErrResourceExhausted
			}
			itemBytes += retained
			item.projected = append([]apiresources.Capability(nil), projected...)
			item.hasProjection = true
		}
		if catalogue != nil {
			// Charge all authorized seeds and selection references once. The
			// public watch uses these frozen declarations for membership changes.
			if itemBytes+256 > MaxSnapshotBytes-bytes {
				return Page{}, ErrResourceExhausted
			}
			bytes += itemBytes + 256
			catalogue[id] = item
		}
		if selection != nil {
			selected, err := selection.matches(item)
			if err != nil {
				return Page{}, err
			}
			if !selected {
				continue
			}
		}
		if catalogue == nil {
			if itemBytes > MaxSnapshotBytes-bytes {
				return Page{}, ErrResourceExhausted
			}
			bytes += itemBytes
		}
		items = append(items, item)
	}
	finalGrant, finalConfig, err := s.authority(ctx, actor)
	if err != nil {
		return Page{}, err
	}
	finalCatalogue, err := s.source.CataloguePosition()
	if err != nil {
		return Page{}, err
	}
	if finalGrant.revision != lastGrant.revision || finalGrant.digest != lastGrant.digest || finalConfig != lastConfig || finalCatalogue != firstCatalogue {
		return Page{}, ErrViewChanged
	}
	pageCount := (len(items) + pageSize - 1) / pageSize
	bytes += pageCount*(160+4*len(lastGrant.displayTimeZone)) + 1024
	if selection != nil && selection.exposed {
		bytes += 2*len(selection.filter.Source()) + 1024
	}
	if bytes > MaxSnapshotBytes {
		return Page{}, ErrResourceExhausted
	}
	token, err := randomToken()
	if err != nil {
		return Page{}, err
	}
	now := s.clock.Now()
	if now.IsZero() {
		return Page{}, ErrInvalidInput
	}
	if err := s.signCursorAt(&cursor, actor, lastGrant, snapshot.CapturedTick); err != nil {
		return Page{}, err
	}
	c := &capture{actor: actor, grantRevision: lastGrant.revision, grantDigest: lastGrant.digest, config: lastConfig, cursor: cursor, items: items, pageSize: pageSize, expires: now.Add(SnapshotTTL), bytes: bytes}
	c.displayTimeZone = lastGrant.displayTimeZone
	if catalogue != nil {
		digest := sha256.Sum256([]byte(token))
		name, nameErr := apitypes.ParseEntityViewName(selection.home.String() + "/entityViews/view-" + hex.EncodeToString(digest[:24]))
		if nameErr != nil {
			return Page{}, nameErr
		}
		if selection.requested != nil {
			name = *selection.requested
		}
		c.name, c.selection, c.catalogue, c.created = name, selection, catalogue, now
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire(now)
	if c.name.Valid() {
		for _, existing := range s.captures {
			if existing.name == c.name {
				if existing.actor != actor {
					return Page{}, authorization.ErrDenied
				}
				if existing.grantDigest != lastGrant.digest || existing.grantRevision != lastGrant.revision {
					return Page{}, authorization.ErrDenied
				}
				return Page{}, ErrAlreadyExists
			}
		}
	}
	if len(s.captures) >= MaxSnapshots || s.used+bytes > MaxTotalBytes {
		return Page{}, ErrResourceExhausted
	}
	page, err := s.pageAt(token, c, 0, pageSize)
	if err != nil {
		return Page{}, err
	}
	// Internal read captures and named EntityViews share the same retirement
	// lifetime, including unary responses retained through the HTTP flush.
	c.lifetime, c.cancel = context.WithCancel(context.Background())
	s.captures[token] = c
	s.used += bytes
	return page, nil
}

func renderPage(token string, c *capture, span pageSpan) Page {
	page := Page{SnapshotToken: token, Cursor: c.cursor, Items: append([]Item(nil), c.items[span.start:span.end]...), NextPageToken: span.next, ExpiresAt: c.expires}
	if c.displayTimeZone != "" {
		page.DisplayProfile = &environmentwire.DisplayProfile{TimeZone: c.displayTimeZone}
	}
	return page
}

// Continuations bind a fixed offset to this immutable capture. No growing
// per-page replay map is needed; changing page_size never changes the cut.
func (s *Service) continuation(snapshot string, offset int) string {
	var value [32]byte
	binary.BigEndian.PutUint64(value[:8], uint64(offset))
	mac := hmac.New(sha256.New, s.proofKey[:])
	mac.Write([]byte("homeward.view-page/v2\x00"))
	mac.Write([]byte(snapshot))
	mac.Write(value[:8])
	copy(value[8:], mac.Sum(nil)[:24])
	return hex.EncodeToString(value[:])
}
func (s *Service) pageAt(token string, c *capture, start, size int) (Page, error) {
	if start < 0 || start > len(c.items) || size < 1 || size > MaxPageSize {
		return Page{}, ErrPageToken
	}
	end := start + size
	if end > len(c.items) {
		end = len(c.items)
	}
	span := pageSpan{start: start, end: end}
	if end < len(c.items) {
		span.next = s.continuation(token, end)
	}
	return renderPage(token, c, span), nil
}

func (s *Service) expire(now time.Time) {
	for token, c := range s.captures {
		if !now.Before(c.expires) {
			s.retireCaptureLocked(token, c)
		}
	}
}

// ReleaseView relinquishes pagination storage, not the independent signed
// replay cursor. Ownership is checked against the authenticated actor; callers
// cannot distinguish foreign, expired and already released snapshot tokens.
func (s *Service) ReleaseView(ctx context.Context, actor ActorID, snapshotToken string) error {
	if !s.ready() || ctx == nil || snapshotToken == "" {
		return ErrInvalidInput
	}
	if _, err := actor.Value(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if capture, exists := s.captures[snapshotToken]; exists && capture.actor == actor {
		s.retireCaptureLocked(snapshotToken, capture)
	}
	return nil
}

func (s *Service) ReadPage(ctx context.Context, actor ActorID, snapshotToken, pageToken string) (Page, error) {
	return s.ReadPageSize(ctx, actor, snapshotToken, pageToken, 0)
}

// ReadPageSize honors an explicitly changed page size within the same capture.
// Zero keeps the initial size for native callers; API adapters supply a bound.
func (s *Service) ReadPageSize(ctx context.Context, actor ActorID, snapshotToken, pageToken string, size int) (Page, error) {
	if pageToken == "" {
		return Page{}, ErrInvalidInput
	}
	return s.readPage(ctx, actor, snapshotToken, pageToken, size)
}

// ReadFirstPage reads the original frozen image, with current ownership, rights,
// configuration and expiry checks. It performs no recapture or provider read.
func (s *Service) ReadFirstPage(ctx context.Context, actor ActorID, snapshotToken string, size int) (Page, error) {
	return s.readPage(ctx, actor, snapshotToken, "", size)
}

func (s *Service) readPage(ctx context.Context, actor ActorID, snapshotToken, pageToken string, size int) (Page, error) {
	if size < 0 || size > MaxPageSize {
		return Page{}, ErrInvalidInput
	}
	if !s.ready() || ctx == nil || snapshotToken == "" {
		return Page{}, ErrInvalidInput
	}
	if _, err := actor.Value(); err != nil {
		return Page{}, err
	}
	now := s.clock.Now()
	if now.IsZero() {
		return Page{}, ErrInvalidInput
	}
	s.mu.Lock()
	preflight, exists := s.captures[snapshotToken]
	if !exists || preflight.actor != actor {
		s.mu.Unlock()
		return Page{}, ErrSnapshotUnknown
	}
	if !now.Before(preflight.expires) {
		s.retireCaptureLocked(snapshotToken, preflight)
		s.mu.Unlock()
		return Page{}, ErrSnapshotExpired
	}
	s.mu.Unlock()
	grant, config, err := s.authority(ctx, actor)
	if err != nil {
		return Page{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now = s.clock.Now()
	if now.IsZero() {
		return Page{}, ErrInvalidInput
	}
	c, ok := s.captures[snapshotToken]
	if !ok {
		return Page{}, ErrSnapshotUnknown
	}
	if !now.Before(c.expires) {
		s.retireCaptureLocked(snapshotToken, c)
		return Page{}, ErrSnapshotExpired
	}
	if c.actor != actor {
		return Page{}, ErrSnapshotUnknown
	}
	if c.grantRevision != grant.revision || c.grantDigest != grant.digest || c.config != config {
		s.retireCaptureLocked(snapshotToken, c)
		return Page{}, ErrViewChanged
	}
	if size == 0 {
		size = c.pageSize
	}
	if pageToken == "" {
		return s.pageAt(snapshotToken, c, 0, size)
	}
	raw, err := hex.DecodeString(pageToken)
	if err != nil || len(raw) != 32 {
		return Page{}, ErrPageToken
	}
	offset := binary.BigEndian.Uint64(raw[:8])
	if offset == 0 || offset >= uint64(len(c.items)) || !hmac.Equal([]byte(pageToken), []byte(s.continuation(snapshotToken, int(offset)))) {
		return Page{}, ErrPageToken
	}
	return s.pageAt(snapshotToken, c, int(offset), size)
}

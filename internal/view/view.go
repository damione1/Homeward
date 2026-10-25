// Package view serves bounded, immutable pages from one sequencer capture.
// Authentication and configuration authorities are injected by composition.
package view

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"reflect"
	"sort"
	"sync"
	"time"

	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	statewire "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/sequencer"
	"google.golang.org/protobuf/proto"
)

var (
	ErrInvalidInput      = errors.New("invalid view input")
	ErrViewChanged       = errors.New("view authority or configuration changed; open a new view")
	ErrSnapshotExpired   = errors.New("snapshot expired; open a new view")
	ErrSnapshotUnknown   = errors.New("unknown snapshot token")
	ErrPageToken         = errors.New("unexpected page token")
	ErrResourceExhausted = errors.New("snapshot memory limit reached")
)

const MaxPageSize = 256
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
	actor    ActorID
	revision uint64
	allowed  map[domain.EntityID]struct{}
	digest   [32]byte
	slots    map[domain.EntityID]map[authorization.CapabilitySlot]struct{}
	complete map[domain.EntityID]bool
	actions  map[domain.EntityID]map[authorization.CapabilitySlot]statewire.ActionAccess
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
		return len(f.ids) > 0 && f.hash == hashIDs(3, f.ids)
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
	config                ConfigStamp
	authorizationRevision uint64
	filterHash            [32]byte
	grantDigest           [32]byte
	proof                 [40]byte
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
	record       integrations.RegistryRecord
	state        domain.ObservedState
	hasState     bool
	capabilities []*wireregistry.CapabilityDescription
	actions      []*statewire.CapabilityActionAccess
}

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
	SnapshotToken string
	Cursor        Cursor
	Items         []Item
	NextPageToken string
	ExpiresAt     time.Time
}

type capture struct {
	actor         ActorID
	grantRevision uint64
	grantDigest   [32]byte
	config        ConfigStamp
	cursor        Cursor
	items         []Item
	pageSize      int
	position      int
	expected      string
	served        map[string]pageSpan
	expires       time.Time
	bytes         int
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
}

func NewService(source *sequencer.Sequencer, auth Authorizer, config Configuration, clock sequencer.Clock) (*Service, error) {
	if source == nil || nilInterface(auth) || nilInterface(config) || nilInterface(clock) {
		return nil, ErrInvalidInput
	}
	if err := source.CheckCaptureAvailable(); err != nil {
		return nil, err
	}
	s := &Service{source: source, auth: auth, config: config, clock: clock, captures: make(map[string]*capture), preparing: make(chan struct{}, MaxPreparations), replaySlots: make(chan struct{}, MaxConcurrentReplays)}
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
	ids := make([]domain.EntityID, 0, len(snapshot.Registry))
	for id, record := range snapshot.Registry {
		if record.Retired() || !filter.permits(id) {
			continue
		}
		if _, allowed := lastGrant.allowed[id]; allowed {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { left, _ := ids[i].Value(); right, _ := ids[j].Value(); return left < right })
	items := make([]Item, 0, len(ids))
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
		if itemBytes > MaxSnapshotBytes-bytes {
			return Page{}, ErrResourceExhausted
		}
		bytes += itemBytes
		items = append(items, Item{record: record, state: state, hasState: hasState, capabilities: descriptions, actions: actions})
	}
	pageCount := (len(items) + pageSize - 1) / pageSize
	bytes += pageCount*160 + 1024
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
	cursor := Cursor{sequence: snapshot.Cursor, config: lastConfig, authorizationRevision: lastGrant.revision, filterHash: filter.hash, grantDigest: lastGrant.digest}
	if err := s.signCursorAt(&cursor, actor, lastGrant, snapshot.CapturedTick); err != nil {
		return Page{}, err
	}
	c := &capture{actor: actor, grantRevision: lastGrant.revision, grantDigest: lastGrant.digest, config: lastConfig, cursor: cursor, items: items, pageSize: pageSize, served: make(map[string]pageSpan), expires: now.Add(SnapshotTTL), bytes: bytes}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire(now)
	if len(s.captures) >= MaxSnapshots || s.used+bytes > MaxTotalBytes {
		return Page{}, ErrResourceExhausted
	}
	page, err := s.nextPage(token, c, "")
	if err != nil {
		return Page{}, err
	}
	s.captures[token] = c
	s.used += bytes
	return page, nil
}

func renderPage(token string, c *capture, span pageSpan) Page {
	return Page{SnapshotToken: token, Cursor: c.cursor, Items: append([]Item(nil), c.items[span.start:span.end]...), NextPageToken: span.next, ExpiresAt: c.expires}
}

func (s *Service) nextPage(token string, c *capture, requestToken string) (Page, error) {
	end := c.position + c.pageSize
	if end > len(c.items) {
		end = len(c.items)
	}
	span := pageSpan{start: c.position, end: end}
	if end < len(c.items) {
		next, err := randomToken()
		if err != nil {
			return Page{}, err
		}
		span.next = next
	}
	c.position = end
	c.expected = span.next
	if requestToken != "" {
		c.served[requestToken] = span
	}
	return renderPage(token, c, span), nil
}

func (s *Service) expire(now time.Time) {
	for token, c := range s.captures {
		if !now.Before(c.expires) {
			s.used -= c.bytes
			delete(s.captures, token)
		}
	}
}

func (s *Service) ReadPage(ctx context.Context, actor ActorID, snapshotToken, pageToken string) (Page, error) {
	if !s.ready() || ctx == nil || snapshotToken == "" || pageToken == "" {
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
		s.used -= preflight.bytes
		delete(s.captures, snapshotToken)
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
		s.used -= c.bytes
		delete(s.captures, snapshotToken)
		return Page{}, ErrSnapshotExpired
	}
	if c.actor != actor {
		return Page{}, ErrSnapshotUnknown
	}
	if c.grantRevision != grant.revision || c.grantDigest != grant.digest || c.config != config {
		s.used -= c.bytes
		delete(s.captures, snapshotToken)
		return Page{}, ErrViewChanged
	}
	if span, served := c.served[pageToken]; served {
		return renderPage(snapshotToken, c, span), nil
	}
	if c.expected == "" || c.expected != pageToken {
		return Page{}, ErrPageToken
	}
	return s.nextPage(snapshotToken, c, pageToken)
}

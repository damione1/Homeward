package scenes

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/sequencer"
)

const (
	maxTemporaryScenes   = 128
	maxConcurrentApplies = 128
	maxSceneMembers      = 256
	maxTemporaryBytes    = 4 << 20
	maxTemporaryTTL      = 24 * time.Hour
)

type Clock interface{ Now() time.Time }
type CaptureSource interface {
	Capture() (sequencer.Snapshot, error)
	CurrentAdmissionEpoch() (string, error)
}
type CommandPort interface {
	CaptureLight(context.Context, domain.HAAlias) (integrations.LightCapture, error)
	CaptureFan(context.Context, domain.HAAlias) (integrations.FanCapture, error)
	CaptureDirect(context.Context, domain.HAAlias, integrations.DirectKind) (integrations.DirectCapture, error)
	DispatchPrepared(context.Context, integrations.PreparedLight) (*actions.CommandResponse, error)
	DispatchPreparedFan(context.Context, integrations.PreparedFan) (*actions.CommandResponse, error)
	DispatchPreparedDirect(context.Context, integrations.PreparedDirect) (*actions.CommandResponse, error)
	RecordedPreparedLight(context.Context, integrations.PreparedLight) (*actions.CommandResponse, bool, error)
	RecordedPreparedFan(context.Context, integrations.PreparedFan) (*actions.CommandResponse, bool, error)
	RecordedPreparedDirect(context.Context, integrations.PreparedDirect) (*actions.CommandResponse, bool, error)
}

type ExplicitMember struct {
	alias domain.HAAlias
	value domain.StateValue
	valid bool
}

func NewExplicitLight(alias domain.HAAlias, value domain.LightValue) (ExplicitMember, error) {
	if !aliasDomain(alias, "light.") {
		return ExplicitMember{}, ErrInvalidScene
	}
	state, err := domain.NewLightStateValue(value)
	if err != nil {
		return ExplicitMember{}, err
	}
	return ExplicitMember{alias: alias, value: state, valid: true}, nil
}
func NewExplicitSwitch(alias domain.HAAlias, on bool) (ExplicitMember, error) {
	if !aliasDomain(alias, "switch.") {
		return ExplicitMember{}, ErrInvalidScene
	}
	return ExplicitMember{alias: alias, value: domain.NewSwitchValue(on), valid: true}, nil
}
func NewExplicitFan(alias domain.HAAlias, speed domain.FanSpeed) (ExplicitMember, error) {
	if !aliasDomain(alias, "fan.") {
		return ExplicitMember{}, ErrInvalidScene
	}
	value, err := domain.NewFanStateValue(speed)
	if err != nil {
		return ExplicitMember{}, err
	}
	return ExplicitMember{alias: alias, value: value, valid: true}, nil
}
func aliasDomain(alias domain.HAAlias, prefix string) bool {
	text, err := alias.Value()
	return err == nil && strings.HasPrefix(text, prefix)
}
func nilPort(value any) bool {
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

type temporaryEntry struct {
	ref            SceneRef
	members        []memberSpec
	omissions      []MemberResult
	epoch          string
	configRevision uint64
	expiresAt      time.Time
	retainedBytes  int
}
type applyLease struct{ cancel context.CancelFunc }
type Service struct {
	catalogue   Catalogue
	capture     CaptureSource
	generation  integrations.GenerationAuthority
	commands    CommandPort
	clock       Clock
	mu          sync.Mutex
	temporary   map[domain.HAAlias]*temporaryEntry
	active      sync.WaitGroup
	applying    map[*applyLease]struct{}
	nextVersion uint64
	retained    int
	closed      bool
}

func NewService(catalogue Catalogue, capture CaptureSource, generation integrations.GenerationAuthority, commands CommandPort, clock Clock) (*Service, error) {
	if len(catalogue.definitions) != 12 || nilPort(capture) || nilPort(generation) || nilPort(commands) || nilPort(clock) {
		return nil, ErrInvalidScene
	}
	return &Service{catalogue: catalogue, capture: capture, generation: generation,
		commands: commands, clock: clock, temporary: make(map[domain.HAAlias]*temporaryEntry),
		applying: make(map[*applyLease]struct{})}, nil
}

func (s *Service) Declarative(id domain.HAAlias) (SceneRef, error) {
	if s == nil {
		return SceneRef{}, ErrInvalidScene
	}
	def, ok := s.catalogue.Definition(id)
	if !ok {
		return SceneRef{}, ErrSceneUnavailable
	}
	return SceneRef{id: id, kind: SceneDeclarative, digest: def.Source().Digest()}, nil
}

type OmissionCode uint8

const (
	OmissionInvalid OmissionCode = iota
	OmissionAlias
	OmissionQuality
	OmissionValue
	OmissionCapability
)

type Omission struct {
	alias  domain.HAAlias
	code   OmissionCode
	reason string
}

func (o Omission) Alias() domain.HAAlias { return o.alias }
func (o Omission) Code() OmissionCode    { return o.code }
func (o Omission) Reason() string        { return o.reason }

// CreateTemporary captures one sequencer snapshot under the current config
// generation. Only CriticalValue is eligible; omissions are retained with
// the scene so later preparation cannot fill them from a newer state.
func (s *Service) CreateTemporary(ctx context.Context, id domain.HAAlias, explicit []ExplicitMember, captureAliases []domain.HAAlias, ttl time.Duration, expectedVersion *uint64) (SceneRef, []Omission, error) {
	if s == nil || ctx == nil || !aliasDomain(id, "scene.") || ttl <= 0 || ttl > maxTemporaryTTL ||
		len(explicit)+len(captureAliases) == 0 || len(explicit)+len(captureAliases) > maxSceneMembers {
		return SceneRef{}, nil, ErrInvalidScene
	}
	if _, exists := s.catalogue.Definition(id); exists {
		return SceneRef{}, nil, ErrSceneConflict
	}
	seen := make(map[domain.HAAlias]bool, len(explicit)+len(captureAliases))
	members := make([]memberSpec, 0, len(explicit)+len(captureAliases))
	for _, item := range explicit {
		if !item.valid || seen[item.alias] {
			return SceneRef{}, nil, ErrInvalidScene
		}
		if _, err := item.value.Kind(); err != nil {
			return SceneRef{}, nil, ErrInvalidScene
		}
		seen[item.alias] = true
		members = append(members, memberSpec{origin: originNative, alias: item.alias, native: item.value})
	}
	for _, alias := range captureAliases {
		if _, err := alias.Value(); err != nil || seen[alias] {
			return SceneRef{}, nil, ErrInvalidScene
		}
		seen[alias] = true
	}
	var result SceneRef
	var omissions []Omission
	err := s.withSceneRead(ctx, func(access *integrations.SceneReadAccess) error {
		return s.generation.WithCurrent(ctx, func(revision uint64) error {
			if revision == 0 {
				return ErrSceneUnavailable
			}
			snapshot, err := s.capture.Capture()
			if err != nil || !snapshot.RegistryReady {
				return ErrSceneUnavailable
			}
			if access != nil && access.RequiresEntityAdmission() {
				for alias := range seen {
					entity, err := snapshotEntity(snapshot, alias)
					if err != nil {
						return integrations.ErrCommandAuthorizationDenied
					}
					if err := access.AdmitEntity(entity, snapshot.Capabilities[entity]); err != nil {
						return integrations.ErrCommandAuthorizationDenied
					}
				}
			}
			epoch, _, err := snapshot.Cursor.Components()
			if err != nil {
				return ErrSceneUnavailable
			}
			now := s.clock.Now()
			if now.IsZero() {
				return ErrInvalidScene
			}
			for _, alias := range captureAliases {
				member, omission := captureMember(snapshot, alias)
				if entity, err := snapshotEntity(snapshot, alias); err == nil {
					member.capturedEntity = entity
				}
				if omission.code != OmissionInvalid {
					omissions = append(omissions, omission)
					members = append(members, memberSpec{alias: alias, omissionCode: omission.code, omissionReason: omission.reason, capturedEntity: member.capturedEntity})
				} else {
					members = append(members, member)
				}
			}
			admitted := 0
			for _, member := range members {
				if member.origin != originInvalid {
					admitted++
				}
			}
			if admitted == 0 {
				return ErrSceneUnavailable
			}
			entry := &temporaryEntry{members: append([]memberSpec(nil), members...), epoch: epoch,
				configRevision: revision, expiresAt: now.Add(ttl)}
			for _, omission := range omissions {
				entry.omissions = append(entry.omissions, MemberResult{alias: omission.alias, field: "capture", reason: omission.reason, outcome: OutcomeRefused})
			}
			entry.retainedBytes = temporaryBytes(id, entry)
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.closed {
				return ErrSceneClosed
			}
			s.purgeLocked(now, epoch, revision)
			prior := s.temporary[id]
			if prior == nil && expectedVersion != nil || prior != nil && (expectedVersion == nil || *expectedVersion != prior.ref.version) {
				return ErrSceneConflict
			}
			if prior == nil && len(s.temporary) >= maxTemporaryScenes {
				return ErrSceneCapacity
			}
			retained := s.retained + entry.retainedBytes
			if prior != nil {
				retained -= prior.retainedBytes
			}
			if retained > maxTemporaryBytes {
				return ErrSceneCapacity
			}
			if s.nextVersion == ^uint64(0) {
				return ErrSceneCapacity
			}
			s.nextVersion++
			entry.ref = SceneRef{id: id, kind: SceneTemporary, version: s.nextVersion}
			s.temporary[id] = entry
			s.retained = retained
			result = entry.ref
			return nil
		})
	})
	if err != nil {
		return SceneRef{}, append([]Omission(nil), omissions...), err
	}
	return result, append([]Omission(nil), omissions...), nil
}

func captureMember(snapshot sequencer.Snapshot, alias domain.HAAlias) (memberSpec, Omission) {
	var found domain.EntityID
	for entity, record := range snapshot.Registry {
		if record.Retired() {
			continue
		}
		_, _, aliases, err := record.Aliases()
		if err != nil {
			continue
		}
		for _, candidate := range aliases {
			if candidate == alias {
				if found != (domain.EntityID{}) && found != entity {
					return memberSpec{}, Omission{alias, OmissionAlias, "alias is ambiguous"}
				}
				found = entity
			}
		}
	}
	if found == (domain.EntityID{}) {
		return memberSpec{}, Omission{alias, OmissionAlias, "alias is not current"}
	}
	state, exists := snapshot.States[found]
	if !exists {
		return memberSpec{}, Omission{alias, OmissionQuality, "fresh observed value is absent"}
	}
	_, _, _, _, revision, err := state.Components()
	value, critical := state.CriticalValue()
	if err != nil || !critical {
		return memberSpec{}, Omission{alias, OmissionQuality, "state is unavailable, stale, estimated or restored"}
	}
	kind, err := value.Kind()
	if err != nil {
		return memberSpec{}, Omission{alias, OmissionValue, "invalid observed value"}
	}
	qualified := false
	wantSchema := ""
	switch kind {
	case domain.ValueLight:
		wantSchema = contracts.LightSchema
	case domain.ValueSwitch:
		wantSchema = contracts.SwitchSchema
	case domain.ValueFan:
		wantSchema = contracts.FanSchema
	}
	for _, description := range snapshot.Capabilities[found] {
		if description != nil && description.SchemaId == wantSchema {
			qualified = true
		}
	}
	if !qualified {
		return memberSpec{}, Omission{alias, OmissionCapability, "command capability is absent"}
	}
	switch kind {
	case domain.ValueLight:
		if !aliasDomain(alias, "light.") {
			break
		}
		light, _ := value.Light()
		on, percent, _, err := light.Components()
		if err == nil && (!on || percent != nil && *percent > 0) {
			return memberSpec{origin: originNative, alias: alias, native: value, capturedRevision: revision}, Omission{}
		}
	case domain.ValueSwitch:
		if aliasDomain(alias, "switch.") {
			return memberSpec{origin: originNative, alias: alias, native: value, capturedRevision: revision}, Omission{}
		}
	case domain.ValueFan:
		if aliasDomain(alias, "fan.") {
			return memberSpec{origin: originNative, alias: alias, native: value, capturedRevision: revision}, Omission{}
		}
	}
	return memberSpec{}, Omission{alias, OmissionCapability, "observed member is not reproducible by this scene profile"}
}

func temporaryBytes(id domain.HAAlias, entry *temporaryEntry) int {
	name, _ := id.Value()
	// Charge the retained backing arrays at their actual element widths. A
	// memberSpec carries both closed value variants inline, so a flat 256-byte
	// allowance can undercount even before its alias/string allocations. The
	// extra fixed charge covers map buckets, pointers and allocator rounding;
	// this is a conservative retention budget, not a heap-size promise.
	size := 1024 + int(unsafe.Sizeof(*entry)) + 2*len(name) + len(entry.epoch)
	size += cap(entry.members) * int(unsafe.Sizeof(memberSpec{}))
	size += cap(entry.omissions) * int(unsafe.Sizeof(MemberResult{}))
	for _, member := range entry.members {
		alias, _ := member.alias.Value()
		size += len(alias) + len(member.omissionReason)
		if entity, err := member.capturedEntity.Value(); err == nil {
			size += len(entity)
		}
	}
	for _, omission := range entry.omissions {
		alias, _ := omission.alias.Value()
		size += len(alias) + len(omission.reason)
	}
	return size
}

func (s *Service) purgeLocked(now time.Time, epoch string, revision uint64) {
	for id, entry := range s.temporary {
		if !entry.expiresAt.After(now) || entry.epoch != epoch || entry.configRevision != revision {
			delete(s.temporary, id)
			s.retained -= entry.retainedBytes
		}
	}
}

func (s *Service) DeleteTemporary(ctx context.Context, ref SceneRef) error {
	if s == nil || ctx == nil || ref.kind != SceneTemporary {
		return ErrInvalidScene
	}
	if _, err := ref.ID(); err != nil {
		return err
	}
	return s.generation.WithCurrent(ctx, func(revision uint64) error {
		epoch, err := s.capture.CurrentAdmissionEpoch()
		if err != nil || revision == 0 {
			return ErrSceneUnavailable
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return ErrSceneClosed
		}
		s.purgeLocked(s.clock.Now(), epoch, revision)
		entry := s.temporary[ref.id]
		if entry == nil || entry.ref != ref {
			return ErrSceneConflict
		}
		delete(s.temporary, ref.id)
		s.retained -= entry.retainedBytes
		return nil
	})
}

func (s *Service) Temporary(ctx context.Context, id domain.HAAlias) (SceneRef, error) {
	if s == nil || ctx == nil || !aliasDomain(id, "scene.") {
		return SceneRef{}, ErrInvalidScene
	}
	var ref SceneRef
	err := s.generation.WithCurrent(ctx, func(revision uint64) error {
		epoch, err := s.capture.CurrentAdmissionEpoch()
		if err != nil || epoch == "" || revision == 0 {
			return ErrSceneUnavailable
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return ErrSceneClosed
		}
		s.purgeLocked(s.clock.Now(), epoch, revision)
		entry := s.temporary[id]
		if entry == nil {
			return ErrSceneUnavailable
		}
		ref = entry.ref
		return nil
	})
	return ref, err
}

func (s *Service) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	clear(s.temporary)
	s.retained = 0
	cancels := make([]context.CancelFunc, 0, len(s.applying))
	for lease := range s.applying {
		cancels = append(cancels, lease.cancel)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	s.active.Wait()
}

package sonos

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	media "github.com/damione1/Homeward/gen/home/media/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type cached struct {
	owner    *pilot
	snapshot *media.Snapshot
	captured time.Time
	topology string
	revision uint64
}
type Runtime struct {
	mu         sync.Mutex
	records    map[domain.EntityID]cached
	epoch      string
	descriptor []byte
	digest     [32]byte
}

func NewRuntime() (*Runtime, error) {
	var nonce [16]byte
	if _, e := rand.Read(nonce[:]); e != nil {
		return nil, e
	}
	set := &descriptorpb.FileDescriptorSet{}
	seen := map[string]bool{}
	var add func(protoreflect.FileDescriptor)
	add = func(f protoreflect.FileDescriptor) {
		if seen[f.Path()] {
			return
		}
		seen[f.Path()] = true
		for i := 0; i < f.Imports().Len(); i++ {
			add(f.Imports().Get(i).FileDescriptor)
		}
		set.File = append(set.File, protodesc.ToFileDescriptorProto(f))
	}
	add(media.File_home_media_v1_media_proto)
	b, e := proto.MarshalOptions{Deterministic: true}.Marshal(set)
	if e != nil {
		return nil, e
	}
	return &Runtime{records: map[domain.EntityID]cached{}, epoch: hex.EncodeToString(nonce[:]), descriptor: b, digest: sha256.Sum256(b)}, nil
}
func (r *Runtime) description(label string) *registry.CapabilityDescription {
	return &registry.CapabilityDescription{SchemaId: Schema, Major: 1, Endpoint: Endpoint, DisplayName: label, DescriptorSet: append([]byte(nil), r.descriptor...), DescriptorSha256: append([]byte(nil), r.digest[:]...), Constraints: &registry.CapabilityDescription_Extension{Extension: &registry.ExtensionConstraints{}}}
}

// Snapshot returns an immutable bounded cached observation. Missing/expired
// observations fail closed rather than turning a restored value into a fresh one.
func (r *Runtime) Snapshot(entity domain.EntityID) (*media.Snapshot, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.records[entity]
	if !ok || c.snapshot == nil || time.Since(c.captured) > 45*time.Second || c.owner.ctx.Err() != nil {
		return nil, false
	}
	return proto.Clone(c.snapshot).(*media.Snapshot), true
}
func (r *Runtime) Factory(s Settings) (integrations.PilotFactory, error) {
	if r == nil {
		return nil, ErrSettings
	}
	if _, e := s.Instance(); e != nil {
		return nil, e
	}
	return func() integrations.Pilot { return &pilot{settings: s, runtime: r} }, nil
}

type pilot struct {
	settings Settings
	runtime  *Runtime
	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
}

func (p *pilot) Start(ctx context.Context, host integrations.SessionHost, tasks integrations.Tasks) error {
	p.mu.Lock()
	if p.ctx != nil {
		p.mu.Unlock()
		return ErrSettings
	}
	p.ctx, p.cancel = context.WithCancel(ctx)
	p.mu.Unlock()
	players := p.settings.Players()
	candidates := make([]integrations.Candidate, 0, len(players))
	for _, player := range players {
		c, e := integrations.NewCandidate(player.SourceKey(), []*registry.CapabilityDescription{p.runtime.description(player.Label)})
		if e != nil {
			return e
		}
		candidates = append(candidates, c)
	}
	bindings, e := host.Discover(p.ctx, candidates)
	if e != nil {
		return e
	}
	if len(bindings) != len(players) {
		return ErrUnavailable
	}
	ids := map[string]domain.EntityID{}
	instance, _ := p.settings.Instance()
	for i, b := range bindings {
		actual, key, e := b.Source()
		_, entity, e2 := b.IDs()
		if e != nil || e2 != nil || actual != instance || key != players[i].SourceKey() {
			return ErrUnavailable
		}
		ids[players[i].UID] = entity
	}
	return tasks.Go(func(taskCtx context.Context) error {
		r := newReader()
		defer r.close()
		defer p.clear()
		timer := time.NewTicker(15 * time.Second)
		defer timer.Stop()
		for {
			if p.ctx.Err() != nil || taskCtx.Err() != nil {
				return nil
			}
			p.poll(p.ctx, r, players, ids)
			health, detail := integrations.HealthReady, "sonos native read-only polling"
			for _, id := range ids {
				if _, ok := p.runtime.Snapshot(id); !ok {
					health, detail = integrations.HealthDegraded, "sonos native observation missing or expired"
					break
				}
			}
			report, err := integrations.NewHealthReport(health, detail)
			if err != nil {
				return err
			}
			if err = host.ReportHealth(p.ctx, report); err != nil {
				if p.ctx.Err() != nil {
					return nil
				}
				return err
			}
			select {
			case <-p.ctx.Done():
				return nil
			case <-taskCtx.Done():
				return nil
			case <-timer.C:
			}
		}
	})
}
func (p *pilot) Stop(context.Context) error {
	p.mu.Lock()
	if p.cancel != nil {
		p.cancel()
	}
	p.mu.Unlock()
	p.clear()
	return nil
}
func (p *pilot) clear() {
	p.runtime.mu.Lock()
	defer p.runtime.mu.Unlock()
	for id, c := range p.runtime.records {
		if c.owner == p {
			delete(p.runtime.records, id)
		}
	}
}

func (p *pilot) poll(parent context.Context, r *reader, players []PlayerInput, ids map[string]domain.EntityID) {
	ctx, cancel := context.WithTimeout(parent, 12*time.Second)
	defer cancel()
	observations := make([]observation, len(players))
	valid := make([]bool, len(players))
	var group sync.WaitGroup
	for i, player := range players {
		group.Add(1)
		go func(i int, player PlayerInput) {
			defer group.Done()
			o, e := r.read(ctx, player)
			if e == nil {
				observations[i], valid[i] = o, true
			}
		}(i, player)
	}
	group.Wait()
	// Playback is read once from each verified, explicitly configured coordinator.
	transports := make([]observation, len(players))
	transportOK := make([]bool, len(players))
	indices := map[string]int{}
	for i, player := range players {
		indices[player.UID] = i
		if valid[i] && observations[i].coordinator == player.UID {
			group.Add(1)
			go func(i int, player PlayerInput) {
				defer group.Done()
				o, e := r.transport(ctx, player)
				if e == nil {
					transports[i], transportOK[i] = o, true
				}
			}(i, player)
		}
	}
	group.Wait()
	if ctx.Err() != nil {
		return
	}
	now := time.Now()
	p.runtime.mu.Lock()
	defer p.runtime.mu.Unlock()
	if parent.Err() != nil {
		return
	}
	for i, player := range players {
		entity := ids[player.UID]
		o := observations[i]
		ci, configured := indices[o.coordinator]
		if !valid[i] || !configured || !valid[ci] || !transportOK[ci] {
			delete(p.runtime.records, entity)
			continue
		}
		if observations[ci].coordinator != o.coordinator || !slices.Equal(o.members, observations[ci].members) {
			// Independent reads can straddle a regrouping. Never attach another
			// group's playback to a fresh observation; reconcile next poll.
			delete(p.runtime.records, entity)
			continue
		}
		var members []*media.Member
		complete := true
		for _, uid := range o.members {
			mi, known := indices[uid]
			if !known {
				complete = false
				break
			}
			id, _ := ids[uid].Value()
			members = append(members, &media.Member{EntityId: id, Label: players[mi].Label})
		}
		if !complete {
			delete(p.runtime.records, entity)
			continue
		}
		sort.Slice(members, func(i, j int) bool { return members[i].EntityId < members[j].EntityId })
		coordinator, _ := ids[o.coordinator].Value()
		identity, _ := entity.Value()
		parts := []string{coordinator}
		for _, m := range members {
			parts = append(parts, m.EntityId)
		}
		topology := strings.Join(parts, "\x00")
		before := p.runtime.records[entity]
		revision := before.revision
		if before.topology != topology || revision == 0 {
			revision++
		}
		play := media.PlaybackState_PLAYBACK_STATE_UNSPECIFIED
		transport := transports[ci]
		switch transport.playback {
		case "STOPPED", "NO_MEDIA_PRESENT":
			play = media.PlaybackState_PLAYBACK_STATE_STOPPED
		case "PAUSED_PLAYBACK":
			play = media.PlaybackState_PLAYBACK_STATE_PAUSED
		case "PLAYING":
			play = media.PlaybackState_PLAYBACK_STATE_PLAYING
		case "TRANSITIONING":
			play = media.PlaybackState_PLAYBACK_STATE_BUFFERING
		}
		snapshot := &media.Snapshot{Route: &media.Route{EntityId: identity, Endpoint: Endpoint, CoordinatorEntityId: coordinator, TopologyRevision: revision, AdmissionEpoch: p.runtime.epoch}, Capabilities: &media.Capabilities{}, ActionAccess: state.ActionAccess_ACTION_ACCESS_DENIED, Playback: play, Title: transport.title, Artist: transport.artist, Collection: transport.collection, VolumeBasisPoints: proto.Uint32(o.volume), PositionMillis: transport.position, DurationMillis: transport.duration, Members: members, Quality: &state.Quality{Availability: state.Availability_AVAILABILITY_AVAILABLE, Freshness: state.Freshness_FRESHNESS_FRESH, Provenance: state.Provenance_PROVENANCE_OBSERVED}, ReceivedAt: timestamppb.New(now), Provenance: "sonos-local-upnp-read", DescriptorSha256: append([]byte(nil), p.runtime.digest[:]...)}
		p.runtime.records[entity] = cached{owner: p, snapshot: snapshot, captured: now, topology: topology, revision: revision}
	}
}

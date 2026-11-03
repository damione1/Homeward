package androidtv

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"net"
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

const idleTimeout = 16 * time.Second // Upstream server pings at five-second intervals.
type cached struct {
	owner     *pilot
	snapshot  *media.Snapshot
	heartbeat time.Time
}
type Runtime struct {
	mu         sync.Mutex
	records    map[domain.EntityID]cached
	epoch      string
	descriptor []byte
	digest     [32]byte
	// Injected only by package tests; production always uses pinned mTLS.
	dial func(context.Context, string, *tls.Config) (net.Conn, error)
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
	return &Runtime{records: map[domain.EntityID]cached{}, epoch: hex.EncodeToString(nonce[:]), descriptor: b, digest: sha256.Sum256(b), dial: func(ctx context.Context, address string, config *tls.Config) (net.Conn, error) {
		d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: config}
		return d.DialContext(ctx, "tcp", address)
	}}, nil
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
func (r *Runtime) Snapshot(id domain.EntityID) (*media.Snapshot, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.records[id]
	if !ok || c.snapshot == nil || time.Since(c.heartbeat) > idleTimeout || c.owner.ctx.Err() != nil {
		return nil, false
	}
	return proto.Clone(c.snapshot).(*media.Snapshot), true
}
func (r *Runtime) description(label string) *registry.CapabilityDescription {
	return &registry.CapabilityDescription{SchemaId: Schema, Major: 1, Endpoint: Endpoint, DisplayName: label, DescriptorSet: append([]byte(nil), r.descriptor...), DescriptorSha256: append([]byte(nil), r.digest[:]...), Constraints: &registry.CapabilityDescription_Extension{Extension: &registry.ExtensionConstraints{}}}
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
	instance, _ := p.settings.Instance()
	ids := make([]domain.EntityID, len(bindings))
	for i, b := range bindings {
		actual, key, e := b.Source()
		_, entity, e2 := b.IDs()
		if e != nil || e2 != nil || actual != instance || key != players[i].SourceKey() {
			return ErrUnavailable
		}
		ids[i] = entity
	}
	for i, player := range players {
		entity := ids[i]
		if e = tasks.Go(func(taskCtx context.Context) error { return p.run(taskCtx, host, player, entity) }); e != nil {
			return e
		}
	}
	return nil
}
func (p *pilot) Stop(context.Context) error {
	p.mu.Lock()
	if p.cancel != nil {
		p.cancel()
	}
	p.mu.Unlock()
	p.runtime.mu.Lock()
	defer p.runtime.mu.Unlock()
	for id, c := range p.runtime.records {
		if c.owner == p {
			delete(p.runtime.records, id)
		}
	}
	return nil
}
func (p *pilot) forget(id domain.EntityID) {
	p.runtime.mu.Lock()
	defer p.runtime.mu.Unlock()
	if c, ok := p.runtime.records[id]; ok && c.owner == p {
		delete(p.runtime.records, id)
	}
}
func (p *pilot) run(taskCtx context.Context, host integrations.SessionHost, player PlayerInput, id domain.EntityID) error {
	ctx, cancel := context.WithCancel(p.ctx)
	defer cancel()
	stop := context.AfterFunc(taskCtx, cancel)
	defer stop()
	defer p.forget(id)
	delay := 100 * time.Millisecond
	for ctx.Err() == nil {
		err := p.connect(ctx, host, player, id)
		p.forget(id)
		if ctx.Err() != nil {
			return nil
		}
		health, _ := integrations.NewHealthReport(integrations.HealthDegraded, "androidtv: authenticated observation unavailable; check pairing credential and device identity")
		if e := host.ReportHealth(ctx, health); e != nil {
			return e
		}
		// No outbound application commands exist, and every connection gets a
		// new protocol/observation. Reconnect can never replay an old key.
		if err == nil {
			delay = 100 * time.Millisecond
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		delay = min(delay*2, 30*time.Second)
	}
	return nil
}
func (p *pilot) connect(ctx context.Context, host integrations.SessionHost, player PlayerInput, id domain.EntityID) error {
	secrets, e := host.Secrets()
	if e != nil {
		return ErrCredential
	}
	instance, _ := p.settings.Instance()
	handle, e := player.CredentialHandle(instance)
	if e != nil {
		return ErrCredential
	}
	value, e := secrets.Read(ctx, handle)
	if e != nil {
		return ErrCredential
	}
	config, e := credentialTLS(value, player.MAC)
	if e != nil {
		return e
	}
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, e := p.runtime.dial(dialCtx, net.JoinHostPort(player.Address, "6466"), config)
	cancel()
	if e != nil {
		return ErrUnavailable
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	r := bufio.NewReaderSize(conn, 4096)
	protocol := newProtocol(player.EnableIME)
	for ctx.Err() == nil {
		if e = conn.SetReadDeadline(time.Now().Add(idleTimeout)); e != nil {
			return ErrUnavailable
		}
		b, e := readFrame(r)
		if e != nil {
			return ErrUnavailable
		}
		reply, changed, e := protocol.handle(b)
		if e != nil {
			return e
		}
		if len(reply) > 0 {
			if e = conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); e != nil {
				return ErrUnavailable
			}
			if e = writeFrame(conn, reply); e != nil {
				return ErrUnavailable
			}
		}
		if !protocol.started {
			continue
		}
		p.cache(id, player.Label, protocol.observation, changed)
		level, detail := integrations.HealthDegraded, "androidtv: one or more native observations unavailable"
		p.runtime.mu.Lock()
		available := 0
		for _, c := range p.runtime.records {
			if c.owner == p && time.Since(c.heartbeat) <= idleTimeout {
				available++
			}
		}
		p.runtime.mu.Unlock()
		if available == len(p.settings.players) {
			level, detail = integrations.HealthReady, "androidtv: pinned native remote observation"
		}
		health, _ := integrations.NewHealthReport(level, detail)
		if e = host.ReportHealth(ctx, health); e != nil {
			return e
		}
	}
	return nil
}
func (p *pilot) cache(id domain.EntityID, label string, o Observation, changed bool) {
	r := p.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.ctx.Err() != nil {
		return
	}
	now := time.Now()
	if before, ok := r.records[id]; ok && before.owner == p && !changed {
		before.heartbeat = now
		r.records[id] = before
		return
	}
	identity, _ := id.Value()
	snapshot := &media.Snapshot{Route: &media.Route{EntityId: identity, Endpoint: Endpoint, CoordinatorEntityId: identity, TopologyRevision: 1, AdmissionEpoch: r.epoch}, Capabilities: &media.Capabilities{}, ActionAccess: state.ActionAccess_ACTION_ACCESS_DENIED, Playback: media.PlaybackState_PLAYBACK_STATE_UNSPECIFIED, SourceId: o.Source, VolumeBasisPoints: o.VolumeBasisPoints, Members: []*media.Member{{EntityId: identity, Label: label}}, Quality: &state.Quality{Availability: state.Availability_AVAILABILITY_AVAILABLE, Freshness: state.Freshness_FRESHNESS_FRESH, Provenance: state.Provenance_PROVENANCE_OBSERVED}, ReceivedAt: timestamppb.New(now), Provenance: "androidtv-local-remote-v2-read", DescriptorSha256: append([]byte(nil), r.digest[:]...)}
	snapshot.PoweredOn = o.Power
	r.records[id] = cached{p, snapshot, now}
}

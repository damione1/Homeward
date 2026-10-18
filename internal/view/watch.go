package view

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/damione1/Homeward/contracts"
	statewire "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/sequencer"
	"google.golang.org/protobuf/proto"
)

const ReplayWindow = 120 * time.Second
const MaxReplayBytes = 32 << 20
const MaxReplayEntries = sequencer.MaxJournalCapacity
const MaxConcurrentReplays = 4
const MaxFrameBytes = 1 << 20

// Change channels must stay registered until their cancellation function is
// called. Producers send a nonblocking signal on every change; coalescing is
// safe because Watch re-reads the current authority after each wake. A closed
// channel means the authority can no longer promise notifications.
type GrantChangeNotifier interface {
	SubscribeGrantChanges(context.Context, ActorID) (<-chan struct{}, func(), error)
}
type ConfigChangeNotifier interface {
	SubscribeConfigChanges(context.Context) (<-chan struct{}, func(), error)
}

func (s *Service) signCursor(cursor *Cursor, actor ActorID, grant Grant) error {
	return s.signCursorAt(cursor, actor, grant, s.source.MonotonicNow())
}

func (s *Service) signCursorAt(cursor *Cursor, actor ActorID, grant Grant, tick time.Duration) error {
	if tick < 0 {
		return ErrInvalidInput
	}
	binary.BigEndian.PutUint64(cursor.proof[:8], uint64(tick))
	mac, err := s.cursorMAC(*cursor, actor, grant, cursor.proof[:8])
	if err != nil {
		return err
	}
	copy(cursor.proof[8:], mac)
	return nil
}

func (s *Service) cursorMAC(cursor Cursor, actor ActorID, grant Grant, tick []byte) ([]byte, error) {
	epoch, sequence, err := cursor.sequence.Components()
	if err != nil {
		return nil, err
	}
	revision, configHash, err := cursor.config.Components()
	if err != nil || !grant.validFor(actor) {
		return nil, ErrInvalidInput
	}
	actorText, _ := actor.Value()
	mac := hmac.New(sha256.New, s.proofKey[:])
	mac.Write([]byte("homeward-view-cursor-v1"))
	var number [8]byte
	write := func(data []byte) {
		binary.BigEndian.PutUint64(number[:], uint64(len(data)))
		mac.Write(number[:])
		mac.Write(data)
	}
	write([]byte(actorText))
	write([]byte(epoch))
	binary.BigEndian.PutUint64(number[:], sequence)
	mac.Write(number[:])
	binary.BigEndian.PutUint64(number[:], revision)
	mac.Write(number[:])
	binary.BigEndian.PutUint64(number[:], cursor.authorizationRevision)
	mac.Write(number[:])
	mac.Write(configHash[:])
	mac.Write(cursor.filterHash[:])
	mac.Write(grant.digest[:])
	mac.Write(tick)
	return mac.Sum(nil), nil
}

func wireCursor(cursor Cursor) (*statewire.ViewCursor, error) {
	sequence, config, authRevision, filterHash, err := cursor.Components()
	if err != nil || cursor.proof == ([40]byte{}) {
		return nil, ErrInvalidInput
	}
	epoch, seq, _ := sequence.Components()
	configRevision, configHash, _ := config.Components()
	return &statewire.ViewCursor{Epoch: epoch, GlobalSequence: seq, ConfigRevision: configRevision, AuthorizationRevision: authRevision, FilterSha256: filterHash[:], ConfigSha256: configHash[:], AdmissionProof: cursor.proof[:], GrantSha256: cursor.grantDigest[:]}, nil
}

func admitFilter(raw *statewire.ViewFilter) (Filter, error) {
	if raw == nil {
		return Filter{}, ErrInvalidInput
	}
	if len(raw.EntityIds) == 0 {
		return All(), nil
	}
	ids := make([]domain.EntityID, 0, len(raw.EntityIds))
	for _, text := range raw.EntityIds {
		id, err := domain.ParseEntityID(text)
		if err != nil {
			return Filter{}, ErrInvalidInput
		}
		ids = append(ids, id)
	}
	return Only(ids)
}

func (s *Service) admitCursor(raw *statewire.ViewCursor, actor ActorID, filter Filter, grant Grant, config ConfigStamp) (Cursor, statewire.ResyncReason) {
	if raw == nil || len(raw.FilterSha256) != 32 || len(raw.ConfigSha256) != 32 || len(raw.AdmissionProof) != 40 || len(raw.GrantSha256) != 32 {
		return Cursor{}, statewire.ResyncReason_RESYNC_REASON_INVALID_CURSOR
	}
	if !hmac.Equal(raw.FilterSha256, filter.hash[:]) {
		return Cursor{}, statewire.ResyncReason_RESYNC_REASON_FILTER_CHANGED
	}
	currentRevision, currentHash, _ := config.Components()
	if raw.ConfigRevision != currentRevision || !hmac.Equal(raw.ConfigSha256, currentHash[:]) {
		return Cursor{}, statewire.ResyncReason_RESYNC_REASON_CONFIGURATION_CHANGED
	}
	if raw.AuthorizationRevision != grant.revision {
		return Cursor{}, statewire.ResyncReason_RESYNC_REASON_AUTHORIZATION_CHANGED
	}
	if !hmac.Equal(raw.GrantSha256, grant.digest[:]) {
		return Cursor{}, statewire.ResyncReason_RESYNC_REASON_AUTHORIZATION_CHANGED
	}
	sequence, err := sequencer.ParseCursor(raw.Epoch, raw.GlobalSequence)
	if err != nil {
		return Cursor{}, statewire.ResyncReason_RESYNC_REASON_INVALID_CURSOR
	}
	current, err := s.source.Start()
	if err != nil {
		return Cursor{}, statewire.ResyncReason_RESYNC_REASON_INVALID_CURSOR
	}
	currentEpoch, _, _ := current.Components()
	if currentEpoch != raw.Epoch {
		return Cursor{}, statewire.ResyncReason_RESYNC_REASON_EPOCH_CHANGED
	}
	cursor := Cursor{sequence: sequence, config: config, authorizationRevision: grant.revision, filterHash: filter.hash, grantDigest: grant.digest}
	copy(cursor.proof[:], raw.AdmissionProof)
	mac, err := s.cursorMAC(cursor, actor, grant, raw.AdmissionProof[:8])
	if err != nil || !hmac.Equal(raw.AdmissionProof[8:], mac) {
		return Cursor{}, statewire.ResyncReason_RESYNC_REASON_INVALID_CURSOR
	}
	issued := time.Duration(binary.BigEndian.Uint64(raw.AdmissionProof[:8]))
	now := s.source.MonotonicNow()
	if now < issued || now-issued > ReplayWindow {
		return Cursor{}, statewire.ResyncReason_RESYNC_REASON_REPLAY_EXPIRED
	}
	return cursor, statewire.ResyncReason_RESYNC_REASON_UNSPECIFIED
}

// WatchView admits a wire cursor only after its actor, exact filter and current
// server authorities have been bound to a process-local HMAC. The notifier
// registrations precede the first authority read, closing the idle-revocation
// race. A stale cursor yields one terminal resync frame.
func (s *Service) WatchView(ctx context.Context, actor ActorID, request *statewire.WatchViewRequest) (*Watch, error) {
	if !s.ready() || ctx == nil || request == nil {
		return nil, ErrInvalidInput
	}
	if _, err := actor.Value(); err != nil {
		return nil, err
	}
	filter, err := admitFilter(request.Filter)
	if err != nil {
		return nil, err
	}
	authNotifier, ok := s.auth.(GrantChangeNotifier)
	if !ok || nilInterface(authNotifier) {
		return nil, ErrInvalidInput
	}
	configNotifier, ok := s.config.(ConfigChangeNotifier)
	if !ok || nilInterface(configNotifier) {
		return nil, ErrInvalidInput
	}
	grantChanges, cancelGrant, err := authNotifier.SubscribeGrantChanges(ctx, actor)
	if err != nil {
		return nil, err
	}
	if grantChanges == nil || cancelGrant == nil {
		if cancelGrant != nil {
			cancelGrant()
		}
		return nil, ErrInvalidInput
	}
	configChanges, cancelConfig, err := configNotifier.SubscribeConfigChanges(ctx)
	if err != nil {
		cancelGrant()
		return nil, err
	}
	if configChanges == nil || cancelConfig == nil {
		cancelGrant()
		if cancelConfig != nil {
			cancelConfig()
		}
		return nil, ErrInvalidInput
	}
	stop := func() { cancelGrant(); cancelConfig() }
	grant, config, err := s.authority(ctx, actor)
	if err != nil {
		stop()
		return nil, err
	}
	cursor, reason := s.admitCursor(request.Cursor, actor, filter, grant, config)
	if reason != statewire.ResyncReason_RESYNC_REASON_UNSPECIFIED {
		stop()
		return terminalWatch(reason), nil
	}
	select {
	case s.replaySlots <- struct{}{}:
	default:
		stop()
		return nil, ErrResourceExhausted
	}
	releaseReplay := func() { <-s.replaySlots }
	sub, replay, _, err := s.source.SubscribeViewAfter(cursor.sequence, MaxReplayEntries)
	if err != nil {
		releaseReplay()
		stop()
		switch {
		case errors.Is(err, sequencer.ErrEpochMismatch):
			return terminalWatch(statewire.ResyncReason_RESYNC_REASON_EPOCH_CHANGED), nil
		case errors.Is(err, sequencer.ErrCursorExpired):
			return terminalWatch(statewire.ResyncReason_RESYNC_REASON_REPLAY_EXPIRED), nil
		case errors.Is(err, sequencer.ErrRegistryUnavailable):
			return terminalWatch(statewire.ResyncReason_RESYNC_REASON_REGISTRY_UNAVAILABLE), nil
		default:
			return nil, err
		}
	}
	bytes := 0
	for _, entry := range replay {
		switch entry.Kind() {
		case sequencer.KindRegistry:
			sub.Close()
			releaseReplay()
			stop()
			return terminalWatch(statewire.ResyncReason_RESYNC_REASON_REGISTRY_CHANGED), nil
		case sequencer.KindCapabilities:
			sub.Close()
			releaseReplay()
			stop()
			return terminalWatch(statewire.ResyncReason_RESYNC_REASON_CAPABILITIES_CHANGED), nil
		case sequencer.KindState:
			_, state, _, _ := entry.State()
			wire, encodeErr := contracts.EncodeObservedState(state)
			if encodeErr != nil {
				sub.Close()
				releaseReplay()
				stop()
				return nil, encodeErr
			}
			bytes += proto.Size(wire) + 256
		case sequencer.KindEvent, sequencer.KindHealth:
			bytes += 256
		case sequencer.KindUnspecified:
			sub.Close()
			releaseReplay()
			stop()
			return nil, ErrInvalidInput
		default:
			sub.Close()
			releaseReplay()
			stop()
			return nil, ErrInvalidInput
		}
		if bytes > MaxReplayBytes {
			sub.Close()
			releaseReplay()
			stop()
			return terminalWatch(statewire.ResyncReason_RESYNC_REASON_REPLAY_EXPIRED), nil
		}
	}
	w := &Watch{done: make(chan struct{}), service: s, actor: actor, filter: filter, grantRevision: grant.revision, grantDigest: grant.digest, config: config, cursor: cursor, sub: sub, replay: replay, grantChanges: grantChanges, configChanges: configChanges, cancelGrant: cancelGrant, cancelConfig: cancelConfig, releaseReplay: releaseReplay}
	if len(replay) == 0 {
		w.replayOnce.Do(w.releaseReplay)
	}
	return w, nil
}

type Watch struct {
	nextMu         sync.Mutex
	mu             sync.Mutex
	replayMu       sync.Mutex
	done           chan struct{}
	service        *Service
	actor          ActorID
	filter         Filter
	grantRevision  uint64
	grantDigest    [32]byte
	config         ConfigStamp
	cursor         Cursor
	sub            *sequencer.Subscription
	replay         []sequencer.Entry
	position       int
	streamSequence uint64
	grantChanges   <-chan struct{}
	configChanges  <-chan struct{}
	cancelGrant    func()
	cancelConfig   func()
	releaseReplay  func()
	replayOnce     sync.Once
	pending        *statewire.WatchViewResponse
	closed         bool
}

func terminalWatch(reason statewire.ResyncReason) *Watch {
	return &Watch{done: make(chan struct{}), pending: &statewire.WatchViewResponse{StreamSequence: 1, Frame: &statewire.WatchViewResponse_ResyncRequired{ResyncRequired: &statewire.ResyncRequired{Reason: reason}}}}
}

func (w *Watch) isClosed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

func (w *Watch) Close() error {
	if w == nil {
		return ErrInvalidInput
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	if w.done != nil {
		close(w.done)
	}
	sub, cancelGrant, cancelConfig := w.sub, w.cancelGrant, w.cancelConfig
	w.mu.Unlock()
	w.replayMu.Lock()
	w.replay = nil
	w.position = 0
	w.replayMu.Unlock()
	if sub != nil {
		_ = sub.Close()
	}
	if w.releaseReplay != nil {
		w.replayOnce.Do(w.releaseReplay)
	}
	if cancelGrant != nil {
		cancelGrant()
	}
	if cancelConfig != nil {
		cancelConfig()
	}
	return nil
}

func (w *Watch) takeReplay() (sequencer.Entry, bool) {
	w.replayMu.Lock()
	defer w.replayMu.Unlock()
	if w.position >= len(w.replay) {
		w.replay = nil
		w.position = 0
		if w.releaseReplay != nil {
			w.replayOnce.Do(w.releaseReplay)
		}
		return sequencer.Entry{}, false
	}
	entry := w.replay[w.position]
	w.position++
	if w.position == len(w.replay) {
		w.replay = nil
		w.position = 0
		if w.releaseReplay != nil {
			w.replayOnce.Do(w.releaseReplay)
		}
	}
	return entry, true
}

func (w *Watch) mailboxReason() (statewire.ResyncReason, error) {
	status, err := w.sub.Status()
	if err != nil {
		return 0, err
	}
	if status.Closed && w.isClosed() {
		return 0, io.EOF
	}
	if status.PendingLoss == nil {
		return statewire.ResyncReason_RESYNC_REASON_UNSPECIFIED, nil
	}
	switch status.PendingLoss.Reason {
	case sequencer.LossViewOverflow:
		return statewire.ResyncReason_RESYNC_REASON_CLIENT_SLOW, nil
	case sequencer.LossRegistryUnavailable:
		return statewire.ResyncReason_RESYNC_REASON_REGISTRY_UNAVAILABLE, nil
	default:
		return 0, ErrInvalidInput
	}
}

func (w *Watch) resync(reason statewire.ResyncReason) (*statewire.WatchViewResponse, error) {
	w.streamSequence++
	frame := &statewire.WatchViewResponse{StreamSequence: w.streamSequence, Frame: &statewire.WatchViewResponse_ResyncRequired{ResyncRequired: &statewire.ResyncRequired{Reason: reason}}}
	_ = w.Close()
	return frame, nil
}

func (w *Watch) authority(ctx context.Context) (Grant, statewire.ResyncReason, error) {
	grant, config, err := w.service.authority(ctx, w.actor)
	if err != nil {
		return Grant{}, 0, err
	}
	if grant.revision != w.grantRevision || grant.digest != w.grantDigest {
		return Grant{}, statewire.ResyncReason_RESYNC_REASON_AUTHORIZATION_CHANGED, nil
	}
	if config != w.config {
		return Grant{}, statewire.ResyncReason_RESYNC_REASON_CONFIGURATION_CHANGED, nil
	}
	return grant, statewire.ResyncReason_RESYNC_REASON_UNSPECIFIED, nil
}

// Next emits contiguous stream positions only for visible state changes. Its
// cursor watermark advances across filtered journal entries. Metadata changes
// terminate the old generation before a stale view can receive another delta.
func (w *Watch) Next(ctx context.Context) (*statewire.WatchViewResponse, error) {
	if w == nil || ctx == nil {
		return nil, ErrInvalidInput
	}
	w.nextMu.Lock()
	defer w.nextMu.Unlock()
	if w.pending != nil {
		frame := w.pending
		w.pending = nil
		_ = w.Close()
		return frame, nil
	}
	if w.isClosed() {
		return nil, io.EOF
	}
	if w.service == nil {
		return nil, ErrInvalidInput
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if w.isClosed() {
			return nil, io.EOF
		}
		grant, reason, err := w.authority(ctx)
		if err != nil {
			_ = w.Close()
			return nil, err
		}
		if reason != statewire.ResyncReason_RESYNC_REASON_UNSPECIFIED {
			return w.resync(reason)
		}
		mailboxReason, err := w.mailboxReason()
		if err != nil {
			return nil, err
		}
		if mailboxReason != statewire.ResyncReason_RESYNC_REASON_UNSPECIFIED {
			return w.resync(mailboxReason)
		}
		entry, replayed := w.takeReplay()
		if !replayed {
			delivery, wake, pollErr := w.sub.Poll()
			switch {
			case errors.Is(pollErr, sequencer.ErrViewOverflow):
				return w.resync(statewire.ResyncReason_RESYNC_REASON_CLIENT_SLOW)
			case errors.Is(pollErr, sequencer.ErrProjectionInvalidated):
				return w.resync(statewire.ResyncReason_RESYNC_REASON_REGISTRY_UNAVAILABLE)
			case pollErr != nil:
				if errors.Is(pollErr, sequencer.ErrSubscriptionClosed) && w.isClosed() {
					return nil, io.EOF
				}
				return nil, pollErr
			}
			if wake != nil {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-w.done:
					return nil, io.EOF
				case _, ok := <-w.grantChanges:
					if !ok {
						return w.resync(statewire.ResyncReason_RESYNC_REASON_AUTHORIZATION_CHANGED)
					}
				case _, ok := <-w.configChanges:
					if !ok {
						return w.resync(statewire.ResyncReason_RESYNC_REASON_CONFIGURATION_CHANGED)
					}
				case <-wake:
				}
				continue
			}
			entry, _ = delivery.Entry()
		}
		tick := w.service.source.MonotonicNow()
		if tick < entry.AdmissionTick() || tick-entry.AdmissionTick() > ReplayWindow {
			return w.resync(statewire.ResyncReason_RESYNC_REASON_REPLAY_EXPIRED)
		}
		// The authority may have changed while Poll removed a queued entry.
		grant, reason, err = w.authority(ctx)
		if err != nil {
			_ = w.Close()
			return nil, err
		}
		if reason != statewire.ResyncReason_RESYNC_REASON_UNSPECIFIED {
			return w.resync(reason)
		}
		mailboxReason, err = w.mailboxReason()
		if err != nil {
			return nil, err
		}
		if mailboxReason != statewire.ResyncReason_RESYNC_REASON_UNSPECIFIED {
			return w.resync(mailboxReason)
		}
		switch entry.Kind() {
		case sequencer.KindRegistry:
			return w.resync(statewire.ResyncReason_RESYNC_REASON_REGISTRY_CHANGED)
		case sequencer.KindCapabilities:
			return w.resync(statewire.ResyncReason_RESYNC_REASON_CAPABILITIES_CHANGED)
		case sequencer.KindState:
			id, state, _, _ := entry.State()
			w.cursor.sequence = entry.Cursor()
			if !w.filter.permits(id) {
				continue
			}
			if _, allowed := grant.allowed[id]; !allowed {
				continue
			}
			wireState, err := contracts.EncodeObservedState(state)
			if err != nil {
				return nil, err
			}
			if err := w.service.signCursor(&w.cursor, w.actor, grant); err != nil {
				return nil, err
			}
			wire, err := wireCursor(w.cursor)
			if err != nil {
				return nil, err
			}
			frame := &statewire.WatchViewResponse{StreamSequence: w.streamSequence + 1, Cursor: wire, Frame: &statewire.WatchViewResponse_Delta{Delta: &statewire.StateDelta{States: []*statewire.ObservedState{wireState}}}}
			if proto.Size(frame) > MaxFrameBytes {
				return w.resync(statewire.ResyncReason_RESYNC_REASON_FRAME_TOO_LARGE)
			}
			tick = w.service.source.MonotonicNow()
			if tick < entry.AdmissionTick() || tick-entry.AdmissionTick() > ReplayWindow {
				return w.resync(statewire.ResyncReason_RESYNC_REASON_REPLAY_EXPIRED)
			}
			w.streamSequence++
			if w.isClosed() {
				return nil, io.EOF
			}
			return frame, nil
		case sequencer.KindEvent, sequencer.KindHealth:
			w.cursor.sequence = entry.Cursor()
			continue
		case sequencer.KindUnspecified:
			return nil, ErrInvalidInput
		default:
			return nil, ErrInvalidInput
		}
	}
}

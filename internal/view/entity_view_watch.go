package view

import (
	"context"
	"errors"
	"google.golang.org/protobuf/proto"
	"io"
	"sync"

	statewire "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/apitypes"
)

// EntityViewChange projects one exact native journal entry against the frozen
// authorized declarations of its capture. A nil Item removes membership.
type EntityViewChange struct {
	Entity apitypes.EntityName
	Item   *Item
	Cursor Cursor
	Resync statewire.ResyncReason
}

type EntityWatch struct {
	service *Service
	actor   ActorID
	capture *capture
	native  *Watch
	parent  context.Context
	ctx     context.Context
	cancel  context.CancelFunc
	stop    func() bool
	once    sync.Once
	next    sync.Mutex
}

// WatchEntityView resumes only a server-signed native cursor for this capture.
// The public transport wraps that cursor and never grants authority through it.
func (s *Service) WatchEntityView(ctx context.Context, actor ActorID, name apitypes.EntityViewName, cursor Cursor) (*EntityWatch, error) {
	token, c, err := s.entityViewCapture(ctx, actor, name)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if c.retired || s.captures[token] != c {
		s.mu.Unlock()
		return nil, ErrSnapshotUnknown
	}
	if c.watchers >= MaxConcurrentReplays {
		s.mu.Unlock()
		return nil, ErrResourceExhausted
	}
	c.watchers++
	s.mu.Unlock()
	watchCtx, cancel := context.WithTimeout(ctx, c.expires.Sub(s.clock.Now()))
	w := &EntityWatch{service: s, actor: actor, capture: c, parent: ctx, ctx: watchCtx, cancel: cancel}
	w.stop = context.AfterFunc(c.lifetime, cancel)
	raw, err := wireCursor(cursor)
	if err != nil {
		_ = w.Close()
		return nil, err
	}
	filter := All()
	filter.hash = c.cursor.filterHash
	w.native, err = s.watchView(watchCtx, actor, filter, raw)
	if err != nil {
		_ = w.Close()
		return nil, err
	}
	// Independent registrations close idle streams on authority/config changes;
	// they do not consume the native watch's own wakeups.
	grantChanges, stopGrant, err := s.auth.(GrantChangeNotifier).SubscribeGrantChanges(watchCtx, actor)
	if err != nil {
		_ = w.Close()
		return nil, err
	}
	configChanges, stopConfig, err := s.config.(ConfigChangeNotifier).SubscribeConfigChanges(watchCtx)
	if err != nil {
		if stopGrant != nil {
			stopGrant()
		}
		_ = w.Close()
		return nil, err
	}
	if grantChanges == nil || configChanges == nil || stopGrant == nil || stopConfig == nil {
		if stopGrant != nil {
			stopGrant()
		}
		if stopConfig != nil {
			stopConfig()
		}
		_ = w.Close()
		return nil, ErrInvalidInput
	}
	if _, _, err := s.entityViewCapture(watchCtx, actor, name); err != nil {
		stopGrant()
		stopConfig()
		_ = w.Close()
		return nil, err
	}
	go func() {
		defer stopGrant()
		defer stopConfig()
		select {
		case <-watchCtx.Done():
		case <-grantChanges:
		case <-configChanges:
		}
		_ = w.Close()
	}()
	// Cancellation must also discard retained replay and mailbox buffers while
	// the consumer is idle. Close is idempotent and releases the capture charge.
	context.AfterFunc(watchCtx, func() { _ = w.Close() })
	return w, nil
}
func (w *EntityWatch) Close() error {
	if w == nil {
		return nil
	}
	w.once.Do(func() {
		w.cancel()
		if w.stop != nil {
			w.stop()
		}
		if w.native != nil {
			_ = w.native.Close()
		}
		// A projection already in progress still owns the immutable seed. Keep
		// its storage charged until Next has dropped every local reference.
		go func() {
			w.next.Lock()
			defer w.next.Unlock()
			w.service.mu.Lock()
			defer w.service.mu.Unlock()
			retained := w.capture
			w.capture = nil
			retained.watchers--
			if retained.retired && retained.watchers == 0 {
				w.service.used -= retained.bytes
			}
		}()
	})
	return nil
}
func (w *EntityWatch) Context() context.Context {
	if w == nil {
		return nil
	}
	return w.ctx
}
func (w *EntityWatch) Next() (*EntityViewChange, error) {
	if w == nil {
		return nil, ErrInvalidInput
	}
	w.next.Lock()
	defer w.next.Unlock()
	for {
		if err := w.ctx.Err(); err != nil {
			if w.parent != nil && w.parent.Err() == nil {
				return nil, ErrViewChanged
			}
			return nil, err
		}
		if _, _, err := w.service.entityViewCapture(w.ctx, w.actor, w.capture.name); err != nil {
			_ = w.Close()
			if w.parent != nil && w.parent.Err() == nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				return nil, ErrViewChanged
			}
			return nil, err
		}
		event, err := w.native.nextEvent(w.ctx)
		if err != nil {
			_ = w.Close()
			if w.parent != nil && w.parent.Err() == nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				return nil, ErrViewChanged
			}
			return nil, err
		}
		if event == nil {
			_ = w.Close()
			if w.parent != nil && w.parent.Err() == nil {
				return nil, ErrViewChanged
			}
			return nil, io.EOF
		}
		if resync := event.frame.GetResyncRequired(); resync != nil {
			_ = w.Close()
			return &EntityViewChange{Resync: resync.Reason}, nil
		}
		seed, ok := w.capture.catalogue[event.entity]
		if !ok {
			continue
		}
		// nextEvent checked the complete native state grant. An absent initial
		// reading does not suppress the first qualified observation. The frozen
		// capability slice still bounds which slots can be projected.
		seed.state, seed.hasState = event.state, true
		cursor := w.native.cursor
		// Native replay authenticates its observation watermark. Definition
		// projection additionally needs the frozen catalogue position, checked
		// by entityViewCapture on both sides of materialization.
		cursor.catalogue = w.capture.cursor.catalogue
		projected, err := w.service.projector(w.ctx, w.actor, seed, cursor)
		if err != nil {
			_ = w.Close()
			return nil, err
		}
		retained, err := validateProjection(event.entity, seed.capabilities, projected)
		if err != nil {
			_ = w.Close()
			return nil, err
		}
		if retained > MaxFrameBytes {
			_ = w.Close()
			return nil, ErrResourceExhausted
		}
		seed.projected = projected
		seed.hasProjection = true
		matches, err := w.capture.selection.matches(seed)
		if err != nil {
			_ = w.Close()
			return nil, err
		}
		if _, _, err := w.service.entityViewCapture(w.ctx, w.actor, w.capture.name); err != nil {
			_ = w.Close()
			return nil, err
		}
		name, err := apitypes.EntityNameForIdentity(w.capture.selection.home, event.entity)
		if err != nil {
			_ = w.Close()
			return nil, err
		}
		change := &EntityViewChange{Entity: name, Cursor: cursor}
		if matches {
			change.Item = &seed
		}
		return change, nil
	}
}

// EntityViewResume encodes a native signed checkpoint for opaque transport
// wrapping. The returned bytes are not a public resource or authority token.
func EntityViewResume(cursor Cursor) ([]byte, error) {
	raw, err := wireCursor(cursor)
	if err != nil {
		return nil, err
	}
	return proto.Marshal(raw)
}

// WatchEntityViewResume checks the current capture before decoding checkpoint
// bytes, then verifies its native proof and all original authority stamps.
func (s *Service) WatchEntityViewResume(ctx context.Context, actor ActorID, name apitypes.EntityViewName, resume []byte) (*EntityWatch, error) {
	_, c, err := s.entityViewCapture(ctx, actor, name)
	if err != nil {
		return nil, err
	}
	if len(resume) == 0 || len(resume) > 2048 {
		return nil, ErrPageToken
	}
	raw := &statewire.ViewCursor{}
	if err := proto.Unmarshal(resume, raw); err != nil || len(raw.ProtoReflect().GetUnknown()) != 0 {
		return nil, ErrPageToken
	}
	grant, config, err := s.authority(ctx, actor)
	if err != nil {
		return nil, err
	}
	filter := All()
	filter.hash = c.cursor.filterHash
	cursor, reason := s.admitCursor(raw, actor, filter, grant, config)
	if reason != statewire.ResyncReason_RESYNC_REASON_UNSPECIFIED {
		return nil, ErrViewChanged
	}
	return s.WatchEntityView(ctx, actor, name, cursor)
}

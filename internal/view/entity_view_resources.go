package view

import (
	"context"
	"sort"
	"time"

	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/entityfilter"
)

// retireCaptureLocked removes the resource and cancels every attached watch.
// A watch that is draining still retains its capture's charged storage; the
// final watcher releases that charge. Callers hold s.mu.
func (s *Service) retireCaptureLocked(token string, c *capture) {
	if c.retired {
		return
	}
	c.retired = true
	delete(s.captures, token)
	if c.cancel != nil {
		c.cancel()
	}
	if c.watchers == 0 {
		s.used -= c.bytes
	}
}

// EntityViewDescriptor is metadata for the existing native capture. Its token
// and signed cursor stay private to transport encoding and confer no authority.
type EntityViewDescriptor struct {
	Name                   apitypes.EntityViewName
	CreateTime, ExpireTime time.Time
	Filter                 entityfilter.Filter
	SnapshotToken          string
	Cursor                 Cursor
}

func entityViewDescriptor(token string, c *capture) EntityViewDescriptor {
	return EntityViewDescriptor{Name: c.name, CreateTime: c.created, ExpireTime: c.expires, Filter: c.selection.filter, SnapshotToken: token, Cursor: c.cursor}
}

func (s *Service) CreateEntityView(ctx context.Context, actor ActorID, home apitypes.HomeName, filter entityfilter.Filter, requested *apitypes.EntityViewName) (EntityViewDescriptor, error) {
	if s == nil || s.projector == nil || !home.Valid() || !filter.Valid() {
		return EntityViewDescriptor{}, ErrInvalidInput
	}
	if requested != nil && (!requested.Valid() || requested.Home() != home) {
		return EntityViewDescriptor{}, ErrInvalidInput
	}
	digest, err := filter.Digest()
	if err != nil {
		return EntityViewDescriptor{}, err
	}
	selection := entitySelection{home: home, filter: filter, digest: digest, exposed: true, requested: requested}
	page, err := s.openView(ctx, actor, All(), MaxPageSize, &selection)
	if err != nil {
		return EntityViewDescriptor{}, err
	}
	s.mu.Lock()
	c, ok := s.captures[page.SnapshotToken]
	if !ok {
		s.mu.Unlock()
		return EntityViewDescriptor{}, ErrSnapshotUnknown
	}
	descriptor := entityViewDescriptor(page.SnapshotToken, c)
	s.mu.Unlock()
	if _, err := s.GetEntityView(ctx, actor, descriptor.Name); err != nil {
		_ = s.ReleaseView(context.WithoutCancel(ctx), actor, page.SnapshotToken)
		return EntityViewDescriptor{}, err
	}
	return descriptor, nil
}

func (s *Service) entityViewCapture(ctx context.Context, actor ActorID, name apitypes.EntityViewName) (string, *capture, error) {
	if !s.ready() || ctx == nil || !name.Valid() {
		return "", nil, ErrInvalidInput
	}
	// Recheck authority before inspecting any capture identity or diagnostic.
	if _, _, err := s.authority(ctx, actor); err != nil {
		return "", nil, err
	}
	s.mu.Lock()
	s.expire(s.clock.Now())
	var token string
	var c *capture
	for candidate, value := range s.captures {
		if value.name == name && value.actor == actor {
			token, c = candidate, value
			break
		}
	}
	s.mu.Unlock()
	if c == nil {
		return "", nil, ErrSnapshotUnknown
	}
	if _, err := s.ReadFirstPage(ctx, actor, token, 1); err != nil {
		return "", nil, err
	}
	catalogue, err := s.source.CataloguePosition()
	if err != nil {
		return "", nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.retired || s.captures[token] != c {
		return "", nil, ErrSnapshotUnknown
	}
	if catalogue != c.cursor.catalogue {
		s.retireCaptureLocked(token, c)
		return "", nil, ErrViewChanged
	}
	return token, c, nil
}
func (s *Service) GetEntityView(ctx context.Context, actor ActorID, name apitypes.EntityViewName) (EntityViewDescriptor, error) {
	token, c, err := s.entityViewCapture(ctx, actor, name)
	if err != nil {
		return EntityViewDescriptor{}, err
	}
	return entityViewDescriptor(token, c), nil
}
func (s *Service) ReadEntityView(ctx context.Context, actor ActorID, name apitypes.EntityViewName, pageToken string, size int) (Page, error) {
	token, _, err := s.entityViewCapture(ctx, actor, name)
	if err != nil {
		return Page{}, err
	}
	page, err := s.readPage(ctx, actor, token, pageToken, size)
	if err != nil {
		return Page{}, err
	}
	if _, _, err := s.entityViewCapture(ctx, actor, name); err != nil {
		return Page{}, err
	}
	return page, nil
}
func (s *Service) ListEntityViews(ctx context.Context, actor ActorID, home apitypes.HomeName) ([]EntityViewDescriptor, error) {
	if !s.ready() || ctx == nil || !home.Valid() {
		return nil, ErrInvalidInput
	}
	if _, _, err := s.authority(ctx, actor); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.expire(s.clock.Now())
	names := make([]apitypes.EntityViewName, 0, len(s.captures))
	for _, c := range s.captures {
		if c.actor == actor && c.name.Valid() && c.name.Home() == home {
			names = append(names, c.name)
		}
	}
	s.mu.Unlock()
	sort.Slice(names, func(i, j int) bool { return names[i].String() < names[j].String() })
	result := make([]EntityViewDescriptor, 0, len(names))
	for _, name := range names {
		descriptor, err := s.GetEntityView(ctx, actor, name)
		if err == ErrSnapshotUnknown || err == ErrSnapshotExpired || err == ErrViewChanged {
			continue
		}
		if err != nil {
			return nil, err
		}
		result = append(result, descriptor)
	}
	return result, nil
}
func (s *Service) DeleteEntityView(ctx context.Context, actor ActorID, name apitypes.EntityViewName) error {
	if !s.ready() || ctx == nil || !name.Valid() {
		return ErrInvalidInput
	}
	if _, _, err := s.authority(ctx, actor); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, c := range s.captures {
		if c.actor == actor && c.name == name {
			s.retireCaptureLocked(token, c)
			break
		}
	}
	return nil
}

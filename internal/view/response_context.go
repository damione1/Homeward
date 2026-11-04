package view

import "context"

// ResponseContext retains no new catalogue or authority. It binds a unary
// response to the existing capture incarnation, deletion and bounded expiry.
// The caller must keep its original capture ownership until response cleanup.
func (s *Service) ResponseContext(ctx context.Context, actor ActorID, token string) (context.Context, context.CancelFunc, error) {
	if _, err := s.ReadFirstPage(ctx, actor, token, 1); err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	c := s.captures[token]
	if c == nil || c.retired || c.actor != actor {
		s.mu.Unlock()
		return nil, nil, ErrSnapshotUnknown
	}
	lifetime, cancel := context.WithTimeout(ctx, c.expires.Sub(s.clock.Now()))
	stop := context.AfterFunc(c.lifetime, cancel)
	s.mu.Unlock()
	return lifetime, func() { stop(); cancel() }, nil
}

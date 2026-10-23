package sequencer

// CataloguePosition is a no-copy fence for immutable registry/capability
// projections. It advances at their existing sequenced admissions, never on
// state/health traffic. Epoch plus the nonwrapping admission sequence prevents
// ABA; nextTime refuses publications at sequence exhaustion. Unavailable
// registry or a configuration publication fails closed just like CaptureBounded.
// It grants no access and exposes no additional registry or command authority.
func (s *Sequencer) CataloguePosition() (Cursor, error) {
	if s == nil || s.journal == nil {
		return Cursor{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.registryState == registryUnavailable || s.configurationPublication != nil {
		return Cursor{}, ErrRegistryUnavailable
	}
	return Cursor{epoch: s.epoch, seq: s.catalogueSequence}, nil
}

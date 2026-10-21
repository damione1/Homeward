package scenes

import "github.com/damione1/Homeward/internal/domain"

type catalogueReservationPhase uint8

const (
	catalogueReservationInvalid catalogueReservationPhase = iota
	catalogueReservationPrepared
	catalogueReservationPublished
	catalogueReservationReleased
)

// CatalogueReservation replaces definitions in the same service. Existing
// preparation slots keep their digests and fail admission if B changed them;
// temporary scenes are governed by their existing config-revision check.
type CatalogueReservation struct {
	service   *Service
	catalogue Catalogue
	phase     catalogueReservationPhase
	changed   []domain.HAAlias
}

func (s *Service) ReserveCatalogue(next Catalogue) (*CatalogueReservation, error) {
	if s == nil || len(next.definitions) != 12 || len(next.order) != 12 {
		return nil, ErrInvalidScene
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.pendingCatalogue != nil {
		return nil, ErrSceneUnavailable
	}
	r := &CatalogueReservation{service: s, catalogue: next, phase: catalogueReservationPrepared}
	for id, old := range s.catalogue.definitions {
		candidate, present := next.definitions[id]
		if !present || !candidate.SameBehavior(old) {
			r.changed = append(r.changed, id)
		}
	}
	for id := range next.definitions {
		if _, present := s.catalogue.definitions[id]; !present {
			r.changed = append(r.changed, id)
		}
	}
	s.pendingCatalogue = r
	return r, nil
}

func (r *CatalogueReservation) ChangedAliases() []domain.HAAlias {
	if r == nil {
		return nil
	}
	return append([]domain.HAAlias(nil), r.changed...)
}
func (r *CatalogueReservation) Release() error {
	if r == nil || r.service == nil {
		return ErrInvalidScene
	}
	s := r.service
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.phase != catalogueReservationPrepared || s.pendingCatalogue != r {
		return ErrInvalidScene
	}
	r.phase, s.pendingCatalogue = catalogueReservationReleased, nil
	return nil
}
func (r *CatalogueReservation) Publish() error {
	if r == nil || r.service == nil {
		return ErrInvalidScene
	}
	s := r.service
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.phase != catalogueReservationPrepared || s.pendingCatalogue != r || s.closed {
		return ErrInvalidScene
	}
	s.catalogue = r.catalogue
	r.phase, s.pendingCatalogue = catalogueReservationPublished, nil
	return nil
}

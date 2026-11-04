package configactivation

import (
	"context"

	"github.com/damione1/Homeward/internal/storage"
)

// PublicationEvidence is constructed only from this native Owner's exact
// coherent current generation and an existing immutable SQL attempt. A zero
// value is invalid. It neither activates nor reconciles a generation.
type PublicationEvidence struct {
	attempt  storage.ActivationAttemptID
	target   storage.ConfigGenerationRevision
	manifest [32]byte
	valid    bool
}

func (v PublicationEvidence) Valid() bool {
	return v.valid && v.attempt.Valid() && v.target.Valid() && v.manifest != ([32]byte{})
}
func (v PublicationEvidence) Attempt() storage.ActivationAttemptID     { return v.attempt }
func (v PublicationEvidence) Target() storage.ConfigGenerationRevision { return v.target }
func (v PublicationEvidence) ManifestSHA256() [32]byte                 { return v.manifest }

// PublishedEvidence binds this native result to its own durable attempt. The
// owner can publish a newer winner during reconciliation, so Published alone
// is insufficient: the caller's COMMIT, attempt and exact target must match.
func (r Result) PublishedEvidence(attempt storage.ConfigActivationAttempt) (PublicationEvidence, bool) {
	if r.outcome != Published || r.transaction != storage.TransactionCommitted || !r.attempt.Valid() ||
		r.attempt != attempt.ID() || !r.revision.Valid() || r.revision != attempt.Target() ||
		attempt.ManifestSHA256() == ([32]byte{}) || attempt.CommittedAt().IsZero() {
		return PublicationEvidence{}, false
	}
	return PublicationEvidence{attempt: attempt.ID(), target: attempt.Target(), manifest: attempt.ManifestSHA256(), valid: true}, true
}

// ObservePublishedAttempt does not infer publication from a higher revision.
// While publication is cut, no new success proof is emitted. A previously
// persisted terminal result stays terminal independently of this live view.
func (o *Owner) ObservePublishedAttempt(ctx context.Context, attempt storage.ConfigActivationAttempt) (PublicationEvidence, bool, error) {
	if o == nil || absentPort(ctx) || !attempt.ID().Valid() || !attempt.Target().Valid() || attempt.ManifestSHA256() == ([32]byte{}) || attempt.CommittedAt().IsZero() {
		return PublicationEvidence{}, false, ErrInvalidStartup
	}
	if err := ctx.Err(); err != nil {
		return PublicationEvidence{}, false, err
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	if !o.attached {
		return PublicationEvidence{}, false, ErrActivationUnavailable
	}
	switch o.phase {
	case ownerCut:
		return PublicationEvidence{}, false, nil
	case ownerClosed:
		return PublicationEvidence{}, false, ErrActivationUnavailable
	case ownerOpen:
		if o.current.Revision() != attempt.Target() || o.current.ManifestSHA256() != attempt.ManifestSHA256() {
			return PublicationEvidence{}, false, nil
		}
		return PublicationEvidence{attempt: attempt.ID(), target: attempt.Target(), manifest: attempt.ManifestSHA256(), valid: true}, true, nil
	case ownerInvalid:
		return PublicationEvidence{}, false, ErrInvalidStartup
	default:
		return PublicationEvidence{}, false, ErrInvalidStartup
	}
}

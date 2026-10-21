package calendars

import (
	"context"
	"sync"
)

// remoteImage is an operation-owned candidate, never a resident pilot cache.
// Event keys carry the caller's admitted CalendarID. Durable revisions are
// assigned by the writer after CAS; no synthetic revision is invented here.
type remoteImage struct {
	binding      CalendarID
	events       []EventSnapshot
	zones        map[string]Zone
	ctx          context.Context
	lease        *imageLease
	parse        *admissionSession
	once         sync.Once
	sqlPreflight bool
	observe      func(remoteDecodeObservation) // private numeric evidence, captured before close
}

func newRemoteImage(ctx context.Context, id CalendarID) (*remoteImage, error) {
	image, _, err := newRemoteImageObserved(ctx, id)
	return image, err
}

func newRemoteImageObserved(ctx context.Context, id CalendarID) (*remoteImage, remoteDecodeObservation, error) {
	if err := contextValid(ctx); err != nil {
		return nil, remoteDecodeObservation{}, err
	}
	if !id.Valid() || id.origin != RemoteICS {
		return nil, remoteDecodeObservation{}, failure(InvalidInput)
	}
	owner, err := beginAdmission()
	if err != nil {
		return nil, remoteDecodeObservation{}, err
	}
	lease, err := images.reserve(imageFixedCharge)
	if err != nil {
		observation := remoteDecodeObservation{Admission: owner.observation, Image: imageCreditObservation{RequestedBytes: uint64(imageFixedCharge)}}
		owner.close()
		return nil, observation, err
	}
	owner.image = lease
	ctx = context.WithValue(ctx, admissionKey{}, owner)
	ctx = withCalendarBudget(ctx, newBudget(ctx))
	return &remoteImage{binding: id, ctx: ctx, lease: lease, parse: owner}, remoteDecodeObservation{Admission: owner.observation, Image: lease.creditObservation()}, nil
}

func (i *remoteImage) owned() bool {
	return i != nil && i.binding.Valid() && i.binding.origin == RemoteICS && i.parse != nil && i.parse.state == leaseOwned && i.lease != nil
}
func (i *remoteImage) close() {
	if i != nil {
		i.once.Do(func() {
			if i.observe != nil {
				i.observe(remoteDecodeObservation{Admission: i.parse.observation, Image: i.lease.creditObservation()})
			}
			i.lease.release()
			i.parse.close()
		})
	}
}

// grow extends the existing slot rather than admitting a second image. The
// counters remain under the same ledger mutex as reserve/release.
func (l *imageLease) grow(extra int64) error {
	if l == nil || l.owner == nil || extra < 0 {
		return failure(InvalidInput)
	}
	l.owner.mu.Lock()
	defer l.owner.mu.Unlock()
	if l.charge <= 0 {
		return failure(InvalidInput)
	}
	if l.prepaid > 0 {
		if extra <= l.prepaid {
			l.prepaid -= extra
			return nil
		}
		extra -= l.prepaid
		l.prepaid = 0
	}
	l.requested = uint64(l.charge) + uint64(extra)
	if extra > MaxImageBytes-l.charge {
		return failure(CapacityExceeded)
	}
	if extra > MaxAggregateImageBytes-l.owner.used {
		return failure(Busy)
	}
	l.charge += extra
	l.owner.used += extra
	return nil
}

// creditObservation is private numeric evidence. Read both counters under the
// ledger mutex, including a refused request, before releasing this lease.
type imageCreditObservation struct {
	ReservedBytes  uint64
	RequestedBytes uint64
}

func (l *imageLease) creditObservation() imageCreditObservation {
	if l == nil || l.owner == nil {
		return imageCreditObservation{}
	}
	l.owner.mu.Lock()
	defer l.owner.mu.Unlock()
	return imageCreditObservation{ReservedBytes: uint64(l.charge), RequestedBytes: l.requested}
}

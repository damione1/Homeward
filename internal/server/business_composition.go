package server

import (
	"context"
	"time"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/mediacontent"
	"github.com/damione1/Homeward/internal/mediasessions"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/workday"
)

// MediaResourceSource declares an actual configured native byte owner. An
// empty list is a real empty catalogue; it never starts a mock or device.
type MediaResourceSource struct{ source mediaSourceRegistration }

func NewMediaResourceSource(entity domain.EntityID, slot authorization.CapabilitySlot, owner mediacontent.Owner) (MediaResourceSource, error) {
	if _, err := entity.Value(); err != nil {
		return MediaResourceSource{}, ErrInvalidPublicPlan
	}
	if _, _, err := slot.Values(); err != nil || nilReactiveInterface(owner) {
		return MediaResourceSource{}, ErrInvalidPublicPlan
	}
	return MediaResourceSource{mediaSourceRegistration{entity, slot, owner}}, nil
}

type businessResourceClock struct{}

func (businessResourceClock) Now() time.Time { return time.Now() }

// businessResourceRuntime borrows the existing remote calendar service when
// configured. Otherwise it owns the sole SQL calendar service for this Run.
// Only MediaSession lease metadata belongs here; native pixels/streams stay
// with the explicitly supplied MediaResourceSource owners.
type businessResourceRuntime struct {
	calendars     *calendarResourceHandler
	media         *mediaResourceHandler
	ownedCalendar *calendars.Service
	sessions      *mediasessions.Service
}

func newBusinessResourceRuntime(ctx context.Context, lists *listComposition, authority *authorization.Authority, remote *remoteCalendarRuntime, workday workday.CalendarReader, sources []MediaResourceSource) (*businessResourceRuntime, error) {
	if ctx == nil || lists == nil || authority == nil || len(sources) > mediacontent.MaxSources {
		return nil, ErrInvalidPublicPlan
	}
	runtime := new(businessResourceRuntime)
	var calendarService *calendars.Service
	if remote != nil {
		calendarService = remote.service
	} else {
		repository, err := calendars.NewRepository(lists.store, lists.manifest)
		if err != nil {
			return nil, err
		}
		calendarService, err = calendars.NewService(repository)
		if err != nil {
			return nil, err
		}
		runtime.ownedCalendar = calendarService
	}
	fail := func(err error) (*businessResourceRuntime, error) { runtime.Close(); return nil, err }
	cursors, err := calendars.NewCursorCodec()
	if err != nil {
		return fail(err)
	}
	runtime.calendars, err = newCalendarResourceHandler(calendarService, authority, cursors, lists.registry, nil, api.CalendarProvenance_CALENDAR_PROVENANCE_OBSERVED)
	if err != nil {
		return fail(err)
	}
	runtime.calendars.workday = workday
	runtime.calendars.inventory = &calendarResourceInventory{lists.registry, lists.manifest}
	runtime.sessions, err = mediasessions.NewService(lists.store, authority, businessResourceClock{})
	if err != nil {
		return fail(err)
	}
	registrations := make([]mediaSourceRegistration, 0, len(sources))
	for _, source := range sources {
		registrations = append(registrations, source.source)
	}
	runtime.media, err = newMediaResourceHandler(runtime.sessions, authority, lists.registry, registrations)
	if err != nil {
		return fail(err)
	}
	return runtime, nil
}
func (r *businessResourceRuntime) Register(plan *PublicPlan) error {
	if r == nil || r.calendars == nil || r.media == nil {
		return ErrInvalidPublicPlan
	}
	if err := registerCalendarResourceRoutes(plan, r.calendars); err != nil {
		return err
	}
	return registerMediaResourceRoutes(plan, r.media)
}
func (r *businessResourceRuntime) Close() {
	if r == nil {
		return
	}
	if r.sessions != nil {
		r.sessions.Close()
	}
	if r.ownedCalendar != nil {
		r.ownedCalendar.Close()
	}
}

// This inventory reads current native registry records, including original
// manifest provenance. It never infers a source from an alias prefix or label.
type calendarResourceInventory struct {
	registry *registry.Repository
	manifest registry.Manifest
}

func (i *calendarResourceInventory) Entities(ctx context.Context) ([]domain.EntityID, error) {
	if i == nil || i.registry == nil {
		return nil, ErrInvalidPublicPlan
	}
	records, err := i.registry.List(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]domain.EntityID, 0)
	for _, record := range records {
		if record.Retired() {
			continue
		}
		original, _, _, err := record.Aliases()
		if err != nil {
			return nil, err
		}
		source, known := i.manifest.SourceFor(original)
		text, valid := source.Value()
		if !known || !valid {
			continue
		}
		switch text {
		case "local_calendar", "remote_calendar", "workday":
		default:
			continue
		}
		_, entity, err := record.IDs()
		if err != nil {
			return nil, err
		}
		result = append(result, entity)
		if len(result) > 128 {
			return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
		}
	}
	return result, nil
}

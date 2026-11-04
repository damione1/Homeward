package server

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/mediacontent"
	"github.com/damione1/Homeward/internal/mediasessions"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type mediaSourceRegistration struct {
	entity domain.EntityID
	slot   authorization.CapabilitySlot
	owner  mediacontent.Owner
}
type mediaResourceHandler struct {
	sessions  *mediasessions.Service
	authority *authorization.Authority
	homes     taskHomeProvider
	cursors   *calendars.CursorCodec
	sources   []mediaSourceRegistration
}

func newMediaResourceHandler(sessions *mediasessions.Service, authority *authorization.Authority, homes taskHomeProvider, sources []mediaSourceRegistration) (*mediaResourceHandler, error) {
	if sessions == nil || authority == nil || nilReactiveInterface(homes) || len(sources) > mediacontent.MaxSources {
		return nil, ErrInvalidPublicPlan
	}
	seen := map[string]bool{}
	for _, source := range sources {
		entity, err := source.entity.Value()
		schema, endpoint, slotErr := source.slot.Values()
		key := fmt.Sprintf("%d:%s%d:%s%d:%s", len(entity), entity, len(schema), schema, len(endpoint), endpoint)
		if err != nil || slotErr != nil || nilReactiveInterface(source.owner) || seen[key] {
			return nil, ErrInvalidPublicPlan
		}
		seen[key] = true
	}
	cursors, err := calendars.NewCursorCodec()
	if err != nil {
		return nil, err
	}
	return &mediaResourceHandler{sessions, authority, homes, cursors, append([]mediaSourceRegistration(nil), sources...)}, nil
}
func (h *mediaResourceHandler) principal(ctx context.Context) (authorization.Principal, error) {
	if h == nil || h.sessions == nil || h.authority == nil || h.cursors == nil || nilReactiveInterface(h.homes) || ctx == nil {
		return authorization.Principal{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil || !principal.Native() {
		return authorization.Principal{}, taskDenied()
	}
	expected, err := principal.Actor()
	actor, present := ActorFromContext(ctx)
	if err != nil || !present || actor != expected {
		return authorization.Principal{}, taskDenied()
	}
	return principal, nil
}
func (h *mediaResourceHandler) home(ctx context.Context, raw apitypes.HomeName) error {
	actual, err := h.homes.HomeName(ctx)
	if err != nil {
		return err
	}
	if !raw.Valid() || !actual.Valid() || raw != actual {
		return taskDenied()
	}
	return nil
}
func (h *mediaResourceHandler) find(home apitypes.HomeName, name apitypes.MediaSourceName) (mediaSourceRegistration, bool) {
	for _, source := range h.sources {
		candidate, err := mediaSourceName(home, source.entity, source.slot)
		if err == nil && candidate == name {
			return source, true
		}
	}
	return mediaSourceRegistration{}, false
}
func (h *mediaResourceHandler) findLocator(locator mediasessions.Locator) (mediaSourceRegistration, bool) {
	for _, source := range h.sources {
		if source.entity == locator.Entity() && source.slot == locator.Slot() {
			return source, true
		}
	}
	return mediaSourceRegistration{}, false
}
func mediaCapture(ctx context.Context, registration mediaSourceRegistration) (mediacontent.Source, error) {
	source, err := registration.owner.Source(ctx, registration.entity, registration.slot)
	if err != nil {
		return mediacontent.Source{}, err
	}
	if !source.Valid() || source.Binding().Entity() != registration.entity || source.Binding().Slot() != registration.slot {
		return mediacontent.Source{}, mediacontent.ErrInvalid
	}
	return source, nil
}
func mediaFinalSource(ctx context.Context, admission *mediasessions.Admission, registration mediaSourceRegistration, source mediacontent.Source) error {
	if !admission.Ready() {
		return taskDenied()
	}
	latest, err := mediaCapture(ctx, registration)
	if err != nil {
		return err
	}
	if latest.Binding() != source.Binding() {
		return mediacontent.ErrSourceChanged
	}
	if !admission.Ready() {
		return taskDenied()
	}
	return nil
}
func withMediaSource[T any](h *mediaResourceHandler, ctx context.Context, raw string, fn func(context.Context, apitypes.HomeName, mediaSourceRegistration, mediacontent.Source, *mediasessions.Admission) (T, error)) (T, error) {
	var zero T
	if _, err := h.principal(ctx); err != nil {
		return zero, mediaResourceError(ctx, err)
	}
	name, err := apitypes.ParseMediaSourceName(raw)
	if err != nil {
		return zero, taskDenied()
	}
	registration, found := h.find(name.Home(), name)
	if !found {
		return zero, taskDenied()
	}
	admission, err := h.sessions.AdmitSource(ctx, registration.entity, registration.slot)
	if err != nil {
		return zero, mediaResourceError(ctx, err)
	}
	defer admission.Release()
	if err := h.home(ctx, name.Home()); err != nil {
		return zero, mediaResourceError(ctx, err)
	}
	source, err := mediaCapture(admission.Context(), registration)
	if err != nil {
		return zero, mediaResourceError(ctx, err)
	}
	out, err := fn(admission.Context(), name.Home(), registration, source, admission)
	if final := h.finalAuthority(ctx, admission, err); final != nil {
		return zero, mediaResourceError(ctx, final)
	}
	if final := mediaFinalSource(admission.Context(), admission, registration, source); final != nil {
		return zero, mediaResourceError(ctx, final)
	}
	if err != nil {
		return zero, mediaResourceError(ctx, err)
	}
	return out, nil
}
func withMediaLocator[T any](h *mediaResourceHandler, ctx context.Context, raw string, fn func(context.Context, mediasessions.Name, mediaSourceRegistration, mediacontent.Source, *mediasessions.Admission) (T, error)) (T, error) {
	var zero T
	if _, err := h.principal(ctx); err != nil {
		return zero, mediaResourceError(ctx, err)
	}
	name, err := mediasessions.ParseName(raw)
	if err != nil {
		return zero, taskDenied()
	}
	locator, err := h.sessions.LookupOwned(ctx, name.ID())
	if err != nil {
		return zero, mediaResourceError(ctx, err)
	}
	registration, found := h.findLocator(locator)
	if !found {
		return zero, taskDenied()
	}
	admission, err := h.sessions.AdmitSource(ctx, locator.Entity(), locator.Slot())
	if err != nil {
		return zero, mediaResourceError(ctx, err)
	}
	defer admission.Release()
	if err := h.home(ctx, name.Home()); err != nil {
		return zero, mediaResourceError(ctx, err)
	}
	source, err := mediaCapture(admission.Context(), registration)
	if err != nil {
		return zero, mediaResourceError(ctx, err)
	}
	if source.Binding().Kind() != locator.Kind() {
		return zero, mediaResourceError(ctx, mediacontent.ErrSourceChanged)
	}
	out, err := fn(admission.Context(), name, registration, source, admission)
	if final := h.finalAuthority(ctx, admission, err); final != nil {
		return zero, mediaResourceError(ctx, final)
	}
	if final := mediaFinalSource(admission.Context(), admission, registration, source); final != nil {
		return zero, mediaResourceError(ctx, final)
	}
	if err != nil {
		return zero, mediaResourceError(ctx, err)
	}
	return out, nil
}
func mediaSessionWire(home apitypes.HomeName, record mediasessions.Record) (*api.MediaSession, error) {
	return mediaSessionWireRevision(home, record, record.EventRevision())
}
func mediaSessionWireRevision(home apitypes.HomeName, record mediasessions.Record, eventRevision int64) (*api.MediaSession, error) {
	name, err := mediasessions.ResourceName(home, record.ID())
	if err != nil {
		return nil, err
	}
	source, err := mediaSourceName(home, record.Source().Entity(), record.Source().Slot())
	if err != nil {
		return nil, err
	}
	kind := api.MediaContentKind_MEDIA_CONTENT_KIND_UNSPECIFIED
	switch record.Source().Kind() {
	case mediasessions.Artwork:
		kind = api.MediaContentKind_MEDIA_CONTENT_KIND_ARTWORK
	case mediasessions.Camera:
		kind = api.MediaContentKind_MEDIA_CONTENT_KIND_CAMERA
	default:
		return nil, mediacontent.ErrInvalid
	}
	out := &api.MediaSession{Name: name.String(), MediaSource: source.String(), ContentKind: kind, CreateTime: timestamppb.New(record.CreateTime()), ExpireTime: timestamppb.New(record.ExpireTime())}
	if record.Source().Kind() == mediasessions.Camera {
		hash := sha256.New()
		hash.Write([]byte(name.String()))
		var revision [8]byte
		binary.BigEndian.PutUint64(revision[:], uint64(eventRevision))
		hash.Write(revision[:])
		out.EventsEtag = fmt.Sprintf("\"%x\"", hash.Sum(nil))
	}
	return out, nil
}
func mediaSourceWire(home apitypes.HomeName, source mediacontent.Source) (*api.MediaSource, error) {
	name, err := mediaSourceName(home, source.Binding().Entity(), source.Binding().Slot())
	if err != nil {
		return nil, err
	}
	entity, err := apitypes.EntityNameForIdentity(home, source.Binding().Entity())
	if err != nil {
		return nil, err
	}
	availability, freshness, provenance, err := source.Quality().Components()
	if err != nil {
		return nil, err
	}
	quality := &api.ObservationQuality{}
	switch availability {
	case domain.AvailabilityAvailable:
		quality.Availability = api.Availability_AVAILABILITY_AVAILABLE
	case domain.AvailabilityUnknown:
		quality.Availability = api.Availability_AVAILABILITY_UNKNOWN
	case domain.AvailabilityUnavailable:
		quality.Availability = api.Availability_AVAILABILITY_UNAVAILABLE
	default:
		return nil, mediacontent.ErrInvalid
	}
	switch freshness {
	case domain.FreshnessFresh:
		quality.Freshness = api.Freshness_FRESHNESS_CURRENT
	case domain.FreshnessStale:
		quality.Freshness = api.Freshness_FRESHNESS_STALE
	default:
		return nil, mediacontent.ErrInvalid
	}
	switch provenance {
	case domain.ProvenanceObserved:
		quality.Provenance = api.Provenance_PROVENANCE_OBSERVED
	case domain.ProvenanceRestored:
		quality.Provenance = api.Provenance_PROVENANCE_RESTORED
	case domain.ProvenanceEstimated:
		quality.Provenance = api.Provenance_PROVENANCE_ESTIMATED
	default:
		return nil, mediacontent.ErrInvalid
	}
	out := &api.MediaSource{Name: name.String(), DisplayName: source.Label(), SourceEntity: entity.String(), Quality: quality}
	switch source.Provenance() {
	case mediacontent.Observed:
		out.DataProvenance = api.MediaDataProvenance_MEDIA_DATA_PROVENANCE_OBSERVED
	case mediacontent.Synthetic:
		out.DataProvenance = api.MediaDataProvenance_MEDIA_DATA_PROVENANCE_SYNTHETIC
	default:
		return nil, mediacontent.ErrInvalid
	}
	switch source.Binding().Kind() {
	case mediasessions.Artwork:
		out.ContentKinds = []api.MediaContentKind{api.MediaContentKind_MEDIA_CONTENT_KIND_ARTWORK}
	case mediasessions.Camera:
		out.ContentKinds = []api.MediaContentKind{api.MediaContentKind_MEDIA_CONTENT_KIND_CAMERA}
	default:
		return nil, mediacontent.ErrInvalid
	}
	return out, nil
}
func (h *mediaResourceHandler) GetMediaSource(ctx context.Context, request *connect.Request[api.GetMediaSourceRequest]) (*connect.Response[api.MediaSource], error) {
	raw := taskRequest(request)
	return withMediaSource(h, ctx, raw.GetName(), func(ctx context.Context, home apitypes.HomeName, _ mediaSourceRegistration, source mediacontent.Source, _ *mediasessions.Admission) (*connect.Response[api.MediaSource], error) {
		if !calendarKnown(raw) {
			return nil, taskInvalid()
		}
		out, err := mediaSourceWire(home, source)
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(out), nil
	})
}
func (h *mediaResourceHandler) CreateMediaSession(ctx context.Context, request *connect.Request[api.CreateMediaSessionRequest]) (*connect.Response[api.MediaSession], error) {
	raw := taskRequest(request)
	return withMediaSource(h, ctx, raw.GetMediaSession().GetMediaSource(), func(ctx context.Context, home apitypes.HomeName, registration mediaSourceRegistration, source mediacontent.Source, admission *mediasessions.Admission) (*connect.Response[api.MediaSession], error) {
		if !calendarKnown(raw) || raw.MediaSession == nil || raw.Parent != home.String() {
			return nil, taskInvalid()
		}
		expected := api.MediaContentKind_MEDIA_CONTENT_KIND_CAMERA
		if source.Binding().Kind() == mediasessions.Artwork {
			expected = api.MediaContentKind_MEDIA_CONTENT_KIND_ARTWORK
		}
		if raw.MediaSession.ContentKind != expected {
			return nil, taskInvalid()
		}
		normalized := businessNormalize(raw).(*api.CreateMediaSessionRequest)
		normalized.MediaSession.Name = ""
		bound, err := businessMutationContext(ctx, apiv2connect.MediaServiceCreateMediaSessionProcedure, raw.RequestId, normalized, func(record mediasessions.Record) (proto.Message, error) { return mediaSessionWire(home, record) })
		if err != nil {
			return nil, err
		}
		replay := func(err error) (*connect.Response[api.MediaSession], error) {
			value, found, failure := businessReplay(err, &api.MediaSession{})
			if found {
				return connect.NewResponse(value), nil
			}
			return nil, mediaResourceFault(failure)
		}
		if raw.RequestId != "" {
			if err := h.sessions.ProbeReplay(bound, admission); err != nil {
				return replay(err)
			}
		}
		revision := int64(0)
		if source.Binding().Kind() == mediasessions.Artwork {
			if err := registration.owner.AdmitArtwork(ctx, source.Binding(), admission.CredentialBinding(), raw.MediaSession.ArtworkReference); err != nil {
				return nil, err
			}
		} else {
			events, err := registration.owner.Events(ctx, source.Binding())
			if err != nil {
				return nil, err
			}
			if !events.Valid() || !events.Matches(source.Binding()) {
				return nil, mediacontent.ErrInvalid
			}
			revision = events.Revision()
		}
		input, err := mediasessions.NewInput(source.Binding(), raw.MediaSession.ArtworkReference, mediasessions.MaxLifetime, revision)
		if err != nil {
			return nil, err
		}
		record, err := h.sessions.Create(bound, admission, input)
		if err != nil {
			return replay(err)
		}
		out, err := mediaSessionWire(home, record)
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(out), nil
	})
}
func (h *mediaResourceHandler) GetMediaSession(ctx context.Context, request *connect.Request[api.GetMediaSessionRequest]) (*connect.Response[api.MediaSession], error) {
	raw := taskRequest(request)
	return withMediaLocator(h, ctx, raw.GetName(), func(ctx context.Context, name mediasessions.Name, registration mediaSourceRegistration, source mediacontent.Source, admission *mediasessions.Admission) (*connect.Response[api.MediaSession], error) {
		if !calendarKnown(raw) {
			return nil, taskInvalid()
		}
		record, err := h.sessions.Get(ctx, admission, name.ID(), source.Binding())
		if err != nil {
			return nil, err
		}
		revision := record.EventRevision()
		if source.Binding().Kind() == mediasessions.Camera {
			events, captureErr := registration.owner.Events(ctx, source.Binding())
			if captureErr != nil {
				return nil, captureErr
			}
			if !events.Valid() || !events.Matches(source.Binding()) {
				return nil, mediacontent.ErrInvalid
			}
			revision = events.Revision()
		}
		out, err := mediaSessionWireRevision(name.Home(), record, revision)
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(out), nil
	})
}
func (h *mediaResourceHandler) DeleteMediaSession(ctx context.Context, request *connect.Request[api.DeleteMediaSessionRequest]) (*connect.Response[emptypb.Empty], error) {
	raw := taskRequest(request)
	return withMediaLocator(h, ctx, raw.GetName(), func(ctx context.Context, name mediasessions.Name, _ mediaSourceRegistration, source mediacontent.Source, admission *mediasessions.Admission) (*connect.Response[emptypb.Empty], error) {
		if !calendarKnown(raw) {
			return nil, taskInvalid()
		}
		bound, err := businessMutationContext(ctx, apiv2connect.MediaServiceDeleteMediaSessionProcedure, raw.RequestId, businessNormalize(raw), func(mediasessions.Record) (proto.Message, error) { return &emptypb.Empty{}, nil })
		if err != nil {
			return nil, err
		}
		_, err = h.sessions.Retire(bound, admission, name.ID(), source.Binding())
		if err != nil {
			value, found, failure := businessReplay(err, &emptypb.Empty{})
			if found {
				return connect.NewResponse(value), nil
			}
			return nil, mediaResourceFault(failure)
		}
		return connect.NewResponse(&emptypb.Empty{}), nil
	})
}

// A revoked credential/policy dominates diagnostics; a possible SQL commit
// dominates cancellation. Caller cancellation alone is never reported as a
// policy denial merely because its retained Permit context also cancelled.
func (h *mediaResourceHandler) finalAuthority(ctx context.Context, admission *mediasessions.Admission, operationErr error) error {
	principal, err := h.principal(ctx)
	if err != nil {
		return err
	}
	current, err := h.authority.Current(principal)
	if err != nil {
		return authorization.ErrDenied
	}
	_, binding, err := current.Binding()
	if err != nil || binding != admission.CredentialBinding() {
		return authorization.ErrDenied
	}
	if mediasessions.IsCode(operationErr, mediasessions.CommitUnknown) {
		return operationErr
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return nil
}

// mediaSourceName adapts the admitted owner slot to the shared pure identity
// function. A playback endpoint never supplies an artwork endpoint implicitly.
func mediaSourceName(home apitypes.HomeName, entity domain.EntityID, slot authorization.CapabilitySlot) (apitypes.MediaSourceName, error) {
	schema, endpoint, err := slot.Values()
	if err != nil {
		return apitypes.MediaSourceName{}, err
	}
	return apitypes.MediaSourceNameForIdentity(home, entity, schema, endpoint)
}

package server

import (
	"context"
	"errors"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/calendars"
)

// Calendar errors carry domain classifications, never a second wire dialect.
func calendarAPIFault(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, authorization.ErrDenied) {
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var domain *calendars.Error
	if !errors.As(err, &domain) || domain == nil {
		return err
	}
	var reason api.ApiErrorReason
	switch domain.Code() {
	case calendars.InvalidInput, calendars.InvalidDate, calendars.InvalidZone, calendars.MissingEnd, calendars.UnsupportedRule, calendars.InvalidOccurrence, calendars.Incompatible, calendars.MissingFloatingZone, calendars.ConflictingEnd, calendars.EndType, calendars.ContradictoryIdentity, calendars.UnsupportedObservanceRule, calendars.ZoneCoverageUnknown, calendars.InconsistentObservance, calendars.InvalidDuration, calendars.DurationLimit, calendars.InvalidStatus:
		reason = api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED
	case calendars.ChildrenPresent:
		reason = api.ApiErrorReason_API_ERROR_REASON_STATE_PRECONDITION_FAILED
	case calendars.ReadOnly:
		reason = api.ApiErrorReason_API_ERROR_REASON_SOURCE_READ_ONLY
	case calendars.Unregistered, calendars.Retired, calendars.NotFound, calendars.Deleted:
		reason = api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND
	case calendars.Conflict, calendars.BindingMismatch:
		reason = api.ApiErrorReason_API_ERROR_REASON_ETAG_MISMATCH
	case calendars.StaleCursor, calendars.ResyncRequired:
		reason = api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED
	case calendars.BudgetExceeded, calendars.CapacityExceeded, calendars.Busy, calendars.RevisionExhausted:
		reason = api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED
	case calendars.CommitUnknown:
		reason = api.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN
	case calendars.Corrupt:
		reason = api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY
	case calendars.Unavailable, calendars.Closed:
		reason = api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE
	case calendars.Canceled:
		reason = api.ApiErrorReason_API_ERROR_REASON_REQUEST_CANCELLED
	case calendars.ErrorInvalid:
		reason = api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE
	default:
		reason = api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE
	}
	return apierrors.New(reason)
}
func calendarRPCError(ctx context.Context, err error) error {
	return apiRPCError(ctx, calendarAPIFault(err))
}

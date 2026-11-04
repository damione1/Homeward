package server

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	boundary "github.com/damione1/Homeward/contracts/apiv2"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/lists"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/view"
)

func listFault(code lists.Code) apierrors.Fault {
	var reason api.ApiErrorReason
	switch code {
	case lists.Invalid:
		reason = api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED
	case lists.Denied, lists.CommittedResponseWithheld:
		reason = api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED
	case lists.NotFound:
		reason = api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND
	case lists.Conflict:
		reason = api.ApiErrorReason_API_ERROR_REASON_ETAG_MISMATCH
	case lists.ImportConflict:
		reason = api.ApiErrorReason_API_ERROR_REASON_RESOURCE_ALREADY_EXISTS
	case lists.StaleCursor, lists.ResyncRequired:
		reason = api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED
	case lists.Capacity:
		reason = api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED
	case lists.Unavailable, lists.Closed:
		reason = api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE
	case lists.CommitUnknown:
		reason = api.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN
	default:
		reason = api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE
	}
	return apierrors.New(reason)
}

// apiFault classifies identities/types, never the human error text or payload.
// Unknown failures remain INTERNAL. Upstream causes do not enter the result.
func apiFault(err error) apierrors.Fault {
	if nilReactiveInterface(err) {
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
	}
	// Authorization dominates diagnostics. An established possible commit/effect
	// then dominates cancellation; cancellation cannot prove that nothing happened.
	if errors.Is(err, authorization.ErrDenied) || errors.Is(err, integrations.ErrCommandAuthorizationDenied) || lists.IsCode(err, lists.Denied) || lists.IsCode(err, lists.CommittedResponseWithheld) {
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED)
	}
	if errors.Is(err, integrations.ErrDurableCommitUnknown) || lists.IsCode(err, lists.CommitUnknown) {
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN)
	}
	if errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) {
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_EFFECT_OUTCOME_UNKNOWN)
	}
	var fault apierrors.Fault
	if errors.As(err, &fault) {
		return fault
	}
	var violation *boundary.Violation
	if errors.As(err, &violation) && violation != nil {
		value, parseErr := apierrors.NewViolation(violation.Descriptor(), violation.Field(), violation.Kind())
		if parseErr == nil {
			return apierrors.Validation(value)
		}
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
	}
	var list *lists.Error
	if errors.As(err, &list) && list != nil {
		return listFault(list.Code())
	}
	var reason api.ApiErrorReason
	switch {
	case errors.Is(err, view.ErrAlreadyExists):
		reason = api.ApiErrorReason_API_ERROR_REASON_RESOURCE_ALREADY_EXISTS
	case errors.Is(err, context.Canceled):
		reason = api.ApiErrorReason_API_ERROR_REASON_REQUEST_CANCELLED
	case errors.Is(err, context.DeadlineExceeded):
		reason = api.ApiErrorReason_API_ERROR_REASON_REQUEST_DEADLINE_EXCEEDED
	case errors.Is(err, view.ErrInvalidInput), errors.Is(err, contracts.ErrInvalidActionRPC), errors.Is(err, integrations.ErrInvalidInput):
		reason = api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED
	case errors.Is(err, view.ErrResourceExhausted), errors.Is(err, authorization.ErrCapacity), errors.Is(err, sequencer.ErrSnapshotBudget):
		reason = api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED
	case errors.Is(err, view.ErrSnapshotUnknown), errors.Is(err, view.ErrSnapshotExpired), errors.Is(err, view.ErrPageToken), errors.Is(err, view.ErrViewChanged):
		reason = api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED
	case errors.Is(err, sequencer.ErrRegistryUnavailable), errors.Is(err, authorization.ErrUnavailable), errors.Is(err, configactivation.ErrActivationUnavailable), errors.Is(err, integrations.ErrDurableLedgerUnavailable), errors.Is(err, integrations.ErrCommandRouteUnavailable):
		reason = api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE
	case errors.Is(err, integrations.ErrCommandIDConflict), errors.Is(err, integrations.ErrCommandPolicyConflict):
		reason = api.ApiErrorReason_API_ERROR_REASON_REQUEST_ID_REUSED
	case errors.Is(err, integrations.ErrCommandIntentTooLarge):
		reason = api.ApiErrorReason_API_ERROR_REASON_REQUEST_TOO_LARGE
	case errors.Is(err, integrations.ErrCommandRouteBusy):
		reason = api.ApiErrorReason_API_ERROR_REASON_RATE_LIMITED
	case errors.Is(err, integrations.ErrCommandPolicyLedgerFull), errors.Is(err, integrations.ErrDurableAdmissionCapacity), errors.Is(err, integrations.ErrActionProgressFull), errors.Is(err, integrations.ErrActionSubscribersFull), errors.Is(err, integrations.ErrActionSubscriberSlow):
		reason = api.ApiErrorReason_API_ERROR_REASON_STREAM_CAPACITY
	default:
		reason = api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE
	}
	return apierrors.New(reason)
}

// Native v2 adapters use this error after object admission. Existing route/TLS,
// body/decompression budgets and revocation publication fences stay outside it.
func apiRPCError(ctx context.Context, err error) error {
	if terminal := authorizationTerminationError(ctx); terminal != nil {
		var reason api.ApiErrorReason
		switch terminal.Code() {
		case connect.CodeUnauthenticated:
			reason = api.ApiErrorReason_API_ERROR_REASON_AUTHENTICATION_REQUIRED
		case connect.CodePermissionDenied:
			reason = api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED
		default:
			reason = api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE
		}
		return apierrors.New(reason).Connect()
	}
	return apiFault(err).Connect()
}

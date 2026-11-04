package server

import (
	"context"
	"errors"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/mcpkeys"
	"github.com/damione1/Homeward/internal/storage"
)

// authAPIFault classifies typed native failures, never error strings, tokens or
// password/proof payloads. It does not translate or call a retired RPC handler.
func authAPIFault(err error) apierrors.Fault {
	var fault apierrors.Fault
	if errors.As(err, &fault) {
		return fault
	}
	var reason api.ApiErrorReason
	switch {
	case errors.Is(err, authorization.ErrDenied), errors.Is(err, storage.ErrAccountPermission), errors.Is(err, storage.ErrSessionPermission), errors.Is(err, storage.ErrSessionAccountDisabled):
		reason = api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED
	case errors.Is(err, storage.ErrAccountCommitUnknown), errors.Is(err, storage.ErrSessionCommitUnknown), authmfa.KindOf(err) == authmfa.ErrorCommitUnknown:
		reason = api.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN
	case errors.Is(err, errSessionUnauthenticated), errors.Is(err, storage.ErrSessionRevoked), errors.Is(err, storage.ErrSessionExpired), errors.Is(err, accounts.ErrInvalidSessionToken):
		reason = api.ApiErrorReason_API_ERROR_REASON_AUTHENTICATION_REQUIRED
	case errors.Is(err, storage.ErrAccountVersionConflict), errors.Is(err, authorization.ErrConflict):
		reason = api.ApiErrorReason_API_ERROR_REASON_ETAG_MISMATCH
	case errors.Is(err, storage.ErrAccountNotFound), errors.Is(err, storage.ErrInvitationNotFound), errors.Is(err, storage.ErrSessionNotFound), errors.Is(err, storage.ErrMCPKeyNotFound), errors.Is(err, storage.ErrAPIResourceNotFound):
		reason = api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND
	case errors.Is(err, storage.ErrAccountConflict):
		reason = api.ApiErrorReason_API_ERROR_REASON_RESOURCE_ALREADY_EXISTS
	case errors.Is(err, storage.ErrBootstrapCompleted), errors.Is(err, storage.ErrLastActiveAdmin), errors.Is(err, storage.ErrInvitationExpired), errors.Is(err, storage.ErrInvitationUnavailable):
		reason = api.ApiErrorReason_API_ERROR_REASON_STATE_PRECONDITION_FAILED
	case errors.Is(err, accounts.ErrInvalidLogin), errors.Is(err, accounts.ErrInvalidDisplayName), errors.Is(err, accounts.ErrInvalidPassword), errors.Is(err, accounts.ErrBlockedPassword), errors.Is(err, accounts.ErrInvalidInvitationToken), errors.Is(err, storage.ErrAccountInvalid), errors.Is(err, storage.ErrSessionInvalid), errors.Is(err, storage.ErrAPIResourceIdentity), errors.Is(err, mcpkeys.ErrInvalid):
		reason = api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED
	case errors.Is(err, accounts.ErrKDFBusy), errors.Is(err, storage.ErrLoginRateLimited), errors.Is(err, errSessionCoordinatorBusy):
		reason = api.ApiErrorReason_API_ERROR_REASON_RATE_LIMITED
	case errors.Is(err, authorization.ErrCapacity), errors.Is(err, storage.ErrAccountListTooLarge):
		reason = api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED
	case errors.Is(err, storage.ErrAPIResourceIntegrity):
		reason = api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY
	case errors.Is(err, context.Canceled):
		reason = api.ApiErrorReason_API_ERROR_REASON_REQUEST_CANCELLED
	case errors.Is(err, context.DeadlineExceeded):
		reason = api.ApiErrorReason_API_ERROR_REASON_REQUEST_DEADLINE_EXCEEDED
	case errors.Is(err, storage.ErrAccountUnavailable), errors.Is(err, storage.ErrSessionUnavailable), errors.Is(err, storage.ErrAPIResourceUnavailable), errors.Is(err, authorization.ErrUnavailable):
		reason = api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE
	default:
		switch authmfa.KindOf(err) {
		case authmfa.ErrorInvalidInput:
			reason = api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED
		case authmfa.ErrorInvalidProof, authmfa.ErrorExpired, authmfa.ErrorConsumed, authmfa.ErrorConflict:
			reason = api.ApiErrorReason_API_ERROR_REASON_AUTHENTICATION_REQUIRED
		case authmfa.ErrorRateLimited, authmfa.ErrorBusy:
			reason = api.ApiErrorReason_API_ERROR_REASON_RATE_LIMITED
		case authmfa.ErrorIntegrity:
			reason = api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY
		case authmfa.ErrorUnavailable:
			reason = api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE
		default:
			reason = api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE
		}
	}
	return apierrors.New(reason)
}

func authAPIError(ctx context.Context, err error) error { return apiRPCError(ctx, authAPIFault(err)) }

// The proof-carrying login/refresh boundary never distinguishes an unknown
// credential owner from an unusable credential. Named resource lookup retains
// its own authorized NOT_FOUND behavior in authAPIError.
func authCredentialError(ctx context.Context, err error) error {
	if errors.Is(err, storage.ErrSessionNotFound) || errors.Is(err, storage.ErrAccountNotFound) || errors.Is(err, storage.ErrSessionAccountDisabled) {
		return authAPIError(ctx, errSessionUnauthenticated)
	}
	return authAPIError(ctx, err)
}

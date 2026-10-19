package server

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
)

type authorizationTerminationCause struct {
	reason authv1.AuthorizationTerminationReason
}

func (authorizationTerminationCause) Error() string {
	return "authorization ended; discard cached view"
}

func authorizationTerminationError(ctx context.Context) *connect.Error {
	if ctx == nil {
		return nil
	}
	var cause authorizationTerminationCause
	if !errors.As(context.Cause(ctx), &cause) {
		return nil
	}
	detail := &authv1.AuthorizationTermination{Reason: cause.reason}
	if _, err := contracts.AdmitAuthorizationTermination(detail); err != nil {
		return nil
	}
	code := connect.CodeUnavailable
	switch cause.reason {
	case authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_CREDENTIAL_REVOKED, authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_CREDENTIAL_EXPIRED:
		code = connect.CodeUnauthenticated
	case authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_ACCOUNT_DISABLED, authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_RIGHTS_CHANGED:
		code = connect.CodePermissionDenied
	case authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_AUTHORITY_UNAVAILABLE:
	case authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_UNSPECIFIED:
		return nil
	default:
		return nil
	}
	err := connect.NewError(code, errors.New("authorization ended; discard cached view"))
	if wire, e := connect.NewErrorDetail(detail); e == nil {
		err.AddDetail(wire)
	}
	return err
}

// The generated handlers retain one common terminal rule even after a stream
// wrote its first frame. Ordinary client cancellation/shutdown has no such
// cause and keeps its original transport error.
type authorizationTerminationInterceptor struct{}

var _ connect.Interceptor = authorizationTerminationInterceptor{}

func (authorizationTerminationInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, request connect.AnyRequest) (connect.AnyResponse, error) {
		response, err := next(ctx, request)
		if terminal := authorizationTerminationError(ctx); terminal != nil {
			return nil, terminal
		}
		return response, err
	}
}
func (authorizationTerminationInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}
func (authorizationTerminationInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		err := next(ctx, conn)
		if terminal := authorizationTerminationError(ctx); terminal != nil {
			return terminal
		}
		return err
	}
}

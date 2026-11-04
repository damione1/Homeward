package server

// Qualification controls decorate the real resource handler. The only calendar
// database owner is its borrowed native Service; no public v1 codec survives.
import (
	"context"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/authorization"
)

type nhf015CalendarHandler struct {
	*calendarResourceHandler
	dropNextReply    atomic.Bool
	unavailable      atomic.Bool
	pauseWatch       atomic.Bool
	mutationRequests atomic.Uint64
	activeStreams    atomic.Int64
}

func (h *nhf015CalendarHandler) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, request connect.AnyRequest) (connect.AnyResponse, error) {
		if h.unavailable.Load() {
			return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE).Connect()
		}
		mutation := false
		for _, route := range calendarResourceRoutes {
			if request.Spec().Procedure == route.path {
				mutation = route.op == auth.ScopeOperation_SCOPE_OPERATION_WRITE
				break
			}
		}
		if mutation {
			h.mutationRequests.Add(1)
		}
		response, err := next(ctx, request)
		if err == nil && mutation && h.dropNextReply.Swap(false) {
			return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN).Connect()
		}
		return response, err
	}
}
func (h *nhf015CalendarHandler) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}
func (h *nhf015CalendarHandler) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, stream connect.StreamingHandlerConn) error {
		h.activeStreams.Add(1)
		defer h.activeStreams.Add(-1)
		principal, err := authorization.PrincipalFromContext(ctx)
		if err != nil {
			return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED).Connect()
		}
		current, err := h.authority.Current(principal)
		if err != nil {
			return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED).Connect()
		}
		_, binding, err := current.Binding()
		if err != nil {
			return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED).Connect()
		}
		return next(ctx, &nhf015CalendarStream{StreamingHandlerConn: stream, ctx: ctx, handler: h, principal: principal, binding: binding})
	}
}

type nhf015CalendarStream struct {
	connect.StreamingHandlerConn
	ctx       context.Context
	handler   *nhf015CalendarHandler
	principal authorization.Principal
	binding   [32]byte
}

func (s *nhf015CalendarStream) Send(message any) error {
	frame, ok := message.(*api.WatchCalendarResponse)
	if !ok {
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE).Connect()
	}
	for frame.GetReady() == nil && s.handler.pauseWatch.Load() {
		current, err := s.handler.authority.Current(s.principal)
		if err != nil {
			return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED).Connect()
		}
		_, binding, err := current.Binding()
		if err != nil || binding != s.binding {
			return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED).Connect()
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return apiRPCError(s.ctx, s.ctx.Err())
		case <-timer.C:
		}
	}
	return s.StreamingHandlerConn.Send(message)
}

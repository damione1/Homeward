package server

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/internal/historyquery"
)

// PublicPlan has already applied TLS, authentication, route authorization and
// audit before invoking this local handler. Its reservation precedes generated
// Connect decoding and remains owned through encoding and response writes.
func historyReservedTransport(runtime *historyRuntime, next http.Handler) http.Handler {
	writer := connect.NewErrorWriter()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := runtime.reader.WithHistoryReadReservation(r.Context(), func(ctx context.Context) error {
			controller := http.NewResponseController(w)
			deadline, ok := ctx.Deadline()
			if !ok {
				return historyquery.ErrUnavailable
			}
			if controller.SetReadDeadline(deadline) != nil || controller.SetWriteDeadline(deadline.Add(publicErrorWriteGrace)) != nil {
				return historyquery.ErrUnavailable
			}
			next.ServeHTTP(w, r.WithContext(ctx))
			return nil
		})
		if err != nil {
			if terminal := authorizationTerminationError(r.Context()); terminal != nil {
				_ = writer.Write(w, r, terminal)
				return
			}
			if cause := historyContextError(r.Context()); cause != nil {
				err = cause
			}
			_ = writer.Write(w, r, historyRPCError(err))
		}
	})
}

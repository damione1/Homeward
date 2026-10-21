package server

import (
	"context"
	"encoding/hex"
	"errors"
	"io"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	statev1 "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/view"
)

type coreRPCHandler struct {
	views    *view.Service
	grants   view.GrantChangeNotifier
	commands *integrations.DurableCommandService
	epoch    integrations.CommandEpochAuthority
}

func coreRPCError(ctx context.Context, err error) error {
	if terminal := authorizationTerminationError(ctx); terminal != nil {
		return terminal
	}
	code := connect.CodeInternal
	switch {
	case errors.Is(err, context.Canceled):
		code = connect.CodeCanceled
	case errors.Is(err, context.DeadlineExceeded):
		code = connect.CodeDeadlineExceeded
	case errors.Is(err, authorization.ErrDenied), errors.Is(err, integrations.ErrCommandAuthorizationDenied):
		code = connect.CodePermissionDenied
	case errors.Is(err, view.ErrInvalidInput), errors.Is(err, contracts.ErrInvalidActionRPC):
		code = connect.CodeInvalidArgument
	case errors.Is(err, view.ErrResourceExhausted), errors.Is(err, authorization.ErrCapacity), errors.Is(err, sequencer.ErrSnapshotBudget):
		code = connect.CodeResourceExhausted
	case errors.Is(err, view.ErrSnapshotUnknown), errors.Is(err, view.ErrSnapshotExpired), errors.Is(err, view.ErrPageToken), errors.Is(err, view.ErrViewChanged):
		code = connect.CodeFailedPrecondition
	case errors.Is(err, sequencer.ErrRegistryUnavailable), errors.Is(err, authorization.ErrUnavailable), errors.Is(err, configactivation.ErrActivationUnavailable):
		code = connect.CodeUnavailable
	}
	return connect.NewError(code, errors.New("core RPC request could not complete"))
}

func coreRPCActor(ctx context.Context) (domain.ActorID, error) {
	if nilReactiveInterface(ctx) {
		return domain.ActorID{}, coreRPCError(ctx, integrations.ErrCommandAuthorizationDenied)
	}
	actor, present := ActorFromContext(ctx)
	if !present || nilReactiveInterface(ctx) || ctx.Err() != nil {
		return domain.ActorID{}, coreRPCError(ctx, integrations.ErrCommandAuthorizationDenied)
	}
	if _, err := actor.Value(); err != nil {
		return domain.ActorID{}, coreRPCError(ctx, integrations.ErrCommandAuthorizationDenied)
	}
	return actor, nil
}

func coreViewFilter(raw *statev1.ViewFilter) (view.Filter, error) {
	if raw == nil {
		return view.Filter{}, view.ErrInvalidInput
	}
	if len(raw.EntityIds) == 0 {
		return view.All(), nil
	}
	if len(raw.EntityIds) > sequencer.MaxProjectedEntities {
		return view.Filter{}, view.ErrInvalidInput
	}
	ids := make([]domain.EntityID, 0, len(raw.EntityIds))
	for _, text := range raw.EntityIds {
		id, err := domain.ParseEntityID(text)
		if err != nil {
			return view.Filter{}, view.ErrInvalidInput
		}
		ids = append(ids, id)
	}
	return view.Only(ids)
}
func corePageToken(text string) bool {
	if len(text) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(text)
	if err != nil {
		return false
	}
	for _, value := range decoded {
		if value != 0 {
			return true
		}
	}
	return false
}

func (h *coreRPCHandler) OpenView(ctx context.Context, request *connect.Request[statev1.OpenViewRequest]) (*connect.Response[statev1.OpenViewResponse], error) {
	actor, err := coreRPCActor(ctx)
	if err != nil {
		return nil, err
	}
	if h == nil || h.views == nil || request == nil || request.Msg == nil || request.Msg.PageSize == 0 || request.Msg.PageSize > view.MaxPageSize || contracts.AdmitCoreViewWire(request.Msg) != nil {
		return nil, coreRPCError(ctx, view.ErrInvalidInput)
	}
	filter, err := coreViewFilter(request.Msg.Filter)
	if err != nil {
		return nil, coreRPCError(ctx, err)
	}
	page, err := h.views.OpenView(ctx, actor, filter, int(request.Msg.PageSize))
	if err != nil {
		return nil, coreRPCError(ctx, err)
	}
	wire, err := view.EncodePage(page)
	if err != nil {
		return nil, coreRPCError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, coreRPCError(ctx, err)
	}
	return connect.NewResponse(wire), nil
}

func (h *coreRPCHandler) ReadPage(ctx context.Context, request *connect.Request[statev1.ReadPageRequest]) (*connect.Response[statev1.ReadPageResponse], error) {
	actor, err := coreRPCActor(ctx)
	if err != nil {
		return nil, err
	}
	if h == nil || h.views == nil || request == nil || request.Msg == nil || contracts.AdmitCoreViewWire(request.Msg) != nil || !corePageToken(request.Msg.SnapshotToken) || !corePageToken(request.Msg.PageToken) {
		return nil, coreRPCError(ctx, view.ErrInvalidInput)
	}
	page, err := h.views.ReadPage(ctx, actor, request.Msg.SnapshotToken, request.Msg.PageToken)
	if err != nil {
		return nil, coreRPCError(ctx, err)
	}
	wire, err := view.EncodePage(page)
	if err != nil {
		return nil, coreRPCError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, coreRPCError(ctx, err)
	}
	return connect.NewResponse(&statev1.ReadPageResponse{Page: wire}), nil
}

func (h *coreRPCHandler) WatchView(ctx context.Context, request *connect.Request[statev1.WatchViewRequest], stream *connect.ServerStream[statev1.WatchViewResponse]) error {
	actor, err := coreRPCActor(ctx)
	if err != nil {
		return err
	}
	if h == nil || h.views == nil || nilReactiveInterface(h.grants) || request == nil || request.Msg == nil || stream == nil || contracts.AdmitCoreViewWire(request.Msg) != nil {
		return coreRPCError(ctx, view.ErrInvalidInput)
	}
	// Subscribe before WatchView's first authority read. Next cannot consume
	// this notification while Send is blocked by HTTP/2 flow control.
	changes, release, err := h.grants.SubscribeGrantChanges(ctx, actor)
	if err != nil {
		return coreRPCError(ctx, err)
	}
	if changes == nil || release == nil {
		if release != nil {
			release()
		}
		return coreRPCError(ctx, view.ErrInvalidInput)
	}
	defer release()
	defer interruptCoreRPCWriteOnChange(ctx, changes)()
	watch, err := h.views.WatchView(ctx, actor, request.Msg)
	if err != nil {
		return coreRPCError(ctx, err)
	}
	defer watch.Close()
	for {
		frame, err := watch.Next(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return coreRPCError(ctx, err)
		}
		if err := ctx.Err(); err != nil {
			return coreRPCError(ctx, err)
		}
		if err := stream.Send(frame); err != nil {
			return coreRPCError(ctx, err)
		}
	}
}

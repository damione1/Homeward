package server

import (
	"connectrpc.com/connect"
	"context"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/lists"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func taskListResultEncoder(home apitypes.HomeName) func(lists.MutationReceipt) (proto.Message, error) {
	return func(receipt lists.MutationReceipt) (proto.Message, error) {
		value, ok := receipt.ListSnapshot()
		if !ok {
			return nil, taskInvalid()
		}
		return taskWireList(home, value)
	}
}
func taskResultEncoder(home apitypes.HomeName) func(lists.MutationReceipt) (proto.Message, error) {
	return func(receipt lists.MutationReceipt) (proto.Message, error) {
		value, ok := receipt.ItemSnapshot()
		if !ok {
			return nil, taskInvalid()
		}
		return taskWire(home, value)
	}
}
func taskEmptyResultEncoder(lists.MutationReceipt) (proto.Message, error) {
	return &emptypb.Empty{}, nil
}
func taskNormalizedUpdate(raw *api.UpdateTaskRequest, fields []lists.ItemField) *api.UpdateTaskRequest {
	copy := businessNormalize(raw).(*api.UpdateTaskRequest)
	copy.UpdateMask = &fieldmaskpb.FieldMask{}
	selected := map[lists.ItemField]bool{}
	for _, field := range fields {
		selected[field] = true
	}
	// Clear every unselected input so ignored values cannot cause key reuse.
	if !selected[lists.ItemSummary] {
		copy.Task.Summary = ""
	} else {
		copy.UpdateMask.Paths = append(copy.UpdateMask.Paths, "summary")
	}
	if !selected[lists.ItemCompleted] {
		copy.Task.Completed = false
	} else {
		copy.UpdateMask.Paths = append(copy.UpdateMask.Paths, "completed")
	}
	if !selected[lists.ItemDescription] {
		copy.Task.Description = nil
	} else {
		copy.UpdateMask.Paths = append(copy.UpdateMask.Paths, "description")
	}
	date := selected[lists.ItemDue] || selected[lists.ItemDueDate]
	instant := selected[lists.ItemDue] || selected[lists.ItemDueInstant]
	if date {
		copy.UpdateMask.Paths = append(copy.UpdateMask.Paths, "due_date")
	}
	if instant {
		copy.UpdateMask.Paths = append(copy.UpdateMask.Paths, "due_instant")
	}
	switch copy.Task.Due.(type) {
	case *api.Task_DueDate:
		if !date {
			copy.Task.Due = nil
		}
	case *api.Task_DueInstant:
		if !instant {
			copy.Task.Due = nil
		}
	}

	return copy
}
func taskListNoopContext(ctx context.Context, method, id string, raw proto.Message, home apitypes.HomeName) (context.Context, error) {
	return businessMutationContext(ctx, method, id, raw, func(value lists.ListSnapshot) (proto.Message, error) { return taskWireList(home, value) })
}
func taskNoopContext(ctx context.Context, method, id string, raw proto.Message, home apitypes.HomeName) (context.Context, error) {
	return businessMutationContext(ctx, method, id, raw, func(value lists.ItemSnapshot) (proto.Message, error) { return taskWire(home, value) })
}

func taskListReplay(ctx context.Context, err error) (*connect.Response[api.TaskList], error) {
	value, replayed, failure := businessReplay(err, &api.TaskList{})
	if replayed {
		return connect.NewResponse(value), nil
	}
	return nil, apiRPCError(ctx, failure)
}
func taskReplay(ctx context.Context, err error) (*connect.Response[api.Task], error) {
	value, replayed, failure := businessReplay(err, &api.Task{})
	if replayed {
		return connect.NewResponse(value), nil
	}
	return nil, apiRPCError(ctx, failure)
}
func taskEmptyReplay(ctx context.Context, err error) (*connect.Response[emptypb.Empty], error) {
	if lists.IsCode(err, lists.ChildrenPresent) {
		return nil, apiRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STATE_PRECONDITION_FAILED))
	}
	value, replayed, failure := businessReplay(err, &emptypb.Empty{})
	if replayed {
		return connect.NewResponse(value), nil
	}
	return nil, apiRPCError(ctx, failure)
}

func taskAppendResultEncoder(home apitypes.HomeName) func(lists.MutationReceipt) (proto.Message, error) {
	return func(receipt lists.MutationReceipt) (proto.Message, error) {
		value, err := taskResultEncoder(home)(receipt)
		if err != nil {
			return nil, err
		}
		task, ok := value.(*api.Task)
		if !ok {
			return nil, taskInvalid()
		}
		return &api.AppendTaskResponse{Task: task}, nil
	}
}
func taskAppendReplay(ctx context.Context, err error) (*connect.Response[api.AppendTaskResponse], error) {
	value, replayed, failure := businessReplay(err, &api.AppendTaskResponse{})
	if replayed {
		return connect.NewResponse(value), nil
	}
	return nil, apiRPCError(ctx, failure)
}

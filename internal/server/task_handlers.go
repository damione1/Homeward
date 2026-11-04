package server

import (
	"context"
	"fmt"
	"math"
	"time"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/lists"
	"github.com/damione1/Homeward/internal/tasknames"
	datepb "google.golang.org/genproto/googleapis/type/date"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type taskHomeProvider interface {
	HomeName(context.Context) (apitypes.HomeName, error)
}
type taskHandler struct {
	service *lists.Service
	cursors *lists.CursorCodec
	homes   taskHomeProvider
}

var _ apiv2connect.TaskServiceHandler = (*taskHandler)(nil)

func newTaskHandler(service *lists.Service, cursors *lists.CursorCodec, homes taskHomeProvider) (*taskHandler, error) {
	if service == nil || cursors == nil || nilReactiveInterface(homes) {
		return nil, ErrInvalidPublicPlan
	}
	return &taskHandler{service, cursors, homes}, nil
}
func taskInvalid() error {
	return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED).Connect()
}
func taskDenied() error {
	return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED).Connect()
}
func taskViolation(message proto.Message, path string, reason api.ValidationReason) error {
	if message == nil {
		return taskInvalid()
	}
	v, err := apierrors.NewViolation(message.ProtoReflect().Descriptor(), path, reason)
	if err != nil {
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE).Connect()
	}
	return apierrors.Validation(v).Connect()
}
func taskRequest[M any](request *connect.Request[M]) *M {
	if request == nil {
		return nil
	}
	return request.Msg
}
func taskKnown(message proto.Message) bool {
	return message != nil && message.ProtoReflect().IsValid() && len(message.ProtoReflect().GetUnknown()) == 0
}
func taskPageSize(raw int32) (int, error) {
	if raw < 0 {
		return 0, taskInvalid()
	}
	if raw == 0 {
		return 100, nil
	}
	return int(min(raw, lists.MaxPage)), nil
}
func (h *taskHandler) admit(ctx context.Context) error {
	if h == nil || h.service == nil || h.cursors == nil || nilReactiveInterface(h.homes) || ctx == nil {
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE).Connect()
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return taskDenied()
	}
	expected, err := principal.Actor()
	actor, ok := ActorFromContext(ctx)
	if err != nil || !ok || actor != expected {
		return taskDenied()
	}
	return nil
}
func (h *taskHandler) home(ctx context.Context, raw string) (apitypes.HomeName, error) {
	name, err := apitypes.ParseHomeName(raw)
	if err != nil {
		return apitypes.HomeName{}, taskInvalid()
	}
	actual, err := h.homes.HomeName(ctx)
	if err != nil {
		return apitypes.HomeName{}, apiRPCError(ctx, err)
	}
	if !actual.Valid() || name != actual {
		return apitypes.HomeName{}, taskDenied()
	}
	return actual, nil
}

// withTaskList retains object authority before validating etags, payloads,
// masks or cursor contents. Malformed identities reveal detail only to a
// collection-authorized actor. No existence query precedes this admission.
func withTaskList[T any](h *taskHandler, ctx context.Context, raw string, op auth.ScopeOperation, fn func(context.Context, tasknames.ListName) (T, error)) (T, error) {
	var zero T
	if err := h.admit(ctx); err != nil {
		return zero, err
	}
	name, err := tasknames.ParseList(raw)
	var admission *lists.Admission
	if err != nil {
		admission, err = h.service.AdmitCollection(ctx, op)
		if err != nil {
			return zero, apiRPCError(ctx, err)
		}
		defer admission.Release()
		if !admission.Ready() {
			return zero, taskDenied()
		}
		return zero, taskInvalid()
	}
	admission, err = h.service.AdmitObject(ctx, name.ID(), op)
	if err != nil {
		return zero, apiRPCError(ctx, err)
	}
	defer admission.Release()
	if _, err = h.home(ctx, name.Home().String()); err != nil {
		return zero, err
	}
	initial, bindErr := h.service.CursorBinding(ctx)
	if bindErr != nil {
		return zero, apiRPCError(ctx, bindErr)
	}
	result, err := fn(admission.Context(), name)
	current, bindErr := h.service.CursorBinding(ctx)
	if bindErr != nil || current != initial {
		return zero, taskDenied()
	}
	if err != nil && apiFault(err).Reason() == api.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN {
		return zero, err
	}
	if ctx.Err() != nil {
		return zero, apiRPCError(ctx, ctx.Err())
	}
	if !admission.Ready() {
		return zero, taskDenied()
	}
	return result, err
}
func withTask[T any](h *taskHandler, ctx context.Context, raw string, op auth.ScopeOperation, fn func(context.Context, tasknames.TaskName) (T, error)) (T, error) {
	var zero T
	name, err := tasknames.ParseTask(raw)
	if err != nil {
		// Recover only the parent shape for admission, never normalize any ID.
		parent := ""
		for i := len(raw) - 1; i >= 0; i-- {
			if raw[i] == '/' {
				if i >= 6 && raw[i-6:i] == "/tasks" {
					parent = raw[:i-6]
				}
				break
			}
		}
		return withTaskList(h, ctx, parent, op, func(context.Context, tasknames.ListName) (T, error) { return zero, taskInvalid() })
	}
	return withTaskList(h, ctx, name.Parent().String(), op, func(admitted context.Context, _ tasknames.ListName) (T, error) { return fn(admitted, name) })
}
func taskListRevision(name tasknames.ListName, etag string) (lists.ListPrecondition, error) {
	expected, err := lists.ExpectedListETag(name.ID(), etag, func(current lists.ListRevision) (string, error) {
		_, number, err := current.Values()
		if err != nil {
			return "", err
		}
		return tasknames.Etag(name.String(), number, 0)
	})
	if err != nil {
		return nil, taskInvalid()
	}
	return expected, nil
}
func taskRevisions(name tasknames.TaskName, etag string) (lists.ListPrecondition, lists.ItemPrecondition, error) {
	list, item, err := lists.ExpectedItemETag(name.Parent().ID(), name.UID(), etag, func(list lists.ListRevision, item lists.ItemRevision) (string, error) {
		_, parent, err := list.Values()
		if err != nil {
			return "", err
		}
		_, _, child, err := item.Values()
		if err != nil {
			return "", err
		}
		return tasknames.Etag(name.String(), parent, child)
	})
	if err != nil {
		return nil, nil, taskInvalid()
	}
	return list, item, nil
}
func taskWireList(home apitypes.HomeName, value lists.ListSnapshot) (*api.TaskList, error) {
	name, err := tasknames.List(home, value.ID())
	if err != nil {
		return nil, err
	}
	_, n, err := value.Revision().Values()
	if err != nil {
		return nil, err
	}
	etag, err := tasknames.Etag(name.String(), n, 0)
	if err != nil {
		return nil, err
	}
	var origin api.TaskListOrigin
	switch value.Origin() {
	case lists.LocalTodo:
		origin = api.TaskListOrigin_TASK_LIST_ORIGIN_LOCAL_TODO
	case lists.ShoppingList:
		origin = api.TaskListOrigin_TASK_LIST_ORIGIN_SHOPPING_LIST
	case lists.Native:
		origin = api.TaskListOrigin_TASK_LIST_ORIGIN_NATIVE
	default:
		return nil, fmt.Errorf("task list origin invalid")
	}
	stamp := timestamppb.New(value.UpdatedAt())
	if err := stamp.CheckValid(); err != nil {
		return nil, err
	}
	return &api.TaskList{Name: name.String(), DisplayName: value.Name(), Origin: origin, UpdateTime: stamp, Etag: etag}, nil
}
func taskWire(home apitypes.HomeName, value lists.ItemSnapshot) (*api.Task, error) {
	parent, err := tasknames.List(home, value.ListID())
	if err != nil {
		return nil, err
	}
	name, err := tasknames.Task(parent, value.UID())
	if err != nil {
		return nil, err
	}
	_, listNumber, err := value.ListRevision().Values()
	if err != nil {
		return nil, err
	}
	_, _, itemNumber, err := value.Revision().Values()
	if err != nil {
		return nil, err
	}
	etag, err := tasknames.Etag(name.String(), listNumber, itemNumber)
	if err != nil {
		return nil, err
	}
	out := &api.Task{Name: name.String(), Summary: value.Summary(), Completed: value.Completed(), Position: value.Position(), Etag: etag}
	if text, ok := value.Description(); ok {
		out.Description = &text
	}
	date, instant, zone, err := value.Due().Values()
	if err != nil {
		return nil, err
	}
	if date != "" {
		parsed, err := time.Parse("2006-01-02", date)
		if err != nil {
			return nil, err
		}
		out.Due = &api.Task_DueDate{DueDate: &datepb.Date{Year: int32(parsed.Year()), Month: int32(parsed.Month()), Day: int32(parsed.Day())}}
	} else if !instant.IsZero() {
		out.Due = &api.Task_DueInstant{DueInstant: &api.TaskDueInstant{DueTime: timestamppb.New(instant), TimeZone: zone}}
	}
	if at, ok := value.CompletedAt(); ok {
		out.CompletionTime = timestamppb.New(at)
	}
	return out, nil
}
func taskInput(raw *api.Task, fields []lists.ItemField) (lists.ItemInput, error) {
	input := lists.ItemInput{Due: lists.NoDue()}
	if raw == nil || !taskKnown(raw) {
		return input, taskInvalid()
	}
	for _, field := range fields {
		switch field {
		case lists.ItemSummary:
			input.Summary = raw.Summary
		case lists.ItemCompleted:
			input.Completed = raw.Completed
		case lists.ItemDescription:
			input.Description = raw.Description
		case lists.ItemDue, lists.ItemDueDate, lists.ItemDueInstant:
			switch due := raw.Due.(type) {
			case nil:
			case *api.Task_DueDate:
				if field == lists.ItemDueInstant {
					continue
				}
				if due == nil || !taskKnown(due.DueDate) {
					return input, taskInvalid()
				}
				date := due.DueDate
				if date.Year < 1 || date.Year > 9999 || date.Month < 1 || date.Month > 12 || date.Day < 1 || date.Day > 31 {
					return input, taskInvalid()
				}
				value, err := lists.NewDueDate(fmt.Sprintf("%04d-%02d-%02d", date.Year, date.Month, date.Day))
				if err != nil {
					return input, taskInvalid()
				}
				input.Due = value
			case *api.Task_DueInstant:
				if field == lists.ItemDueDate {
					continue
				}
				if due == nil || !taskKnown(due.DueInstant) || !taskKnown(due.DueInstant.DueTime) || due.DueInstant.DueTime.CheckValid() != nil {
					return input, taskInvalid()
				}
				value, err := lists.NewDueInstant(due.DueInstant.DueTime.AsTime(), due.DueInstant.TimeZone)
				if err != nil {
					return input, taskInvalid()
				}
				input.Due = value
			default:
				return input, taskInvalid()
			}
		default:
			return input, taskInvalid()
		}
	}
	return input, nil
}

var taskEditableFields = []lists.ItemField{lists.ItemSummary, lists.ItemCompleted, lists.ItemDescription, lists.ItemDue}

// Mask omission infers only populated fields. A present empty mask is a no-op;
// a sole '*' replaces editable fields. Readonly fields are ignored even when
// selected, and identity/etag selections never mutate the identity or CAS.
func taskMask(raw *api.Task, mask *fieldmaskpb.FieldMask) ([]lists.ItemField, error) {
	if raw == nil {
		return nil, taskInvalid()
	}
	if mask == nil {
		var fields []lists.ItemField
		if raw.Summary != "" {
			fields = append(fields, lists.ItemSummary)
		}
		if raw.Completed {
			fields = append(fields, lists.ItemCompleted)
		}
		if raw.Description != nil {
			fields = append(fields, lists.ItemDescription)
		}
		switch raw.Due.(type) {
		case *api.Task_DueDate:
			fields = append(fields, lists.ItemDueDate)
		case *api.Task_DueInstant:
			fields = append(fields, lists.ItemDueInstant)
		}
		return fields, nil
	}
	if !taskKnown(mask) {
		return nil, taskInvalid()
	}
	if len(mask.Paths) == 1 && mask.Paths[0] == "*" {
		return append([]lists.ItemField(nil), taskEditableFields...), nil
	}
	var fields []lists.ItemField
	seen := map[lists.ItemField]bool{}
	for _, path := range mask.Paths {
		var field lists.ItemField
		switch path {
		case "summary":
			field = lists.ItemSummary
		case "completed":
			field = lists.ItemCompleted
		case "description":
			field = lists.ItemDescription
		case "due_date":
			field = lists.ItemDueDate
		case "due_instant":
			field = lists.ItemDueInstant
		case "name", "etag", "position", "completion_time":
			continue
		default:
			return nil, taskInvalid()
		}
		if !seen[field] {
			seen[field] = true
			fields = append(fields, field)
		}
	}
	return fields, nil
}
func (h *taskHandler) ListTaskLists(ctx context.Context, request *connect.Request[api.ListTaskListsRequest]) (*connect.Response[api.ListTaskListsResponse], error) {
	return withTaskCollection(h, ctx, auth.ScopeOperation_SCOPE_OPERATION_READ, func(ctx context.Context) (*connect.Response[api.ListTaskListsResponse], error) {
		if request == nil || !taskKnown(request.Msg) {
			return nil, taskInvalid()
		}
		raw := taskRequest(request)
		home, err := h.home(ctx, raw.Parent)
		if err != nil {
			return nil, err
		}
		limit, err := taskPageSize(raw.PageSize)
		if err != nil {
			return nil, err
		}
		codec, err := h.cursorFor(ctx)
		if err != nil {
			return nil, err
		}
		cursor, err := codec.DecodeList(raw.PageToken)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		page, err := h.service.ListLists(ctx, cursor, limit)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		if page.TotalSize() > math.MaxInt32 {
			return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED).Connect()
		}
		out := &api.ListTaskListsResponse{TotalSize: int32(page.TotalSize())}
		for _, value := range page.Entries() {
			resource, err := taskWireList(home, value)
			if err != nil {
				return nil, apiRPCError(ctx, err)
			}
			out.TaskLists = append(out.TaskLists, resource)
		}
		if next, ok := page.Next(); ok {
			out.NextPageToken, err = codec.EncodeList(next)
			if err != nil {
				return nil, apiRPCError(ctx, err)
			}
		}
		return connect.NewResponse(out), nil
	})
}
func (h *taskHandler) GetTaskList(ctx context.Context, request *connect.Request[api.GetTaskListRequest]) (*connect.Response[api.TaskList], error) {
	raw := taskRequest(request)
	return withTaskList(h, ctx, raw.GetName(), auth.ScopeOperation_SCOPE_OPERATION_READ, func(ctx context.Context, name tasknames.ListName) (*connect.Response[api.TaskList], error) {
		if !taskKnown(raw) {
			return nil, taskInvalid()
		}
		value, err := h.service.GetList(ctx, name.ID())
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		out, err := taskWireList(name.Home(), value)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		return connect.NewResponse(out), nil
	})
}
func (h *taskHandler) CreateTaskList(ctx context.Context, request *connect.Request[api.CreateTaskListRequest]) (*connect.Response[api.TaskList], error) {
	return withTaskCollection(h, ctx, auth.ScopeOperation_SCOPE_OPERATION_WRITE, func(ctx context.Context) (*connect.Response[api.TaskList], error) {
		if request == nil || !taskKnown(request.Msg) || !taskKnown(request.Msg.TaskList) {
			return nil, taskInvalid()
		}
		raw := taskRequest(request)
		home, err := h.home(ctx, raw.Parent)
		if err != nil {
			return nil, err
		}
		if raw.TaskList.Name != "" {
			return nil, taskViolation(raw, "task_list.name", api.ValidationReason_VALIDATION_REASON_INCONSISTENT_FIELDS)
		}
		payload, err := lists.NewListPayload(raw.TaskList.DisplayName)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		normalized := businessNormalize(raw).(*api.CreateTaskListRequest)
		normalized.TaskList.Etag = ""
		ctx, err = businessMutationContext(ctx, apiv2connect.TaskServiceCreateTaskListProcedure, raw.RequestId, normalized, taskListResultEncoder(home))
		if err != nil {
			return nil, err
		}
		receipt, err := h.service.CreateList(ctx, payload)
		if err != nil {
			return taskListReplay(ctx, err)
		}
		value, ok := receipt.ListSnapshot()
		if !ok {
			return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE).Connect()
		}
		out, err := taskWireList(home, value)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		return connect.NewResponse(out), nil
	})
}
func (h *taskHandler) UpdateTaskList(ctx context.Context, request *connect.Request[api.UpdateTaskListRequest]) (*connect.Response[api.TaskList], error) {
	raw := taskRequest(request)
	return withTaskList(h, ctx, raw.GetTaskList().GetName(), auth.ScopeOperation_SCOPE_OPERATION_WRITE, func(ctx context.Context, name tasknames.ListName) (*connect.Response[api.TaskList], error) {
		if !taskKnown(raw) || !taskKnown(raw.TaskList) {
			return nil, taskInvalid()
		}
		rev, err := taskListRevision(name, raw.TaskList.Etag)
		if err != nil {
			return nil, err
		}
		selected := raw.TaskList.DisplayName != ""
		if raw.UpdateMask != nil {
			if !taskKnown(raw.UpdateMask) {
				return nil, taskInvalid()
			}
			selected = false
			for _, path := range raw.UpdateMask.Paths {
				switch path {
				case "display_name", "*":
					selected = true
				case "name", "etag", "origin", "update_time":
				default:
					return nil, taskInvalid()
				}
			}
			if len(raw.UpdateMask.Paths) > 1 {
				for _, path := range raw.UpdateMask.Paths {
					if path == "*" {
						return nil, taskInvalid()
					}
				}
			}
		}
		normalized := businessNormalize(raw).(*api.UpdateTaskListRequest)
		normalized.UpdateMask = &fieldmaskpb.FieldMask{}
		if selected {
			normalized.UpdateMask.Paths = []string{"display_name"}
		} else {
			normalized.TaskList.DisplayName = ""
		}
		var value lists.ListSnapshot
		if selected {
			ctx, err = businessMutationContext(ctx, apiv2connect.TaskServiceUpdateTaskListProcedure, raw.RequestId, normalized, taskListResultEncoder(name.Home()))
			if err != nil {
				return nil, err
			}
			receipt, err := h.service.RenameList(ctx, name.ID(), rev, raw.TaskList.DisplayName)
			if err != nil {
				return taskListReplay(ctx, err)
			}
			var ok bool
			value, ok = receipt.ListSnapshot()
			if !ok {
				return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE).Connect()
			}
		} else {
			ctx, err = taskListNoopContext(ctx, apiv2connect.TaskServiceUpdateTaskListProcedure, raw.RequestId, normalized, name.Home())
			if err != nil {
				return nil, err
			}
			value, err = h.service.CheckListUpdate(ctx, name.ID(), rev)
			if err != nil {
				return taskListReplay(ctx, err)
			}
		}
		out, err := taskWireList(name.Home(), value)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		return connect.NewResponse(out), nil
	})
}
func (h *taskHandler) DeleteTaskList(ctx context.Context, request *connect.Request[api.DeleteTaskListRequest]) (*connect.Response[emptypb.Empty], error) {
	raw := taskRequest(request)
	return withTaskList(h, ctx, raw.GetName(), auth.ScopeOperation_SCOPE_OPERATION_WRITE, func(ctx context.Context, name tasknames.ListName) (*connect.Response[emptypb.Empty], error) {
		if !taskKnown(raw) {
			return nil, taskInvalid()
		}
		rev, err := taskListRevision(name, raw.Etag)
		if err != nil {
			return nil, err
		}
		ctx, err = businessMutationContext(ctx, apiv2connect.TaskServiceDeleteTaskListProcedure, raw.RequestId, businessNormalize(raw), taskEmptyResultEncoder)
		if err != nil {
			return nil, err
		}
		if _, err = h.service.DeleteListResource(ctx, name.ID(), rev, raw.Force); err != nil {
			return taskEmptyReplay(ctx, err)
		}
		return connect.NewResponse(&emptypb.Empty{}), nil
	})
}
func (h *taskHandler) ListTasks(ctx context.Context, request *connect.Request[api.ListTasksRequest]) (*connect.Response[api.ListTasksResponse], error) {
	raw := taskRequest(request)
	return withTaskList(h, ctx, raw.GetParent(), auth.ScopeOperation_SCOPE_OPERATION_READ, func(ctx context.Context, name tasknames.ListName) (*connect.Response[api.ListTasksResponse], error) {
		if !taskKnown(raw) {
			return nil, taskInvalid()
		}
		limit, err := taskPageSize(raw.PageSize)
		if err != nil {
			return nil, err
		}
		codec, err := h.cursorFor(ctx)
		if err != nil {
			return nil, err
		}
		cursor, err := codec.DecodeItem(raw.PageToken)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		page, err := h.service.ListItems(ctx, name.ID(), cursor, limit)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		if page.TotalSize() > math.MaxInt32 {
			return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED).Connect()
		}
		out := &api.ListTasksResponse{TotalSize: int32(page.TotalSize())}
		for _, item := range page.Entries() {
			value, err := taskWire(name.Home(), item)
			if err != nil {
				return nil, apiRPCError(ctx, err)
			}
			out.Tasks = append(out.Tasks, value)
		}
		if next, ok := page.Next(); ok {
			out.NextPageToken, err = codec.EncodeItem(next)
			if err != nil {
				return nil, apiRPCError(ctx, err)
			}
		}
		return connect.NewResponse(out), nil
	})
}
func (h *taskHandler) GetTask(ctx context.Context, request *connect.Request[api.GetTaskRequest]) (*connect.Response[api.Task], error) {
	raw := taskRequest(request)
	return withTask(h, ctx, raw.GetName(), auth.ScopeOperation_SCOPE_OPERATION_READ, func(ctx context.Context, name tasknames.TaskName) (*connect.Response[api.Task], error) {
		if !taskKnown(raw) {
			return nil, taskInvalid()
		}
		value, err := h.service.GetItem(ctx, name.Parent().ID(), name.UID())
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		out, err := taskWire(name.Parent().Home(), value)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		return connect.NewResponse(out), nil
	})
}
func (h *taskHandler) AppendTask(ctx context.Context, request *connect.Request[api.AppendTaskRequest]) (*connect.Response[api.AppendTaskResponse], error) {
	raw := taskRequest(request)
	return withTaskList(h, ctx, raw.GetName(), auth.ScopeOperation_SCOPE_OPERATION_WRITE, func(ctx context.Context, name tasknames.ListName) (*connect.Response[api.AppendTaskResponse], error) {
		rev, payload, err := taskAddition(raw, name)
		if err != nil {
			return nil, err
		}
		normalized := businessNormalize(raw).(*api.AppendTaskRequest)
		ctx, err = businessMutationContext(ctx, apiv2connect.TaskServiceAppendTaskProcedure, raw.RequestId, normalized, taskAppendResultEncoder(name.Home()))
		if err != nil {
			return nil, err
		}
		receipt, err := h.service.AddItem(ctx, name.ID(), rev, payload)
		if err != nil {
			return taskAppendReplay(ctx, err)
		}
		value, ok := receipt.ItemSnapshot()
		if !ok {
			return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE).Connect()
		}
		out, err := taskWire(name.Home(), value)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		return connect.NewResponse(&api.AppendTaskResponse{Task: out}), nil
	})
}
func (h *taskHandler) UpdateTask(ctx context.Context, request *connect.Request[api.UpdateTaskRequest]) (*connect.Response[api.Task], error) {
	raw := taskRequest(request)
	return withTask(h, ctx, raw.GetTask().GetName(), auth.ScopeOperation_SCOPE_OPERATION_WRITE, func(ctx context.Context, name tasknames.TaskName) (*connect.Response[api.Task], error) {
		if !taskKnown(raw) || !taskKnown(raw.Task) {
			return nil, taskInvalid()
		}
		list, item, err := taskRevisions(name, raw.Task.Etag)
		if err != nil {
			return nil, err
		}
		fields, err := taskMask(raw.Task, raw.UpdateMask)
		if err != nil {
			return nil, err
		}
		var value lists.ItemSnapshot
		normalized := taskNormalizedUpdate(raw, fields)
		if len(fields) == 0 {
			ctx, err = taskNoopContext(ctx, apiv2connect.TaskServiceUpdateTaskProcedure, raw.RequestId, normalized, name.Parent().Home())
			if err != nil {
				return nil, err
			}
			value, err = h.service.CheckItemUpdate(ctx, name.Parent().ID(), name.UID(), list, item)
		} else {
			input, e := taskInput(raw.Task, fields)
			if e != nil {
				return nil, e
			}
			patch, e := lists.NewItemPatch(input, fields)
			if e != nil {
				return nil, apiRPCError(ctx, e)
			}
			ctx, err = businessMutationContext(ctx, apiv2connect.TaskServiceUpdateTaskProcedure, raw.RequestId, normalized, taskResultEncoder(name.Parent().Home()))
			if err != nil {
				return nil, err
			}
			receipt, e := h.service.PatchItem(ctx, name.Parent().ID(), name.UID(), list, item, patch)
			if e != nil {
				return taskReplay(ctx, e)
			}
			var ok bool
			value, ok = receipt.ItemSnapshot()
			if !ok {
				return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE).Connect()
			}
		}
		if err != nil {
			return taskReplay(ctx, err)
		}
		out, err := taskWire(name.Parent().Home(), value)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		return connect.NewResponse(out), nil
	})
}
func (h *taskHandler) DeleteTask(ctx context.Context, request *connect.Request[api.DeleteTaskRequest]) (*connect.Response[emptypb.Empty], error) {
	raw := taskRequest(request)
	return withTask(h, ctx, raw.GetName(), auth.ScopeOperation_SCOPE_OPERATION_WRITE, func(ctx context.Context, name tasknames.TaskName) (*connect.Response[emptypb.Empty], error) {
		if !taskKnown(raw) {
			return nil, taskInvalid()
		}
		list, item, err := taskRevisions(name, raw.Etag)
		if err != nil {
			return nil, err
		}
		ctx, err = businessMutationContext(ctx, apiv2connect.TaskServiceDeleteTaskProcedure, raw.RequestId, businessNormalize(raw), taskEmptyResultEncoder)
		if err != nil {
			return nil, err
		}
		if _, err = h.service.DeleteItem(ctx, name.Parent().ID(), name.UID(), list, item); err != nil {
			return taskEmptyReplay(ctx, err)
		}
		return connect.NewResponse(&emptypb.Empty{}), nil
	})
}
func (h *taskHandler) MoveTask(ctx context.Context, request *connect.Request[api.MoveTaskRequest]) (*connect.Response[api.Task], error) {
	raw := taskRequest(request)
	return withTask(h, ctx, raw.GetName(), auth.ScopeOperation_SCOPE_OPERATION_WRITE, func(ctx context.Context, name tasknames.TaskName) (*connect.Response[api.Task], error) {
		if !taskKnown(raw) {
			return nil, taskInvalid()
		}
		list, item, err := taskRevisions(name, raw.Etag)
		if err != nil {
			return nil, err
		}
		var after *lists.ItemUID
		switch to := raw.Destination.(type) {
		case *api.MoveTaskRequest_First:
			if to == nil || !taskKnown(to.First) {
				return nil, taskInvalid()
			}
		case *api.MoveTaskRequest_Predecessor:
			if to == nil {
				return nil, taskInvalid()
			}
			other, err := tasknames.ParseTask(to.Predecessor)
			if err != nil || other.Parent() != name.Parent() || other == name {
				return nil, taskInvalid()
			}
			uid := other.UID()
			after = &uid
		default:
			return nil, taskInvalid()
		}
		ctx, err = businessMutationContext(ctx, apiv2connect.TaskServiceMoveTaskProcedure, raw.RequestId, businessNormalize(raw), taskResultEncoder(name.Parent().Home()))
		if err != nil {
			return nil, err
		}
		receipt, err := h.service.MoveItem(ctx, name.Parent().ID(), name.UID(), after, list, item)
		if err != nil {
			return taskReplay(ctx, err)
		}
		value, ok := receipt.ItemSnapshot()
		if !ok {
			return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE).Connect()
		}
		out, err := taskWire(name.Parent().Home(), value)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		return connect.NewResponse(out), nil
	})
}
func (h *taskHandler) WatchTaskLists(ctx context.Context, request *connect.Request[api.WatchTaskListsRequest], stream *connect.ServerStream[api.WatchTaskListsResponse]) error {
	if err := h.admit(ctx); err != nil {
		return err
	}
	if request == nil || !taskKnown(request.Msg) || stream == nil {
		return taskInvalid()
	}
	raw := taskRequest(request)
	var sub *lists.Subscription
	var home apitypes.HomeName
	var err error
	if raw.TaskList != nil {
		name, parseErr := tasknames.ParseList(*raw.TaskList)
		if parseErr != nil {
			return taskDenied()
		}
		sub, err = h.service.SubscribeList(ctx, name.ID())
		if err == nil {
			home, err = h.home(sub.Context(), name.Home().String())
			if err == nil && raw.Parent != home.String() {
				err = taskInvalid()
			}
		}
	} else {
		sub, err = h.service.SubscribeCatalog(ctx)
		if err == nil {
			home, err = h.home(sub.Context(), raw.Parent)
		}
	}
	if err != nil && sub != nil {
		sub.Close()
	}
	if err != nil {
		return apiRPCError(ctx, err)
	}
	defer sub.Close()
	defer interruptCoreRPCWriteOnCancel(ctx, sub.Context())()
	if sub.Context() == nil || sub.Context().Err() != nil {
		return taskDenied()
	}
	if err := stream.Send(&api.WatchTaskListsResponse{Sequence: 1, Frame: &api.WatchTaskListsResponse_Ready{Ready: &emptypb.Empty{}}}); err != nil {
		return err
	}
	sequence := int64(1)
	for {
		change, err := sub.Next(ctx)
		if lists.IsCode(err, lists.ResyncRequired) {
			if sequence == math.MaxInt64 {
				return apiRPCError(ctx, err)
			}
			return stream.Send(&api.WatchTaskListsResponse{Sequence: sequence + 1, Frame: &api.WatchTaskListsResponse_ResyncRequired{ResyncRequired: &emptypb.Empty{}}})
		}
		if err != nil {
			return apiRPCError(ctx, err)
		}
		if sequence == math.MaxInt64 {
			return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED).Connect()
		}
		name, err := tasknames.List(home, change.ListID())
		if err != nil {
			return apiRPCError(ctx, err)
		}
		_, n, err := change.Revision().Values()
		if err != nil {
			return apiRPCError(ctx, err)
		}
		etag, err := tasknames.Etag(name.String(), n, 0)
		if err != nil {
			return apiRPCError(ctx, err)
		}
		sequence++
		if err := stream.Send(&api.WatchTaskListsResponse{Sequence: sequence, Frame: &api.WatchTaskListsResponse_Changed{Changed: &api.TaskListChanged{TaskList: name.String(), Etag: etag}}}); err != nil {
			return err
		}
	}
}

// Assert the admitted native identity remains the lists domain identity.
var _ domain.ListID = tasknames.ListName{}.ID()

func (h *taskHandler) cursorFor(ctx context.Context) (*lists.CursorCodec, error) {
	binding, err := h.service.CursorBinding(ctx)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	codec, err := h.cursors.BindCaller(binding)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	return codec, nil
}

// taskAppendInput converts the current closed editable value into the common
// typed owner admission. No output-only resource fields can enter this path.
func taskAppendInput(raw *api.TaskContent) (lists.ItemInput, error) {
	if !taskKnown(raw) {
		return lists.ItemInput{}, taskInvalid()
	}
	value := &api.Task{Summary: raw.Summary, Completed: raw.Completed, Description: raw.Description}
	switch due := raw.Due.(type) {
	case nil:
	case *api.TaskContent_DueDate:
		if due == nil {
			return lists.ItemInput{}, taskInvalid()
		}
		value.Due = &api.Task_DueDate{DueDate: due.DueDate}
	case *api.TaskContent_DueInstant:
		if due == nil {
			return lists.ItemInput{}, taskInvalid()
		}
		value.Due = &api.Task_DueInstant{DueInstant: due.DueInstant}
	default:
		return lists.ItemInput{}, taskInvalid()
	}
	return taskInput(value, taskEditableFields)
}

func withTaskCollection[T any](h *taskHandler, ctx context.Context, op auth.ScopeOperation, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	if err := h.admit(ctx); err != nil {
		return zero, err
	}
	admission, err := h.service.AdmitCollection(ctx, op)
	if err != nil {
		return zero, apiRPCError(ctx, err)
	}
	defer admission.Release()
	initial, err := h.service.CursorBinding(ctx)
	if err != nil {
		return zero, apiRPCError(ctx, err)
	}
	result, operationErr := fn(admission.Context())
	current, err := h.service.CursorBinding(ctx)
	if err != nil || current != initial {
		return zero, taskDenied()
	}
	if operationErr != nil && apiFault(operationErr).Reason() == api.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN {
		return zero, operationErr
	}
	if ctx.Err() != nil {
		return zero, apiRPCError(ctx, ctx.Err())
	}
	if !admission.Ready() {
		return zero, taskDenied()
	}
	return result, operationErr
}

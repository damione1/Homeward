package server

import (
	"context"
	"errors"
	"math"
	"time"

	"connectrpc.com/connect"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	listsv1 "github.com/damione1/Homeward/gen/home/lists/v1"
	"github.com/damione1/Homeward/gen/home/lists/v1/listsv1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/lists"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type listHandler struct {
	service *lists.Service
	cursors *lists.CursorCodec
}

var _ listsv1connect.ListServiceHandler = (*listHandler)(nil)

func newListHandler(service *lists.Service, cursors *lists.CursorCodec) (*listHandler, error) {
	if service == nil || cursors == nil {
		return nil, ErrInvalidPublicPlan
	}
	return &listHandler{service: service, cursors: cursors}, nil
}

func listRPCCode(code lists.Code) error {
	return listFault(code).Connect()
}

func listRPCError(err error) error {
	if err == nil {
		return nil
	}
	return apiFault(err).Connect()
}

func (h *listHandler) admit(ctx context.Context) error {
	if h == nil || h.service == nil || h.cursors == nil || ctx == nil {
		return listRPCCode(lists.Unavailable)
	}
	actor, ok := ActorFromContext(ctx)
	if !ok {
		return listRPCCode(lists.Denied)
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return listRPCCode(lists.Denied)
	}
	admitted, err := principal.Actor()
	if err != nil || admitted != actor {
		return listRPCCode(lists.Denied)
	}
	return nil
}

func listID(raw *listsv1.ListId) (domain.ListID, error) {
	if raw == nil {
		return domain.ListID{}, listRPCCode(lists.Invalid)
	}
	id, err := domain.ParseListID(raw.Value)
	if err != nil {
		return domain.ListID{}, listRPCCode(lists.Invalid)
	}
	return id, nil
}
func itemUID(raw *listsv1.ItemUid) (lists.ItemUID, error) {
	if raw == nil {
		return lists.ItemUID{}, listRPCCode(lists.Invalid)
	}
	id, err := lists.ParseItemUID(raw.Value)
	if err != nil {
		return lists.ItemUID{}, listRPCCode(lists.Invalid)
	}
	return id, nil
}
func expectedListRevision(raw *listsv1.ListRevision, id domain.ListID) (lists.ListRevision, error) {
	if raw == nil || raw.Number > math.MaxInt64 {
		return lists.ListRevision{}, listRPCCode(lists.Invalid)
	}
	bound, err := listID(raw.ListId)
	if err != nil || bound != id {
		return lists.ListRevision{}, listRPCCode(lists.Invalid)
	}
	value, err := lists.NewListRevision(id, int64(raw.Number))
	if err != nil {
		return lists.ListRevision{}, listRPCCode(lists.Invalid)
	}
	return value, nil
}
func expectedItemRevision(raw *listsv1.ItemRevision, id domain.ListID, uid lists.ItemUID) (lists.ItemRevision, error) {
	if raw == nil || raw.Number > math.MaxInt64 {
		return lists.ItemRevision{}, listRPCCode(lists.Invalid)
	}
	bound, err := listID(raw.ListId)
	if err != nil || bound != id {
		return lists.ItemRevision{}, listRPCCode(lists.Invalid)
	}
	item, err := itemUID(raw.ItemUid)
	if err != nil || item != uid {
		return lists.ItemRevision{}, listRPCCode(lists.Invalid)
	}
	value, err := lists.NewItemRevision(id, uid, int64(raw.Number))
	if err != nil {
		return lists.ItemRevision{}, listRPCCode(lists.Invalid)
	}
	return value, nil
}
func itemPayload(raw *listsv1.ItemPayload) (lists.ItemPayload, error) {
	// A future oneof alternative is retained as unknown protobuf fields, not
	// represented by Due. Admit the closed command before treating nil as NoDue.
	if raw == nil || raw.Completed == nil || len(raw.ProtoReflect().GetUnknown()) != 0 {
		return lists.ItemPayload{}, listRPCCode(lists.Invalid)
	}
	input := lists.ItemInput{Summary: raw.Summary, Completed: *raw.Completed, Description: raw.Description, Due: lists.NoDue()}
	switch due := raw.Due.(type) {
	case nil:
	case *listsv1.ItemPayload_DueDate:
		if due == nil {
			return lists.ItemPayload{}, listRPCCode(lists.Invalid)
		}
		value, err := lists.NewDueDate(due.DueDate)
		if err != nil {
			return lists.ItemPayload{}, listRPCCode(lists.Invalid)
		}
		input.Due = value
	case *listsv1.ItemPayload_DueInstant:
		if due == nil || due.DueInstant == nil {
			return lists.ItemPayload{}, listRPCCode(lists.Invalid)
		}
		instant := due.DueInstant
		if len(instant.ProtoReflect().GetUnknown()) != 0 || instant.At == nil ||
			len(instant.At.ProtoReflect().GetUnknown()) != 0 || instant.At.CheckValid() != nil {
			return lists.ItemPayload{}, listRPCCode(lists.Invalid)
		}
		value, err := lists.NewDueInstant(instant.At.AsTime(), instant.Zone)
		if err != nil {
			return lists.ItemPayload{}, listRPCCode(lists.Invalid)
		}
		input.Due = value
	default:
		return lists.ItemPayload{}, listRPCCode(lists.Invalid)
	}
	if raw.CompletedAt != nil {
		if len(raw.CompletedAt.ProtoReflect().GetUnknown()) != 0 || raw.CompletedAt.CheckValid() != nil {
			return lists.ItemPayload{}, listRPCCode(lists.Invalid)
		}
		completed := raw.CompletedAt.AsTime()
		input.CompletedAt = &completed
	}
	value, err := lists.NewItemPayload(input)
	if err != nil {
		return lists.ItemPayload{}, listRPCCode(lists.Invalid)
	}
	return value, nil
}

func wireListRevision(value lists.ListRevision) (*listsv1.ListRevision, error) {
	id, number, err := value.Values()
	if err != nil || number < 1 {
		return nil, listRPCCode(lists.Unavailable)
	}
	text, err := id.Value()
	if err != nil {
		return nil, listRPCCode(lists.Unavailable)
	}
	return &listsv1.ListRevision{ListId: &listsv1.ListId{Value: text}, Number: uint64(number)}, nil
}
func wireItemRevision(value lists.ItemRevision) (*listsv1.ItemRevision, error) {
	id, uid, number, err := value.Values()
	if err != nil || number < 1 {
		return nil, listRPCCode(lists.Unavailable)
	}
	listText, listErr := id.Value()
	itemText, itemErr := uid.Value()
	if listErr != nil || itemErr != nil {
		return nil, listRPCCode(lists.Unavailable)
	}
	return &listsv1.ItemRevision{ListId: &listsv1.ListId{Value: listText}, ItemUid: &listsv1.ItemUid{Value: itemText}, Number: uint64(number)}, nil
}
func wireTime(value time.Time) (*timestamppb.Timestamp, error) {
	stamp := timestamppb.New(value.UTC())
	if err := stamp.CheckValid(); err != nil {
		return nil, listRPCCode(lists.Unavailable)
	}
	return stamp, nil
}
func wireList(value lists.ListSnapshot) (*listsv1.ListSnapshot, error) {
	id, err := value.ID().Value()
	if err != nil {
		return nil, listRPCCode(lists.Unavailable)
	}
	var origin listsv1.ListOrigin
	switch value.Origin() {
	case lists.LocalTodo:
		origin = listsv1.ListOrigin_LIST_ORIGIN_LOCAL_TODO
	case lists.ShoppingList:
		origin = listsv1.ListOrigin_LIST_ORIGIN_SHOPPING_LIST
	case lists.Native:
		origin = listsv1.ListOrigin_LIST_ORIGIN_NATIVE
	default:
		return nil, listRPCCode(lists.Unavailable)
	}
	revision, err := wireListRevision(value.Revision())
	if err != nil || revision.ListId.Value != id {
		return nil, listRPCCode(lists.Unavailable)
	}
	updated, err := wireTime(value.UpdatedAt())
	if err != nil {
		return nil, err
	}
	out := &listsv1.ListSnapshot{Id: &listsv1.ListId{Value: id}, Origin: origin, Name: value.Name(), Revision: revision, UpdatedAt: updated}
	if alias, ok := value.Alias(); ok {
		text, err := alias.Value()
		if err != nil {
			return nil, listRPCCode(lists.Unavailable)
		}
		out.LegacyAlias = &text
	}
	return out, nil
}
func wireItem(value lists.ItemSnapshot) (*listsv1.ItemSnapshot, error) {
	listText, listErr := value.ListID().Value()
	uidText, uidErr := value.UID().Value()
	if listErr != nil || uidErr != nil || value.Position() < 0 {
		return nil, listRPCCode(lists.Unavailable)
	}
	revision, err := wireItemRevision(value.Revision())
	if err != nil || revision.ListId.Value != listText || revision.ItemUid.Value != uidText {
		return nil, listRPCCode(lists.Unavailable)
	}
	completed := value.Completed()
	payload := &listsv1.ItemPayload{Summary: value.Summary(), Completed: &completed}
	if description, present := value.Description(); present {
		payload.Description = &description
	}
	date, instant, zone, err := value.Due().Values()
	if err != nil {
		return nil, listRPCCode(lists.Unavailable)
	}
	if date != "" {
		payload.Due = &listsv1.ItemPayload_DueDate{DueDate: date}
	} else if !instant.IsZero() {
		stamp, err := wireTime(instant)
		if err != nil {
			return nil, err
		}
		payload.Due = &listsv1.ItemPayload_DueInstant{DueInstant: &listsv1.DueInstant{At: stamp, Zone: zone}}
	}
	if at, present := value.CompletedAt(); present {
		stamp, err := wireTime(at)
		if err != nil {
			return nil, err
		}
		payload.CompletedAt = stamp
	}
	return &listsv1.ItemSnapshot{ListId: &listsv1.ListId{Value: listText}, Uid: &listsv1.ItemUid{Value: uidText}, Value: payload, Position: value.Position(), Revision: revision}, nil
}
func wireReceipt(value lists.MutationReceipt) (*listsv1.MutationReceipt, error) {
	if value.Outcome() != lists.Committed {
		return nil, listRPCCode(lists.Unavailable)
	}
	listText, err := value.ListID().Value()
	if err != nil {
		return nil, listRPCCode(lists.Unavailable)
	}
	listRevision, ok := value.ListRevision()
	if !ok {
		return nil, listRPCCode(lists.Unavailable)
	}
	wireRevision, err := wireListRevision(listRevision)
	if err != nil || wireRevision.ListId.Value != listText {
		return nil, listRPCCode(lists.Unavailable)
	}
	out := &listsv1.MutationReceipt{ListId: &listsv1.ListId{Value: listText}, ListRevision: wireRevision}
	if uid, present := value.ItemUID(); present {
		itemText, err := uid.Value()
		if err != nil {
			return nil, listRPCCode(lists.Unavailable)
		}
		itemRevision, ok := value.ItemRevision()
		if !ok {
			return nil, listRPCCode(lists.Unavailable)
		}
		wireItem, err := wireItemRevision(itemRevision)
		if err != nil || wireItem.ListId.Value != listText || wireItem.ItemUid.Value != itemText {
			return nil, listRPCCode(lists.Unavailable)
		}
		out.ItemUid, out.ItemRevision = &listsv1.ItemUid{Value: itemText}, wireItem
	} else if _, present := value.ItemRevision(); present {
		return nil, listRPCCode(lists.Unavailable)
	}
	return out, nil
}

func (h *listHandler) ListLists(ctx context.Context, request *connect.Request[listsv1.ListListsRequest]) (*connect.Response[listsv1.ListListsResponse], error) {
	if err := h.admit(ctx); err != nil {
		return nil, err
	}
	if request == nil || request.Msg == nil || request.Msg.PageSize < 1 || request.Msg.PageSize > lists.MaxPage {
		return nil, listRPCCode(lists.Invalid)
	}
	cursor, err := h.cursors.DecodeList(request.Msg.PageToken)
	if err != nil {
		return nil, listRPCError(err)
	}
	page, err := h.service.ListLists(ctx, cursor, int(request.Msg.PageSize))
	if err != nil {
		return nil, listRPCError(err)
	}
	out := &listsv1.ListListsResponse{Lists: make([]*listsv1.ListSnapshot, 0, len(page.Entries()))}
	for _, entry := range page.Entries() {
		encoded, err := wireList(entry)
		if err != nil {
			return nil, err
		}
		out.Lists = append(out.Lists, encoded)
	}
	if next, ok := page.Next(); ok {
		out.NextPageToken, err = h.cursors.EncodeList(next)
		if err != nil {
			return nil, listRPCError(err)
		}
	}
	return connect.NewResponse(out), nil
}
func (h *listHandler) CountLists(ctx context.Context, request *connect.Request[listsv1.CountListsRequest]) (*connect.Response[listsv1.CountListsResponse], error) {
	if err := h.admit(ctx); err != nil {
		return nil, err
	}
	if request == nil || request.Msg == nil {
		return nil, listRPCCode(lists.Invalid)
	}
	count, err := h.service.CountLists(ctx)
	if err != nil {
		return nil, listRPCError(err)
	}
	return connect.NewResponse(&listsv1.CountListsResponse{Count: count}), nil
}
func (h *listHandler) GetList(ctx context.Context, request *connect.Request[listsv1.GetListRequest]) (*connect.Response[listsv1.GetListResponse], error) {
	if err := h.admit(ctx); err != nil {
		return nil, err
	}
	if request == nil || request.Msg == nil {
		return nil, listRPCCode(lists.Invalid)
	}
	id, err := listID(request.Msg.ListId)
	if err != nil {
		return nil, err
	}
	value, err := h.service.GetList(ctx, id)
	if err != nil {
		return nil, listRPCError(err)
	}
	encoded, err := wireList(value)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&listsv1.GetListResponse{List: encoded}), nil
}
func (h *listHandler) CreateList(ctx context.Context, request *connect.Request[listsv1.CreateListRequest]) (*connect.Response[listsv1.CreateListResponse], error) {
	if err := h.admit(ctx); err != nil {
		return nil, err
	}
	if request == nil || request.Msg == nil {
		return nil, listRPCCode(lists.Invalid)
	}
	payload, err := lists.NewListPayload(request.Msg.Name)
	if err != nil {
		return nil, listRPCCode(lists.Invalid)
	}
	receipt, err := h.service.CreateList(ctx, payload)
	if err != nil {
		return nil, listRPCError(err)
	}
	encoded, err := wireReceipt(receipt)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&listsv1.CreateListResponse{Receipt: encoded}), nil
}
func (h *listHandler) RenameList(ctx context.Context, request *connect.Request[listsv1.RenameListRequest]) (*connect.Response[listsv1.RenameListResponse], error) {
	if err := h.admit(ctx); err != nil {
		return nil, err
	}
	if request == nil || request.Msg == nil {
		return nil, listRPCCode(lists.Invalid)
	}
	id, err := listID(request.Msg.ListId)
	if err != nil {
		return nil, err
	}
	expected, err := expectedListRevision(request.Msg.ExpectedRevision, id)
	if err != nil {
		return nil, err
	}
	receipt, err := h.service.RenameList(ctx, id, expected, request.Msg.Name)
	if err != nil {
		return nil, listRPCError(err)
	}
	encoded, err := wireReceipt(receipt)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&listsv1.RenameListResponse{Receipt: encoded}), nil
}
func (h *listHandler) DeleteList(ctx context.Context, request *connect.Request[listsv1.DeleteListRequest]) (*connect.Response[listsv1.DeleteListResponse], error) {
	if err := h.admit(ctx); err != nil {
		return nil, err
	}
	if request == nil || request.Msg == nil {
		return nil, listRPCCode(lists.Invalid)
	}
	id, err := listID(request.Msg.ListId)
	if err != nil {
		return nil, err
	}
	expected, err := expectedListRevision(request.Msg.ExpectedRevision, id)
	if err != nil {
		return nil, err
	}
	receipt, err := h.service.DeleteList(ctx, id, expected)
	if err != nil {
		return nil, listRPCError(err)
	}
	encoded, err := wireReceipt(receipt)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&listsv1.DeleteListResponse{Receipt: encoded}), nil
}

func (h *listHandler) ListItems(ctx context.Context, request *connect.Request[listsv1.ListItemsRequest]) (*connect.Response[listsv1.ListItemsResponse], error) {
	if err := h.admit(ctx); err != nil {
		return nil, err
	}
	if request == nil || request.Msg == nil || request.Msg.PageSize < 1 || request.Msg.PageSize > lists.MaxPage {
		return nil, listRPCCode(lists.Invalid)
	}
	id, err := listID(request.Msg.ListId)
	if err != nil {
		return nil, err
	}
	cursor, err := h.cursors.DecodeItem(request.Msg.PageToken)
	if err != nil {
		return nil, listRPCError(err)
	}
	page, err := h.service.ListItems(ctx, id, cursor, int(request.Msg.PageSize))
	if err != nil {
		return nil, listRPCError(err)
	}
	out := &listsv1.ListItemsResponse{Items: make([]*listsv1.ItemSnapshot, 0, len(page.Entries()))}
	for _, entry := range page.Entries() {
		encoded, err := wireItem(entry)
		if err != nil {
			return nil, err
		}
		out.Items = append(out.Items, encoded)
	}
	if next, ok := page.Next(); ok {
		out.NextPageToken, err = h.cursors.EncodeItem(next)
		if err != nil {
			return nil, listRPCError(err)
		}
	}
	return connect.NewResponse(out), nil
}
func (h *listHandler) CountItems(ctx context.Context, request *connect.Request[listsv1.CountItemsRequest]) (*connect.Response[listsv1.CountItemsResponse], error) {
	if err := h.admit(ctx); err != nil {
		return nil, err
	}
	if request == nil || request.Msg == nil {
		return nil, listRPCCode(lists.Invalid)
	}
	id, err := listID(request.Msg.ListId)
	if err != nil {
		return nil, err
	}
	count, err := h.service.CountItems(ctx, id)
	if err != nil {
		return nil, listRPCError(err)
	}
	return connect.NewResponse(&listsv1.CountItemsResponse{Count: count}), nil
}
func (h *listHandler) GetItem(ctx context.Context, request *connect.Request[listsv1.GetItemRequest]) (*connect.Response[listsv1.GetItemResponse], error) {
	if err := h.admit(ctx); err != nil {
		return nil, err
	}
	if request == nil || request.Msg == nil {
		return nil, listRPCCode(lists.Invalid)
	}
	id, err := listID(request.Msg.ListId)
	if err != nil {
		return nil, err
	}
	uid, err := itemUID(request.Msg.ItemUid)
	if err != nil {
		return nil, err
	}
	value, err := h.service.GetItem(ctx, id, uid)
	if err != nil {
		return nil, listRPCError(err)
	}
	encoded, err := wireItem(value)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&listsv1.GetItemResponse{Item: encoded}), nil
}

// withListTarget checks object authority before constructing admitted values.
// The callback performs representation admission only. Repository operations
// obtain a fresh permit/CAS fence and retain their existing commit semantics.
func withListTarget[T any](h *listHandler, ctx context.Context, raw *listsv1.ListId, op authv1.ScopeOperation,
	fn func(domain.ListID) (T, error)) (T, error) {
	var zero T
	var id domain.ListID
	var err error
	if raw != nil {
		id, err = domain.ParseListID(raw.Value)
	} else {
		err = errors.New("missing identity")
	}
	var admission *lists.Admission
	if err != nil {
		admission, err = h.service.AdmitCollection(ctx, op)
		if err != nil {
			return zero, listRPCError(err)
		}
		defer admission.Release()
		if !admission.Ready() {
			return zero, listRPCCode(lists.Denied)
		}
		return zero, listRPCCode(lists.Invalid)
	}
	admission, err = h.service.AdmitObject(ctx, id, op)
	if err != nil {
		return zero, listRPCError(err)
	}
	defer admission.Release()
	value, err := fn(id)
	if !admission.Ready() {
		return zero, listRPCCode(lists.Denied)
	}
	return value, err
}

type listAddition struct {
	id       domain.ListID
	expected lists.ListRevision
	payload  lists.ItemPayload
}

func (h *listHandler) AddItem(ctx context.Context, request *connect.Request[listsv1.AddItemRequest]) (*connect.Response[listsv1.AddItemResponse], error) {
	if err := h.admit(ctx); err != nil {
		return nil, err
	}
	var target *listsv1.ListId
	if request != nil && request.Msg != nil {
		target = request.Msg.ListId
	}
	admitted, err := withListTarget(h, ctx, target, authv1.ScopeOperation_SCOPE_OPERATION_WRITE,
		func(id domain.ListID) (listAddition, error) {
			return parseListAddition(request.Msg, id)
		})
	if err != nil {
		return nil, err
	}
	receipt, err := h.service.AddItem(ctx, admitted.id, admitted.expected, admitted.payload)
	if err != nil {
		return nil, listRPCError(err)
	}
	encoded, err := wireReceipt(receipt)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&listsv1.AddItemResponse{Receipt: encoded}), nil
}
func (h *listHandler) EditItem(ctx context.Context, request *connect.Request[listsv1.EditItemRequest]) (*connect.Response[listsv1.EditItemResponse], error) {
	if err := h.admit(ctx); err != nil {
		return nil, err
	}
	if request == nil || request.Msg == nil {
		return nil, listRPCCode(lists.Invalid)
	}
	id, err := listID(request.Msg.ListId)
	if err != nil {
		return nil, err
	}
	uid, err := itemUID(request.Msg.ItemUid)
	if err != nil {
		return nil, err
	}
	expectedList, err := expectedListRevision(request.Msg.ExpectedListRevision, id)
	if err != nil {
		return nil, err
	}
	expectedItem, err := expectedItemRevision(request.Msg.ExpectedItemRevision, id, uid)
	if err != nil {
		return nil, err
	}
	payload, err := itemPayload(request.Msg.Item)
	if err != nil {
		return nil, err
	}
	receipt, err := h.service.EditItem(ctx, id, uid, expectedList, expectedItem, payload)
	if err != nil {
		return nil, listRPCError(err)
	}
	encoded, err := wireReceipt(receipt)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&listsv1.EditItemResponse{Receipt: encoded}), nil
}
func (h *listHandler) DeleteItem(ctx context.Context, request *connect.Request[listsv1.DeleteItemRequest]) (*connect.Response[listsv1.DeleteItemResponse], error) {
	if err := h.admit(ctx); err != nil {
		return nil, err
	}
	if request == nil || request.Msg == nil {
		return nil, listRPCCode(lists.Invalid)
	}
	id, err := listID(request.Msg.ListId)
	if err != nil {
		return nil, err
	}
	uid, err := itemUID(request.Msg.ItemUid)
	if err != nil {
		return nil, err
	}
	expectedList, err := expectedListRevision(request.Msg.ExpectedListRevision, id)
	if err != nil {
		return nil, err
	}
	expectedItem, err := expectedItemRevision(request.Msg.ExpectedItemRevision, id, uid)
	if err != nil {
		return nil, err
	}
	receipt, err := h.service.DeleteItem(ctx, id, uid, expectedList, expectedItem)
	if err != nil {
		return nil, listRPCError(err)
	}
	encoded, err := wireReceipt(receipt)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&listsv1.DeleteItemResponse{Receipt: encoded}), nil
}
func (h *listHandler) SetCompleted(ctx context.Context, request *connect.Request[listsv1.SetCompletedRequest]) (*connect.Response[listsv1.SetCompletedResponse], error) {
	if err := h.admit(ctx); err != nil {
		return nil, err
	}
	if request == nil || request.Msg == nil || request.Msg.Completed == nil {
		return nil, listRPCCode(lists.Invalid)
	}
	id, err := listID(request.Msg.ListId)
	if err != nil {
		return nil, err
	}
	uid, err := itemUID(request.Msg.ItemUid)
	if err != nil {
		return nil, err
	}
	expectedList, err := expectedListRevision(request.Msg.ExpectedListRevision, id)
	if err != nil {
		return nil, err
	}
	expectedItem, err := expectedItemRevision(request.Msg.ExpectedItemRevision, id, uid)
	if err != nil {
		return nil, err
	}
	receipt, err := h.service.SetCompleted(ctx, id, uid, expectedList, expectedItem, *request.Msg.Completed)
	if err != nil {
		return nil, listRPCError(err)
	}
	encoded, err := wireReceipt(receipt)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&listsv1.SetCompletedResponse{Receipt: encoded}), nil
}
func (h *listHandler) MoveItem(ctx context.Context, request *connect.Request[listsv1.MoveItemRequest]) (*connect.Response[listsv1.MoveItemResponse], error) {
	if err := h.admit(ctx); err != nil {
		return nil, err
	}
	if request == nil || request.Msg == nil {
		return nil, listRPCCode(lists.Invalid)
	}
	id, err := listID(request.Msg.ListId)
	if err != nil {
		return nil, err
	}
	uid, err := itemUID(request.Msg.ItemUid)
	if err != nil {
		return nil, err
	}
	expectedList, err := expectedListRevision(request.Msg.ExpectedListRevision, id)
	if err != nil {
		return nil, err
	}
	expectedItem, err := expectedItemRevision(request.Msg.ExpectedItemRevision, id, uid)
	if err != nil {
		return nil, err
	}
	var after *lists.ItemUID
	switch destination := request.Msg.Destination.(type) {
	case *listsv1.MoveItemRequest_AtStart:
		if destination == nil || destination.AtStart == nil {
			return nil, listRPCCode(lists.Invalid)
		}
	case *listsv1.MoveItemRequest_AfterUid:
		if destination == nil {
			return nil, listRPCCode(lists.Invalid)
		}
		value, err := itemUID(destination.AfterUid)
		if err != nil {
			return nil, err
		}
		after = &value
	default:
		return nil, listRPCCode(lists.Invalid)
	}
	receipt, err := h.service.MoveItem(ctx, id, uid, after, expectedList, expectedItem)
	if err != nil {
		return nil, listRPCError(err)
	}
	encoded, err := wireReceipt(receipt)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&listsv1.MoveItemResponse{Receipt: encoded}), nil
}
func (h *listHandler) WatchLists(ctx context.Context, request *connect.Request[listsv1.WatchListsRequest], stream *connect.ServerStream[listsv1.WatchListsResponse]) error {
	if err := h.admit(ctx); err != nil {
		return err
	}
	if request == nil || request.Msg == nil || stream == nil {
		return listRPCCode(lists.Invalid)
	}
	var sub *lists.Subscription
	var err error
	switch scope := request.Msg.Scope.(type) {
	case *listsv1.WatchListsRequest_Catalog:
		if scope == nil || scope.Catalog == nil {
			return listRPCCode(lists.Invalid)
		}
		sub, err = h.service.SubscribeCatalog(ctx)
	case *listsv1.WatchListsRequest_ListId:
		if scope == nil {
			return listRPCCode(lists.Invalid)
		}
		id, parseErr := listID(scope.ListId)
		if parseErr != nil {
			return parseErr
		}
		sub, err = h.service.SubscribeList(ctx, id)
	default:
		return listRPCCode(lists.Invalid)
	}
	if err != nil {
		return listRPCError(err)
	}
	defer sub.Close()
	if err := stream.Send(&listsv1.WatchListsResponse{StreamSequence: 1, Frame: &listsv1.WatchListsResponse_Ready{Ready: &listsv1.WatchReady{}}}); err != nil {
		return err
	}
	sequence := uint64(1)
	for {
		change, err := sub.Next(ctx)
		if lists.IsCode(err, lists.ResyncRequired) {
			if sequence == math.MaxUint64 {
				return listRPCCode(lists.Capacity)
			}
			return stream.Send(&listsv1.WatchListsResponse{StreamSequence: sequence + 1, Frame: &listsv1.WatchListsResponse_ResyncRequired{ResyncRequired: &listsv1.ListResyncRequired{}}})
		}
		if err != nil {
			return listRPCError(err)
		}
		if sequence == math.MaxUint64 {
			return listRPCCode(lists.Capacity)
		}
		id, idErr := change.ListID().Value()
		revision, revisionErr := wireListRevision(change.Revision())
		if idErr != nil || revisionErr != nil || revision.ListId.Value != id {
			return listRPCCode(lists.Unavailable)
		}
		sequence++
		if err := stream.Send(&listsv1.WatchListsResponse{StreamSequence: sequence, Frame: &listsv1.WatchListsResponse_Changed{Changed: &listsv1.ListChanged{ListId: &listsv1.ListId{Value: id}, Revision: revision}}}); err != nil {
			return err
		}
	}
}

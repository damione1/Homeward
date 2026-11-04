package server

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/entityfilter"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/view"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type homeResourceOwner interface {
	HomeName(context.Context) (apitypes.HomeName, error)
	HomeMetadata(context.Context) (apitypes.HomeMetadata, error)
	ResolveEntityName(context.Context, apitypes.EntityName) (domain.EntityID, error)
	UpdateEntityMetadata(context.Context, authorization.MetadataEvidence, domain.EntityID, apitypes.EntityUpdate, func(apitypes.EntityMetadata) ([]byte, error)) (apitypes.EntityMetadata, error)
}

// This adapter owns no device, registry, observed state or snapshot store.
// Its cipher authenticates a bounded continuation into the existing view owner.
type homeResourceHandler struct {
	apiv2connect.UnimplementedHomeServiceHandler
	owner     homeResourceOwner
	authority *authorization.Authority
	views     *view.Service
	pages     cipher.AEAD
}

func newHomeResourceHandler(owner homeResourceOwner, authority *authorization.Authority, views *view.Service) (*homeResourceHandler, error) {
	if nilReactiveInterface(owner) || authority == nil || views == nil {
		return nil, ErrInvalidPublicPlan
	}
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	pages, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &homeResourceHandler{owner: owner, authority: authority, views: views, pages: pages}, nil
}

func homeValidation(message proto.Message, field string, rule api.ValidationReason) error {
	issue, err := apierrors.NewViolation(message.ProtoReflect().Descriptor(), field, rule)
	if err != nil {
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
	}
	return apierrors.Validation(issue)
}
func homePageSize(message proto.Message, size int32) (int, error) {
	if size < 0 {
		return 0, homeValidation(message, "page_size", api.ValidationReason_VALIDATION_REASON_OUT_OF_RANGE)
	}
	if size == 0 {
		return 100, nil
	}
	if size > 500 {
		return 500, nil
	}
	return int(size), nil
}
func (h *homeResourceHandler) current(ctx context.Context, permission *authorization.Permission) (authorization.Principal, authorization.Current, error) {
	if h == nil || h.authority == nil {
		return authorization.Principal{}, authorization.Current{}, authorization.ErrUnavailable
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return principal, authorization.Current{}, authorization.ErrDenied
	}
	current, err := h.authority.Current(principal)
	if err == nil && permission != nil {
		err = current.AdmitRoute(*permission)
	}
	return principal, current, err
}
func homeReadPermission() authorization.Permission {
	permission, _ := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY, auth.ScopeOperation_SCOPE_OPERATION_READ)
	return permission
}
func homeAdminPermission() authorization.Permission {
	permission, _ := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER)
	return permission
}

func (h *homeResourceHandler) GetHome(ctx context.Context, request *connect.Request[api.GetHomeRequest]) (*connect.Response[api.Home], error) {
	ctx, finishResponse, responseErr := h.beginReadResponse(ctx)
	if responseErr != nil {
		return nil, apiRPCError(ctx, responseErr)
	}
	defer finishResponse()
	principal, _, err := h.current(ctx, nil)
	if err != nil || !principal.Native() {
		return nil, apiRPCError(ctx, authorization.ErrDenied)
	}
	if request == nil || request.Msg == nil {
		return nil, apiRPCError(ctx, homeValidation(&api.GetHomeRequest{}, "name", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	name, err := apitypes.ParseHomeName(request.Msg.Name)
	if err != nil {
		return nil, apiRPCError(ctx, homeValidation(request.Msg, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME))
	}
	metadata, err := h.owner.HomeMetadata(ctx)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if name != metadata.Name() {
		return nil, apiRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND))
	}
	if _, _, err = h.current(ctx, nil); err != nil {
		return nil, apiRPCError(ctx, err)
	}
	return connect.NewResponse(&api.Home{Name: name.String(), CreateTime: timestamppb.New(metadata.CreateTime())}), nil
}

func (h *homeResourceHandler) ListHomes(ctx context.Context, request *connect.Request[api.ListHomesRequest]) (*connect.Response[api.ListHomesResponse], error) {
	ctx, finishResponse, responseErr := h.beginReadResponse(ctx)
	if responseErr != nil {
		return nil, apiRPCError(ctx, responseErr)
	}
	defer finishResponse()
	principal, _, err := h.current(ctx, nil)
	if err != nil || !principal.Native() {
		return nil, apiRPCError(ctx, authorization.ErrDenied)
	}
	if request == nil || request.Msg == nil {
		return nil, apiRPCError(ctx, homeValidation(&api.ListHomesRequest{}, "", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	if _, err := homePageSize(request.Msg, request.Msg.PageSize); err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if request.Msg.PageToken != "" {
		return nil, apiRPCError(ctx, homeValidation(request.Msg, "page_token", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT))
	}
	metadata, err := h.owner.HomeMetadata(ctx)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if _, _, err = h.current(ctx, nil); err != nil {
		return nil, apiRPCError(ctx, err)
	}
	return connect.NewResponse(&api.ListHomesResponse{Homes: []*api.Home{{Name: metadata.Name().String(), CreateTime: timestamppb.New(metadata.CreateTime())}}}), nil
}

func encodeEntity(home apitypes.HomeName, metadata apitypes.EntityMetadata) (*api.Entity, error) {
	if !metadata.Valid() {
		return nil, registry.ErrStorage
	}
	name, err := apitypes.EntityNameForIdentity(home, metadata.Entity())
	if err != nil {
		return nil, err
	}
	out := &api.Entity{Name: name.String(), DisplayName: metadata.Label().String(), Etag: metadata.ETag().String()}
	if created, present := metadata.CreateTime(); present {
		out.CreateTime = timestamppb.New(created)
	}
	if updated, present := metadata.UpdateTime(); present {
		out.UpdateTime = timestamppb.New(updated)
	}
	return out, nil
}

// Unknown and unreadable names have the same denied result unless the current
// caller independently holds installation administration. Lookup is no grant.
func (h *homeResourceHandler) resolve(ctx context.Context, name apitypes.EntityName, current authorization.Current) (domain.EntityID, error) {
	id, err := h.owner.ResolveEntityName(ctx, name)
	if errors.Is(err, registry.ErrUnconfigured) {
		permission := homeAdminPermission()
		object, _ := authorization.AdministrativeObject(auth.ScopeFamily_SCOPE_FAMILY_GLOBAL_CONFIGURATION)
		if current.Authorize(permission, object) == nil {
			return domain.EntityID{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND)
		}
		return domain.EntityID{}, authorization.ErrDenied
	}
	return id, err
}
func (h *homeResourceHandler) entityPage(ctx context.Context, name apitypes.EntityName, current authorization.Current) (view.Page, error) {
	id, err := h.resolve(ctx, name, current)
	if err != nil {
		return view.Page{}, err
	}
	actor, err := coreRPCActor(ctx)
	if err != nil {
		return view.Page{}, err
	}
	filter, err := view.Only([]domain.EntityID{id})
	if err != nil {
		return view.Page{}, err
	}
	page, err := h.views.OpenView(ctx, actor, filter, 1)
	if err == nil && len(page.Items) != 1 {
		_ = h.views.ReleaseView(context.WithoutCancel(ctx), actor, page.SnapshotToken)
		return view.Page{}, authorization.ErrDenied
	}
	return page, err
}

func (h *homeResourceHandler) GetEntity(ctx context.Context, request *connect.Request[api.GetEntityRequest]) (*connect.Response[api.Entity], error) {
	ctx, finishResponse, responseErr := h.beginReadResponse(ctx)
	if responseErr != nil {
		return nil, apiRPCError(ctx, responseErr)
	}
	defer finishResponse()
	permission := homeReadPermission()
	_, current, err := h.current(ctx, &permission)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, apiRPCError(ctx, homeValidation(&api.GetEntityRequest{}, "name", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	name, err := apitypes.ParseEntityName(request.Msg.Name)
	if err != nil {
		return nil, apiRPCError(ctx, homeValidation(request.Msg, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME))
	}
	page, err := h.entityPage(ctx, name, current)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	actor, _ := coreRPCActor(ctx)
	defer releaseReadResponse(ctx, func() { _ = h.views.ReleaseView(context.WithoutCancel(ctx), actor, page.SnapshotToken) })
	out, err := encodeEntity(name.Home(), page.Items[0].Registry().Metadata())
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if err := h.retainReadCapture(ctx, page.SnapshotToken); err != nil {
		return nil, apiRPCError(ctx, err)
	}
	return connect.NewResponse(out), nil
}

type homePageCursor struct {
	Snapshot string `json:"s"`
	Page     string `json:"p"`
	Offset   int    `json:"o,omitempty"`
}

func homePageBinding(current authorization.Current, parent, filter, method string) ([]byte, error) {
	_, binding, err := current.Binding()
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal([]string{parent, filter, method})
	digest := sha256.Sum256(encoded)
	return append(binding[:], digest[:]...), nil
}
func (h *homeResourceHandler) sealPage(cursor homePageCursor, binding []byte) (string, error) {
	plain, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, h.pages.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(h.pages.Seal(nonce, nonce, plain, binding)), nil
}
func (h *homeResourceHandler) openPage(token string, binding []byte) (homePageCursor, error) {
	var cursor homePageCursor
	if len(token) > 2048 {
		return cursor, view.ErrPageToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) < h.pages.NonceSize()+h.pages.Overhead() {
		return cursor, view.ErrPageToken
	}
	plain, err := h.pages.Open(nil, raw[:h.pages.NonceSize()], raw[h.pages.NonceSize():], binding)
	if err != nil || json.Unmarshal(plain, &cursor) != nil || !corePageToken(cursor.Snapshot) || !(corePageToken(cursor.Page) && cursor.Offset == 0 || cursor.Page == "" && cursor.Offset > 0 && cursor.Offset <= view.MaxSnapshotBytes/512) {
		return homePageCursor{}, view.ErrPageToken
	}
	return cursor, nil
}

func (h *homeResourceHandler) ListEntities(ctx context.Context, request *connect.Request[api.ListEntitiesRequest]) (*connect.Response[api.ListEntitiesResponse], error) {
	ctx, finishResponse, responseErr := h.beginReadResponse(ctx)
	if responseErr != nil {
		return nil, apiRPCError(ctx, responseErr)
	}
	defer finishResponse()
	permission := homeReadPermission()
	_, current, err := h.current(ctx, &permission)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, apiRPCError(ctx, homeValidation(&api.ListEntitiesRequest{}, "parent", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	parent, err := apitypes.ParseHomeName(r.Parent)
	if err != nil {
		return nil, apiRPCError(ctx, homeValidation(r, "parent", api.ValidationReason_VALIDATION_REASON_INVALID_NAME))
	}
	size, err := homePageSize(r, r.PageSize)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	selected, err := entityfilter.Parse(r.Filter)
	if err != nil {
		return nil, apiRPCError(ctx, homeValidation(r, "filter", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT))
	}
	home, err := h.owner.HomeName(ctx)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if home != parent {
		return nil, apiRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND))
	}
	digest, err := selected.Digest()
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	binding, err := homePageBinding(current, parent.String(), base64.RawURLEncoding.EncodeToString(digest[:]), "ListEntities")
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	actor, err := coreRPCActor(ctx)
	if err != nil {
		return nil, err
	}
	var page view.Page
	if r.PageToken != "" {
		cursor, err := h.openPage(r.PageToken, binding)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		page, err = h.views.ReadPageSize(ctx, actor, cursor.Snapshot, cursor.Page, size)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
	} else {
		page, err = h.views.OpenEntityView(ctx, actor, parent, selected, size)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
	}
	retain := r.PageToken != ""
	defer func() {
		if !retain {
			releaseReadResponse(ctx, func() { _ = h.views.ReleaseView(context.WithoutCancel(ctx), actor, page.SnapshotToken) })
		}
	}()
	out := &api.ListEntitiesResponse{}
	for _, item := range page.Items {
		entity, err := encodeEntity(home, item.Registry().Metadata())
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
		out.Entities = append(out.Entities, entity)
	}
	if page.NextPageToken != "" {
		out.NextPageToken, err = h.sealPage(homePageCursor{Snapshot: page.SnapshotToken, Page: page.NextPageToken}, binding)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
	}
	retain = retain || out.NextPageToken != ""
	if err := h.retainReadCapture(ctx, page.SnapshotToken); err != nil {
		return nil, apiRPCError(ctx, err)
	}
	return connect.NewResponse(out), nil
}

package server

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/integrations"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// List envelopes reserve room for a maximal continuation. Each admitted
// individual Operation is at most1MiB, so every valid item fits alone.
const maxOperationListBytes = 2 << 20
const operationPageOverhead = 4096 + 32

// A creation cutoff is a stable query bound, not an exported database snapshot:
// a transaction started before it can become visible after committing. Owners
// retain all authorization and execution; these ports only return identities.
type operationNamesProvider interface {
	ListOperationNames(context.Context, apitypes.HomeName, *apitypes.OperationName, uint32, time.Time) ([]apitypes.OperationName, *apitypes.OperationName, error)
	FindOperationsByRequest(context.Context, apitypes.HomeName, apitypes.RequestID, time.Time) ([]apitypes.OperationName, error)
}

func newOperationRPCHandler(physical, configuration operationReadProvider, authority *authorization.Authority) (*operationRPCHandler, error) {
	if authority == nil || nilReactiveInterface(physical) && nilReactiveInterface(configuration) {
		return nil, ErrInvalidPublicPlan
	}
	for _, provider := range []operationReadProvider{physical, configuration} {
		if !nilReactiveInterface(provider) {
			if _, ok := provider.(operationNamesProvider); !ok {
				return nil, ErrInvalidPublicPlan
			}
		}
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
	return &operationRPCHandler{physical: physical, configuration: configuration, authority: authority, pages: pages}, nil
}

type operationPage struct {
	Stage   int       `json:"s"`
	After   string    `json:"a,omitempty"`
	Offset  int       `json:"o,omitempty"`
	Cutoff  time.Time `json:"c"`
	Expires time.Time `json:"e"`
}

func invalidOperationPage() error {
	return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED)
}
func (h *operationRPCHandler) sealOperationPage(cursor operationPage, binding []byte) (string, error) {
	data, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, h.pages.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(h.pages.Seal(nonce, nonce, data, binding)), nil
}
func (h *operationRPCHandler) openOperationPage(text string, binding []byte) (operationPage, error) {
	var cursor operationPage
	if len(text) == 0 || len(text) > 4096 {
		return cursor, invalidOperationPage()
	}
	raw, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil || len(raw) < h.pages.NonceSize()+h.pages.Overhead() {
		return cursor, invalidOperationPage()
	}
	data, err := h.pages.Open(nil, raw[:h.pages.NonceSize()], raw[h.pages.NonceSize():], binding)
	if err != nil {
		return cursor, invalidOperationPage()
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cursor) != nil || cursor.Stage < 0 || cursor.Stage > 1 || cursor.Offset < 0 || cursor.Offset > 200 || cursor.Cutoff.IsZero() || !time.Now().Before(cursor.Expires) || cursor.Expires.Sub(cursor.Cutoff) != time.Minute {
		return operationPage{}, invalidOperationPage()
	}
	return cursor, nil
}
func operationRequestFilter(text string) (*apitypes.RequestID, string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, "", nil
	}
	if len(text) > 256 {
		return nil, "", errors.New("invalid operation filter")
	}
	field, value, ok := strings.Cut(text, "=")
	if !ok || strings.TrimSpace(field) != "metadata.request_id" {
		return nil, "", errors.New("invalid operation filter")
	}
	var raw string
	if json.Unmarshal([]byte(strings.TrimSpace(value)), &raw) != nil {
		return nil, "", errors.New("invalid operation filter")
	}
	key, err := apitypes.ParseRequestID(raw)
	if err != nil {
		return nil, "", err
	}
	return &key, `metadata.request_id="` + key.String() + `"`, nil
}
func operationHidden(err error) bool {
	return errors.Is(err, authorization.ErrDenied) || errors.Is(err, integrations.ErrCommandAuthorizationDenied) || err != nil && apiFault(err).Reason() == api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED
}

func (h *operationRPCHandler) ListOperations(ctx context.Context, request *connect.Request[longrunningpb.ListOperationsRequest]) (*connect.Response[longrunningpb.ListOperationsResponse], error) {
	if _, err := coreRPCActor(ctx); err != nil {
		return nil, err
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil || h == nil || h.authority == nil || h.pages == nil {
		return nil, apiRPCError(ctx, authorization.ErrDenied)
	}
	current, err := h.authority.Current(principal)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if request == nil || request.Msg == nil {
		return nil, apiRPCError(ctx, homeValidation(&longrunningpb.ListOperationsRequest{}, "name", api.ValidationReason_VALIDATION_REASON_REQUIRED_VALUE))
	}
	r := request.Msg
	home, err := apitypes.ParseHomeName(r.Name)
	if err != nil {
		return nil, apiRPCError(ctx, homeValidation(r, "name", api.ValidationReason_VALIDATION_REASON_INVALID_NAME))
	}
	size, err := homePageSize(r, r.PageSize)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	key, normalized, err := operationRequestFilter(r.Filter)
	if err != nil {
		return nil, apiRPCError(ctx, homeValidation(r, "filter", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT))
	}
	binding, err := homePageBinding(current, home.String(), normalized, "ListOperations")
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	now := time.Now().UTC()
	cursor := operationPage{Cutoff: now, Expires: now.Add(time.Minute)}
	if r.PageToken != "" {
		cursor, err = h.openOperationPage(r.PageToken, binding)
		if err != nil {
			return nil, apiRPCError(ctx, err)
		}
	}
	out := &longrunningpb.ListOperationsResponse{}
	used := operationPageOverhead
	appendOperation := func(value *longrunningpb.Operation) bool {
		cost := protowire.SizeTag(1) + protowire.SizeBytes(proto.Size(value))
		if cost > maxOperationListBytes-used {
			return false
		}
		out.Operations = append(out.Operations, value)
		used += cost
		return true
	}
	if key != nil {
		if cursor.Stage != 0 || cursor.After != "" {
			return nil, apiRPCError(ctx, invalidOperationPage())
		}
		names := make([]apitypes.OperationName, 0, 200)
		for _, provider := range []operationReadProvider{h.physical, h.configuration} {
			if nilReactiveInterface(provider) {
				continue
			}
			values, findErr := provider.(operationNamesProvider).FindOperationsByRequest(ctx, home, *key, cursor.Cutoff)
			if operationHidden(findErr) {
				continue
			}
			if findErr != nil {
				return nil, apiRPCError(ctx, findErr)
			}
			if len(values) > 100 {
				return nil, apiRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY))
			}
			names = append(names, values...)
		}
		sort.Slice(names, func(i, j int) bool { return names[i].String() < names[j].String() })
		if cursor.Offset > len(names) {
			return nil, apiRPCError(ctx, invalidOperationPage())
		}
		limit := min(cursor.Offset+size, len(names))
		end := cursor.Offset
		for end < limit {
			name := names[end]
			if name.Home() != home {
				return nil, apiRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY))
			}
			value, readErr := h.read(ctx, name)
			if operationHidden(readErr) {
				end++
				continue
			}
			if readErr != nil {
				return nil, apiRPCError(ctx, readErr)
			}
			if !appendOperation(value) {
				break
			}
			end++
		}
		if end < len(names) {
			cursor.Offset = end
			out.NextPageToken, err = h.sealOperationPage(cursor, binding)
		}
	} else {
		if cursor.Offset != 0 {
			return nil, apiRPCError(ctx, invalidOperationPage())
		}
		providers := []operationReadProvider{h.physical, h.configuration}
	providerScan:
		for cursor.Stage < len(providers) {
			provider := providers[cursor.Stage]
			if nilReactiveInterface(provider) {
				cursor.Stage++
				cursor.After = ""
				continue
			}
			remaining := size - len(out.Operations)
			if remaining == 0 {
				out.NextPageToken, err = h.sealOperationPage(cursor, binding)
				break
			}
			var after *apitypes.OperationName
			if cursor.After != "" {
				value, parseErr := apitypes.ParseOperationName(cursor.After)
				if parseErr != nil || value.Home() != home {
					return nil, apiRPCError(ctx, invalidOperationPage())
				}
				after = &value
			}
			names, next, listErr := provider.(operationNamesProvider).ListOperationNames(ctx, home, after, uint32(remaining), cursor.Cutoff)
			if operationHidden(listErr) {
				cursor.Stage++
				cursor.After = ""
				continue
			}
			if listErr != nil {
				return nil, apiRPCError(ctx, listErr)
			}
			if len(names) > remaining {
				return nil, apiRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY))
			}
			batchAfter := cursor.After
			previous := cursor.After
			for _, name := range names {
				if name.Home() != home || name.String() <= previous {
					return nil, apiRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY))
				}
				previous = name.String()
				value, readErr := h.read(ctx, name)
				if operationHidden(readErr) {
					cursor.After = name.String()
					continue
				}
				if readErr != nil {
					return nil, apiRPCError(ctx, readErr)
				}
				if !appendOperation(value) {
					out.NextPageToken, err = h.sealOperationPage(cursor, binding)
					break providerScan
				}
				cursor.After = name.String()
			}
			if next != nil {
				if !next.Valid() || next.Home() != home || next.String() <= batchAfter || len(names) > 0 && next.String() < previous {
					return nil, apiRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY))
				}
				cursor.After = next.String()
				out.NextPageToken, err = h.sealOperationPage(cursor, binding)
				break
			}
			cursor.Stage++
			cursor.After = ""
		}
	}
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	if proto.Size(out) > maxOperationListBytes {
		return nil, apiRPCError(ctx, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY))
	}
	fresh, err := h.authority.Current(principal)
	if err != nil {
		return nil, apiRPCError(ctx, err)
	}
	latest, err := homePageBinding(fresh, home.String(), normalized, "ListOperations")
	if err != nil || !bytes.Equal(binding, latest) {
		return nil, apiRPCError(ctx, invalidOperationPage())
	}
	if err := authorizationTerminationError(ctx); err != nil {
		return nil, apiRPCError(ctx, err)
	}
	return connect.NewResponse(out), nil
}

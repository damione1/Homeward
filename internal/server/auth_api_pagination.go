package server

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io"
	"sort"
	"time"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/authorization"
	"google.golang.org/protobuf/proto"
)

type authPageCursor struct {
	Capture [32]byte `json:"c"`
	After   string   `json:"a"`
	Expire  int64    `json:"e"`
}
type namedAuthResource interface {
	proto.Message
	GetName() string
}

func authPageSize(request proto.Message, size int32, token string) (int, error) {
	if size < 0 {
		return 0, authInvalid(request, "page_size", api.ValidationReason_VALIDATION_REASON_OUT_OF_RANGE)
	}
	if len(token) > 2048 {
		return 0, authInvalid(request, "page_token", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT)
	}
	if size == 0 {
		return 50, nil
	}
	return min(int(size), 100), nil
}

// A bounded fresh native collection is compared to the previous authorized cut.
// Membership and state mutations force resync. Native activity measurements are
// returned fresh but excluded from the cut: AdmitAccess itself advances a
// session’s last_use_time and sliding expire_time on every page request. The token reveals neither IDs nor metadata, and another credential,
// parent, rights revision or method cannot reuse it. Changing page_size is valid.
func authPage[T namedAuthResource](ctx context.Context, coordinator *sessionCoordinator, pages cipher.AEAD, request proto.Message, parent, method, token string, size int, values []T) ([]T, string, error) {
	if coordinator == nil || coordinator.authority == nil || pages == nil || size < 1 || size > 100 {
		return nil, "", apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return nil, "", err
	}
	current, err := coordinator.authority.Current(principal)
	if err != nil {
		return nil, "", err
	}
	binding, err := homePageBinding(current, parent, "", method)
	if err != nil {
		return nil, "", err
	}
	if len(values) > 10000 {
		return nil, "", apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].GetName() < values[j].GetName() })
	hash := sha256.New()
	bytesUsed := 0
	previous := ""
	for _, value := range values {
		if value.GetName() == "" || value.GetName() == previous {
			return nil, "", apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
		}
		previous = value.GetName()
		wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(authPageState(value))
		if err != nil {
			return nil, "", apierrors.New(api.ApiErrorReason_API_ERROR_REASON_STORED_DATA_INTEGRITY)
		}
		bytesUsed += len(wire)
		if bytesUsed > 8<<20 {
			return nil, "", apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
		}
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(wire)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write(wire)
	}
	var capture [32]byte
	copy(capture[:], hash.Sum(nil))
	now := coordinator.clock.Now()
	cursor := authPageCursor{Capture: capture, Expire: now.Add(5 * time.Minute).Unix()}
	start := 0
	if token != "" {
		invalid := authInvalid(request, "page_token", api.ValidationReason_VALIDATION_REASON_INVALID_FORMAT)
		plain, err := openAPIPage(pages, token, binding)
		if err != nil {
			return nil, "", invalid
		}
		decoder := json.NewDecoder(bytes.NewReader(plain))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&cursor) != nil || cursor.After == "" {
			return nil, "", invalid
		}
		var extra json.RawMessage
		if decoder.Decode(&extra) != io.EOF {
			return nil, "", invalid
		}
		if cursor.Capture != capture || cursor.Expire <= now.Unix() {
			return nil, "", apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED)
		}
		index := sort.Search(len(values), func(i int) bool { return values[i].GetName() >= cursor.After })
		if index == len(values) || values[index].GetName() != cursor.After {
			return nil, "", invalid
		}
		start = index + 1
	}
	end := min(start+size, len(values))
	next := ""
	if end < len(values) {
		cursor.After = values[end-1].GetName()
		plain, err := json.Marshal(cursor)
		if err != nil {
			return nil, "", err
		}
		next, err = sealAPIPage(pages, plain, binding)
		if err != nil {
			return nil, "", err
		}
	}
	latest, err := coordinator.authority.Current(principal)
	if err != nil {
		return nil, "", err
	}
	latestBinding, err := homePageBinding(latest, parent, "", method)
	if err != nil || !bytes.Equal(binding, latestBinding) {
		return nil, "", authorization.ErrDenied
	}
	return values[start:end], next, nil
}

// These timestamps measure use, not membership or configured authority. Never
// freeze them in returned resources or change their native NHA198 update path.
func authPageState(value proto.Message) proto.Message {
	switch typed := value.(type) {
	case *api.Session:
		state := proto.Clone(typed).(*api.Session)
		state.LastUseTime = nil
		state.ExpireTime = nil
		return state
	case *api.FactorCredential:
		state := proto.Clone(typed).(*api.FactorCredential)
		state.LastUseTime = nil
		return state
	case *api.McpKey:
		state := proto.Clone(typed).(*api.McpKey)
		state.LastUseTime = nil
		return state
	default:
		return value
	}
}

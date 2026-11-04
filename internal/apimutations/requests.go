// Package apimutations owns same-transaction deduplication of nonphysical API
// mutations. It neither dispatches effects nor retries an owner operation.
package apimutations

import (
	"context"
	"crypto/sha256"
	"errors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"strings"
)

const MaxResultBytes = 1 << 20
const MaxRecords = 4096
const MaxActorRecords = 256
const MaxStoredBytes = 64 << 20
const MaxActorStoredBytes = 8 << 20

// Results are retained for 24 hours from the original committed intention.
// Replay never extends expiry, and no client may infer retry permission from it.
const RetentionHours = 24

var ErrInvalidRequest = errors.New("invalid mutation request identity")

type Request struct {
	actor, method, id string
	fingerprint       [32]byte
}

// New admits generated method identity and a canonicalized intention. Absent
// request_id disables caller deduplication without granting any replay policy.
func New(ctx context.Context, method, id string, normalized []byte) (Request, error) {
	if id == "" {
		return Request{}, nil
	}
	parsed, err := apitypes.ParseRequestID(id)
	if err != nil {
		return Request{}, ErrInvalidRequest
	}
	if len(method) < 1 || len(method) > 256 || !strings.HasPrefix(method, "/home.api.v2.") || len(normalized) > MaxResultBytes {
		return Request{}, ErrInvalidRequest
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return Request{}, authorization.ErrDenied
	}
	actor, err := principal.Actor()
	if err != nil {
		return Request{}, authorization.ErrDenied
	}
	text, err := actor.Value()
	if err != nil || len(text) > 255 {
		return Request{}, ErrInvalidRequest
	}
	return Request{actor: text, method: method, id: parsed.String(), fingerprint: sha256.Sum256(normalized)}, nil
}

type requestKey[T any] struct{}
type binding[T any] struct {
	request Request
	encode  func(T) ([]byte, error)
}

// WithResultEncoder keeps the owner result type explicit through the generic
// transaction hook. Only pure bounded encoding is allowed under SQL locks.
func WithResultEncoder[T any](ctx context.Context, request Request, encode func(T) ([]byte, error)) (context.Context, error) {
	if ctx == nil {
		return nil, ErrInvalidRequest
	}
	if request.id == "" {
		return ctx, nil
	}
	if request.actor == "" || request.method == "" || request.fingerprint == ([32]byte{}) || encode == nil {
		return nil, ErrInvalidRequest
	}
	return context.WithValue(ctx, requestKey[T]{}, binding[T]{request, encode}), nil
}

type ErrorCode uint8

const (
	Replayed ErrorCode = iota + 1
	RequestIDReused
	Capacity
	StoredResultCorrupt
)

type Error struct {
	code   ErrorCode
	result []byte
}

func (e *Error) Error() string { return "API mutation result unavailable [redacted]" }
func (e *Error) Code() ErrorCode {
	if e == nil {
		return StoredResultCorrupt
	}
	return e.code
}
func (e *Error) ReplayResult() ([]byte, bool) {
	if e == nil || e.code != Replayed {
		return nil, false
	}
	return append([]byte(nil), e.result...), true
}
func IsResultError(err error) bool { var own *Error; return errors.As(err, &own) }

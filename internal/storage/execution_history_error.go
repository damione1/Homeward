package storage

import (
	"context"
	"errors"
	"reflect"
)

// historyFailure retains classification and the original cause for internal
// errors.Is checks, while its rendered text cannot disclose SQL or driver data.
type historyFailureError struct {
	kind, cause error
}

func (e historyFailureError) Error() string   { return e.kind.Error() }
func (e historyFailureError) Unwrap() []error { return []error{e.kind, e.cause} }

func historyFailure(kind, cause error) error {
	if cause == nil || errors.Is(cause, kind) {
		return kind
	}
	return historyFailureError{kind: kind, cause: cause}
}

// A Context interface may contain a typed nil pointer. Reject it at every
// history boundary before invoking methods or constructing timeout children.
func validHistoryContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	value := reflect.ValueOf(ctx)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return !value.IsNil()
	default:
		return true
	}
}

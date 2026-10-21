// Package backups defines storage for opaque archive bytes. It does not create,
// decrypt, restore or schedule archives.
package backups

import "reflect"

type ErrorKind uint8

const (
	InvalidInput ErrorKind = iota + 1
	Unsupported
	NotFound
	Conflict
	PermissionDenied
	Capacity
	Unavailable
	Corrupt
	Cancelled
	Closed
)

// This table is the sole public spelling of the closed error vocabulary.
var errorNames = [...]string{"", "invalid input", "unsupported provider", "not found", "conflict", "permission denied", "capacity exceeded", "unavailable", "corrupt artifact", "cancelled", "closed"}

func (k ErrorKind) Valid() bool { return k >= InvalidInput && k <= Closed }
func (k ErrorKind) String() string {
	if !k.Valid() {
		return errorNames[Unavailable]
	}
	return errorNames[k]
}

// Error deliberately has no cause, path, payload or arbitrary diagnostic text.
// Filesystem errors must be classified at the adapter boundary.
type Error struct{ kind ErrorKind }

func NewError(kind ErrorKind) *Error {
	if !kind.Valid() {
		kind = Unavailable
	}
	return &Error{kind: kind}
}
func (e *Error) Kind() ErrorKind {
	if e == nil || !e.kind.Valid() {
		return Unavailable
	}
	return e.kind
}
func (e *Error) Error() string { return "backup: " + e.Kind().String() }
func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	return ok && e != nil && other != nil && e.Kind() == other.Kind()
}

// IsNil also rejects an interface containing a typed nil at admission boundaries.
func IsNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	}
	return false
}

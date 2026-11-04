package apitypes

import (
	"errors"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

var (
	ErrPercentage = errors.New("percentage must be finite and between zero and one hundred")
	ErrRequestID  = errors.New("request ID must be a UUIDv4")
	ErrIntent     = errors.New("explicit admitted command intent required")
	ErrDeadline   = errors.New("valid command deadline required")
)

// Percentage admits intrinsic bounds. Its legitimate zero value is zero percent.
// Instance bounds and increments are checked separately, under current authority.
type Percentage struct{ value float64 }

func NewPercentage(value float64) (Percentage, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
		return Percentage{}, ErrPercentage
	}
	return Percentage{value}, nil
}
func (v Percentage) Value() float64 { return v.value }

// RequestID is an admitted UUIDv4, not an actor, capability or command authority.
type RequestID struct{ value string }

func ParseRequestID(value string) (RequestID, error) {
	id, err := uuid.Parse(value)
	if err != nil || len(value) != 36 || id.Version() != 4 || id.Variant() != uuid.RFC4122 || !strings.EqualFold(value, id.String()) {
		return RequestID{}, ErrRequestID
	}
	return RequestID{id.String()}, nil
}
func (v RequestID) String() string { return v.value }
func (v RequestID) Valid() bool    { return v.value != "" }

// Intent is sealed to values admitted here. Executable request construction
// rejects nil, typed nil and zero PowerCommand; it never guesses an intention.
type Intent interface{ apiIntent() }

// PowerCommand's representation is private. The zero value is absent, not OFF.
type PowerCommand struct{ state uint8 }

func PowerOn() PowerCommand        { return PowerCommand{1} }
func PowerOff() PowerCommand       { return PowerCommand{2} }
func (PowerCommand) apiIntent()    {}
func (v PowerCommand) IsOn() bool  { return v.state == 1 }
func (v PowerCommand) Valid() bool { return v.state == 1 || v.state == 2 }

// LevelCommand preserves zero. Its value is valid even as a Go zero value;
// absence of wire intention is rejected before constructing it.
type LevelCommand struct{ percentage Percentage }

func SetLevel(percentage Percentage) LevelCommand { return LevelCommand{percentage} }
func (LevelCommand) apiIntent()                   {}
func (v LevelCommand) Percentage() Percentage     { return v.percentage }

// Command is an immutable representation-admitted request. It does not prove
// permission, future expiry, instance bounds, epoch/CAS or durable admission.
type Command struct {
	target         CapabilityName
	intent         Intent
	requestID      RequestID
	expireTime     time.Time
	definitionETag string
}

// NewCommand is the sole complete construction path. requestID=nil explicitly
// means no caller deduplication; passing a nonnil zero key is invalid.
func NewCommand(target CapabilityName, intent Intent, requestID *RequestID, expireTime time.Time) (Command, error) {
	if !target.Valid() {
		return Command{}, ErrResourceName
	}
	switch value := intent.(type) {
	case PowerCommand:
		if !value.Valid() {
			return Command{}, ErrIntent
		}
	case MediaCommand:
		if !value.Valid() {
			return Command{}, ErrIntent
		}
	case LightCommand:
		if !value.Valid() {
			return Command{}, ErrIntent
		}
	case FanCommand:
		if !value.Valid() {
			return Command{}, ErrIntent
		}
	case ThermalCommand:
		if !value.Valid() {
			return Command{}, ErrIntent
		}
	case CoverCommand:
		if !value.Valid() {
			return Command{}, ErrIntent
		}
	case LockCommand:
		if !value.Valid() {
			return Command{}, ErrIntent
		}
	case AlarmCommand:
		if !value.Valid() {
			return Command{}, ErrIntent
		}
	case HelperCommand:
		if !value.Valid() {
			return Command{}, ErrIntent
		}
	case LevelCommand:
		// Private Percentage is always finite and in bounds, including zero.
	default:
		return Command{}, ErrIntent
	}
	if expireTime.IsZero() || expireTime.Year() < 1 || expireTime.Year() > 9999 {
		return Command{}, ErrDeadline
	}
	var id RequestID
	if requestID != nil {
		if !requestID.Valid() {
			return Command{}, ErrRequestID
		}
		id = *requestID
	}
	return Command{target: target, intent: intent, requestID: id, expireTime: expireTime.UTC()}, nil
}

func (v Command) Target() CapabilityName       { return v.target }
func (v Command) Intent() Intent               { return v.intent }
func (v Command) RequestID() (RequestID, bool) { return v.requestID, v.requestID.Valid() }
func (v Command) ExpireTime() time.Time        { return v.expireTime }

// Valid rejects Go zero Command before consumption by an execution boundary.
func (v Command) Valid() bool {
	var id *RequestID
	if v.requestID.Valid() {
		id = &v.requestID
	}
	_, err := NewCommand(v.target, v.intent, id, v.expireTime)
	return err == nil
}

// WithDefinitionETag retains an opaque current definition binding. It does not
// establish authorization; execution compares it to the current registry cut.
func (v Command) WithDefinitionETag(etag string) (Command, error) {
	if !v.Valid() || len(etag) == 0 || len(etag) > 256 || !utf8.ValidString(etag) {
		return Command{}, ErrIntent
	}
	for _, r := range etag {
		if unicode.IsControl(r) {
			return Command{}, ErrIntent
		}
	}
	v.definitionETag = etag
	return v, nil
}
func (v Command) DefinitionETag() string { return v.definitionETag }

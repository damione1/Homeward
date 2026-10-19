// Package jobs schedules explicitly durable, one-shot work. Ordinary script
// waits and delays have no representation here and remain process-local.
package jobs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
)

const MaxCommandBytes = 64 * 1024

type ErrorKind uint8

const (
	ErrorUnspecified ErrorKind = iota
	Invalid
	Conflict
	Unavailable
	CommitUnknown
	LeaseLost
	Closed
	Corrupt
	Capacity
)

type Error struct{ kind ErrorKind }

func (e *Error) Kind() ErrorKind {
	if e == nil {
		return ErrorUnspecified
	}
	return e.kind
}
func (e *Error) Error() string {
	switch e.Kind() {
	case Invalid:
		return "durable jobs: invalid input"
	case Conflict:
		return "durable jobs: immutable identity conflict"
	case Unavailable:
		return "durable jobs: storage unavailable"
	case CommitUnknown:
		return "durable jobs: commit outcome unknown"
	case LeaseLost:
		return "durable jobs: lease no longer owned"
	case Closed:
		return "durable jobs: admissions closed"
	case Corrupt:
		return "durable jobs: stored definition is invalid"
	case Capacity:
		return "durable jobs: creation admission capacity exhausted"
	case ErrorUnspecified:
		return "durable jobs: unspecified error"
	default:
		return "durable jobs: invalid error kind"
	}
}
func (e *Error) Is(other error) bool {
	var target *Error
	return errors.As(other, &target) && e != nil && target != nil && e.kind == target.kind
}

func failure(kind ErrorKind) error { return &Error{kind: kind} }

type JobID struct{ text string }
type OccurrenceID struct{ text string }
type IdempotencyKey struct{ text string }
type BackupKey struct{ text string }
type Version struct{ number int64 }

func identity(value string) bool {
	return len(value) > 0 && len(value) <= 256 && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}
func ParseJobID(value string) (JobID, error) {
	if !identity(value) {
		return JobID{}, failure(Invalid)
	}
	return JobID{value}, nil
}
func ParseOccurrenceID(value string) (OccurrenceID, error) {
	if !identity(value) {
		return OccurrenceID{}, failure(Invalid)
	}
	return OccurrenceID{value}, nil
}
func ParseIdempotencyKey(value string) (IdempotencyKey, error) {
	if !identity(value) {
		return IdempotencyKey{}, failure(Invalid)
	}
	return IdempotencyKey{value}, nil
}
func ParseBackupKey(value string) (BackupKey, error) {
	if !identity(value) {
		return BackupKey{}, failure(Invalid)
	}
	return BackupKey{value}, nil
}
func NewVersion(number uint64) (Version, error) {
	if number == 0 || number > math.MaxInt64 {
		return Version{}, failure(Invalid)
	}
	return Version{int64(number)}, nil
}
func (v JobID) Value() (string, error)          { return checkedIdentity(v.text) }
func (v OccurrenceID) Value() (string, error)   { return checkedIdentity(v.text) }
func (v IdempotencyKey) Value() (string, error) { return checkedIdentity(v.text) }
func (v BackupKey) Value() (string, error)      { return checkedIdentity(v.text) }
func (v Version) Value() (uint64, error) {
	if v.number <= 0 {
		return 0, failure(Invalid)
	}
	return uint64(v.number), nil
}
func checkedIdentity(text string) (string, error) {
	if !identity(text) {
		return "", failure(Invalid)
	}
	return text, nil
}

type lateKind uint8

const (
	lateUnspecified lateKind = iota
	lateSkip
	lateRunBeforeExpiry
)

// LatePolicy is explicit even for the common run-before-expiration case.
// SkipLate's tolerance includes ordinary polling latency; it is not a cron rule.
type LatePolicy struct {
	kind      lateKind
	tolerance time.Duration
}

func SkipLate(tolerance time.Duration) (LatePolicy, error) {
	if tolerance <= 0 || tolerance > 30*24*time.Hour || tolerance%time.Microsecond != 0 {
		return LatePolicy{}, failure(Invalid)
	}
	return LatePolicy{lateSkip, tolerance}, nil
}
func RunBeforeExpiry() LatePolicy { return LatePolicy{kind: lateRunBeforeExpiry} }
func (p LatePolicy) valid() bool {
	switch p.kind {
	case lateSkip:
		_, err := SkipLate(p.tolerance)
		return err == nil
	case lateRunBeforeExpiry:
		return p.tolerance == 0
	case lateUnspecified:
		return false
	default:
		return false
	}
}

// SessionGrant delegates only the named, real persisted session. Re-admission
// must reject expiration/revocation; this is not a permanent automation grant.
// Only trusted server composition may create it for a native session. Knowing
// another SessionID is not authority: a future scoped/MCP credential must never
// be converted to this native grant, even for the same account. Future public
// admission needs its own ceiling-preserving grant; this tranche adds no API.
type SessionGrant struct {
	owner   domain.ActorID
	session contracts.SessionID
}

func NewSessionGrant(owner domain.ActorID, session contracts.SessionID) (SessionGrant, error) {
	text, err := owner.Value()
	if err != nil {
		return SessionGrant{}, failure(Invalid)
	}
	if _, err := contracts.ParseAccountID(text); err != nil {
		return SessionGrant{}, failure(Invalid)
	}
	if _, err := session.Value(); err != nil {
		return SessionGrant{}, failure(Invalid)
	}
	return SessionGrant{owner, session}, nil
}
func (g SessionGrant) Values() (domain.ActorID, contracts.SessionID, error) {
	if _, err := NewSessionGrant(g.owner, g.session); err != nil {
		return domain.ActorID{}, contracts.SessionID{}, err
	}
	return g.owner, g.session, nil
}

type WorkKind uint8

const (
	WorkUnspecified WorkKind = iota
	SessionCommand
	SystemBackup
)

// Work is immutable intent data, never a previously admitted command. The
// command template deliberately has no command ID, epoch or expiration.
type Work struct {
	kind    WorkKind
	grant   SessionGrant
	command []byte
	backup  BackupKey
}

func NewSessionCommand(grant SessionGrant, template *actions.CommandRequest) (Work, error) {
	if _, _, err := grant.Values(); err != nil || template == nil || proto.Size(template) > MaxCommandBytes {
		return Work{}, failure(Invalid)
	}
	if template.CommandId != "" || template.AdmissionEpoch != "" || template.ExpiresAt != nil || template.ConfigRevision == 0 || template.CapabilitySchemaId == "" || template.Endpoint == "" || contracts.RejectUnknownCommandFields(template) != nil {
		return Work{}, failure(Invalid)
	}
	// Stable entity identity prevents a persisted alias being silently retargeted.
	if template.Target == nil {
		return Work{}, failure(Invalid)
	}
	target, ok := template.Target.Reference.(*registry.EntityRef_EntityId)
	if !ok || target == nil {
		return Work{}, failure(Invalid)
	}
	if _, err := domain.ParseEntityID(target.EntityId); err != nil || !intentArguments(template.Arguments) {
		return Work{}, failure(Invalid)
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(template)
	if err != nil || len(encoded) > MaxCommandBytes {
		return Work{}, failure(Invalid)
	}
	return Work{kind: SessionCommand, grant: grant, command: encoded}, nil
}
func intentArguments(args *actions.ActionArgs) bool {
	if args == nil {
		return false
	}
	// Shape admission only. The live catalogue remains the capability authority.
	switch value := args.Arguments.(type) {
	case *actions.ActionArgs_SetSwitch:
		return value != nil && value.SetSwitch != nil
	case *actions.ActionArgs_SetLight:
		return value != nil && value.SetLight != nil
	case *actions.ActionArgs_SetFan:
		return value != nil && value.SetFan != nil
	case *actions.ActionArgs_SetClimate:
		return value != nil && value.SetClimate != nil
	case *actions.ActionArgs_Extension:
		return value != nil && value.Extension != nil
	default:
		return false
	}
}
func NewSystemBackup(key BackupKey) (Work, error) {
	if _, err := key.Value(); err != nil {
		return Work{}, err
	}
	return Work{kind: SystemBackup, backup: key}, nil
}
func (w Work) Kind() WorkKind { return w.kind }
func (w Work) SessionCommand() (SessionGrant, *actions.CommandRequest, error) {
	if w.kind != SessionCommand || len(w.command) == 0 || len(w.command) > MaxCommandBytes || w.backup != (BackupKey{}) {
		return SessionGrant{}, nil, failure(Invalid)
	}
	request := new(actions.CommandRequest)
	if err := proto.Unmarshal(w.command, request); err != nil {
		return SessionGrant{}, nil, failure(Invalid)
	}
	if _, err := NewSessionCommand(w.grant, request); err != nil {
		return SessionGrant{}, nil, err
	}
	return w.grant, request, nil
}
func (w Work) Backup() (BackupKey, error) {
	if w.kind != SystemBackup || w.grant != (SessionGrant{}) || len(w.command) != 0 {
		return BackupKey{}, failure(Invalid)
	}
	_, err := w.backup.Value()
	return w.backup, err
}
func (w Work) valid() bool {
	switch w.kind {
	case SessionCommand:
		_, _, err := w.SessionCommand()
		return err == nil
	case SystemBackup:
		_, err := w.Backup()
		return err == nil
	case WorkUnspecified:
		return false
	default:
		return false
	}
}

type DefinitionInput struct {
	ID           JobID
	Version      Version
	DeadlineUTC  time.Time
	Timezone     string
	ExpiresAtUTC time.Time
	LatePolicy   LatePolicy
	Work         Work
}

// Definition is a one-shot, versioned definition. Timezone records operator
// intent; it never changes the absolute instant or introduces recurrence/DST.
type Definition struct {
	id       JobID
	version  Version
	deadline time.Time
	zone     string
	expires  time.Time
	late     LatePolicy
	work     Work
}

func utcInstant(value time.Time) (time.Time, error) {
	_, offset := value.Zone()
	normalized := value.UTC().Truncate(time.Microsecond)
	if normalized.IsZero() || normalized.Year() < 1 || normalized.Year() > 9999 || offset != 0 {
		return time.Time{}, failure(Invalid)
	}
	return normalized, nil
}
func AdmitDefinition(input DefinitionInput) (Definition, error) {
	deadline, deadlineErr := utcInstant(input.DeadlineUTC)
	expires, expiresErr := utcInstant(input.ExpiresAtUTC)
	if _, err := input.ID.Value(); err != nil {
		return Definition{}, err
	}
	if _, err := input.Version.Value(); err != nil {
		return Definition{}, err
	}
	if deadlineErr != nil || expiresErr != nil || !expires.After(deadline) || !input.LatePolicy.valid() || !input.Work.valid() || input.Timezone == "" || input.Timezone == "Local" || len(input.Timezone) > 128 {
		return Definition{}, failure(Invalid)
	}
	if _, err := time.LoadLocation(input.Timezone); err != nil {
		return Definition{}, failure(Invalid)
	}
	work := input.Work
	work.command = bytes.Clone(work.command)
	return Definition{input.ID, input.Version, deadline, input.Timezone, expires, input.LatePolicy, work}, nil
}
func (d Definition) Input() DefinitionInput {
	work := d.work
	work.command = bytes.Clone(work.command)
	return DefinitionInput{d.id, d.version, d.deadline, d.zone, d.expires, d.late, work}
}
func (d Definition) valid() bool { _, err := AdmitDefinition(d.Input()); return err == nil }
func (d Definition) equal(other Definition) bool {
	return d.id == other.id && d.version == other.version && d.deadline.Equal(other.deadline) && d.zone == other.zone && d.expires.Equal(other.expires) && d.late == other.late && d.work.kind == other.work.kind && d.work.grant == other.work.grant && d.work.backup == other.work.backup && bytes.Equal(d.work.command, other.work.command)
}

type State uint8

const (
	StateUnspecified State = iota
	Pending
	Claimed
	Dispatching
	Succeeded
	Rejected
	Expired
	Skipped
	Indeterminate
)

func stateFromSQL(value int16) (State, error) {
	state := State(value)
	// Explicit cases guard both Go's zero value and future SQL states.
	switch state {
	case Pending, Claimed, Dispatching, Succeeded, Rejected, Expired, Skipped, Indeterminate:
		if int16(state) == value {
			return state, nil
		}
	case StateUnspecified:
		return StateUnspecified, failure(Corrupt)
	default:
		return StateUnspecified, failure(Corrupt)
	}
	return StateUnspecified, failure(Corrupt)
}

func commandID(occurrence OccurrenceID) domain.CommandID {
	// A namespace and fixed hash keep IDs bounded and stable across workers,
	// definition reloads and retries. No restart or admission epoch enters it.
	digest := sha256.Sum256([]byte("homeward.durable-job.command.v1\x00" + occurrence.text))
	value, _ := domain.ParseCommandID("job-" + hex.EncodeToString(digest[:]))
	return value
}

func nilValue(value any) bool {
	if value == nil {
		return true
	}
	switch reflect.ValueOf(value).Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflect.ValueOf(value).IsNil()
	default:
		return false
	}
}

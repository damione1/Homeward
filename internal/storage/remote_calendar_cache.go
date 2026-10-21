package storage

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type RemoteCalendarFailure uint8

const (
	RemoteCalendarInvalid RemoteCalendarFailure = iota + 1
	RemoteCalendarUnavailable
	RemoteCalendarConflict
	RemoteCalendarCorrupt
	RemoteCalendarCommitUnknown
	RemoteCalendarRevisionExhausted
)

type RemoteCalendarError struct{ kind RemoteCalendarFailure }

func (e *RemoteCalendarError) Kind() RemoteCalendarFailure {
	if e == nil {
		return 0
	}
	return e.kind
}
func (e *RemoteCalendarError) Error() string {
	switch e.Kind() {
	case RemoteCalendarInvalid:
		return "remote calendar: invalid storage input"
	case RemoteCalendarUnavailable:
		return "remote calendar: storage unavailable"
	case RemoteCalendarConflict:
		return "remote calendar: committed source or revision changed"
	case RemoteCalendarCorrupt:
		return "remote calendar: persisted header is corrupt"
	case RemoteCalendarCommitUnknown:
		return "remote calendar: commit outcome unknown"
	case RemoteCalendarRevisionExhausted:
		return "remote calendar: revision exhausted"
	default:
		return "remote calendar: invalid storage failure"
	}
}
func (e *RemoteCalendarError) GoString() string   { return e.Error() }
func remoteFailure(k RemoteCalendarFailure) error { return &RemoteCalendarError{kind: k} }
func remoteNil(value any) bool {
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
func remoteContext(ctx context.Context) error {
	if remoteNil(ctx) {
		return remoteFailure(RemoteCalendarInvalid)
	}
	return ctx.Err()
}
func remoteSQLFailure(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return remoteFailure(RemoteCalendarUnavailable)
}
func remoteText(s string, maximum int, required bool) bool {
	return (!required || len(s) > 0) && len(s) <= maximum && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n")
}
func remoteStamp(t time.Time) bool { return !t.IsZero() && t.Year() >= 1 && t.Year() <= 9999 }
func remoteKey(k [32]byte) bool    { return k != ([32]byte{}) }

// Binding is produced only after reading the authoritative registry row. It
// is a storage projection, never an alternate registry or a writable origin.
type RemoteCalendarBinding struct {
	entity                   domain.EntityID
	instance, device, source string
}

func (b RemoteCalendarBinding) Valid() bool {
	_, e := b.entity.Value()
	return e == nil && remoteText(b.instance, 255, true) && remoteText(b.device, 1024, true) && remoteText(b.source, 1024, true)
}
func (b RemoteCalendarBinding) Entity() domain.EntityID      { return b.entity }
func (b RemoteCalendarBinding) String() string               { return "remote calendar binding [redacted]" }
func (b RemoteCalendarBinding) GoString() string             { return b.String() }
func (b RemoteCalendarBinding) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func ReadRemoteCalendarBinding(ctx context.Context, tx pgx.Tx, entity domain.EntityID, write bool) (RemoteCalendarBinding, error) {
	if err := remoteContext(ctx); err != nil {
		return RemoteCalendarBinding{}, err
	}
	id, e := entity.Value()
	if e != nil || remoteNil(tx) {
		return RemoteCalendarBinding{}, remoteFailure(RemoteCalendarInvalid)
	}
	query := `SELECT b.instance_id,b.source_device,b.source_entity,e.tombstoned FROM public.registry_entities e
        JOIN public.registry_source_bindings b ON b.entity_id=e.id
        JOIN public.registry_device_sources d ON d.instance_id=b.instance_id AND d.source_device=b.source_device AND d.device_id=e.device_id
        WHERE e.id=$1`
	if write {
		query += ` FOR UPDATE OF e FOR SHARE OF b,d`
	} else {
		query += ` FOR SHARE OF e,b,d`
	}
	b := RemoteCalendarBinding{entity: entity}
	var retired bool
	if err := tx.QueryRow(ctx, query, id).Scan(&b.instance, &b.device, &b.source, &retired); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return b, remoteFailure(RemoteCalendarConflict)
		}
		return b, remoteSQLFailure(ctx)
	}
	if retired || !b.Valid() {
		return RemoteCalendarBinding{}, remoteFailure(RemoteCalendarConflict)
	}
	return b, nil
}

// Scope owns authenticated comparison tokens, not credential values. The
// canonical key is produced by the configuration/conditional boundary.
type RemoteCalendarScope struct {
	key         [32]byte
	credentials []SecretRevision
}

func NewRemoteCalendarScope(key [32]byte, credentials []SecretRevision) (RemoteCalendarScope, error) {
	if !remoteKey(key) || len(credentials) > 3 {
		return RemoteCalendarScope{}, remoteFailure(RemoteCalendarInvalid)
	}
	copyTokens := slices.Clone(credentials)
	for i, v := range copyTokens {
		if !v.validFor(v.handle) {
			return RemoteCalendarScope{}, remoteFailure(RemoteCalendarInvalid)
		}
		if _, _, e := v.handle.Components(); e != nil {
			return RemoteCalendarScope{}, remoteFailure(RemoteCalendarInvalid)
		}
		for _, p := range copyTokens[:i] {
			if p.handle == v.handle {
				return RemoteCalendarScope{}, remoteFailure(RemoteCalendarInvalid)
			}
		}
	}
	slices.SortFunc(copyTokens, func(a, b SecretRevision) int {
		ai, _ := a.handle.instance.Value()
		bi, _ := b.handle.instance.Value()
		if c := strings.Compare(ai, bi); c != 0 {
			return c
		}
		an, _ := a.handle.name.Value()
		bn, _ := b.handle.name.Value()
		return strings.Compare(an, bn)
	})
	// Authenticated versions always participate in the scope, even when a
	// caller accidentally reuses the same configuration/resource key.
	h := sha256.New()
	h.Write([]byte("homeward/remote-calendar-scope/1"))
	h.Write(key[:])
	var word [8]byte
	for _, v := range copyTokens {
		instance, _ := v.handle.instance.Value()
		name, _ := v.handle.name.Value()
		binary.BigEndian.PutUint64(word[:], uint64(len(instance)))
		h.Write(word[:])
		h.Write([]byte(instance))
		binary.BigEndian.PutUint64(word[:], uint64(len(name)))
		h.Write(word[:])
		h.Write([]byte(name))
		binary.BigEndian.PutUint64(word[:], uint64(v.number))
		h.Write(word[:])
	}
	var derived [32]byte
	copy(derived[:], h.Sum(nil))
	return RemoteCalendarScope{key: derived, credentials: copyTokens}, nil
}
func (s RemoteCalendarScope) Valid() bool {
	if !remoteKey(s.key) || len(s.credentials) > 3 {
		return false
	}
	for _, v := range s.credentials {
		if !v.validFor(v.handle) {
			return false
		}
	}
	return true
}
func (s RemoteCalendarScope) Key() ([32]byte, bool)        { return s.key, s.Valid() }
func (s RemoteCalendarScope) String() string               { return "remote calendar scope [redacted]" }
func (s RemoteCalendarScope) GoString() string             { return s.String() }
func (s RemoteCalendarScope) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func checkRemoteCredentials(ctx context.Context, tx pgx.Tx, b RemoteCalendarBinding, s RemoteCalendarScope) error {
	if !s.Valid() {
		return remoteFailure(RemoteCalendarInvalid)
	}
	for _, v := range s.credentials {
		instance, _ := v.handle.instance.Value()
		name, _ := v.handle.name.Value()
		if instance != b.instance {
			return remoteFailure(RemoteCalendarConflict)
		}
		var number int64
		if err := tx.QueryRow(ctx, `SELECT revision FROM public.integration_secrets WHERE instance_id=$1 AND secret_name=$2 FOR SHARE`, instance, name).Scan(&number); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return remoteFailure(RemoteCalendarConflict)
			}
			return remoteSQLFailure(ctx)
		}
		if number != v.number {
			return remoteFailure(RemoteCalendarConflict)
		}
	}
	return nil
}

type RemoteValidatorKind uint8

const (
	RemoteValidatorsNone RemoteValidatorKind = iota + 1
	RemoteValidatorsETag
	RemoteValidatorsLastModified
	RemoteValidatorsBoth
)

type RemoteCalendarValidators struct {
	kind     RemoteValidatorKind
	etag     string
	modified time.Time
}

func remoteETag(s string) bool {
	if len(s) > 1024 {
		return false
	}
	if strings.HasPrefix(s, "W/") {
		s = s[2:]
	}
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return false
	}
	for i := 1; i < len(s)-1; i++ {
		c := s[i]
		if c != 0x21 && !(c >= 0x23 && c <= 0x7e) && c < 0x80 {
			return false
		}
	}
	return true
}
func NewRemoteCalendarValidators(kind RemoteValidatorKind, etag string, modified time.Time) (RemoteCalendarValidators, error) {
	v := RemoteCalendarValidators{kind: kind, etag: etag, modified: modified.UTC()}
	if !v.Valid() {
		return RemoteCalendarValidators{}, remoteFailure(RemoteCalendarInvalid)
	}
	v.etag = strings.Clone(etag)
	return v, nil
}
func (v RemoteCalendarValidators) Valid() bool {
	switch v.kind {
	case RemoteValidatorsNone:
		return v.etag == "" && v.modified.IsZero()
	case RemoteValidatorsETag:
		return remoteETag(v.etag) && v.modified.IsZero()
	case RemoteValidatorsLastModified:
		return v.etag == "" && remoteStamp(v.modified) && v.modified.Nanosecond() == 0
	case RemoteValidatorsBoth:
		return remoteETag(v.etag) && remoteStamp(v.modified) && v.modified.Nanosecond() == 0
	default:
		return false
	}
}
func (v RemoteCalendarValidators) Kind() RemoteValidatorKind {
	if !v.Valid() {
		return 0
	}
	return v.kind
}
func (v RemoteCalendarValidators) ETag() (string, bool) {
	return v.etag, v.Valid() && (v.kind == RemoteValidatorsETag || v.kind == RemoteValidatorsBoth)
}
func (v RemoteCalendarValidators) LastModified() (time.Time, bool) {
	return v.modified, v.Valid() && (v.kind == RemoteValidatorsLastModified || v.kind == RemoteValidatorsBoth)
}
func (v RemoteCalendarValidators) Equal(other RemoteCalendarValidators) bool {
	return v.Valid() && other.Valid() && v.kind == other.kind && v.etag == other.etag && v.modified.Equal(other.modified)
}
func (v RemoteCalendarValidators) String() string               { return "remote calendar validators [redacted]" }
func (v RemoteCalendarValidators) GoString() string             { return v.String() }
func (v RemoteCalendarValidators) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

type RemoteCalendarObservation struct {
	source         RemoteCalendarScope
	id             [32]byte
	at, freshUntil time.Time
	validators     RemoteCalendarValidators
	http           RemoteCalendarHTTPMetadata
	admission      RemoteCalendarHTTPAdmission
}

func NewRemoteCalendarObservation(source RemoteCalendarScope, id [32]byte, at, freshUntil time.Time, validators RemoteCalendarValidators) (RemoteCalendarObservation, error) {
	o := RemoteCalendarObservation{source: source, id: id, at: at.UTC(), freshUntil: freshUntil.UTC(), validators: validators, http: NoRemoteCalendarHTTPMetadata()}
	if !o.Valid() {
		return RemoteCalendarObservation{}, remoteFailure(RemoteCalendarInvalid)
	}
	return o, nil
}
func (o RemoteCalendarObservation) Valid() bool {
	return o.source.Valid() && remoteKey(o.id) && remoteStamp(o.at) && remoteStamp(o.freshUntil) && !o.freshUntil.Before(o.at) && !o.freshUntil.After(o.at.Add(24*time.Hour)) && o.validators.Valid() && o.http.Valid() && (o.http.Present() && o.admission.valid() || !o.http.Present() && o.admission == (RemoteCalendarHTTPAdmission{}))
}
func (o RemoteCalendarObservation) Source() RemoteCalendarScope  { return o.source }
func (o RemoteCalendarObservation) ID() ([32]byte, bool)         { return o.id, o.Valid() }
func (o RemoteCalendarObservation) String() string               { return "remote calendar observation [redacted]" }
func (o RemoteCalendarObservation) GoString() string             { return o.String() }
func (o RemoteCalendarObservation) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// Counts are an untrusted scalar projection. Admission is repeated at both
// SQL preflight and payload decode; these never authorize an allocation alone.
type RemoteCalendarCounts struct{ Events, Zones, Points, Exceptions, TextBytes, SourceBytes, Observances, RDates int64 }

func (c RemoteCalendarCounts) Valid() bool {
	return c.Events >= 0 && c.Events <= 1024 && c.Zones >= 0 && c.Zones <= 72 && c.Points >= 0 && c.Points <= 524288 && c.Exceptions >= 0 && c.Exceptions <= 262144 && c.TextBytes >= 0 && c.TextBytes <= 33554432 && c.SourceBytes >= 0 && c.SourceBytes <= 524288 && c.Observances >= 0 && c.Observances <= 128 && c.RDates >= 0 && c.RDates <= 32768
}

type RemoteCalendarPayload struct {
	data   []byte
	digest [32]byte
	counts RemoteCalendarCounts
	charge int64
}

// The caller owns the byte slice throughout the synchronous Store operation.
// No copy or reference survives its return; calendars reserves before encoding.
func BorrowRemoteCalendarPayload(data []byte, digest [32]byte, counts RemoteCalendarCounts, charge int64) (RemoteCalendarPayload, error) {
	p := RemoteCalendarPayload{data: data, digest: digest, counts: counts, charge: charge}
	if len(data) < 20 || len(data) > 33554432 || !counts.Valid() || charge < remoteMinimumCharge(int64(len(data)), counts) || charge > 33554432 || sha256.Sum256(data) != digest {
		return RemoteCalendarPayload{}, remoteFailure(RemoteCalendarInvalid)
	}
	return p, nil
}
func (p RemoteCalendarPayload) String() string               { return "remote calendar payload [redacted]" }
func (p RemoteCalendarPayload) GoString() string             { return p.String() }
func (p RemoteCalendarPayload) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func remoteMinimumCharge(bytes int64, c RemoteCalendarCounts) int64 {
	// All terms have independently bounded nonnegative inputs before use.
	return 2097152 + 3*bytes + c.Zones*256 + c.Events*512 + c.Events*16384 + c.Points*256 + c.Exceptions*2048
}

type RemoteCalendarState uint8

const (
	RemoteCalendarNeverValidated RemoteCalendarState = iota + 1
	RemoteCalendarValidatedFresh
	RemoteCalendarValidatedStale
)

type RemoteCalendarReason uint8

const (
	RemoteCalendarReasonNone RemoteCalendarReason = iota
	RemoteCalendarReasonTransport
	RemoteCalendarReasonContent
	RemoteCalendarReasonCachePolicy
	RemoteCalendarReasonExpired
	RemoteCalendarReasonReconfigured
	RemoteCalendarReasonPublication
	RemoteCalendarReasonUnavailable
)

func (r RemoteCalendarReason) Valid() bool { return r <= RemoteCalendarReasonUnavailable }

type RemoteCalendarHeader struct {
	binding                          RemoteCalendarBinding
	name                             string
	revision                         int64
	updated                          time.Time
	source, representation           [32]byte
	state                            RemoteCalendarState
	validated, attempted, freshUntil time.Time
	reason                           RemoteCalendarReason
	validators                       RemoteCalendarValidators
	version, profile                 int16
	length, charge                   int64
	digest                           [32]byte
	counts                           RemoteCalendarCounts
	http                             RemoteCalendarHTTPMetadata
	conditional                      RemoteCalendarConditionalDisposition
}

func (h RemoteCalendarHeader) Binding() RemoteCalendarBinding { return h.binding }
func (h RemoteCalendarHeader) Name() string                   { return h.name }
func (h RemoteCalendarHeader) Revision() int64                { return h.revision }
func (h RemoteCalendarHeader) UpdatedAt() time.Time           { return h.updated }
func (h RemoteCalendarHeader) State() RemoteCalendarState     { return h.state }
func (h RemoteCalendarHeader) SourceKey() ([32]byte, bool)    { return h.source, remoteKey(h.source) }
func (h RemoteCalendarHeader) RepresentationKey() ([32]byte, bool) {
	return h.representation, remoteKey(h.representation)
}
func (h RemoteCalendarHeader) ValidatedAt() (time.Time, bool) {
	return h.validated, remoteStamp(h.validated)
}
func (h RemoteCalendarHeader) AttemptedAt() (time.Time, bool) {
	return h.attempted, remoteStamp(h.attempted)
}
func (h RemoteCalendarHeader) FreshUntil() (time.Time, bool) {
	return h.freshUntil, remoteStamp(h.freshUntil)
}
func (h RemoteCalendarHeader) Reason() RemoteCalendarReason         { return h.reason }
func (h RemoteCalendarHeader) Validators() RemoteCalendarValidators { return h.validators }
func (h RemoteCalendarHeader) Counts() RemoteCalendarCounts         { return h.counts }
func (h RemoteCalendarHeader) ImageCharge() int64                   { return h.charge }
func (h RemoteCalendarHeader) PayloadLength() int64                 { return h.length }
func (h RemoteCalendarHeader) Digest() [32]byte                     { return h.digest }
func (h RemoteCalendarHeader) Version() (int16, int16)              { return h.version, h.profile }
func (h RemoteCalendarHeader) String() string                       { return "remote calendar header [redacted]" }
func (h RemoteCalendarHeader) GoString() string                     { return h.String() }
func (h RemoteCalendarHeader) MarshalJSON() ([]byte, error)         { return []byte(`"[redacted]"`), nil }
func (h RemoteCalendarHeader) Valid() bool {
	if !h.binding.Valid() || !remoteText(h.name, 512, true) || h.revision <= 0 || !remoteStamp(h.updated) || !h.reason.Valid() || !h.validators.Valid() || !h.http.Valid() || !h.conditional.Valid() || h.conditional == PreserveAdmittedValidators && !h.http.Present() {
		return false
	}
	switch h.state {
	case RemoteCalendarNeverValidated:
		return h.length == 0 && h.charge == 0 && h.version == 0 && h.profile == 0 && h.digest == ([32]byte{}) && h.representation == ([32]byte{}) && h.validated.IsZero() && h.freshUntil.IsZero() && h.counts == (RemoteCalendarCounts{}) && h.validators.kind == RemoteValidatorsNone && !h.http.Present() && h.conditional == RequireFullRepresentation
	case RemoteCalendarValidatedFresh, RemoteCalendarValidatedStale:
		if !remoteKey(h.source) || !remoteKey(h.representation) || !remoteStamp(h.validated) || !remoteStamp(h.attempted) || !remoteStamp(h.freshUntil) || h.length < 20 || h.length > 33554432 || !h.counts.Valid() || h.charge < remoteMinimumCharge(h.length, h.counts) || h.charge > 33554432 || !remoteKey(h.digest) {
			return false
		}
		return h.state != RemoteCalendarValidatedFresh || (h.source == h.representation && remoteKey(h.source) && h.reason == RemoteCalendarReasonNone && !h.freshUntil.Before(h.validated))
	default:
		return false
	}
}
func scanRemoteHeader(ctx context.Context, tx pgx.Tx, b RemoteCalendarBinding, write bool) (RemoteCalendarHeader, error) {
	entity, _ := b.entity.Value()
	q := `SELECT instance_id,source_device,source_entity,name,revision,updated_at,source_scope,state,representation_scope,
        validated_at,attempted_at,fresh_until,reason,etag,last_modified,payload_version,payload_profile,
        coalesce(octet_length(payload),0)::bigint,digest,event_count,zone_count,point_count,exception_count,
        text_bytes,source_bytes,observance_count,rdate_count,image_charge,coalesce(octet_length(http_metadata),0)::bigint,conditional_state,http_metadata IS NOT NULL FROM public.remote_calendar_cache WHERE entity_id=$1`
	if write {
		q += ` FOR UPDATE`
	} else {
		q += ` FOR SHARE`
	}
	h := RemoteCalendarHeader{binding: b, http: NoRemoteCalendarHTTPMetadata()}
	var metadataLength int64
	var metadataPresent bool
	var conditional int16
	var instance, device, source string
	var scope, repr, digest []byte
	var updated, validated, attempted, until, modified pgtype.Timestamptz
	var etag []byte
	var version, profile pgtype.Int2
	var counts [9]pgtype.Int8
	var state, reason int16
	args := []any{&instance, &device, &source, &h.name, &h.revision, &updated, &scope, &state, &repr, &validated, &attempted, &until, &reason, &etag, &modified, &version, &profile, &h.length, &digest}
	for i := range counts {
		args = append(args, &counts[i])
	}
	args = append(args, &metadataLength, &conditional, &metadataPresent)
	if err := tx.QueryRow(ctx, q, entity).Scan(args...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return h, remoteFailure(RemoteCalendarConflict)
		}
		return h, remoteSQLFailure(ctx)
	}
	if instance != b.instance || device != b.device || source != b.source || len(scope) != 0 && len(scope) != 32 || len(repr) != 0 && len(repr) != 32 || len(digest) != 0 && len(digest) != 32 || !updated.Valid || updated.InfinityModifier != pgtype.Finite || state < 1 || state > 3 || reason < 0 || reason > 7 {
		return h, remoteFailure(RemoteCalendarCorrupt)
	}
	copy(h.source[:], scope)
	copy(h.representation[:], repr)
	copy(h.digest[:], digest)
	h.updated = updated.Time.UTC()
	h.state = RemoteCalendarState(state)
	h.reason = RemoteCalendarReason(reason)
	for _, v := range []pgtype.Timestamptz{validated, attempted, until, modified} {
		if v.Valid && (v.InfinityModifier != pgtype.Finite || !remoteStamp(v.Time)) {
			return h, remoteFailure(RemoteCalendarCorrupt)
		}
	}
	if validated.Valid {
		h.validated = validated.Time.UTC()
	}
	if attempted.Valid {
		h.attempted = attempted.Time.UTC()
	}
	if until.Valid {
		h.freshUntil = until.Time.UTC()
	}
	kind := RemoteValidatorsNone
	if etag != nil {
		kind = RemoteValidatorsETag
	}
	if modified.Valid {
		if etag != nil {
			kind = RemoteValidatorsBoth
		} else {
			kind = RemoteValidatorsLastModified
		}
	}
	h.validators = RemoteCalendarValidators{kind: kind, etag: string(etag)}
	if modified.Valid {
		h.validators.modified = modified.Time.UTC()
	}
	h.version = version.Int16
	h.profile = profile.Int16
	if h.state == RemoteCalendarNeverValidated {
		if version.Valid || profile.Valid || digest != nil || repr != nil {
			return h, remoteFailure(RemoteCalendarCorrupt)
		}
		for _, v := range counts {
			if v.Valid {
				return h, remoteFailure(RemoteCalendarCorrupt)
			}
		}
	} else {
		if !version.Valid || !profile.Valid {
			return h, remoteFailure(RemoteCalendarCorrupt)
		}
		for _, v := range counts {
			if !v.Valid {
				return h, remoteFailure(RemoteCalendarCorrupt)
			}
		}
		h.counts = RemoteCalendarCounts{Events: counts[0].Int64, Zones: counts[1].Int64, Points: counts[2].Int64, Exceptions: counts[3].Int64, TextBytes: counts[4].Int64, SourceBytes: counts[5].Int64, Observances: counts[6].Int64, RDates: counts[7].Int64}
		h.charge = counts[8].Int64
	}
	if metadataPresent != (metadataLength > 0) || metadataLength < 0 || metadataLength > RemoteCalendarHTTPMetadataLimit || metadataLength > 0 && metadataLength < 69 || conditional < 1 || conditional > 2 {
		return h, remoteFailure(RemoteCalendarCorrupt)
	}
	h.conditional = RemoteCalendarConditionalDisposition(conditional)
	// The scalar bound is checked under the same row lock before the bytea
	// projection is allowed to allocate. No untrusted size is used as credit.
	if metadataLength > 0 {
		var data []byte
		if e := tx.QueryRow(ctx, `SELECT http_metadata FROM public.remote_calendar_cache WHERE entity_id=$1 AND revision=$2`, entity, h.revision).Scan(&data); e != nil {
			return h, remoteSQLFailure(ctx)
		}
		if int64(len(data)) != metadataLength {
			return h, remoteFailure(RemoteCalendarCorrupt)
		}
		var e error
		h.http, e = DecodeRemoteCalendarHTTPMetadata(data)
		if e != nil {
			return h, e
		}
	}
	if !h.Valid() {
		return h, remoteFailure(RemoteCalendarCorrupt)
	}
	return h, nil
}
func ReadRemoteCalendarHeader(ctx context.Context, tx pgx.Tx, b RemoteCalendarBinding, write bool) (RemoteCalendarHeader, error) {
	if err := remoteContext(ctx); err != nil {
		return RemoteCalendarHeader{}, err
	}
	if remoteNil(tx) || !b.Valid() {
		return RemoteCalendarHeader{}, remoteFailure(RemoteCalendarInvalid)
	}
	return scanRemoteHeader(ctx, tx, b, write)
}

// Must be called under the header's identity/cache locks after calendars has
// acquired its full preflight reserve. The only returned allocation is bytea.
func ReadRemoteCalendarPayload(ctx context.Context, tx pgx.Tx, h RemoteCalendarHeader) ([]byte, error) {
	if err := remoteContext(ctx); err != nil {
		return nil, err
	}
	if remoteNil(tx) || !h.Valid() || h.state == RemoteCalendarNeverValidated {
		return nil, remoteFailure(RemoteCalendarInvalid)
	}
	entity, _ := h.binding.entity.Value()
	var data []byte
	if err := tx.QueryRow(ctx, `SELECT payload FROM public.remote_calendar_cache WHERE entity_id=$1 AND revision=$2`, entity, h.revision).Scan(&data); err != nil {
		return nil, remoteSQLFailure(ctx)
	}
	if int64(len(data)) != h.length || sha256.Sum256(data) != h.digest {
		return nil, remoteFailure(RemoteCalendarCorrupt)
	}
	return data, nil
}
func EnsureRemoteCalendar(ctx context.Context, tx pgx.Tx, b RemoteCalendarBinding, name string) (RemoteCalendarHeader, error) {
	if err := remoteContext(ctx); err != nil {
		return RemoteCalendarHeader{}, err
	}
	if remoteNil(tx) || !b.Valid() || !remoteText(name, 512, true) {
		return RemoteCalendarHeader{}, remoteFailure(RemoteCalendarInvalid)
	}
	actual, err := ReadRemoteCalendarBinding(ctx, tx, b.entity, true)
	if err != nil {
		return RemoteCalendarHeader{}, err
	}
	if actual != b {
		return RemoteCalendarHeader{}, remoteFailure(RemoteCalendarConflict)
	}
	entity, _ := b.entity.Value()
	if _, err := tx.Exec(ctx, `INSERT INTO public.remote_calendar_cache(entity_id,instance_id,source_device,source_entity,origin,name,revision,updated_at,state,reason)
        VALUES($1,$2,$3,$4,2,$5,1,clock_timestamp(),1,0) ON CONFLICT(entity_id) DO NOTHING`, entity, b.instance, b.device, b.source, name); err != nil {
		return RemoteCalendarHeader{}, remoteSQLFailure(ctx)
	}
	return scanRemoteHeader(ctx, tx, b, true)
}

type RemoteCalendarExpected struct {
	binding  RemoteCalendarBinding
	revision int64
	source   [32]byte
}

func ExpectRemoteCalendar(h RemoteCalendarHeader, s RemoteCalendarScope) (RemoteCalendarExpected, error) {
	if !h.Valid() || !s.Valid() || h.source != s.key {
		return RemoteCalendarExpected{}, remoteFailure(RemoteCalendarConflict)
	}
	return RemoteCalendarExpected{binding: h.binding, revision: h.revision, source: s.key}, nil
}

type RemoteCalendarCommit struct {
	binding                     RemoteCalendarBinding
	source, digest, observation [32]byte
	revision                    int64
}

func (c RemoteCalendarCommit) Valid() bool {
	return c.binding.Valid() && remoteKey(c.source) && remoteKey(c.digest) && remoteKey(c.observation) && c.revision > 0
}
func (c RemoteCalendarCommit) Entity() (domain.EntityID, bool) { return c.binding.entity, c.Valid() }
func (c RemoteCalendarCommit) Revision() (int64, bool)         { return c.revision, c.Valid() }
func (c RemoteCalendarCommit) Digest() ([32]byte, bool)        { return c.digest, c.Valid() }
func (c RemoteCalendarCommit) Matches(entity domain.EntityID, source, observation [32]byte) bool {
	return c.Valid() && c.binding.entity == entity && c.source == source && c.observation == observation
}
func (c RemoteCalendarCommit) String() string               { return "remote calendar commit [redacted]" }
func (c RemoteCalendarCommit) GoString() string             { return c.String() }
func (c RemoteCalendarCommit) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func (s *Store) remoteTransaction(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	if err := remoteContext(ctx); err != nil {
		return err
	}
	var callback error
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	outcome, err := s.InTransactionOutcome(bounded, func(tx pgx.Tx) error { callback = fn(bounded, tx); return callback })
	if outcome == TransactionIndeterminate {
		return remoteFailure(RemoteCalendarCommitUnknown)
	}
	if err != nil {
		if callback != nil {
			return callback
		}
		if bounded.Err() != nil {
			return bounded.Err()
		}
		return remoteFailure(RemoteCalendarUnavailable)
	}
	if outcome != TransactionCommitted {
		return remoteFailure(RemoteCalendarUnavailable)
	}
	return nil
}
func remoteNext(n int64) (int64, error) {
	if n <= 0 {
		return 0, remoteFailure(RemoteCalendarCorrupt)
	}
	if n == math.MaxInt64 {
		return 0, remoteFailure(RemoteCalendarRevisionExhausted)
	}
	return n + 1, nil
}
func lockedRemoteExpected(ctx context.Context, tx pgx.Tx, e RemoteCalendarExpected, scope RemoteCalendarScope) (RemoteCalendarHeader, error) {
	if !e.binding.Valid() || e.revision <= 0 || e.source != scope.key {
		return RemoteCalendarHeader{}, remoteFailure(RemoteCalendarInvalid)
	}
	b, err := ReadRemoteCalendarBinding(ctx, tx, e.binding.entity, true)
	if err != nil {
		return RemoteCalendarHeader{}, err
	}
	if b != e.binding {
		return RemoteCalendarHeader{}, remoteFailure(RemoteCalendarConflict)
	}
	h, err := scanRemoteHeader(ctx, tx, b, true)
	if err != nil {
		return h, err
	}
	if h.revision != e.revision || h.source != e.source {
		return h, remoteFailure(RemoteCalendarConflict)
	}
	if err := checkRemoteCredentials(ctx, tx, b, scope); err != nil {
		return h, err
	}
	return h, nil
}
func (s *Store) ConfigureRemoteCalendar(ctx context.Context, b RemoteCalendarBinding, scope RemoteCalendarScope, name string) (RemoteCalendarHeader, error) {
	if !b.Valid() || !scope.Valid() || !remoteText(name, 512, true) {
		return RemoteCalendarHeader{}, remoteFailure(RemoteCalendarInvalid)
	}
	var result RemoteCalendarHeader
	err := s.remoteTransaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		actual, e := ReadRemoteCalendarBinding(ctx, tx, b.entity, true)
		if e != nil {
			return e
		}
		if actual != b {
			return remoteFailure(RemoteCalendarConflict)
		}
		h, e := scanRemoteHeader(ctx, tx, b, true)
		if e != nil {
			return e
		}
		if e = checkRemoteCredentials(ctx, tx, b, scope); e != nil {
			return e
		}
		if h.source == scope.key && h.name == name {
			result = h
			return nil
		}
		next, e := remoteNext(h.revision)
		if e != nil {
			return e
		}
		entity, _ := b.entity.Value()
		if h.source == scope.key {
			_, e = tx.Exec(ctx, `UPDATE public.remote_calendar_cache SET name=$2,revision=$3,updated_at=clock_timestamp() WHERE entity_id=$1`, entity, name, next)
		} else {
			_, e = tx.Exec(ctx, `UPDATE public.remote_calendar_cache SET source_scope=$2,name=$3,revision=$4,updated_at=clock_timestamp(),state=CASE WHEN state=1 THEN 1 ELSE 3 END,reason=5,etag=NULL,last_modified=NULL,http_metadata=NULL,conditional_state=2 WHERE entity_id=$1`, entity, scope.key[:], name, next)
		}
		if e != nil {
			return remoteSQLFailure(ctx)
		}
		result, e = scanRemoteHeader(ctx, tx, b, true)
		return e
	})
	if err != nil {
		return RemoteCalendarHeader{}, err
	}
	return result, nil
}
func validatorSQL(v RemoteCalendarValidators) (any, any) {
	var etag, modified any
	if s, ok := v.ETag(); ok {
		etag = []byte(s)
	}
	if t, ok := v.LastModified(); ok {
		modified = t
	}
	return etag, modified
}
func (s *Store) ReplaceRemoteCalendar(ctx context.Context, expected RemoteCalendarExpected, o RemoteCalendarObservation, p RemoteCalendarPayload) (RemoteCalendarCommit, RemoteCalendarHeader, error) {
	if !o.Valid() || o.http.Present() && o.admission.kind != remoteHTTPRepresentation || len(p.data) < 20 || !p.counts.Valid() || p.charge < remoteMinimumCharge(int64(len(p.data)), p.counts) || p.charge > 33554432 || sha256.Sum256(p.data) != p.digest {
		return RemoteCalendarCommit{}, RemoteCalendarHeader{}, remoteFailure(RemoteCalendarInvalid)
	}
	var result RemoteCalendarHeader
	err := s.remoteTransaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		h, e := lockedRemoteExpected(ctx, tx, expected, o.source)
		if e != nil {
			return e
		}
		next, e := remoteNext(h.revision)
		if e != nil {
			return e
		}
		entity, _ := h.binding.entity.Value()
		etag, modified := validatorSQL(o.validators)
		_, e = tx.Exec(ctx, `UPDATE public.remote_calendar_cache SET revision=$2,updated_at=clock_timestamp(),state=2,representation_scope=$3,validated_at=$4,attempted_at=$4,fresh_until=$5,reason=0,etag=$6,last_modified=$7,payload_version=1,payload_profile=1,payload=$8,digest=$9,event_count=$10,zone_count=$11,point_count=$12,exception_count=$13,text_bytes=$14,source_bytes=$15,observance_count=$16,rdate_count=$17,image_charge=$18,http_metadata=$19,conditional_state=$20 WHERE entity_id=$1`, entity, next, o.source.key[:], o.at, o.freshUntil, etag, modified, p.data, p.digest[:], p.counts.Events, p.counts.Zones, p.counts.Points, p.counts.Exceptions, p.counts.TextBytes, p.counts.SourceBytes, p.counts.Observances, p.counts.RDates, p.charge, observationHTTPSQL(o), observationConditionalSQL(o))
		if e != nil {
			return remoteSQLFailure(ctx)
		}
		result, e = scanRemoteHeader(ctx, tx, h.binding, true)
		return e
	})
	if err != nil {
		return RemoteCalendarCommit{}, RemoteCalendarHeader{}, err
	}
	return RemoteCalendarCommit{binding: result.binding, source: o.source.key, digest: result.digest, observation: o.id, revision: result.revision}, result, nil
}
func (s *Store) RevalidateRemoteCalendar(ctx context.Context, expected RemoteCalendarExpected, o RemoteCalendarObservation, sent RemoteCalendarValidators, digest [32]byte) (RemoteCalendarCommit, RemoteCalendarHeader, error) {
	if !o.Valid() || o.http.Present() && (o.admission.kind != remoteHTTPRevalidation || o.admission.digest != digest) || !sent.Valid() || sent.kind == RemoteValidatorsNone || !remoteKey(digest) {
		return RemoteCalendarCommit{}, RemoteCalendarHeader{}, remoteFailure(RemoteCalendarInvalid)
	}
	var result RemoteCalendarHeader
	err := s.remoteTransaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		h, e := lockedRemoteExpected(ctx, tx, expected, o.source)
		if e != nil {
			return e
		}
		if h.state == RemoteCalendarNeverValidated || h.representation != o.source.key || h.digest != digest || !h.validators.Equal(sent) || o.http.Present() && (h.conditional != PreserveAdmittedValidators || !h.http.Present()) {
			return remoteFailure(RemoteCalendarConflict)
		}
		next, e := remoteNext(h.revision)
		if e != nil {
			return e
		}
		entity, _ := h.binding.entity.Value()
		etag, modified := validatorSQL(o.validators)
		_, e = tx.Exec(ctx, `UPDATE public.remote_calendar_cache SET revision=$2,updated_at=clock_timestamp(),state=2,validated_at=$3,attempted_at=$3,fresh_until=$4,reason=0,etag=$5,last_modified=$6,http_metadata=$7,conditional_state=$8 WHERE entity_id=$1`, entity, next, o.at, o.freshUntil, etag, modified, observationHTTPSQL(o), observationConditionalSQL(o))
		if e != nil {
			return remoteSQLFailure(ctx)
		}
		result, e = scanRemoteHeader(ctx, tx, h.binding, true)
		return e
	})
	if err != nil {
		return RemoteCalendarCommit{}, RemoteCalendarHeader{}, err
	}
	return RemoteCalendarCommit{binding: result.binding, source: o.source.key, digest: result.digest, observation: o.id, revision: result.revision}, result, nil
}
func (s *Store) DegradeRemoteCalendar(ctx context.Context, expected RemoteCalendarExpected, scope RemoteCalendarScope, at time.Time, reason RemoteCalendarReason) (RemoteCalendarHeader, error) {
	return s.RejectRemoteCalendar(ctx, expected, scope, at, reason, PreserveAdmittedValidators)
}
func (s *Store) RejectRemoteCalendar(ctx context.Context, expected RemoteCalendarExpected, scope RemoteCalendarScope, at time.Time, reason RemoteCalendarReason, disposition RemoteCalendarConditionalDisposition) (RemoteCalendarHeader, error) {
	if !disposition.Valid() || !scope.Valid() || !remoteStamp(at) || !reason.Valid() || reason == RemoteCalendarReasonNone {
		return RemoteCalendarHeader{}, remoteFailure(RemoteCalendarInvalid)
	}
	var result RemoteCalendarHeader
	err := s.remoteTransaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		h, e := lockedRemoteExpected(ctx, tx, expected, scope)
		if e != nil {
			return e
		}
		next, e := remoteNext(h.revision)
		if e != nil {
			return e
		}
		entity, _ := h.binding.entity.Value()
		_, e = tx.Exec(ctx, `UPDATE public.remote_calendar_cache SET revision=$2,updated_at=clock_timestamp(),state=CASE WHEN state=1 THEN 1 ELSE 3 END,attempted_at=$3,reason=$4,conditional_state=CASE WHEN $5=2 THEN 2 ELSE conditional_state END,etag=CASE WHEN $5=2 THEN NULL ELSE etag END,last_modified=CASE WHEN $5=2 THEN NULL ELSE last_modified END WHERE entity_id=$1`, entity, next, at, int16(reason), int16(disposition))
		if e != nil {
			return remoteSQLFailure(ctx)
		}
		result, e = scanRemoteHeader(ctx, tx, h.binding, true)
		return e
	})
	if err != nil {
		return RemoteCalendarHeader{}, err
	}
	return result, nil
}

func observationHTTPSQL(o RemoteCalendarObservation) any {
	data, _ := o.http.Encode()
	if data == nil {
		return nil
	}
	return data
}
func observationConditionalSQL(o RemoteCalendarObservation) int16 {
	if o.http.Present() {
		return int16(PreserveAdmittedValidators)
	}
	return int16(RequireFullRepresentation)
}

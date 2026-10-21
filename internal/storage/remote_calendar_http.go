package storage

import (
	"encoding/binary"
	"slices"
	"strings"
	"time"

	"github.com/damione1/Homeward/internal/domain"
)

const RemoteCalendarHTTPMetadataLimit = 32 << 10

// The persisted vocabulary is deliberately restricted to representation cache
// policy. Authorization, URLs, cookies and arbitrary headers have no variant.
type RemoteHTTPFieldKind uint8

const (
	RemoteHTTPCacheControl RemoteHTTPFieldKind = iota + 1
	RemoteHTTPVary
	RemoteHTTPDate
	RemoteHTTPAge
	RemoteHTTPExpires
)

type RemoteHTTPPresence uint8

const (
	RemoteHTTPAbsent RemoteHTTPPresence = iota + 1
	RemoteHTTPPresent
)

type RemoteHTTPField struct {
	kind     RemoteHTTPFieldKind
	presence RemoteHTTPPresence
	values   []string
}

func NewRemoteHTTPField(kind RemoteHTTPFieldKind, presence RemoteHTTPPresence, values []string) (RemoteHTTPField, error) {
	f := RemoteHTTPField{kind: kind, presence: presence, values: values}
	if !f.valid() {
		return RemoteHTTPField{}, remoteFailure(RemoteCalendarInvalid)
	}
	f.values = slices.Clone(values)
	for i := range f.values {
		f.values[i] = strings.Clone(f.values[i])
	}
	return f, nil
}
func (f RemoteHTTPField) valid() bool {
	if f.kind < RemoteHTTPCacheControl || f.kind > RemoteHTTPExpires {
		return false
	}
	switch f.presence {
	case RemoteHTTPAbsent:
		return len(f.values) == 0
	case RemoteHTTPPresent:
		if len(f.values) == 0 || len(f.values) > RemoteCalendarHTTPMetadataLimit/2 {
			return false
		}
		size := 0
		for _, v := range f.values {
			if len(v) > RemoteCalendarHTTPMetadataLimit || strings.ContainsAny(v, "\x00\r\n") {
				return false
			}
			size += len(v) + 2
			if size > RemoteCalendarHTTPMetadataLimit {
				return false
			}
		}
		return true
	default:
		return false
	}
}
func (f RemoteHTTPField) Values() ([]string, bool) {
	return slices.Clone(f.values), f.valid() && f.presence == RemoteHTTPPresent
}
func (f RemoteHTTPField) String() string               { return "remote HTTP field [redacted]" }
func (f RemoteHTTPField) GoString() string             { return f.String() }
func (f RemoteHTTPField) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

type remoteHTTPMetadataKind uint8

const (
	remoteHTTPMetadataAbsent remoteHTTPMetadataKind = iota + 1
	remoteHTTPMetadataStored
)

type RemoteCalendarHTTPMetadata struct {
	kind       remoteHTTPMetadataKind
	profile    [32]byte
	fields     [5]RemoteHTTPField
	received   time.Time
	initialAge time.Duration
}

func NoRemoteCalendarHTTPMetadata() RemoteCalendarHTTPMetadata {
	return RemoteCalendarHTTPMetadata{kind: remoteHTTPMetadataAbsent}
}
func NewRemoteCalendarHTTPMetadata(profile [32]byte, fields []RemoteHTTPField, received time.Time, initialAge time.Duration) (RemoteCalendarHTTPMetadata, error) {
	if !remoteKey(profile) || len(fields) != 5 || !remoteStamp(received) || initialAge < 0 || initialAge > 24*time.Hour {
		return RemoteCalendarHTTPMetadata{}, remoteFailure(RemoteCalendarInvalid)
	}
	m := RemoteCalendarHTTPMetadata{kind: remoteHTTPMetadataStored, profile: profile, received: received.UTC(), initialAge: initialAge}
	size := 54
	for i, f := range fields {
		if !f.valid() || f.kind != RemoteHTTPFieldKind(i+1) {
			return RemoteCalendarHTTPMetadata{}, remoteFailure(RemoteCalendarInvalid)
		}
		size += 3
		for _, v := range f.values {
			size += 2 + len(v)
			if size > RemoteCalendarHTTPMetadataLimit {
				return RemoteCalendarHTTPMetadata{}, remoteFailure(RemoteCalendarInvalid)
			}
		}
	}
	if size > RemoteCalendarHTTPMetadataLimit {
		return RemoteCalendarHTTPMetadata{}, remoteFailure(RemoteCalendarInvalid)
	}
	for i, f := range fields {
		m.fields[i], _ = NewRemoteHTTPField(f.kind, f.presence, f.values)
	}
	return m, nil
}
func (m RemoteCalendarHTTPMetadata) Valid() bool {
	switch m.kind {
	case remoteHTTPMetadataAbsent:
		if m.profile != ([32]byte{}) || !m.received.IsZero() || m.initialAge != 0 {
			return false
		}
		for _, f := range m.fields {
			if f.kind != 0 || f.presence != 0 || len(f.values) != 0 {
				return false
			}
		}
		return true
	case remoteHTTPMetadataStored:
		if !remoteKey(m.profile) || !remoteStamp(m.received) || m.initialAge < 0 || m.initialAge > 24*time.Hour {
			return false
		}
		size := 54
		for i, f := range m.fields {
			if !f.valid() || f.kind != RemoteHTTPFieldKind(i+1) {
				return false
			}
			size += 3
			for _, v := range f.values {
				size += 2 + len(v)
				if size > RemoteCalendarHTTPMetadataLimit {
					return false
				}
			}
		}
		return size <= RemoteCalendarHTTPMetadataLimit
	default:
		return false
	}
}
func (m RemoteCalendarHTTPMetadata) Present() bool {
	return m.Valid() && m.kind == remoteHTTPMetadataStored
}
func (m RemoteCalendarHTTPMetadata) Profile() ([32]byte, bool) { return m.profile, m.Present() }
func (m RemoteCalendarHTTPMetadata) AgeAnchor() (time.Time, time.Duration, bool) {
	return m.received, m.initialAge, m.Present()
}
func (m RemoteCalendarHTTPMetadata) Field(kind RemoteHTTPFieldKind) RemoteHTTPField {
	if !m.Present() || kind < 1 || kind > 5 {
		return RemoteHTTPField{}
	}
	return m.fields[int(kind)-1]
}
func (m RemoteCalendarHTTPMetadata) String() string               { return "remote HTTP metadata [redacted]" }
func (m RemoteCalendarHTTPMetadata) GoString() string             { return m.String() }
func (m RemoteCalendarHTTPMetadata) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func (m RemoteCalendarHTTPMetadata) Encode() ([]byte, error) {
	if !m.Valid() {
		return nil, remoteFailure(RemoteCalendarInvalid)
	}
	if !m.Present() {
		return nil, nil
	}
	size := 54
	for _, f := range m.fields {
		size += 3
		for _, v := range f.values {
			size += 2 + len(v)
		}
	}
	out := make([]byte, 0, size)
	out = append(out, 1, byte(m.kind))
	out = append(out, m.profile[:]...)
	out = binary.BigEndian.AppendUint64(out, uint64(m.received.Unix()))
	out = binary.BigEndian.AppendUint32(out, uint32(m.received.Nanosecond()))
	out = binary.BigEndian.AppendUint64(out, uint64(m.initialAge))
	for _, f := range m.fields {
		out = append(out, byte(f.presence))
		out = binary.BigEndian.AppendUint16(out, uint16(len(f.values)))
		for _, v := range f.values {
			out = binary.BigEndian.AppendUint16(out, uint16(len(v)))
			out = append(out, v...)
		}
	}
	return out, nil
}
func DecodeRemoteCalendarHTTPMetadata(data []byte) (RemoteCalendarHTTPMetadata, error) {
	if len(data) == 0 {
		return NoRemoteCalendarHTTPMetadata(), nil
	}
	if len(data) < 69 || len(data) > RemoteCalendarHTTPMetadataLimit || data[0] != 1 || data[1] != byte(remoteHTTPMetadataStored) {
		return RemoteCalendarHTTPMetadata{}, remoteFailure(RemoteCalendarCorrupt)
	}
	var profile [32]byte
	copy(profile[:], data[2:34])
	received := time.Unix(int64(binary.BigEndian.Uint64(data[34:42])), int64(binary.BigEndian.Uint32(data[42:46]))).UTC()
	age := binary.BigEndian.Uint64(data[46:54])
	if age > uint64(24*time.Hour) || binary.BigEndian.Uint32(data[42:46]) >= 1000000000 {
		return RemoteCalendarHTTPMetadata{}, remoteFailure(RemoteCalendarCorrupt)
	}
	pos := 54
	fields := make([]RemoteHTTPField, 0, 5)
	for i := 0; i < 5; i++ {
		if len(data)-pos < 3 {
			return RemoteCalendarHTTPMetadata{}, remoteFailure(RemoteCalendarCorrupt)
		}
		presence := RemoteHTTPPresence(data[pos])
		n := int(binary.BigEndian.Uint16(data[pos+1 : pos+3]))
		pos += 3
		if n > (len(data)-pos)/2 || n > RemoteCalendarHTTPMetadataLimit/2 {
			return RemoteCalendarHTTPMetadata{}, remoteFailure(RemoteCalendarCorrupt)
		}
		// Lengths are preflighted before the collection allocation or copies.
		end := pos
		for j := 0; j < n; j++ {
			if len(data)-end < 2 {
				return RemoteCalendarHTTPMetadata{}, remoteFailure(RemoteCalendarCorrupt)
			}
			l := int(binary.BigEndian.Uint16(data[end : end+2]))
			end += 2
			if l > len(data)-end {
				return RemoteCalendarHTTPMetadata{}, remoteFailure(RemoteCalendarCorrupt)
			}
			end += l
		}
		values := make([]string, 0, n)
		for j := 0; j < n; j++ {
			l := int(binary.BigEndian.Uint16(data[pos : pos+2]))
			pos += 2
			values = append(values, string(data[pos:pos+l]))
			pos += l
		}
		f, e := NewRemoteHTTPField(RemoteHTTPFieldKind(i+1), presence, values)
		if e != nil {
			return RemoteCalendarHTTPMetadata{}, remoteFailure(RemoteCalendarCorrupt)
		}
		fields = append(fields, f)
	}
	if pos != len(data) {
		return RemoteCalendarHTTPMetadata{}, remoteFailure(RemoteCalendarCorrupt)
	}
	m, e := NewRemoteCalendarHTTPMetadata(profile, fields, received, time.Duration(age))
	if e != nil {
		return RemoteCalendarHTTPMetadata{}, remoteFailure(RemoteCalendarCorrupt)
	}
	return m, nil
}

type RemoteCalendarConditionalDisposition uint8

const (
	PreserveAdmittedValidators RemoteCalendarConditionalDisposition = iota + 1
	RequireFullRepresentation
)

func (d RemoteCalendarConditionalDisposition) Valid() bool {
	return d == PreserveAdmittedValidators || d == RequireFullRepresentation
}

// Selection can only be obtained from an admitted, durable cache header. It
// contains no payload or secret and cannot manufacture a commit receipt.
type RemoteCalendarSelection struct{ header RemoteCalendarHeader }

func (h RemoteCalendarHeader) Selection() RemoteCalendarSelection {
	return RemoteCalendarSelection{header: h}
}
func (s RemoteCalendarSelection) Valid() bool { return s.header.Valid() }
func (s RemoteCalendarSelection) Matches(entity domain.EntityID, scope RemoteCalendarScope) bool {
	return s.Valid() && scope.Valid() && s.header.binding.entity == entity && s.header.source == scope.key
}
func (s RemoteCalendarSelection) Conditional(scope RemoteCalendarScope) bool {
	h := s.header
	return s.Valid() && scope.Valid() && h.source == scope.key && h.representation == scope.key && h.state != RemoteCalendarNeverValidated && h.conditional == PreserveAdmittedValidators && h.http.Present() && h.validators.kind != RemoteValidatorsNone
}
func (s RemoteCalendarSelection) Validators() RemoteCalendarValidators { return s.header.validators }
func (s RemoteCalendarSelection) Metadata() RemoteCalendarHTTPMetadata { return s.header.http }
func (s RemoteCalendarSelection) Digest() ([32]byte, bool) {
	return s.header.digest, s.Valid() && s.header.state != RemoteCalendarNeverValidated
}
func (s RemoteCalendarSelection) Revision() (int64, bool) { return s.header.revision, s.Valid() }
func (s RemoteCalendarSelection) SourceTuple() (string, string, string, bool) {
	if !s.Valid() {
		return "", "", "", false
	}
	return s.header.binding.SourceTuple()
}
func (s RemoteCalendarSelection) String() string               { return "remote calendar selection [redacted]" }
func (s RemoteCalendarSelection) GoString() string             { return s.String() }
func (s RemoteCalendarSelection) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func (b RemoteCalendarBinding) SourceTuple() (string, string, string, bool) {
	return b.instance, b.device, b.source, b.Valid()
}

type remoteHTTPAdmissionKind uint8

const (
	remoteHTTPRepresentation remoteHTTPAdmissionKind = iota + 1
	remoteHTTPRevalidation
)

type RemoteCalendarHTTPAdmission struct {
	kind   remoteHTTPAdmissionKind
	digest [32]byte
}

func RemoteCalendarHTTPRepresentation(bodyDigest [32]byte) (RemoteCalendarHTTPAdmission, error) {
	if !remoteKey(bodyDigest) {
		return RemoteCalendarHTTPAdmission{}, remoteFailure(RemoteCalendarInvalid)
	}
	return RemoteCalendarHTTPAdmission{kind: remoteHTTPRepresentation, digest: bodyDigest}, nil
}
func RemoteCalendarHTTPRevalidation(selectedDigest [32]byte) (RemoteCalendarHTTPAdmission, error) {
	if !remoteKey(selectedDigest) {
		return RemoteCalendarHTTPAdmission{}, remoteFailure(RemoteCalendarInvalid)
	}
	return RemoteCalendarHTTPAdmission{kind: remoteHTTPRevalidation, digest: selectedDigest}, nil
}
func (a RemoteCalendarHTTPAdmission) valid() bool {
	return (a.kind == remoteHTTPRepresentation || a.kind == remoteHTTPRevalidation) && remoteKey(a.digest)
}
func (o RemoteCalendarObservation) AdmitsRepresentation(bodyDigest [32]byte) bool {
	return o.Valid() && (!o.http.Present() || o.admission.kind == remoteHTTPRepresentation && o.admission.digest == bodyDigest)
}
func NewRemoteCalendarHTTPObservation(source RemoteCalendarScope, id [32]byte, at, until time.Time, validators RemoteCalendarValidators, metadata RemoteCalendarHTTPMetadata, admission RemoteCalendarHTTPAdmission) (RemoteCalendarObservation, error) {
	o, e := NewRemoteCalendarObservation(source, id, at, until, validators)
	if e != nil || !metadata.Present() || !admission.valid() {
		return RemoteCalendarObservation{}, remoteFailure(RemoteCalendarInvalid)
	}
	o.http = metadata
	o.admission = admission
	return o, nil
}

package observability

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/gen/home/observability/v1/observabilityv1connect"
)

const cursorTTL = 60 * time.Second

type CursorCodec struct {
	key     [32]byte
	epoch   ProcessObservationEpoch
	elapsed func() time.Duration
}

func (c CursorCodec) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("observability: cursor codec"))
}
func (c CursorCodec) String() string   { return "observability: cursor codec" }
func (c CursorCodec) GoString() string { return c.String() }
func (c CursorCodec) MarshalJSON() ([]byte, error) {
	return []byte(`"observability: cursor codec"`), nil
}
func NewCursorCodec(epoch ProcessObservationEpoch) (*CursorCodec, error) {
	if !epoch.valid() {
		return nil, invalid(obs.ObservabilityField_OBSERVABILITY_FIELD_PROCESS_EPOCH)
	}
	origin := time.Now()
	codec := &CursorCodec{epoch: epoch, elapsed: func() time.Duration { return time.Since(origin) }}
	if _, err := rand.Read(codec.key[:]); err != nil || codec.key == [32]byte{} {
		return nil, failure(obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_UNAVAILABLE, obs.ObservabilityField_OBSERVABILITY_FIELD_PAGE_TOKEN)
	}
	return codec, nil
}
func (c *CursorCodec) valid() bool {
	return c != nil && c.epoch.valid() && c.key != [32]byte{} && c.elapsed != nil
}
func (c *CursorCodec) tick() (time.Duration, error) {
	if !c.valid() {
		return 0, stale()
	}
	now := c.elapsed()
	if now < 0 || now > time.Duration(math.MaxInt64)-cursorTTL {
		return 0, stale()
	}
	return now, nil
}
func (c *CursorCodec) expiry() (time.Duration, error) {
	now, err := c.tick()
	if err != nil {
		return 0, err
	}
	return now + cursorTTL, nil
}
func (c *CursorCodec) aead() (cipher.AEAD, error) {
	if !c.valid() {
		return nil, stale()
	}
	block, err := aes.NewCipher(c.key[:])
	if err != nil {
		return nil, stale()
	}
	return cipher.NewGCM(block)
}
func (c *CursorCodec) seal(procedure string, payload []byte) (string, error) {
	aead, err := c.aead()
	if err != nil {
		return "", err
	}
	if len(payload) > 256 {
		return "", stale()
	}
	var nonce [12]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return "", failure(obs.ObservabilityErrorCode_OBSERVABILITY_ERROR_CODE_UNAVAILABLE, obs.ObservabilityField_OBSERVABILITY_FIELD_PAGE_TOKEN)
	}
	sealed := make([]byte, 1+len(nonce), 1+len(nonce)+len(payload)+aead.Overhead())
	sealed[0] = 1
	copy(sealed[1:], nonce[:])
	sealed = aead.Seal(sealed, nonce[:], payload, []byte(procedure))
	if base64.RawURLEncoding.EncodedLen(len(sealed)) > 512 {
		return "", stale()
	}
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}
func (c *CursorCodec) open(procedure, token string) ([]byte, error) {
	if token == "" || len(token) > 512 {
		return nil, stale()
	}
	aead, err := c.aead()
	if err != nil {
		return nil, err
	}
	sealed, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(sealed) < 1+aead.NonceSize()+aead.Overhead() || sealed[0] != 1 {
		return nil, stale()
	}
	payload, err := aead.Open(nil, sealed[1:1+aead.NonceSize()], sealed[1+aead.NonceSize():], []byte(procedure))
	if err != nil || len(payload) > 256 {
		return nil, stale()
	}
	return payload, nil
}
func appendNumber(dst []byte, value uint64) []byte {
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], value)
	return append(dst, raw[:]...)
}
func (c *CursorCodec) base(binding ReadBinding, deadline time.Duration) ([]byte, error) {
	now, err := c.tick()
	if err != nil || !binding.valid() || deadline <= now || deadline-now > cursorTTL {
		return nil, stale()
	}
	payload := make([]byte, 0, 128)
	payload = append(payload, c.epoch.value[:]...)
	payload = appendNumber(payload, binding.revision)
	payload = append(payload, binding.digest[:]...)
	payload = appendNumber(payload, uint64(deadline))
	return payload, nil
}
func (c *CursorCodec) checkBase(payload []byte, binding ReadBinding) (time.Duration, error) {
	now, err := c.tick()
	if err != nil || len(payload) < 64 || !binding.valid() || !bytes.Equal(payload[:16], c.epoch.value[:]) || binary.BigEndian.Uint64(payload[16:24]) != binding.revision || !bytes.Equal(payload[24:56], binding.digest[:]) {
		return 0, stale()
	}
	raw := binary.BigEndian.Uint64(payload[56:64])
	if raw > math.MaxInt64 {
		return 0, stale()
	}
	deadline := time.Duration(raw)
	if deadline <= now || deadline-now > cursorTTL {
		return 0, stale()
	}
	return deadline, nil
}

type LogCursor struct {
	epoch       ProcessObservationEpoch
	binding     ReadBinding
	upper, next uint64
	deadline    time.Duration
	first       bool
}

func (c *CursorCodec) BeginLogs(binding ReadBinding, upper uint64) (LogCursor, error) {
	deadline, err := c.expiry()
	if err != nil || !binding.valid() {
		return LogCursor{}, stale()
	}
	return LogCursor{epoch: c.epoch, binding: binding, upper: upper, deadline: deadline, first: true}, nil
}
func (c *CursorCodec) EncodeLogs(cursor LogCursor) (string, error) {
	if !c.valid() || cursor.epoch != c.epoch || cursor.first || cursor.next == 0 || cursor.next > cursor.upper {
		return "", stale()
	}
	payload, err := c.base(cursor.binding, cursor.deadline)
	if err != nil {
		return "", err
	}
	payload = appendNumber(payload, cursor.upper)
	payload = appendNumber(payload, cursor.next)
	return c.seal(observabilityv1connect.ObservabilityServiceQueryRuntimeLogsProcedure, payload)
}
func (c *CursorCodec) DecodeLogs(token string, binding ReadBinding) (LogCursor, error) {
	payload, err := c.open(observabilityv1connect.ObservabilityServiceQueryRuntimeLogsProcedure, token)
	if err != nil || len(payload) != 80 {
		return LogCursor{}, stale()
	}
	deadline, err := c.checkBase(payload, binding)
	if err != nil {
		return LogCursor{}, err
	}
	upper, next := binary.BigEndian.Uint64(payload[64:72]), binary.BigEndian.Uint64(payload[72:80])
	if next == 0 || next > upper {
		return LogCursor{}, stale()
	}
	return LogCursor{epoch: c.epoch, binding: binding, upper: upper, next: next, deadline: deadline}, nil
}

type AuditCursor struct {
	epoch         ProcessObservationEpoch
	binding       ReadBinding
	source        obs.AuditSource
	upper, before int64
	deadline      time.Duration
	first         bool
}

func auditSource(source obs.AuditSource) bool {
	switch source {
	case obs.AuditSource_AUDIT_SOURCE_AUTHENTICATION, obs.AuditSource_AUDIT_SOURCE_SECRET_ACCESS:
		return true
	case obs.AuditSource_AUDIT_SOURCE_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func (c *CursorCodec) BeginAudits(binding ReadBinding, source obs.AuditSource, upper int64) (AuditCursor, error) {
	deadline, err := c.expiry()
	if err != nil || !binding.valid() || !auditSource(source) || upper < 0 {
		return AuditCursor{}, stale()
	}
	return AuditCursor{epoch: c.epoch, binding: binding, source: source, upper: upper, deadline: deadline, first: true}, nil
}
func (cursor AuditCursor) After(before int64) (AuditCursor, error) {
	if !cursor.epoch.valid() || !cursor.binding.valid() || before <= 0 || before > cursor.upper {
		return AuditCursor{}, stale()
	}
	cursor.before = before
	cursor.first = false
	return cursor, nil
}
func (cursor AuditCursor) Position() (int64, int64, error) {
	if !cursor.epoch.valid() || cursor.first || cursor.before <= 0 || cursor.before > cursor.upper {
		return 0, 0, stale()
	}
	return cursor.upper, cursor.before, nil
}
func (c *CursorCodec) EncodeAudits(cursor AuditCursor) (string, error) {
	if !c.valid() || cursor.epoch != c.epoch || cursor.first || !auditSource(cursor.source) || cursor.before <= 0 || cursor.before > cursor.upper {
		return "", stale()
	}
	payload, err := c.base(cursor.binding, cursor.deadline)
	if err != nil {
		return "", err
	}
	payload = appendNumber(payload, uint64(cursor.source))
	payload = appendNumber(payload, uint64(cursor.upper))
	payload = appendNumber(payload, uint64(cursor.before))
	return c.seal(observabilityv1connect.ObservabilityServiceQueryAuditsProcedure, payload)
}
func (c *CursorCodec) DecodeAudits(token string, binding ReadBinding, source obs.AuditSource) (AuditCursor, error) {
	if !auditSource(source) {
		return AuditCursor{}, stale()
	}
	payload, err := c.open(observabilityv1connect.ObservabilityServiceQueryAuditsProcedure, token)
	if err != nil || len(payload) != 88 {
		return AuditCursor{}, stale()
	}
	deadline, err := c.checkBase(payload, binding)
	if err != nil {
		return AuditCursor{}, err
	}
	if binary.BigEndian.Uint64(payload[64:72]) != uint64(source) {
		return AuditCursor{}, stale()
	}
	upper, before := binary.BigEndian.Uint64(payload[72:80]), binary.BigEndian.Uint64(payload[80:88])
	if upper > math.MaxInt64 || before == 0 || before > upper {
		return AuditCursor{}, stale()
	}
	return AuditCursor{epoch: c.epoch, binding: binding, source: source, upper: int64(upper), before: int64(before), deadline: deadline}, nil
}

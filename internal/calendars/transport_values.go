package calendars

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math"
)

// ExpectedRevision admits only identity-bound transport expectations. An absent
// resource has number zero; all persisted resources have a positive revision.
func ExpectedRevision(id CalendarID, uid EventUID, presence Presence, number uint64) (Revision, error) {
	if number > math.MaxInt64 {
		return Revision{}, failure(InvalidInput)
	}
	r := Revision{calendar: id, uid: uid, presence: presence, number: int64(number)}
	if !r.validFor(id, uid) {
		return Revision{}, failure(InvalidInput)
	}
	return r, nil
}

// CursorCodec is process-local. Restart, credential/policy changes and changing
// the exact query invalidate continuations without retaining server-side pages.
type CursorCodec struct{ key [32]byte }

func NewCursorCodec() (*CursorCodec, error) {
	c := new(CursorCodec)
	if _, err := rand.Read(c.key[:]); err != nil {
		return nil, err
	}
	return c, nil
}
func (c *CursorCodec) Seal(payload []byte, binding [32]byte) (string, error) {
	if c == nil || c.key == ([32]byte{}) || binding == ([32]byte{}) || len(payload) > 4096 {
		return "", failure(InvalidInput)
	}
	mac := hmac.New(sha256.New, c.key[:])
	mac.Write(binding[:])
	mac.Write(payload)
	raw := append(append([]byte(nil), payload...), mac.Sum(nil)...)
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
func (c *CursorCodec) Open(token string, binding [32]byte) ([]byte, error) {
	if c == nil || c.key == ([32]byte{}) || len(token) > 5600 || binding == ([32]byte{}) {
		return nil, failure(StaleCursor)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) < 32 || len(raw) > 4128 {
		return nil, failure(StaleCursor)
	}
	body, signature := raw[:len(raw)-32], raw[len(raw)-32:]
	mac := hmac.New(sha256.New, c.key[:])
	mac.Write(binding[:])
	mac.Write(body)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return nil, failure(StaleCursor)
	}
	return body, nil
}

type cursorImage struct {
	Entity            string
	Revision          uint64
	Window            [32]byte
	Start, UID, Point string
}

func (c *CursorCodec) Encode(cursor Cursor, binding [32]byte) (string, error) {
	if !cursor.set || !cursor.revision.validFor(cursor.calendar, EventUID{}) || cursor.revision.presence != Present {
		return "", failure(InvalidInput)
	}
	entity, _ := cursor.calendar.entity.Value()
	number, _ := cursor.revision.Number()
	raw, err := json.Marshal(cursorImage{entity, number, cursor.window, cursor.after.start, cursor.after.uid, cursor.after.point})
	if err != nil {
		return "", err
	}
	return c.Seal(raw, binding)
}
func (c *CursorCodec) Decode(token string, id CalendarID, w Window, binding [32]byte) (Cursor, error) {
	if token == "" {
		return Cursor{}, nil
	}
	raw, err := c.Open(token, binding)
	if err != nil {
		return Cursor{}, err
	}
	var image cursorImage
	if json.Unmarshal(raw, &image) != nil {
		return Cursor{}, failure(StaleCursor)
	}
	entity, _ := id.entity.Value()
	hash := sha256.Sum256([]byte(w.identity()))
	if !id.Valid() || !w.Valid() || image.Entity != entity || image.Window != hash || image.Revision == 0 || image.Revision > math.MaxInt64 || image.Start == "" || image.UID == "" || image.Point == "" {
		return Cursor{}, failure(StaleCursor)
	}
	return Cursor{set: true, calendar: id, revision: Revision{calendar: id, presence: Present, number: int64(image.Revision)}, window: hash, after: occurrenceOrder{image.Start, image.UID, image.Point}}, nil
}

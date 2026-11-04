package calendars

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Precondition is a closed owner-side CAS condition. Native callers use an
// exact Revision; resource callers use an opaque token compared under SQL locks.
type Precondition interface {
	validFor(CalendarID, EventUID) bool
	matches(Revision) bool
}

func (r Revision) matches(current Revision) bool { return r == current }

type resourceETag struct {
	calendar CalendarID
	uid      EventUID
	token    string
	encode   func(Revision) (string, error)
}

// ExpectedResourceETag admits representation only. The encoder must be pure:
// the repository invokes it on the locked current revision, without provider IO.
// A differently formatted but admitted token is a CAS conflict, never decoded.
func ExpectedResourceETag(id CalendarID, uid EventUID, token string, encode func(Revision) (string, error)) (Precondition, error) {
	if !id.Valid() || !utf8.ValidString(token) || len(token) < 1 || len(token) > 256 || encode == nil {
		return nil, failure(InvalidInput)
	}
	for _, character := range token {
		if unicode.IsControl(character) {
			return nil, failure(InvalidInput)
		}
	}
	return resourceETag{id, uid, strings.Clone(token), encode}, nil
}
func (e resourceETag) validFor(id CalendarID, uid EventUID) bool {
	return e.calendar == id && e.uid == uid && id.Valid() && e.token != "" && e.encode != nil
}
func (e resourceETag) matches(current Revision) bool {
	if !current.validFor(e.calendar, e.uid) || current.presence != Present {
		return false
	}
	token, err := e.encode(current)
	return err == nil && token == e.token
}

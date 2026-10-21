package remotecalendar

import (
	"github.com/damione1/Homeward/internal/calendars"
	"time"
)

// PublicationWindow is produced only by the Source publication admission.
// It preserves the original monotone expiry when a window is renewed locally.
// It grants no writer, HTTP, executor or SQL capability.
type PublicationWindow struct {
	window          calendars.AutomationWindow
	wall, until     time.Time
	mono, untilMono time.Duration
}

func (p PublicationWindow) Window() calendars.AutomationWindow { return p.window }
func (p PublicationWindow) Deadline() (time.Time, time.Time, time.Duration, time.Duration, bool) {
	ok := p.window.Valid() && !p.wall.IsZero() && p.until.After(p.wall) && p.mono >= 0 && p.untilMono > p.mono
	until, present := p.window.RemoteState().FreshUntil()
	freshness, fresh := p.window.RemoteState().Freshness()
	ok = ok && present && fresh && freshness == calendars.RemoteFresh && until.Equal(p.until)
	return p.wall, p.until, p.mono, p.untilMono, ok
}
func (PublicationWindow) String() string                 { return "remote calendar publication [redacted]" }
func (p PublicationWindow) GoString() string             { return p.String() }
func (p PublicationWindow) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

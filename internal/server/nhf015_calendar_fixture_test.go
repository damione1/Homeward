package server

import (
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	cv "github.com/damione1/Homeward/gen/home/calendars/v1"
	"github.com/damione1/Homeward/internal/calendars"
	"google.golang.org/protobuf/encoding/protojson"
)

type nhf015CalendarControl string

const (
	nhf015LoseCalendarReply   nhf015CalendarControl = "lose-next-reply"
	nhf015RevokeCalendarRead  nhf015CalendarControl = "revoke-read"
	nhf015AllowCalendarRead   nhf015CalendarControl = "allow-read"
	nhf015RevokeCalendarWrite nhf015CalendarControl = "revoke-write"
	nhf015AllowCalendarWrite  nhf015CalendarControl = "allow-write"
)

func (a *nhf015CalendarControl) UnmarshalJSON(raw []byte) error {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	switch nhf015CalendarControl(value) {
	case nhf015LoseCalendarReply, nhf015RevokeCalendarRead, nhf015AllowCalendarRead, nhf015RevokeCalendarWrite, nhf015AllowCalendarWrite:
		*a = nhf015CalendarControl(value)
		return nil
	default:
		return errors.New("unknown synthetic calendar control")
	}
}

type nhf015CalendarSeeds struct {
	SyntheticOnly bool `json:"synthetic_only"`
	Events        []struct {
		UID   string          `json:"uid"`
		Event json.RawMessage `json:"event"`
	} `json:"events"`
}

func nhf015ReadCalendarSeeds(root string) (nhf015CalendarSeeds, error) {
	var seeds nhf015CalendarSeeds
	file, err := os.Open(filepath.Join(root, "agenda.json"))
	if err != nil {
		return seeds, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 65537))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&seeds); err != nil {
		return seeds, err
	}
	if decoder.Decode(new(any)) != io.EOF || !seeds.SyntheticOnly || len(seeds.Events) == 0 || len(seeds.Events) > 32 {
		return seeds, errors.New("bounded synthetic agenda required")
	}
	seen := make(map[string]bool)
	for _, seed := range seeds.Events {
		if _, err = calendars.ParseUID(seed.UID); err != nil || seen[seed.UID] {
			return seeds, errors.New("invalid or duplicate synthetic UID")
		}
		seen[seed.UID] = true
	}
	return seeds, nil
}

// This persistent qualification process is opt-in, creates its own database and
// has no integration manager, device driver, fetcher or production registration.
func TestNHF015CalendarServer(t *testing.T) {
	out, root := os.Getenv("HOMEWARD_NHF015_FIXTURE"), os.Getenv("HOMEWARD_NHF015_SOURCES")
	if out == "" {
		t.Skip("explicit isolated NHF015 fixture only")
	}
	if os.Getenv("HOMEWARD_NHF015_SYNTHETIC_ONLY") != "1" || !filepath.IsAbs(out) || filepath.Clean(out) != out || !filepath.IsAbs(root) {
		t.Fatal("fresh absolute synthetic-only fixture required")
	}
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	seeds, err := nhf015ReadCalendarSeeds(root)
	if err != nil {
		t.Fatal(err)
	}
	r := nhf015NewCalendarRig(t, 10*time.Minute)
	id := r.create(t)
	for _, seed := range seeds.Events {
		event := new(cv.Event)
		if err = protojson.Unmarshal(seed.Event, event); err != nil {
			t.Fatal(err)
		}
		r.add(t, id, seed.UID, event)
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: r.calendarServer.Certificate().Raw})
	ids := make(map[string]string)
	for origin, name := range map[calendars.Origin]string{calendars.Local: "local", calendars.RemoteICS: "remote", calendars.Calculated: "calculated"} {
		ids[name] = r.ids[origin].EntityId
	}
	nhf003WritePrivate(t, out, "ready.json", map[string]any{"mode": "synthetic-only", "origin": r.calendarServer.URL, "ca_pem": string(certificate), "entities": ids, "reader": map[string]string{"login_name": "NHA162 Admin", "password": "NHA162 synthetic admin password canary"}, "production_calendars": false})
	defer nhf003WritePrivate(t, out, "result.json", map[string]any{"real_device_driver_instantiated": false, "real_device_writes": 0, "synthetic_only": true})
	read, write := true, true
	policy := func() {
		grants := ""
		if read {
			grants += `  - scope: {family: calendars, operation: read}
    objects: {entities: {all: {capabilities: {all: {}}}}}
`
		}
		if write {
			grants += `  - scope: {family: calendars, operation: write}
    objects: {entities: {all: {capabilities: {all: {}}}}}
`
		}
		if grants == "" {
			grants = "  []\n"
		}
		nha162SetPolicy(t, r.nha162RPCRig, r.admin, grants)
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var sequence uint64
	for {
		select {
		case <-r.ctx.Done():
			t.Fatal(r.ctx.Err())
		case <-ticker.C:
			if _, err = os.Stat(filepath.Join(out, "stop")); err == nil {
				return
			}
			file, err := os.Open(filepath.Join(out, "control.json"))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			var command struct {
				Sequence uint64                `json:"sequence"`
				Action   nhf015CalendarControl `json:"action"`
			}
			decoder := json.NewDecoder(io.LimitReader(file, 4097))
			decoder.DisallowUnknownFields()
			err = decoder.Decode(&command)
			extra := decoder.Decode(new(any))
			file.Close()
			if err != nil || extra != io.EOF {
				t.Fatal("invalid calendar control", err)
			}
			if command.Sequence == sequence {
				continue
			}
			if command.Sequence != sequence+1 {
				t.Fatal("unbound calendar control sequence")
			}
			switch command.Action {
			case nhf015LoseCalendarReply:
				r.handler.dropNextReply.Store(true)
			case nhf015RevokeCalendarRead:
				read = false
				policy()
			case nhf015AllowCalendarRead:
				read = true
				policy()
			case nhf015RevokeCalendarWrite:
				write = false
				policy()
			case nhf015AllowCalendarWrite:
				write = true
				policy()
			default:
				t.Fatal("invalid calendar control")
			}
			sequence = command.Sequence
			nhf003WritePrivate(t, out, fmt.Sprintf("ack-%02d.json", sequence), command)
		}
	}
}

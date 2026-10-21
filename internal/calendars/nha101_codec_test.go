package calendars

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func nha101Raw(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/nha101/fixtures/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func nha101Admit(t *testing.T, id CalendarID, raw []byte) *remoteImage {
	t.Helper()
	image, err := admitRemoteICS(context.Background(), id, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(image.close)
	return image
}
func nha101EncodeClose(t *testing.T, image *remoteImage) remotePayload {
	t.Helper()
	payload, err := encodeRemoteImage(context.Background(), image)
	image.close()
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestNHA101CodecEmptyAndPrimitiveGoldens(t *testing.T) {
	f := nha100Setup(t)
	id := f.bind(t, "calendar.famille")
	image := nha101Admit(t, id, []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n"))
	payload := nha101EncodeClose(t, image)
	golden, err := os.ReadFile("testdata/nha101/remote-v1-empty.hex")
	if err != nil {
		t.Fatal(err)
	}
	want, err := hex.DecodeString(strings.TrimSpace(string(golden)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload.data, want) {
		t.Fatalf("empty payload: got %x, want %x", payload.data, want)
	}
	decoded, err := decodeRemoteImage(context.Background(), id, payload)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(decoded.close)
	if len(decoded.events) != 0 || len(decoded.zones) != 0 || decoded.binding != id {
		t.Fatal("empty validated representation lost its identity")
	}
	decoded.close()
	// An empty payload is a representation only. Cache quality is a separate
	// contract, still unimplemented here; it must not be inferred from this test.
	raw, err := os.ReadFile("testdata/nha101/remote-v1-primitives.json")
	if err != nil {
		t.Fatal(err)
	}
	var goldens map[string]string
	if err = json.Unmarshal(raw, &goldens); err != nil {
		t.Fatal(err)
	}
	empty, err := PresentText("", SummaryField)
	if err != nil {
		t.Fatal(err)
	}
	letter, err := PresentText("A", SummaryField)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		name  string
		value TextValue
	}{{"text_absent", AbsentText()}, {"text_present_empty", empty}, {"text_present_A", letter}} {
		w := remoteEncoder{data: make([]byte, 16)}
		w.textValue(entry.value)
		if w.err != nil || hex.EncodeToString(w.data[:w.n]) != goldens[entry.name] {
			t.Fatalf("text golden %s: got %x", entry.name, w.data[:w.n])
		}
	}
	local := nha100Point(t, Timed, "2026-11-01T01:30:00")
	first := local
	first.origin = originResolved
	first.resolved = nha100Instant(t, "2026-11-01T05:30:00Z")
	second := local
	second.origin = originResolved
	second.resolved = nha100Instant(t, "2026-11-01T06:30:00Z")
	for _, entry := range []struct {
		name  string
		point Point
	}{{"point_fold_civil", local}, {"point_fold_first", first}, {"point_fold_second", second}} {
		w := remoteEncoder{data: make([]byte, 64)}
		w.point(entry.point)
		if w.err != nil || hex.EncodeToString(w.data[:w.n]) != goldens[entry.name] {
			t.Fatalf("Point golden %s: got %x error=%v", entry.name, w.data[:w.n], w.err)
		}
		r := remoteDecoder{data: w.data[:w.n]}
		got := r.point()
		if r.err != nil || got != entry.point {
			t.Fatal("typed Point roundtrip", r.err)
		}
	}
	w := remoteEncoder{data: make([]byte, 4)}
	w.date(nha100Date(t, "0001-01-01"))
	if hex.EncodeToString(w.data) != goldens["date_civil_zero"] {
		t.Fatal("civil0001 was encoded as absent")
	}
}

func TestNHA101CodecSourceDefinitionDuplicatesAndDataID(t *testing.T) {
	f := nha100Setup(t)
	id := f.bind(t, "calendar.famille")
	const observance = "BEGIN:STANDARD\r\nDTSTART:20260101T020000\r\nTZOFFSETFROM:+0000\r\nTZOFFSETTO:+0000\r\nRRULE:FREQ=YEARLY;BYMONTH=1;BYMONTHDAY=1;COUNT=2\r\nEND:STANDARD\r\n"
	raw := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTIMEZONE\r\nTZID:Codec/Duplicate\r\n" + strings.Repeat(observance, 12) + "END:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:source-roundtrip\r\nDTSTART;TZID=Codec/Duplicate:20261002T090000\r\nDURATION:PT1H\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	image := nha101Admit(t, id, raw)
	z := image.zones["Codec/Duplicate"]
	if len(z.embedded.source.Observances) != 12 || len(z.embedded.observances) != 1 {
		image.close()
		t.Fatal("wire duplicates were lost or expanded as twelve producers")
	}
	dataID := z.DataID()
	before := queryTime(t, image, "2026")
	payload := nha101EncodeClose(t, image)
	decoded, err := decodeRemoteImage(context.Background(), id, payload)
	if err != nil {
		t.Fatal(err)
	}
	defer decoded.close()
	z = decoded.zones["Codec/Duplicate"]
	if z.DataID() != dataID || len(z.embedded.source.Observances) != 12 || len(z.embedded.observances) != 1 {
		t.Fatal("codec changed embedded DataID or original definition")
	}
	if decoded.events[0].definition.span.zone.embedded != z.embedded {
		t.Fatal("event copied its embedded producer instead of referencing the dictionary")
	}
	after := queryTime(t, decoded, "2026")
	if len(before) != 1 || len(after) != 1 || before[0].ID() != after[0].ID() {
		t.Fatal("source roundtrip changed occurrence identity")
	}
	a, b, _, _ := after[0].Span().Instants()
	if a.String() != "2026-10-02T09:00:00Z" || b.String() != "2026-10-02T10:00:00Z" {
		t.Fatal("source roundtrip changed occurrence instants")
	}
	again, err := encodeRemoteImage(context.Background(), decoded)
	if err != nil || !bytes.Equal(payload.data, again.data) {
		t.Fatal("remote-v1 is not canonical after source reconstruction", err)
	}
}

func TestNHA101CodecContentSpanSetsAndOverrides(t *testing.T) {
	f := nha100Setup(t)
	id := f.bind(t, "calendar.famille")
	for _, name := range []string{"01-date.ics", "02-utc.ics", "03-iana.ics", "04-default-date.ics", "05-point.ics", "06-duration.ics", "07-overrides.ics", "08-vtimezone-fixed.ics", "09-vtimezone-collision.ics", "10-vtimezone-dst.ics", "11-floating-xwr.ics", "16-folded-text.ics", "17-observance-sparse.ics", "18-master-cancelled.ics", "19-master-cancelled-active-override.ics", "20-duration-p1d-vs-pt24h.ics", "24-recurrence-id-utc.ics", "25-recurrence-id-chicago.ics"} {
		t.Run(name, func(t *testing.T) {
			image := nha101Admit(t, id, nha101Raw(t, name))
			payload := nha101EncodeClose(t, image)
			decoded, err := decodeRemoteImage(context.Background(), id, payload)
			if err != nil {
				t.Fatal(err)
			}
			defer decoded.close()
			for _, s := range decoded.events {
				if s.key.calendar != id || !s.definition.Valid() {
					t.Fatal("decoded event lost admitted identity or definition")
				}
			}
			again, err := encodeRemoteImage(context.Background(), decoded)
			if err != nil || !bytes.Equal(payload.data, again.data) {
				t.Fatal("roundtrip lost source/content/span/set fields", err)
			}
		})
	}
}

func TestNHA101CodecMalformedAndUnknownAreDistinct(t *testing.T) {
	f := nha100Setup(t)
	id := f.bind(t, "calendar.famille")
	payload := nha101EncodeClose(t, nha101Admit(t, id, nha101Raw(t, "07-overrides.ics")))
	for n := 0; n < len(payload.data); n++ {
		truncated := remotePayload{data: payload.data[:n], digest: sha256.Sum256(payload.data[:n])}
		image, err := decodeRemoteImage(context.Background(), id, truncated)
		if image != nil {
			image.close()
			t.Fatal("truncated image admitted")
		}
		nha100Code(t, err, Corrupt)
	}
	for _, offset := range []int{8, 10} {
		data := bytes.Clone(payload.data)
		data[offset] = 0xff
		_, err := decodeRemoteImage(context.Background(), id, remotePayload{data: data, digest: sha256.Sum256(data)})
		nha100Code(t, err, Incompatible)
	}
	bad := payload
	bad.digest[0] ^= 0xff
	_, err := decodeRemoteImage(context.Background(), id, bad)
	nha100Code(t, err, Corrupt)
	// This unknown status is inside a well-framed, correctly hashed image.
	data := bytes.Clone(payload.data)
	eventStart := remoteHeaderBytes + 4
	uidLen := int(data[eventStart])<<8 | int(data[eventStart+1])
	data[eventStart+2+uidLen] = 0xff
	_, err = decodeRemoteImage(context.Background(), id, remotePayload{data: data, digest: sha256.Sum256(data)})
	nha100Code(t, err, Incompatible)
	data = bytes.Clone(payload.data)
	binary.BigEndian.PutUint16(data[14:16], MaxEvents+1)
	_, err = decodeRemoteImage(context.Background(), id, remotePayload{data: data, digest: sha256.Sum256(data)})
	nha100Code(t, err, CapacityExceeded)
	available := nha101Admit(t, id, nha101Raw(t, "02-utc.ics"))
	available.close()
}

func TestNHA101CodecFoldAndGapOriginsSurvive(t *testing.T) {
	f := nha100Setup(t)
	id := f.bind(t, "calendar.famille")
	const fold = "BEGIN:VEVENT\r\nUID:fold-roundtrip\r\nDTSTART;TZID=America/New_York:20261101T013000\r\nDURATION:PT30M\r\nRDATE:20261101T063000Z\r\nEND:VEVENT\r\n"
	const gap = "BEGIN:VEVENT\r\nUID:gap-roundtrip\r\nDTSTART;TZID=America/New_York:20260307T023000\r\nDURATION:PT1H\r\n"
	const a = "RDATE;TZID=America/New_York:20260308T023000\r\n"
	const b = "RDATE;TZID=America/New_York:20260308T033000\r\n"
	const c = "RDATE:20260308T073000Z\r\n"
	var canonical []byte
	for _, dates := range []string{a + b + c, c + b + a, b + a + c} {
		image := nha101Admit(t, id, []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\n"+fold+gap+dates+"END:VEVENT\r\nEND:VCALENDAR\r\n"))
		payload := nha101EncodeClose(t, image)
		if canonical == nil {
			canonical = bytes.Clone(payload.data)
		} else if !bytes.Equal(canonical, payload.data) {
			t.Fatal("wire order changed canonical explicit origins")
		}
		decoded, err := decodeRemoteImage(context.Background(), id, payload)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(decoded.close)
		values := queryTime(t, decoded, "2026")
		folds := byUID(values, "fold-roundtrip")
		gaps := byUID(values, "gap-roundtrip")
		if len(folds) != 2 || folds[0].ID() == folds[1].ID() {
			decoded.close()
			t.Fatal("codec collapsed the first and second fold")
		}
		start, _, _, _ := folds[0].Span().Instants()
		second, _, _, _ := folds[1].Span().Instants()
		if start.String() != "2026-11-01T05:30:00Z" || second.String() != "2026-11-01T06:30:00Z" {
			decoded.close()
			t.Fatal("fold instants changed")
		}
		if len(gaps) != 2 || gaps[1].ID().Point().String() != "2026-03-08T02:30:00" {
			decoded.close()
			t.Fatal("gap aliases changed their single canonical origin")
		}
		decoded.close()
	}
}

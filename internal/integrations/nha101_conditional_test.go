package integrations

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/storage"
)

func TestNHA101ConditionalMetadataDurableMergeAndExpiry(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	profile := sha256.Sum256([]byte("fixed representation profile"))
	first := http.Header{"Cache-Control": []string{"max-age=120"}, "Vary": []string{"Accept, Authorization"}, "Date": []string{now.Format(http.TimeFormat)}, "Age": []string{"20"}}
	meta, until, err := parseConditionalMetadata(first, storage.NoRemoteCalendarHTTPMetadata(), now, 0, profile)
	if err != nil || !until.Equal(now.Add(100*time.Second)) {
		t.Fatalf("metadata expiry: %v %v", until, err)
	}
	encoded, err := meta.Encode()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := storage.DecodeRemoteCalendarHTTPMetadata(encoded)
	if err != nil {
		t.Fatal(err)
	}
	_, mergedUntil, err := parseConditionalMetadata(http.Header{}, restored, now.Add(10*time.Second), 0, profile)
	if err != nil || !mergedUntil.Equal(until) {
		t.Fatalf("304 lost previous policy/date/age: %v %v", mergedUntil, err)
	}
	empty, _, err := parseConditionalMetadata(http.Header{"Cache-Control": []string{""}}, restored, now, 0, profile)
	if err != nil {
		t.Fatal(err)
	}
	field := empty.Field(storage.RemoteHTTPCacheControl)
	values, present := field.Values()
	if !present || len(values) != 1 || values[0] != "" {
		t.Fatal("present-empty field became absent")
	}
	for _, control := range []string{"no-cache", "max-age=0"} {
		_, expires, e := parseConditionalMetadata(http.Header{"Cache-Control": []string{control}}, storage.NoRemoteCalendarHTTPMetadata(), now, 0, profile)
		if e != nil || !expires.Equal(now) {
			t.Fatalf("must revalidate %q: %v %v", control, expires, e)
		}
	}
	_, defaultUntil, err := parseConditionalMetadata(http.Header{}, storage.NoRemoteCalendarHTTPMetadata(), now, 0, profile)
	if err != nil || !defaultUntil.Equal(now.Add(24*time.Hour)) {
		t.Fatal("default freshness is not bounded nominal daily poll")
	}
	for _, h := range []http.Header{{"Cache-Control": []string{"no-store"}}, {"Vary": []string{"*"}}, {"Vary": []string{"Cookie"}}} {
		if _, _, err := parseConditionalMetadata(h, storage.NoRemoteCalendarHTTPMetadata(), now, 0, profile); err == nil {
			t.Fatal("unrepresentable cache policy admitted")
		}
	}
}

func TestNHA101Conditional304ValidatorSelection(t *testing.T) {
	modified := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	strong, _ := storage.NewRemoteCalendarValidators(storage.RemoteValidatorsBoth, `"one"`, modified)
	for _, h := range []http.Header{{"Etag": []string{`"one"`}}, {"Etag": []string{`W/"one"`}}, {}} {
		if err := selectConditional304(strong, h); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range []http.Header{{"Etag": []string{`"two"`}}, {"Last-Modified": []string{modified.Add(time.Hour).Format(http.TimeFormat)}}} {
		if err := selectConditional304(strong, h); err == nil {
			t.Fatal("304 selected incompatible validator")
		}
	}
	none, _ := storage.NewRemoteCalendarValidators(storage.RemoteValidatorsNone, "", time.Time{})
	if err := selectConditional304(none, http.Header{}); err == nil {
		t.Fatal("initial304 selected a representation")
	}
	weak, _ := storage.NewRemoteCalendarValidators(storage.RemoteValidatorsETag, `W/"one"`, time.Time{})
	if err := selectConditional304(weak, http.Header{"Etag": []string{`"one"`}}); err == nil {
		t.Fatal("strong304 selected only weak stored validator")
	}
}

func TestNHA101ConditionalMalformedMetadataPreservesAdmittedValidators(t *testing.T) {
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	profile := conditionalRepresentationProfile()
	for _, header := range []http.Header{{"Cache-Control": []string{"max-age=+1"}}, {"Cache-Control": []string{`max-age=""12""`}}, {"Age": []string{"+1"}}, {"Date": []string{"invalid date"}}, {"Cache-Control": []string{strings.Repeat("x", storage.RemoteCalendarHTTPMetadataLimit+1)}}} {
		_, _, err := parseConditionalMetadata(header, storage.NoRemoteCalendarHTTPMetadata(), now, 0, profile)
		failure, ok := err.(*ConditionalError)
		if !ok || failure.Kind() != ConditionalInvalidResponse || failure.Disposition() != storage.PreserveAdmittedValidators {
			t.Fatal("malformed metadata discarded old admitted validators", err)
		}
	}
	sent, _ := storage.NewRemoteCalendarValidators(storage.RemoteValidatorsETag, `"valid"`, time.Time{})
	header := http.Header{"Etag": []string{`"valid"`}, "Last-Modified": []string{now.Format(http.TimeFormat)}}
	if err := selectConditional304(sent, header); err != nil {
		t.Fatal("matching ETag did not admit new metadata", err)
	}
	merged, err := conditionalResponseValidators(header, sent, ConditionalNotModified304)
	if err != nil || merged.Kind() != storage.RemoteValidatorsBoth {
		t.Fatal("304 failed to promote admitted Last-Modified", err)
	}
}

func TestNHA101ConditionalPublicAdmissionsAndZeroValues(t *testing.T) {
	if _, err := (ConditionalAttempt{}).Scope(); err == nil {
		t.Fatal("zero attempt admitted")
	}
	if _, err := (ConditionalObservation{}).StoreObservation(); err == nil {
		t.Fatal("zero observation admitted")
	}
	if _, err := (ConditionalObservation{}).TakeRepresentationBytes(); err == nil {
		t.Fatal("zero observation supplied a body")
	}
	if _, err := NewConditionalFactory(ConditionalSettings{}, nil); err == nil {
		t.Fatal("zero factory settings admitted")
	}
	var typedNil *conditionalHTTP
	typedNil.Close()
	if err := typedNil.Invalidate(context.Background(), ConditionalRejectExpired); err == nil {
		t.Fatal("typednil carrier invalidation admitted")
	}
	key, _ := NewSourceKey("test-calendar", "calendar")
	resource, err := PublicConditionalResource("https://calendar.invalid/feed?z=2&a=1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = NewConditionalSettings(key, resource, ConditionalAuth{}, PublicConditionalDestination(), sha256.Sum256([]byte("profile"))); err == nil {
		t.Fatal("zero auth became None")
	}
	if _, err = NewConditionalSettings(key, resource, NoConditionalAuth(), ConditionalDestination{}, sha256.Sum256([]byte("profile"))); err == nil {
		t.Fatal("zero destination became public")
	}
}

func TestNHA101ConditionalObservationDeadlineAdmission(t *testing.T) {
	if _, err := (ConditionalObservation{}).FreshUntil(); err == nil {
		t.Fatal("zero deadline admitted")
	}
	if _, err := (ConditionalObservation{}).TakeRepresentationBytes(); err == nil {
		t.Fatal("zero body transferred")
	}
}

func TestNHA101ConditionalBodyTransferOwnership(t *testing.T) {
	raw := []byte("bounded body")
	o := &conditionalObservation{kind: ConditionalRepresentation200, bodyState: conditionalBodyAvailable, body: raw}
	taken, e := takeConditionalBody(o)
	if e != nil || &taken[0] != &raw[0] || o.body != nil || o.bodyState != conditionalBodyTaken {
		t.Fatal("body was copied or retained", e)
	}
	if _, e = takeConditionalBody(o); e == nil {
		t.Fatal("body taken twice")
	}
	for _, invalid := range []*conditionalObservation{nil, {}, {kind: ConditionalNotModified304, bodyState: conditionalBodyNotApplicable}, {kind: ConditionalRepresentation200, bodyState: conditionalBodyAvailable, consumed: true}} {
		if _, e = takeConditionalBody(invalid); e == nil {
			t.Fatal("invalid body ownership admitted")
		}
	}
}

func TestNHA101ConditionalObservationTimeAndTerminalBounds(t *testing.T) {
	if _, e := (ConditionalObservation{}).ObservationTime(); e == nil {
		t.Fatal("zero observation time admitted")
	}
	at := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	o := &conditionalObservation{at: at, until: at.Add(time.Hour)}
	if conditionalObservationTimeAdmitted(at.Add(-time.Nanosecond), o) || conditionalObservationTimeAdmitted(at.Add(time.Hour), o) || !conditionalObservationTimeAdmitted(at, o) {
		t.Fatal("observation temporal admission wrong")
	}
}

package storage

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"
)

func TestNHA224ImportObservationBindsBodyWithoutHTTPMetadata(t *testing.T) {
	scope, err := NewRemoteCalendarScope(sha256.Sum256([]byte("source")), nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	body := sha256.Sum256([]byte("synthetic body"))
	observation, err := NewRemoteCalendarImportObservation(scope, sha256.Sum256([]byte("observation")), now, now.Add(time.Hour), body)
	if err != nil || !observation.Valid() || !observation.AdmitsRepresentation(body) || observation.AdmitsRepresentation(sha256.Sum256([]byte("different"))) {
		t.Fatal("import body not bound", err)
	}
	if observation.http.Present() || observation.validators.kind != RemoteValidatorsNone || observationHTTPSQL(observation) != nil || observationConditionalSQL(observation) != int16(RequireFullRepresentation) {
		t.Fatal("import retained HTTP metadata")
	}
	sent, _ := NewRemoteCalendarValidators(RemoteValidatorsETag, `"synthetic"`, time.Time{})
	var store *Store // Reject an import observation before any SQL access.
	if _, _, err := store.RevalidateRemoteCalendar(context.Background(), RemoteCalendarExpected{}, observation, sent, body); err == nil {
		t.Fatal("import became a revalidation")
	}
	if _, err := NewRemoteCalendarImportObservation(scope, sha256.Sum256([]byte("observation")), now, now.Add(25*time.Hour), body); err == nil {
		t.Fatal("unbounded import freshness")
	}
}

package storage

import (
	"context"
	"github.com/damione1/Homeward/internal/recorderevidence"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/historyquery"
)

// This storage-unit fixture admits only present, validated native evidence.
// Public-route authorization tests use the real authority instead.
type nha203RecordedScopeFixture struct{}

func (nha203RecordedScopeFixture) AuthorizeRecorderEvidence(_ context.Context, e recorderevidence.Evidence) (RecorderVisibility, error) {
	if !e.Present() {
		return RecorderHidden, nil
	}
	return RecorderVisible, nil
}
func nha203EvidenceRecord(t *testing.T, row RecorderRecord) RecorderRecord {
	return nha203EvidenceSlot(t, row, "measurement")
}
func nha203EvidenceSlot(t *testing.T, row RecorderRecord, endpoint string) RecorderRecord {
	t.Helper()
	entity, err := domain.ParseEntityID(row.observed.entity)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID(row.instance)
	if err != nil {
		t.Fatal(err)
	}
	device, _ := domain.ParseDeviceID("synthetic-history-device")
	source, _ := recorderevidence.NewSource("synthetic-history-device", "measurement")
	slot, err := authorization.NewCapabilitySlot("core.measurement.v1", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := recorderevidence.New(device, entity, instance, source, []authorization.CapabilitySlot{slot})
	if err != nil {
		t.Fatal(err)
	}
	row, err = row.WithEvidence(proof)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func nha203HistoryEntityRecord(t *testing.T, name string, seq uint64, admitted, received time.Time, value float64, semantic domain.SemanticClass) RecorderRecord {
	return nha203EvidenceRecord(t, nha032EntityRecord(t, name, seq, admitted, received, value, semantic))
}

type nha203RecordedSlotFixture string

func (allowed nha203RecordedSlotFixture) AuthorizeRecorderEvidence(_ context.Context, e recorderevidence.Evidence) (RecorderVisibility, error) {
	if !e.Present() {
		return RecorderHidden, nil
	}
	for _, slot := range e.Slots() {
		_, endpoint, err := slot.Values()
		if err != nil {
			return RecorderVisibilityUnspecified, err
		}
		if endpoint != string(allowed) {
			return RecorderHidden, nil
		}
	}
	return RecorderVisible, nil
}

func TestNHA203HistoryRecordedScopesAndLegacyRows(t *testing.T) {
	ctx := context.Background()
	settings := testSettings(t, testCluster(t))
	s, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if _, err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	epoch := nha031Epoch(t, 203)
	nha031Open(t, s, epoch)
	at := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Minute)
	entity := "synthetic-recorded-scopes"
	a := nha203EvidenceSlot(t, nha032EntityRecord(t, entity, 1, at, at, 11, domain.SemanticMeasurement), "slot-a")
	b := nha203EvidenceSlot(t, nha032EntityRecord(t, entity, 2, at, at, 22, domain.SemanticMeasurement), "slot-b")
	legacy := nha032EntityRecord(t, entity, 3, at, at, 33, domain.SemanticMeasurement)
	nha032Persist(t, s, epoch, 0, 3, []RecorderRecord{a, b, legacy}, nil, nil)
	q := nha164Query(t, historyquery.Observations, entity, at.Add(-time.Second), at.Add(time.Second), 1)
	for _, test := range []struct {
		scope string
		want  float64
	}{{"slot-a", 11}, {"slot-b", 22}} {
		page, err := s.ReadHistoricalRecorderPage(ctx, q, nil, nha203RecordedSlotFixture(test.scope))
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Observations) != 1 || page.Observations[0].State.GetMeasurement().Value != test.want || page.Next != nil {
			page.Release()
			t.Fatalf("scope %s crossed frozen boundary", test.scope)
		}
		page.Release()
	}
	page, err := s.ReadHistoricalRecorderPage(ctx, q, nil, nha203RecordedSlotFixture("absent"))
	if err != nil {
		t.Fatal(err)
	}
	defer page.Release()
	if len(page.Observations) != 0 || page.Next != nil {
		t.Fatal("hidden or legacy rows became public")
	}
	var retained int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM public.state_history WHERE entity_id=$1`, entity).Scan(&retained); err != nil || retained != 3 {
		t.Fatalf("legacy evidence was reconstructed/deleted: %d %v", retained, err)
	}
	page.Release()
	s.Close()
	restarted, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	if _, err = restarted.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := restarted.ReadHistoricalRecorderPage(ctx, q, nil, nha203RecordedSlotFixture("slot-b"))
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Release()
	if len(recovered.Observations) != 1 || recovered.Observations[0].State.GetMeasurement().Value != 22 {
		t.Fatal("restart changed frozen scope or exposed legacy rows")
	}

}

func nha203SeedWeighted(t *testing.T, s *Store) (RecorderEpoch, time.Time) {
	epoch := nha031Epoch(t, 320)
	nha031Open(t, s, epoch)
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	rows := []RecorderRecord{nha032Record(t, 1, at, at, 10, domain.SemanticMeasurement), nha032Record(t, 2, at.Add(15*time.Minute), at.Add(15*time.Minute), 30, domain.SemanticMeasurement), nha032Record(t, 3, at.Add(time.Hour), at.Add(time.Hour), 50, domain.SemanticMeasurement)}
	for i := range rows {
		rows[i] = nha203EvidenceRecord(t, rows[i])
	}
	nha032Persist(t, s, epoch, 0, 3, rows, nil, nil)
	return epoch, at
}

func TestNHA203AggregatesNeverJoinDifferentRecordedScopes(t *testing.T) {
	s := nha032SQLFixture(t)
	ctx := context.Background()
	epoch := nha031Epoch(t, 204)
	nha031Open(t, s, epoch)
	at := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	entity := "synthetic-scope-aggregates"
	rows := make([]RecorderRecord, 0, 4)
	for index, value := range []float64{10, 20, 100, 200} {
		instant := at.Add(time.Duration(index) * 15 * time.Minute)
		endpoint := "slot-a"
		if index >= 2 {
			endpoint = "slot-b"
		}
		rows = append(rows, nha203EvidenceSlot(t, nha032EntityRecord(t, entity, uint64(index+1), instant, instant, value, domain.SemanticMeasurement), endpoint))
	}
	nha032Persist(t, s, epoch, 0, 4, rows, nil, nil)
	if _, more, err := s.AggregateRecorderPrefix(ctx, nha032Boundary(t, epoch, 4, at.Add(time.Hour))); err != nil || more {
		t.Fatalf("aggregate bounded prefix: %v %v", more, err)
	}
	q := nha164Query(t, historyquery.Aggregates, entity, at, at.Add(time.Hour), 100)
	for _, test := range []struct {
		scope    string
		mean     float64
		duration time.Duration
	}{{"slot-a", 10, 15 * time.Minute}, {"slot-b", 150, 30 * time.Minute}} {
		page, err := s.ReadHistoricalRecorderPage(ctx, q, nil, nha203RecordedSlotFixture(test.scope))
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Aggregates) != 1 {
			page.Release()
			t.Fatalf("scope %s merged/duplicated: %d", test.scope, len(page.Aggregates))
		}
		mean, present := page.Aggregates[0].Values.Mean()
		if !present || mean != test.mean || page.Aggregates[0].Values.Input().Duration != test.duration {
			page.Release()
			t.Fatalf("scope %s inherited values from a different scope", test.scope)
		}
		page.Release()
	}
	page, err := s.ReadHistoricalRecorderPage(ctx, q, nil, nha203RecordedScopeFixture{})
	if err != nil {
		t.Fatal(err)
	}
	defer page.Release()
	if len(page.Aggregates) != 2 || page.Aggregates[0].GroupDigest == page.Aggregates[1].GroupDigest {
		t.Fatal("same descriptor/quality collapsed distinct authorization scopes")
	}
}

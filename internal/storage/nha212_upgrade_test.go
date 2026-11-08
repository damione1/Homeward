//go:build nha212_upgrade_qualification

package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/domain"
)

// This explicit upgrade qualification needs Git object cd3d9e0. The overlay
// compiles that release's actual migrator and admission constants; it never
// fabricates migration history or a schema window in a current database.
func TestNHA212Historical30UpgradePreservesData(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if mode := os.Getenv("NHA212_HISTORICAL_MODE"); mode != "" {
		if CurrentVersion().Number() != 30 {
			t.Fatal("helper is not the historical SQL30 migrator")
		}
		port, err := strconv.Atoi(os.Getenv("NHA212_HISTORICAL_PORT"))
		if err != nil {
			t.Fatal(err)
		}
		s, err := Open(ctx, testSettings(t, port))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		version, err := s.Migrate(ctx)
		if mode == "reject" {
			var classified *Error
			if !errors.As(err, &classified) || classified.Kind() != FutureSchema {
				t.Fatalf("SQL30 admitted SQL30: %v %v", version, err)
			}
			return
		}
		if err != nil || version.Number() != 30 {
			t.Fatalf("historical migration: %v %v", version, err)
		}
		if _, err := s.BootstrapMFAPolicy(ctx); err != nil {
			t.Fatal(err)
		}
		admin := nha049AccountInput(t, "Synthetic Upgrade Administrator", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
		if _, err := s.BootstrapAdmin(ctx, admin); err != nil {
			t.Fatal(err)
		}
		actor, _ := domain.ParseActorID("nha212-upgrade")
		command, _ := domain.ParseCommandID("nha212-indeterminate")
		if _, created, outcome, err := s.AcceptCommandIntent(ctx, command, actor, "synthetic-epoch", []byte("synthetic intent")); err != nil || !created || outcome != TransactionCommitted {
			t.Fatal("seed ledger", err)
		}
		if moved, _, err := s.MoveCommandIntent(ctx, command, IntentAccepted, IntentDispatching, nil, IntentNoError); err != nil || !moved {
			t.Fatal(err)
		}
		if moved, _, err := s.MoveCommandIntent(ctx, command, IntentDispatching, IntentIndeterminate, []byte("synthetic uncertain outcome"), IntentOutcomeIndeterminate); err != nil || !moved {
			t.Fatal(err)
		}
		nha212SeedLegacyFan(t, ctx, s)
		return
	}
	rootBytes, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatal(err)
	}
	root := strings.TrimSpace(string(rootBytes))
	temporary := t.TempDir()
	replacements := map[string]string{}
	for _, name := range []string{"migrations.go", "schema_compatibility_release.go"} {
		path := "internal/storage/" + name
		data, err := exec.CommandContext(ctx, "git", "-C", root, "show", "cd3d9e0cc534ff8bd22d6ce0e14911950ea12303:"+path).Output()
		if err != nil {
			t.Fatal("historical source unavailable", err)
		}
		replacement := filepath.Join(temporary, name)
		if err := os.WriteFile(replacement, data, 0600); err != nil {
			t.Fatal(err)
		}
		replacements[filepath.Join(root, path)] = replacement
	}
	overlay, _ := json.Marshal(map[string]any{"Replace": replacements})
	overlayPath := filepath.Join(temporary, "overlay.json")
	if err := os.WriteFile(overlayPath, overlay, 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(temporary, "sql30.test")
	build := exec.CommandContext(ctx, "go", "test", "-p", "1", "-c", "-tags", "nha212_upgrade_qualification", "-overlay", overlayPath, "-o", binary, "./internal/storage")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("historical build: %v\n%s", err, output)
	}
	port := testCluster(t)
	historical := func(mode string) {
		t.Helper()
		child := exec.CommandContext(ctx, binary, "-test.run=^TestNHA212Historical30UpgradePreservesData$", "-test.timeout=45s")
		child.Env = append(os.Environ(), "NHA212_HISTORICAL_MODE="+mode, "NHA212_HISTORICAL_PORT="+strconv.Itoa(port))
		if output, err := child.CombinedOutput(); err != nil {
			t.Fatalf("historical %s: %v\n%s", mode, err, output)
		}
	}
	historical("seed")
	s, err := Open(ctx, testSettings(t, port))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	columns := nha204Columns(t, ctx, s)
	delete(columns, "homeward_schema_window")
	delete(columns, "state_history")
	delete(columns, "recorder_snapshots")
	before := nha204Snapshot(t, ctx, s, columns)
	history := func() string {
		t.Helper()
		var value string
		if err := s.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(h) ORDER BY version)::text FROM public.homeward_schema_migrations h WHERE version<=30`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	originalHistory := history()
	var oldPayload []byte
	if err = s.pool.QueryRow(ctx, `SELECT payload FROM public.state_history WHERE sequence=1`).Scan(&oldPayload); err != nil {
		t.Fatal(err)
	}
	// Failure after a history row was converted must roll the entire migration
	// back, including schema history and the previous row's authenticated bytes.
	var snapshotDigest []byte
	if err = s.pool.QueryRow(ctx, `SELECT record_digest FROM public.recorder_snapshots`).Scan(&snapshotDigest); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE public.recorder_snapshots SET record_digest=$1`, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Migrate(ctx); err == nil {
		t.Fatal("corrupt snapshot did not roll back migration")
	}
	var rolledBack []byte
	var version int
	if err = s.pool.QueryRow(ctx, `SELECT payload FROM public.state_history WHERE sequence=1`).Scan(&rolledBack); err != nil || !bytes.Equal(rolledBack, oldPayload) {
		t.Fatal("partial migration survived rollback", err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT max(version) FROM public.homeward_schema_migrations`).Scan(&version); err != nil || version != 30 {
		t.Fatal("failed migration advanced schema", err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE public.recorder_snapshots SET record_digest=$1`, snapshotDigest); err != nil {
		t.Fatal(err)
	}
	if version, err := s.Migrate(ctx); err != nil || version != CurrentVersion() {
		t.Fatalf("30→31: %v %v", version, err)
	}
	nha204AssertSnapshot(t, before, nha204Snapshot(t, ctx, s, columns))
	if history() != originalHistory {
		t.Fatal("SQL30 history changed")
	}
	expected, changed, err := migrateLegacyFanPayload(oldPayload)
	if err != nil || !changed {
		t.Fatal(err)
	}
	var stored []byte
	for _, query := range []string{`SELECT payload FROM public.state_history WHERE sequence=1`, `SELECT payload FROM public.recorder_snapshots`} {
		if err = s.pool.QueryRow(ctx, query).Scan(&stored); err != nil || !bytes.Equal(stored, expected) {
			t.Fatal("migrated history/snapshot mismatch", err)
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var wire state.ObservedState
	if err = proto.Unmarshal(stored, &wire); err != nil {
		t.Fatal(err)
	}
	if _, err = verifyHistoricalRecord(ctx, tx, nha030Epoch(t), 1, &wire, stored); err != nil {
		t.Fatal("migrated digest", err)
	}
	_ = tx.Rollback(ctx)
	historical("reject")
	nha204AssertSnapshot(t, before, nha204Snapshot(t, ctx, s, columns))
	if history() != originalHistory {
		t.Fatal("rejected old binary changed history")
	}
}

func nha212SeedLegacyFan(t *testing.T, ctx context.Context, s *Store) {
	t.Helper()
	epoch := nha030Epoch(t)
	if _, _, err := s.OpenRecorderEpoch(ctx, epoch, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	at := timestamppb.New(time.Date(2026, 11, 8, 12, 0, 0, 123456789, time.UTC))
	wire := &state.ObservedState{Entity: &registry.EntityRef{Reference: &registry.EntityRef_EntityId{EntityId: "Host:synthetic/fan"}}, Quality: &state.Quality{Availability: state.Availability_AVAILABILITY_AVAILABLE, Freshness: state.Freshness_FRESHNESS_FRESH, Provenance: state.Provenance_PROVENANCE_OBSERVED}, Revision: 7, ObservedAt: at, ReceivedAt: at, LastChanged: at, LastUpdated: at, LastReported: at, SourceSession: "synthetic-old-session", SourceSeq: proto.Uint64(0)}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	// Frozen canonical SQL30 fan enum OFF field, not encoded by the new schema.
	raw = append(raw, 0x72, 0x02, 0x08, 0x01)
	if err = proto.Unmarshal(raw, wire); err != nil {
		t.Fatal(err)
	}
	observed, err := fanMigrationObserved(wire, raw)
	if err != nil {
		t.Fatal(err)
	}
	observed.entityDigest = sha256.Sum256([]byte(observed.entity))
	row := RecorderRecord{sequence: 1, instance: "synthetic-adapter", admittedAt: at.AsTime(), observed: observed}
	row.digest = digestRecorderRecord(row)
	snapshot := RecorderSnapshotRecord{observed: observed}
	snapshot.digest = digestRecorderSnapshot(snapshot)
	err = s.InTransaction(ctx, func(tx pgx.Tx) error {
		if err := insertRecorderState(ctx, tx, epoch, row); err != nil {
			return err
		}
		return insertRecorderSnapshot(ctx, tx, epoch, 1, snapshot)
	})
	if err != nil {
		t.Fatal(err)
	}
}

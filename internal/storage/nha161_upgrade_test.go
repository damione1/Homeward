//go:build nha161_upgrade_qualification

package storage

import (
	"context"
	"encoding/json"
	"errors"
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

// This explicit upgrade qualification needs Git object 1440a02. The overlay
// compiles that release's actual migrator and admission constants; it never
// fabricates migration history or a schema window in a current database.
func TestNHA161Historical29UpgradePreservesData(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if mode := os.Getenv("NHA161_HISTORICAL_MODE"); mode != "" {
		if CurrentVersion().Number() != 29 {
			t.Fatal("helper is not the historical SQL29 migrator")
		}
		port, err := strconv.Atoi(os.Getenv("NHA161_HISTORICAL_PORT"))
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
				t.Fatalf("SQL29 admitted SQL30: %v %v", version, err)
			}
			return
		}
		if err != nil || version.Number() != 29 {
			t.Fatalf("historical migration: %v %v", version, err)
		}
		if _, err := s.BootstrapMFAPolicy(ctx); err != nil {
			t.Fatal(err)
		}
		admin := nha049AccountInput(t, "Synthetic Upgrade Administrator", authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
		if _, err := s.BootstrapAdmin(ctx, admin); err != nil {
			t.Fatal(err)
		}
		actor, _ := domain.ParseActorID("nha161-upgrade")
		command, _ := domain.ParseCommandID("nha161-indeterminate")
		if _, created, outcome, err := s.AcceptCommandIntent(ctx, command, actor, "synthetic-epoch", []byte("synthetic intent")); err != nil || !created || outcome != TransactionCommitted {
			t.Fatal("seed ledger", err)
		}
		if moved, _, err := s.MoveCommandIntent(ctx, command, IntentAccepted, IntentDispatching, nil, IntentNoError); err != nil || !moved {
			t.Fatal(err)
		}
		if moved, _, err := s.MoveCommandIntent(ctx, command, IntentDispatching, IntentIndeterminate, []byte("synthetic uncertain outcome"), IntentOutcomeIndeterminate); err != nil || !moved {
			t.Fatal(err)
		}
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
		data, err := exec.CommandContext(ctx, "git", "-C", root, "show", "1440a02a79dee7cb1f3b8b1366578365ddb3076c:"+path).Output()
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
	binary := filepath.Join(temporary, "sql29.test")
	build := exec.CommandContext(ctx, "go", "test", "-p", "1", "-c", "-tags", "nha161_upgrade_qualification", "-overlay", overlayPath, "-o", binary, "./internal/storage")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("historical build: %v\n%s", err, output)
	}
	port := testCluster(t)
	historical := func(mode string) {
		t.Helper()
		child := exec.CommandContext(ctx, binary, "-test.run=^TestNHA161Historical29UpgradePreservesData$", "-test.timeout=45s")
		child.Env = append(os.Environ(), "NHA161_HISTORICAL_MODE="+mode, "NHA161_HISTORICAL_PORT="+strconv.Itoa(port))
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
	before := nha204Snapshot(t, ctx, s, columns)
	history := func() string {
		t.Helper()
		var value string
		if err := s.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(h) ORDER BY version)::text FROM public.homeward_schema_migrations h WHERE version<=29`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	originalHistory := history()
	if version, err := s.Migrate(ctx); err != nil || version != CurrentVersion() {
		t.Fatalf("29→30: %v %v", version, err)
	}
	nha204AssertSnapshot(t, before, nha204Snapshot(t, ctx, s, columns))
	if history() != originalHistory {
		t.Fatal("SQL29 history changed")
	}
	historical("reject")
	nha204AssertSnapshot(t, before, nha204Snapshot(t, ctx, s, columns))
	if history() != originalHistory {
		t.Fatal("rejected old binary changed history")
	}
}

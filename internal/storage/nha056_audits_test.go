package storage

import (
	"context"
	"strings"
	"testing"
	"time"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/observability"
)

func TestNHA056AuditReadersWhitelistBoundBeforeMaterialization(t *testing.T) {
	port := testCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, testSettings(t, port))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for action := 1; action <= 29; action++ {
		if _, err := store.pool.Exec(ctx, `INSERT INTO public.auth_audit(actor_account_id,action,outcome,route_path) VALUES(NULL,$1,1,'/NHA056_RAW_ROUTE_CANARY')`, action); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO public.auth_audit(actor_account_id,action,outcome) VALUES($1,1,2)`, strings.Repeat("NHA056_ACTOR_CANARY", 65536)); err != nil {
		t.Fatal(err)
	}
	size, _ := observability.NewPageSize(10)
	request, _ := NewAuthenticationAuditRead(size, FirstAuditPosition())
	page, err := store.ReadAuthenticationAudits(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows()) != 10 || page.Rows()[0].MetadataInvalid() == nil {
		t.Fatal("unbounded actor was materialized as valid")
	}
	position, more := page.Next()
	if !more {
		t.Fatal("no keyset continuation")
	}
	seen := 10
	for more {
		request, _ = NewAuthenticationAuditRead(size, position)
		page, err = store.ReadAuthenticationAudits(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range page.Rows() {
			if _, present := row.Actor(); present {
				t.Fatal("target or absent actor invented")
			}
			if row.Action() == obs.AuthAuditAction_AUTH_AUDIT_ACTION_UNSPECIFIED {
				t.Fatal("action conversion lost")
			}
		}
		seen += len(page.Rows())
		position, more = page.Next()
	}
	if seen != 30 {
		t.Fatalf("keyset saw %d rows", seen)
	}
	for _, reason := range []string{"foreign_handle", "inactive_session"} {
		if _, err := store.pool.Exec(ctx, `INSERT INTO public.secret_access_audit(requesting_instance,target_instance,reason) VALUES('NHA056_REQUESTER_CANARY','NHA056_TARGET_CANARY',$1)`, reason); err != nil {
			t.Fatal(err)
		}
	}
	secretRequest, _ := NewSecretAccessAuditRead(size, FirstAuditPosition())
	secrets, err := store.ReadSecretAccessAudits(ctx, secretRequest)
	if err != nil || len(secrets.Rows()) != 2 {
		t.Fatal("secret audit reader", err)
	}
	for _, row := range secrets.Rows() {
		if row.Reason() == 0 {
			t.Fatal("reason lost")
		}
	}
}

func TestNHA056AuditConversionsCoverExactlyThePersistedClosedSets(t *testing.T) {
	seen := make(map[obs.AuthAuditAction]bool)
	for action := authActionBootstrap; action <= authActionMFAPolicyRead; action++ {
		value, ok := auditActionWire(action)
		if !ok || value == 0 || seen[value] {
			t.Fatal("missing or duplicated persisted action conversion")
		}
		seen[value] = true
	}
	if len(seen) != 29 || len(obs.AuthAuditAction_name) != 30 {
		t.Fatal("audit source/schema drift requires conversion review")
	}
	for _, invalid := range []authAction{0, -1, 30} {
		if _, ok := auditActionWire(invalid); ok {
			t.Fatal("unknown action accepted")
		}
	}
	if _, ok := secretAuditReasonWire("NHA056_UNKNOWN_REASON_CANARY"); ok {
		t.Fatal("unknown reason accepted")
	}
	var typedNil *nha049NilContext
	size, _ := observability.NewPageSize(1)
	request, _ := NewAuthenticationAuditRead(size, FirstAuditPosition())
	if _, err := (&Store{}).ReadAuthenticationAudits(typedNil, request); err == nil {
		t.Fatal("typed-nil SQL context accepted")
	}
}

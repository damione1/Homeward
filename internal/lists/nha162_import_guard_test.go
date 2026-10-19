package lists

import (
	"testing"

	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
)

func TestNHA162ImportAliasGuardUsesCurrentWriteFence(t *testing.T) {
	f := nha103Setup(t)
	adminCtx := f.as(t, f.admin)
	alias, err := domain.ParseHAAlias("todo.brico")
	if err != nil {
		t.Fatal(err)
	}
	other, err := domain.ParseHAAlias("todo.other")
	if err != nil {
		t.Fatal(err)
	}
	imported, err := f.service.ImportLegacyList(adminCtx, alias, mustLegacyList(t, "Synthetic historical list"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.VerifyLegacyImportAlias(adminCtx, imported.ListID(), alias); err != nil {
		t.Fatalf("correct alias rejected: %v", err)
	}
	if err := f.service.VerifyLegacyImportAlias(adminCtx, imported.ListID(), other); !IsCode(err, ImportConflict) {
		t.Fatalf("wrong alias was accepted or exposed list contents: %v", err)
	}
	native, err := f.service.CreateList(adminCtx, mustList(t, "Synthetic native list"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.VerifyLegacyImportAlias(adminCtx, native.ListID(), alias); !IsCode(err, ImportConflict) {
		t.Fatalf("native list admitted as historical: %v", err)
	}

	user := nha103Account(t, "NHA162 Synthetic Importer", authv1.AccountRole_ACCOUNT_ROLE_USER)
	if _, err := f.store.CreateAccount(f.ctx, f.admin, user); err != nil {
		t.Fatal(err)
	}
	writeOnly := `  - scope: {family: lists, operation: write}
    objects: {lists: {all: {}}}
`
	state := nha103Replace(t, f, f.admin, user.ID, nha103Source(t, user.ID, writeOnly))
	if err := f.authority.Publish(state); err != nil {
		t.Fatal(err)
	}
	userCtx := f.as(t, user.ID)
	if _, err := f.service.GetList(userCtx, imported.ListID()); !IsCode(err, Denied) {
		t.Fatalf("WRITE unexpectedly implied READ: %v", err)
	}
	if err := f.service.VerifyLegacyImportAlias(userCtx, imported.ListID(), alias); err != nil {
		t.Fatalf("WRITE-only guard rejected: %v", err)
	}
	disabled := authv1.AccountStatus_ACCOUNT_STATUS_DISABLED
	if _, err := f.store.UpdateAccount(f.ctx, f.admin, user.ID, storage.AccountChanges{Status: &disabled}); err != nil {
		t.Fatal(err)
	}
	// Memory authority remains stale; the account SQL fence must still deny.
	if err := f.service.VerifyLegacyImportAlias(userCtx, imported.ListID(), alias); !IsCode(err, Denied) {
		t.Fatalf("disabled account passed import guard: %v", err)
	}
}

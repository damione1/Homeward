package configactivation

import (
	"context"
	"errors"
	"testing"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
)

func TestNHA166RefreshApplyRollbackCannotEnterOwnerOperation(t *testing.T) {
	// A nil Store and unattached runtime are deliberate tripwires: refusal must
	// precede reservation, compiler, SQL, publication and staged workspace IO.
	owner := &Owner{input: OwnerInput{Startup: config.StartupInputs{}.WithMQTTReadOnlyObservation()}, phase: ownerOpen, subs: make(map[chan struct{}]struct{})}
	revision, _ := storage.NewConfigGenerationRevision(1)
	manifest := [32]byte{1}
	author, _ := storage.NewConfigAuthor("nha166:double")
	source, _ := storage.NewConfigSource("nha166:double")
	request, err := NewWorkspaceRequest(RetainDefinitions(), author, source, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	precondition, err := NewWorkspacePrecondition(revision, manifest, [32]byte{2})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := request.WithWorkspacePrecondition(precondition)
	if err != nil {
		t.Fatal(err)
	}
	changed, cancel, err := owner.SubscribeConfigChanges(context.Background())
	if err != nil || changed == nil || cancel == nil {
		t.Fatal("observation blocked config read subscription", err)
	}
	cancel()

	tests := []struct {
		name string
		run  func() error
	}{
		{"activate", func() error { _, err := owner.Activate(context.Background(), request); return err }},
		{"reconcile", func() error { _, err := owner.Reconcile(context.Background()); return err }},
		{"program", func() error {
			_, err := owner.ReconcileWithProgram(context.Background(), revision, manifest, ProgramImage{admitted: true})
			return err
		}},
		{"workspace_apply_or_rollback", func() error {
			_, err := owner.ActivateWorkspaceEdit(context.Background(), workspace, func(context.Context) error { t.Fatal("workspace stage entered"); return nil })
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); !errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
				t.Fatal("did not refuse immutable observation before effects", err)
			}
		})
	}
	if owner.active || owner.pending != nil || owner.attached {
		t.Fatal("refusal changed Owner state")
	}
}

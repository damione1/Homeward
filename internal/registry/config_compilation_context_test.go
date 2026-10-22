package registry_test

import (
	"errors"
	"testing"

	"github.com/damione1/Homeward/internal/configcompilation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
)

func nha055RegistryCompilationInput(t *testing.T, manifest registry.Manifest, records []integrations.RegistryRecord) configcompilation.Context {
	t.Helper()
	revision, valid := manifest.Revision()
	if !valid {
		t.Fatal("missing scope revision")
	}
	text, err := revision.Value()
	if err != nil {
		t.Fatal(err)
	}
	digest, valid := manifest.Digest()
	if !valid {
		t.Fatal("missing manifest digest")
	}
	var identities []configcompilation.Identity
	for _, record := range records {
		device, entity, err := record.IDs()
		if err != nil {
			t.Fatal(err)
		}
		instance, key, err := record.Source()
		if err != nil {
			t.Fatal(err)
		}
		sourceDevice, sourceEntity, err := key.Components()
		if err != nil {
			t.Fatal(err)
		}
		original, current, aliases, err := record.Aliases()
		if err != nil {
			t.Fatal(err)
		}
		identities = append(identities, configcompilation.Identity{Device: device, Entity: entity, Instance: instance, SourceDevice: sourceDevice, SourceEntity: sourceEntity, Original: original, Current: current, Aliases: aliases, Retired: record.Retired()})
	}
	result, err := configcompilation.NewContext(text, digest, identities, nil)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestNHA055CompilationIdentitiesAreConfirmedInTheSameRealConfigTransaction(t *testing.T) {
	store, ctx, _ := nha015Store(t)
	repo := nha015Repository(t, store)
	_, sink, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.AttachProjection(ctx, sink); err != nil {
		t.Fatal(err)
	}
	instance := nha015Instance(t, "nha055-compilation-sql")
	key := nha015Source(t, "nha055-compilation-device", "relay")
	const original = "switch.arc_soundbar_crossfade"
	const historical = "switch.nha055_previous"
	extraKey := nha015Source(t, "nha055-unrelated-device", "relay")
	identity, err := repo.ConfigureBinding(ctx, instance, key, original)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Rename(ctx, identity.Entity, historical); err != nil {
		t.Fatal(err)
	}
	if err := repo.Rename(ctx, identity.Entity, original); err != nil {
		t.Fatal(err)
	}
	manifest, err := repo.ScopeManifest()
	if err != nil {
		t.Fatal(err)
	}
	records, err := repo.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	baseline := nha055RegistryCompilationInput(t, manifest, records)
	refused := errors.New("NHA055 definite rollback after successful confirmation")
	cases := []struct {
		name   string
		mutate func(*registry.ConfigWriter) error
		change func(*testing.T, configcompilation.Context) configcompilation.Context
		accept bool
	}{
		{name: "unchanged", accept: true},
		{name: "unrelated-sql-identity", accept: true, mutate: func(w *registry.ConfigWriter) error {
			_, err := w.ConfigureBinding(instance, extraKey, "switch.chauffe_eau")
			return err
		}},
		{name: "tombstone", mutate: func(w *registry.ConfigWriter) error { return w.Tombstone(identity.Entity) }},
		{name: "current-alias", mutate: func(w *registry.ConfigWriter) error { return w.Rename(identity.Entity, historical) }},
		{name: "added-historical-alias", mutate: func(w *registry.ConfigWriter) error {
			if err := w.Rename(identity.Entity, "switch.nha055_added"); err != nil {
				return err
			}
			return w.Rename(identity.Entity, original)
		}},
		{name: "wrong-device", change: func(t *testing.T, input configcompilation.Context) configcompilation.Context {
			items := input.Identities()
			device, err := domain.ParseDeviceID("dev_nha055_different")
			if err != nil {
				t.Fatal(err)
			}
			items[0].Device = device
			changed, err := configcompilation.NewContext(input.ManifestRevision(), input.ManifestSHA256(), items, input.Capabilities())
			if err != nil {
				t.Fatal(err)
			}
			return changed
		}},
		{name: "wrong-source", change: func(t *testing.T, input configcompilation.Context) configcompilation.Context {
			items := input.Identities()
			items[0].SourceDevice = "nha055-unbound-device"
			changed, err := configcompilation.NewContext(input.ManifestRevision(), input.ManifestSHA256(), items, input.Capabilities())
			if err != nil {
				t.Fatal(err)
			}
			return changed
		}},
		{name: "wrong-manifest", change: func(t *testing.T, input configcompilation.Context) configcompilation.Context {
			digest := input.ManifestSHA256()
			digest[0] ^= 1
			changed, err := configcompilation.NewContext(input.ManifestRevision(), digest, input.Identities(), input.Capabilities())
			if err != nil {
				t.Fatal(err)
			}
			return changed
		}},
		{name: "zero-context", change: func(*testing.T, configcompilation.Context) configcompilation.Context {
			return configcompilation.Context{}
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			input := baseline
			if test.change != nil {
				input = test.change(t, input)
			}
			gate, err := repo.BeginActivation(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var escaped *registry.ConfigWriter
			var confirmed error
			var applyErr error
			outcome, transactionErr := store.InConfigTransactionOutcome(ctx, func(config *storage.ConfigTx) error {
				// Deliberately swallow both errors. A failed identity confirmation
				// must still poison this same ConfigTx and roll back its mutations.
				applyErr = gate.ApplyInConfigTx(config, func(writer *registry.ConfigWriter) error {
					escaped = writer
					if test.mutate != nil {
						if err := test.mutate(writer); err != nil {
							return err
						}
					}
					confirmed = writer.ConfirmCompilationIdentities(input)
					return nil
				})
				if test.accept {
					return refused
				}
				return nil
			})
			if escaped == nil {
				t.Fatalf("confirmation callback was not executed: apply=%v transaction=%v", applyErr, transactionErr)
			}
			if test.accept && (confirmed != nil || applyErr != nil || !errors.Is(transactionErr, refused)) {
				t.Fatalf("unchanged SQL input refused: confirmation=%v apply=%v transaction=%v", confirmed, applyErr, transactionErr)
			}
			if !test.accept && (confirmed == nil || transactionErr == nil) {
				t.Fatalf("changed input escaped swallowed-error rollback: confirmation=%v transaction=%v", confirmed, transactionErr)
			}
			if outcome != storage.TransactionNotCommitted {
				t.Fatalf("identity confirmation allowed commit: outcome=%v err=%v", outcome, transactionErr)
			}
			if err := gate.ResolveNotCommitted(outcome); err != nil {
				t.Fatal(err)
			}
			if err := gate.Release(); err != nil {
				t.Fatal(err)
			}
			if err := escaped.ConfirmCompilationIdentities(baseline); !errors.Is(err, registry.ErrInvalidInput) {
				t.Fatalf("escaped writer remained usable: %v", err)
			}
			after, err := repo.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if nha055RegistryCompilationInput(t, manifest, after).SHA256() != baseline.SHA256() {
				t.Fatal("rejected activation mutated durable identities or aliases")
			}
		})
	}
}

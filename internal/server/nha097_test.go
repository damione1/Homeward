package server

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

func TestNHA097RunRestoresActiveHelpersWithoutOtherDrivers(t *testing.T) {
	cluster := testpg.Start(t)
	configuration := nha030ServerConfig(t, cluster.Port)
	settings, err := configuration.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := bundle.CompileV2(filepath.Join("..", "config", "bundle", "testdata", "nha097"), "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := storage.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		store.Close()
		t.Fatal(err)
	}
	dataDir, err := configuration.DataDir()
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, err := store.BootstrapSecrets(ctx, dataDir); err != nil {
		store.Close()
		t.Fatal(err)
	}
	document, err := storage.NewConfigDocumentID("nha097.configuration")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	binding, err := storage.NewConfigDocumentBinding(document, "configuration.yaml")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	author, err := storage.NewConfigAuthor("nha097 test")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	source, err := storage.NewConfigSource("isolated corpus")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	metadata, err := storage.NewConfigVersionMetadata(author, source)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	outcome, err := store.InConfigTransactionOutcome(ctx, func(tx *storage.ConfigTx) error {
		refs, err := tx.SaveCandidate(candidate, []storage.ConfigDocumentBinding{binding}, metadata)
		if err != nil {
			return err
		}
		generation, err := tx.RegisterGeneration(candidate, refs)
		if err != nil {
			return err
		}
		return tx.ActivateGeneration(storage.ExpectNoConfigActive(), generation)
	})
	if err != nil || outcome != storage.TransactionCommitted {
		store.Close()
		t.Fatalf("preselected generation: %v %v", outcome, err)
	}
	store.Close()

	observer := nha030ServerObserver(t, cluster.Port)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	var listener net.ListenConfig
	go func() {
		done <- RunWithStartup(runCtx, configuration.startup, listener.Listen, func(net.Addr) { close(ready) })
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Run exited before helpers became ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not start")
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		var bindings, restored int
		err := observer.QueryRow(ctx, `SELECT count(*) FROM public.registry_source_bindings WHERE instance_id='homeward-helpers-v1'`).Scan(&bindings)
		if err == nil {
			err = observer.QueryRow(ctx, `SELECT count(*) FROM public.helper_values`).Scan(&restored)
		}
		if err == nil && bindings == 26 && restored == 6 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("active corpus did not bind/restore: bindings=%d values=%d err=%v", bindings, restored, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	var castRows int
	if err := observer.QueryRow(ctx, `SELECT count(*) FROM public.helper_values v JOIN public.registry_entities e ON e.id=v.entity_id WHERE e.manifest_alias='input_button.cast_to_nest_hub'`).Scan(&castRows); err != nil || castRows != 0 {
		t.Fatalf("Cast acquired an invented initial press: %d %v", castRows, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("Run did not join local helper pilot")
	}
}

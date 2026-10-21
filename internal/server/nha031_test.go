package server

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

func TestNHA031RunCompositionAndShutdown(t *testing.T) {
	for _, withFactory := range []bool{true, false} {
		name := "Run"
		if withFactory {
			name = "RunWithFactory"
		}
		t.Run(name, func(t *testing.T) {
			cluster := testpg.Start(t)
			cfg := nha030ServerConfig(t, cluster.Port)
			observer := nha030ServerObserver(t, cluster.Port)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var liveStore *storage.Store
			var epoch storage.RecorderEpoch
			factory := RuntimeFactory(func(factoryCtx context.Context, store *storage.Store) (RuntimePlan, error) {
				liveStore = store
				var raw string
				if err := observer.QueryRow(factoryCtx, `SELECT encode(epoch,'hex') FROM public.recorder_epochs ORDER BY run_id DESC LIMIT 1`).Scan(&raw); err != nil {
					return RuntimePlan{}, err
				}
				var err error
				epoch, err = storage.ParseRecorderEpoch(raw)
				if err != nil {
					return RuntimePlan{}, err
				}
				request, err := storage.NewRecorderCoverageRequest(epoch, 0, epoch, nil)
				if err != nil {
					return RuntimePlan{}, err
				}
				coverage, err := store.ReadRecorderCoverage(factoryCtx, request)
				if err != nil || coverage.Kind() != storage.RecorderCoverageReadComplete || len(coverage.Epochs()) != 1 || len(coverage.Gaps()) != 0 {
					return RuntimePlan{}, fmt.Errorf("Recorder was not readable from Run's migrated Store before admission: %#v %v", coverage, err)
				}
				return RuntimePlan{Identity: unconfiguredIdentity{}}, nil
			})
			ready := make(chan struct{})
			result := make(chan error, 1)
			var listener net.ListenConfig
			go func() {
				if withFactory {
					result <- RunWithStartupFactory(ctx, cfg.startup, listener.Listen, func(net.Addr) { close(ready) }, factory)
				} else {
					result <- RunWithStartup(ctx, cfg.startup, listener.Listen, func(net.Addr) { close(ready) })
				}
			}()
			finished := false
			t.Cleanup(func() {
				if finished {
					return
				}
				cancel()
				select {
				case <-result:
				case <-time.After(12 * time.Second):
					t.Error("Run did not drain")
				}
			})
			select {
			case <-ready:
			case err := <-result:
				finished = true
				t.Fatalf("Run exited before ready: %v", err)
			case <-time.After(8 * time.Second):
				t.Fatal("Run did not become ready")
			}
			if withFactory {
				request, err := storage.NewRecorderCoverageRequest(epoch, 0, epoch, nil)
				if err != nil {
					t.Fatal(err)
				}
				coverage, err := liveStore.ReadRecorderCoverage(context.Background(), request)
				if err != nil || coverage.Kind() != storage.RecorderCoverageReadComplete {
					t.Fatalf("live Store lost coverage: %#v %v", coverage, err)
				}
			}
			cancel()
			select {
			case err := <-result:
				finished = true
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(12 * time.Second):
				t.Fatal("Run did not drain")
			}
			count, clean, err := nha030EpochState(observer)
			if err != nil || count != 1 || !clean {
				t.Fatalf("Recorder/Store shutdown order: count=%d clean=%v err=%v", count, clean, err)
			}
		})
	}
}

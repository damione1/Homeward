package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/ecobee"
	"github.com/damione1/Homeward/internal/ecobee/ecobeetest"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
)

func nha136Operational(address, accessory string) []byte {
	return []byte(fmt.Sprintf("ecobee:\n  instance: nha136-synthetic\n  address: %q\n  accessory_id: %q\n  credential_secret: synthetic-pairing\n", address, accessory))
}
func nha136ServerCredential(t *testing.T, fixture *ecobeetest.Fixture) storage.SecretValue {
	t.Helper()
	raw, err := json.Marshal(ecobee.PairingInput{Version: 1, ControllerID: fixture.Controller.DeviceID, ControllerPublicKey: fixture.Controller.PublicKey, ControllerPrivateKey: fixture.Controller.PrivateKey, AccessoryID: fixture.Pairing.DeviceID, AccessoryPublicKey: fixture.Pairing.PublicKey})
	if err != nil {
		t.Fatal(err)
	}
	value, err := storage.NewSecretValue(raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestNHA136DefaultRunEcobeeOnlyScopeAndEncryptedIdentity(t *testing.T) {
	for _, selected := range []string{"v1", "ecobee_mvp_v1"} {
		t.Run(selected, func(t *testing.T) {
			thermostat := ecobeetest.New(t)
			cluster := testpg.Start(t)
			data := shortPrivateDataDir(t)
			operational := nha136Operational(thermostat.Address, thermostat.Pairing.DeviceID)
			launch := nha137LaunchFixture(cluster.Port, data, selected)
			parsed, err := config.ParseWithEnv(append(append([]byte(nil), launch...), operational...), func(string) (string, bool) { return testpg.Password(), true })
			if err != nil {
				t.Fatal(err)
			}
			connection, err := parsed.PostgresSettings()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			store, err := storage.Open(ctx, connection)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Migrate(ctx); err != nil {
				store.Close()
				t.Fatal(err)
			}
			secrets, err := store.BootstrapSecrets(ctx, data)
			if err != nil {
				store.Close()
				t.Fatal(err)
			}
			settings, ok := parsed.EcobeeSettings()
			if !ok {
				store.Close()
				t.Fatal("Ecobee settings missing")
			}
			if err := secrets.RotateCredential(ctx, settings.CredentialHandle(), nha136ServerCredential(t, thermostat)); err != nil {
				store.Close()
				t.Fatal(err)
			}
			store.Close()
			// All setup pairing has completed before Run. Its cold start may verify/read,
			// but it must never PUT, re-pair or mint a new controller identity.
			beforePUT := thermostat.CharacteristicPUT.Load()
			beforeVerify := thermostat.VerifyRequests()
			for cycle := 0; cycle < 2; cycle++ {
				startup := nha137StartupFixture(t, operational, launch)
				runCtx, stop := context.WithCancel(ctx)
				done := make(chan error, 1)
				ready := make(chan net.Addr, 1)
				actual := make(chan *observationRuntime, 1)
				joined := false
				t.Cleanup(func() {
					if joined {
						return
					}
					stop()
					select {
					case <-done:
					case <-time.After(8 * time.Second):
						t.Error("NHA136 owned Run failed to join")
					}
				})
				go func() {
					done <- runWithDependencies(runCtx, startup, (&net.ListenConfig{}).Listen, func(a net.Addr) { ready <- a }, nil, runDependencies{observeRuntime: func(r *observationRuntime) { actual <- r }})
				}()
				if selected == "v1" {
					select {
					case err := <-done:
						joined = true
						if !errors.Is(err, registry.ErrOutsideManifest) {
							stop()
							t.Fatalf("baseline refusal: %v", err)
						}
					case <-ready:
						stop()
						t.Fatal("baseline Ecobee reached ready")
					case <-ctx.Done():
						stop()
						t.Fatal(ctx.Err())
					}
					stop()
					if thermostat.VerifyRequests() != beforeVerify || thermostat.CharacteristicGET.Load() != 0 || thermostat.CharacteristicPUT.Load() != beforePUT {
						t.Fatal("baseline opened thermostat transport")
					}
					break
				}
				var runtime *observationRuntime
				select {
				case runtime = <-actual:
				case err := <-done:
					stop()
					t.Fatal(err)
				case <-ctx.Done():
					stop()
					t.Fatal(ctx.Err())
				}
				select {
				case <-ready:
				case err := <-done:
					stop()
					t.Fatal(err)
				case <-ctx.Done():
					stop()
					t.Fatal(ctx.Err())
				}
				// Subscription/barrier, not sleeps, waits for the actual configured pilot.
				subscription, _, err := runtime.sequence.SubscribeUI()
				if err != nil {
					stop()
					t.Fatal(err)
				}
				alias, err := registry.ParseHAAlias(ecobee.Alias)
				if err != nil {
					stop()
					t.Fatal(err)
				}
				var observed bool
				for !observed {
					entity, resolveErr := runtime.sequence.ResolveCommandAlias(alias)
					if resolveErr == nil {
						state, present, stateErr := runtime.sequence.State(entity)
						if stateErr != nil {
							stop()
							t.Fatal(stateErr)
						}
						if present {
							value, fresh := state.CriticalValue()
							_, isClimate := value.Climate()
							observed = fresh && isClimate
						}
					}
					if observed {
						break
					}
					if _, err := subscription.Next(ctx); err != nil {
						subscription.Close()
						stop()
						t.Fatal("climate publication barrier", err)
					}
				}
				subscription.Close()
				if thermostat.CharacteristicPUT.Load() != beforePUT {
					stop()
					t.Fatal("cold start issued command")
				}
				stop()
				select {
				case err := <-done:
					joined = true
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("Run did not join", ctx.Err())
				}
			}
		})
	}
}

func TestNHA136ActualActivationRejectsEcobeeUnderBaselineBeforeNetwork(t *testing.T) {
	thermostat := ecobeetest.New(t)
	rig := nha029StartOwner(t, nha029OwnerParent, nha029OwnerChild, nil)
	defer rig.close(t)
	before := thermostat.VerifyRequests()
	source := append(append([]byte(nil), rig.raw...), nha136Operational(thermostat.Address, thermostat.Pairing.DeviceID)...)
	if err := os.WriteFile(rig.path, source, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := rig.owner.Activate(ctx, rig.request(t, nil, nil, nil)); err == nil {
		t.Fatal("baseline operational replacement admitted Ecobee")
	}
	if thermostat.VerifyRequests() != before || thermostat.CharacteristicGET.Load() != 0 || thermostat.CharacteristicPUT.Load() != 0 {
		t.Fatal("refused activation opened thermostat")
	}

}

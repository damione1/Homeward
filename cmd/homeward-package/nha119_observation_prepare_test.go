package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/server"
)

func nha119PreparationFixture(t *testing.T) (string, string, string, []byte) {
	t.Helper()
	root, credentials, _ := packageFixture(t)
	inputs := filepath.Join(root, "observation-inputs")
	if err := os.Mkdir(inputs, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credentials, "password"), []byte(strings.Repeat("a", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "configuration.yaml")
	text := "listen: '127.0.0.1:0'\ndata_dir: '" + filepath.Join(root, "data") + "'\npostgres:\n  host: '127.0.0.1'\n  port: 1\n  database: synthetic\n  user: synthetic\n  password_env: HOMEWARD_POSTGRES_PASSWORD\n  sslmode: disable\n"
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"mqtt_username": "synthetic-mqtt-user", "mqtt_password": "synthetic-mqtt-password",
		"login_name": "synthetic-observer", "display_name": "Synthetic Observer", "account_password": "NHA119_Synthetic_account_password_123!",
		"mqtt_mappings": []map[string]string{{"device_identifier": "synthetic-device", "unique_id": "synthetic-sensor", "alias": "sensor.synthetic_temperature"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inputs, "inputs.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return inputs, credentials, path, raw
}

func nha119PreparationUncalled(calls *int) observationPreparationDependencies {
	return observationPreparationDependencies{
		load: func(string, config.BootstrapChoice, config.EnvLookup) (config.StartupInputs, error) {
			*calls++
			return config.StartupInputs{}, errPackage
		},
		mapping: func(string, string, string) (server.MQTTReadOnlyObservationMapping, error) {
			*calls++
			return server.MQTTReadOnlyObservationMapping{}, errPackage
		},
		input: func(string, string, string, string, string, ...server.MQTTReadOnlyObservationMapping) (server.MQTTReadOnlyPreparationInput, error) {
			*calls++
			return server.MQTTReadOnlyPreparationInput{}, errPackage
		},
		prepare: func(context.Context, config.StartupInputs, server.MQTTReadOnlyPreparationInput) error {
			*calls++
			return errPackage
		},
	}
}

func TestNHA119ObservationPreparationUsesPhysicalGuardedStartupAndOfflineTypedHelper(t *testing.T) {
	inputs, credentials, path, raw := nha119PreparationFixture(t)
	var out bytes.Buffer
	loaded, mapped, constructed, prepared := 0, 0, 0, 0
	deps := observationPreparationDependencies{
		load: func(actual string, choice config.BootstrapChoice, lookup config.EnvLookup) (config.StartupInputs, error) {
			loaded++
			if actual != path {
				t.Fatal("preparation did not select its physical configuration")
			}
			if password, ok := lookup("HOMEWARD_POSTGRES_PASSWORD"); !ok || password != strings.Repeat("a", 64) {
				t.Fatal("private PostgreSQL credential was not selected over stale environment entries")
			}
			for _, key := range []string{"mqtt_username", "mqtt_password", "login_name", "display_name", "account_password"} {
				if _, exists := lookup(key); exists {
					t.Fatal("preparation payload entered the environment lookup")
				}
			}
			return config.LoadStartupPaths(actual, choice, lookup)
		},
		mapping: func(deviceIdentifier, uniqueID, alias string) (server.MQTTReadOnlyObservationMapping, error) {
			mapped++
			if deviceIdentifier != "synthetic-device" || uniqueID != "synthetic-sensor" || alias != "sensor.synthetic_temperature" {
				t.Fatal("explicit mapping tuple was changed")
			}
			return server.NewMQTTReadOnlyObservationMapping(deviceIdentifier, uniqueID, alias)
		},
		input: func(username, password, login, display, accountPassword string, mappings ...server.MQTTReadOnlyObservationMapping) (server.MQTTReadOnlyPreparationInput, error) {
			constructed++
			if len(mappings) != 1 {
				t.Fatal("explicit mapping was not passed to opaque input")
			}
			if username != "synthetic-mqtt-user" || password != "synthetic-mqtt-password" || login != "synthetic-observer" || display != "Synthetic Observer" || accountPassword != "NHA119_Synthetic_account_password_123!" {
				t.Fatal("private payload was not passed exactly to the opaque input constructor")
			}
			return server.NewMQTTReadOnlyPreparationInput(username, password, login, display, accountPassword, mappings...)
		},
		prepare: func(ctx context.Context, startup config.StartupInputs, _ server.MQTTReadOnlyPreparationInput) error {
			prepared++
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 90*time.Second || !startup.Valid() || !startup.MQTTReadOnlyObservation() {
				t.Fatal("offline helper lacked its bounded context or startup guard")
			}
			physical, _, ok := startup.WorkspaceDocumentLocation()
			if !ok || physical != path {
				t.Fatal("offline helper received detached startup instead of its physical file")
			}
			return nil // No database, Run, listener, MQTT or HA operation.
		},
	}
	env := []string{"HOMEWARD_POSTGRES_PASSWORD=stale", "HOMEWARD_POSTGRES_PASSWORD=second-stale", "OTHER=preserved"}
	if err := runObservationPreparation(context.Background(), inputs, credentials, path, env, &out, deps); err != nil {
		t.Fatal("valid isolated preparation failed")
	}
	if loaded != 1 || mapped != 1 || constructed != 1 || prepared != 1 || out.String() != "observation preparation complete; runtime not started\n" {
		t.Fatal("preparation call sequence or fixed output differs")
	}
	retained, err := os.ReadFile(filepath.Join(inputs, "inputs.json"))
	if err != nil || !bytes.Equal(retained, raw) || env[0] != "HOMEWARD_POSTGRES_PASSWORD=stale" {
		t.Fatal("preparation mutated its private input or caller environment")
	}
}

func TestNHA119ObservationPreparationRejectsStrictJSONBeforeAnyHelper(t *testing.T) {
	_, _, _, valid := nha119PreparationFixture(t)
	duplicate := append([]byte(`{"mqtt_username":"duplicate",`), valid[1:]...)
	unknown := append(bytes.Clone(valid[:len(valid)-1]), []byte(`,"unknown":"synthetic-secret"}`)...)
	nullValue := bytes.Replace(valid, []byte(`"mqtt_username":"synthetic-mqtt-user"`), []byte(`"mqtt_username":null`), 1)
	numberValue := bytes.Replace(valid, []byte(`"mqtt_username":"synthetic-mqtt-user"`), []byte(`"mqtt_username":123`), 1)
	blankValue := bytes.Replace(valid, []byte(`"mqtt_username":"synthetic-mqtt-user"`), []byte(`"mqtt_username":" "`), 1)
	invalidUTF8 := bytes.Replace(valid, []byte("synthetic-mqtt-user"), []byte{0xff}, 1)
	for _, bad := range [][]byte{[]byte(`[]`), []byte(`{}`), []byte(`null`), duplicate, unknown, nullValue, numberValue, blankValue, invalidUTF8} {
		inputs, credentials, path, _ := nha119PreparationFixture(t)
		if err := os.WriteFile(filepath.Join(inputs, "inputs.json"), bad, 0600); err != nil {
			t.Fatal(err)
		}
		calls := 0
		var out bytes.Buffer
		if err := runObservationPreparation(context.Background(), inputs, credentials, path, nil, &out, nha119PreparationUncalled(&calls)); err == nil || calls != 0 || out.Len() != 0 {
			t.Fatal("invalid JSON reached a helper or produced output")
		}
	}
	inputs, credentials, path, raw := nha119PreparationFixture(t)
	if err := os.WriteFile(filepath.Join(inputs, "inputs.json"), append(raw, []byte(` {}`)...), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	if err := runObservationPreparation(context.Background(), inputs, credentials, path, nil, io.Discard, nha119PreparationUncalled(&calls)); err == nil || calls != 0 {
		t.Fatal("trailing JSON reached a helper")
	}
}

func TestNHA119ObservationPreparationRejectsUnsafeFilesBeforeAnyHelper(t *testing.T) {
	for _, kind := range []string{"missing", "symlink", "fifo", "public-file", "public-directory", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			inputs, credentials, path, _ := nha119PreparationFixture(t)
			file := filepath.Join(inputs, "inputs.json")
			switch kind {
			case "public-file":
				if err := os.Chmod(file, 0644); err != nil {
					t.Fatal(err)
				}
			case "public-directory":
				if err := os.Chmod(inputs, 0755); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				if err := os.WriteFile(file, bytes.Repeat([]byte("x"), (1<<20)+1), 0600); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				if kind == "symlink" {
					if err := os.Symlink(path, file); err != nil {
						t.Fatal(err)
					}
				} else if kind == "fifo" {
					if err := syscall.Mkfifo(file, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			calls := 0
			var out bytes.Buffer
			if err := runObservationPreparation(context.Background(), inputs, credentials, path, nil, &out, nha119PreparationUncalled(&calls)); err == nil || calls != 0 || out.Len() != 0 {
				t.Fatal("unsafe private input reached a helper or produced output")
			}
		})
	}
}

func TestNHA119ObservationPreparationRedactsHelperFailureAndCancellation(t *testing.T) {
	for _, cancelDuringHelper := range []bool{false, true} {
		inputs, credentials, path, _ := nha119PreparationFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		var out bytes.Buffer
		deps := observationPreparationDependencies{load: config.LoadStartupPaths, mapping: server.NewMQTTReadOnlyObservationMapping, input: server.NewMQTTReadOnlyPreparationInput,
			prepare: func(context.Context, config.StartupInputs, server.MQTTReadOnlyPreparationInput) error {
				if cancelDuringHelper {
					cancel()
					return nil
				}
				return errors.New("synthetic-helper-sensitive-detail")
			}}
		err := runObservationPreparation(ctx, inputs, credentials, path, nil, &out, deps)
		cancel()
		if err != errPackage || out.Len() != 0 {
			t.Fatal("helper detail leaked or cancellation reported success")
		}
	}
	for _, args := range [][]string{{"prepare-observation", "inputs.json"}, {"prepare-observation", "--password=synthetic"}} {
		if err := run(args, io.Discard); err == nil {
			t.Fatal("preparation accepted additional arguments")
		}
	}
}

func TestNHA119ObservationPreparationRejectsInvalidExplicitMappings(t *testing.T) {
	_, _, _, valid := nha119PreparationFixture(t)
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(valid, &payload); err != nil {
		t.Fatal(err)
	}
	badMappings := []string{
		`null`, `{}`, `[]`, `[null]`, `[{}]`,
		`[{"device_identifier":"synthetic-device","unique_id":"synthetic-sensor"}]`,
		`[{"device_identifier":"synthetic-device","unique_id":"synthetic-sensor","alias":null}]`,
		`[{"device_identifier":"synthetic-device","unique_id":"synthetic-sensor","alias":12}]`,
		`[{"device_identifier":"synthetic-device","unique_id":"synthetic-sensor","alias":" "}]`,
		`[{"device_identifier":"synthetic-device","unique_id":"synthetic-sensor","alias":"sensor.synthetic_temperature","unknown":"x"}]`,
		`[{"device_identifier":"synthetic-device","unique_id":"synthetic-sensor","alias":"sensor.synthetic_temperature","alias":"duplicate"}]`,
	}
	tuple := `{"device_identifier":"synthetic-device","unique_id":"synthetic-sensor","alias":"sensor.synthetic_temperature"}`
	badMappings = append(badMappings, "["+strings.TrimSuffix(strings.Repeat(tuple+",", 257), ",")+"]")
	for _, bad := range badMappings {
		payload["mqtt_mappings"] = json.RawMessage(bad)
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		inputs, credentials, path, _ := nha119PreparationFixture(t)
		if err := os.WriteFile(filepath.Join(inputs, "inputs.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		calls := 0
		var out bytes.Buffer
		if err := runObservationPreparation(context.Background(), inputs, credentials, path, nil, &out, nha119PreparationUncalled(&calls)); err != errPackage || calls != 0 || out.Len() != 0 {
			t.Fatal("invalid mapping JSON reached a helper or produced output")
		}
	}
	delete(payload, "mqtt_mappings")
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeObservationPreparation(raw); err != errPackage {
		t.Fatal("missing mappings accepted")
	}
	payload["mqtt_mappings"] = json.RawMessage("[" + strings.TrimSuffix(strings.Repeat(tuple+",", 256), ",") + "]")
	raw, err = json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeObservationPreparation(raw)
	if err != nil || len(decoded.mappings) != 256 {
		t.Fatal("JSON mapping upper bound differs")
	}
}

func TestNHA119ObservationPreparationStopsOnOpaqueMappingRejection(t *testing.T) {
	inputs, credentials, path, _ := nha119PreparationFixture(t)
	calls := 0
	deps := nha119PreparationUncalled(&calls)
	deps.load = config.LoadStartupPaths
	deps.mapping = func(string, string, string) (server.MQTTReadOnlyObservationMapping, error) {
		return server.MQTTReadOnlyObservationMapping{}, errors.New("synthetic-sensitive-mapping-detail")
	}
	var out bytes.Buffer
	if err := runObservationPreparation(context.Background(), inputs, credentials, path, nil, &out, deps); err != errPackage || calls != 0 || out.Len() != 0 {
		t.Fatal("rejected opaque mapping reached input/preparation or leaked details")
	}
}

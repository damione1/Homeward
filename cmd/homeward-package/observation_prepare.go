package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/server"
)

const observationBootstrapRoot = "/run/homeward-observation-bootstrap"

type observationPreparationPayload struct {
	username, password, login, display, accountPassword string
	mappings                                            []observationPreparationMapping
}

type observationPreparationMapping struct {
	deviceIdentifier, uniqueID, alias string
}

type observationPreparationDependencies struct {
	load    func(string, config.BootstrapChoice, config.EnvLookup) (config.StartupInputs, error)
	mapping func(string, string, string) (server.MQTTReadOnlyObservationMapping, error)
	input   func(string, string, string, string, string, ...server.MQTTReadOnlyObservationMapping) (server.MQTTReadOnlyPreparationInput, error)
	prepare func(context.Context, config.StartupInputs, server.MQTTReadOnlyPreparationInput) error
}

func prepareObservation(out io.Writer) error {
	return runObservationPreparation(context.Background(), observationBootstrapRoot, credentialRoot,
		packageRoot+"/configuration.yaml", os.Environ(), out, observationPreparationDependencies{
			load: config.LoadStartupPaths, mapping: server.NewMQTTReadOnlyObservationMapping, input: server.NewMQTTReadOnlyPreparationInput,
			prepare: server.PrepareMQTTReadOnlyObservation,
		})
}

// This is a one-shot offline operator operation. No server Run, listener, driver
// or MQTT client is constructed here; only the explicitly supplied helper runs.
func runObservationPreparation(parent context.Context, inputsPath, credentialsPath, configPath string, existing []string, out io.Writer, deps observationPreparationDependencies) error {
	if parent.Err() != nil || deps.load == nil || deps.mapping == nil || deps.input == nil || deps.prepare == nil {
		return errPackage
	}
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	root, err := openPrivate(inputsPath)
	if err != nil {
		return errPackage
	}
	defer root.Close()
	raw, err := readPrivate(root, "inputs.json")
	if err != nil {
		clear(raw)
		return errPackage
	}
	defer clear(raw)
	payload, err := decodeObservationPreparation(raw)
	if err != nil {
		return errPackage
	}
	credentials, err := openPrivate(credentialsPath)
	if err != nil {
		return errPackage
	}
	defer credentials.Close()
	password, err := readPrivate(credentials, "password")
	if err != nil || len(password) != 64 {
		clear(password)
		return errPackage
	}
	defer clear(password)
	env := databaseEnvironment(existing, string(password))
	lookup := func(name string) (string, bool) {
		for _, entry := range env {
			key, value, ok := strings.Cut(entry, "=")
			if ok && key == name {
				return value, true
			}
		}
		return "", false
	}
	startup, err := deps.load(configPath, config.DefaultBootstrapChoice(), lookup)
	if err != nil || !startup.Valid() {
		return errPackage
	}
	if _, _, physical := startup.WorkspaceDocumentLocation(); !physical {
		return errPackage
	}
	startup = startup.WithMQTTReadOnlyObservation()
	mappings := make([]server.MQTTReadOnlyObservationMapping, 0, len(payload.mappings))
	for _, tuple := range payload.mappings {
		mapping, err := deps.mapping(tuple.deviceIdentifier, tuple.uniqueID, tuple.alias)
		if err != nil || ctx.Err() != nil {
			return errPackage
		}
		mappings = append(mappings, mapping)
	}
	input, err := deps.input(payload.username, payload.password, payload.login, payload.display, payload.accountPassword, mappings...)
	if err != nil || ctx.Err() != nil {
		return errPackage
	}
	if err := deps.prepare(ctx, startup, input); err != nil || ctx.Err() != nil {
		return errPackage
	}
	_, err = fmt.Fprintln(out, "observation preparation complete; runtime not started")
	return err
}

func decodeObservationPreparation(raw []byte) (observationPreparationPayload, error) {
	var result observationPreparationPayload
	if !utf8.Valid(raw) {
		return result, errPackage
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return observationPreparationPayload{}, errPackage
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return observationPreparationPayload{}, errPackage
		}
		seen[key] = true
		var destination *string
		switch key {
		case "mqtt_mappings":
			var err error
			result.mappings, err = decodeObservationMappings(decoder)
			if err != nil {
				return observationPreparationPayload{}, errPackage
			}
			continue
		case "mqtt_username":
			destination = &result.username
		case "mqtt_password":
			destination = &result.password
		case "login_name":
			destination = &result.login
		case "display_name":
			destination = &result.display
		case "account_password":
			destination = &result.accountPassword
		default:
			return observationPreparationPayload{}, errPackage
		}
		if decoder.Decode(destination) != nil || strings.TrimSpace(*destination) == "" {
			return observationPreparationPayload{}, errPackage
		}
	}
	last, err := decoder.Token()
	if err != nil || last != json.Delim('}') || len(seen) != 6 {
		return observationPreparationPayload{}, errPackage
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return observationPreparationPayload{}, errPackage
	}
	return result, nil
}

// Mapping tuples are operator supplied. No discovery, inference or fallback is used.
func decodeObservationMappings(decoder *json.Decoder) ([]observationPreparationMapping, error) {
	first, err := decoder.Token()
	if err != nil || first != json.Delim('[') {
		return nil, errPackage
	}
	var mappings []observationPreparationMapping
	for decoder.More() {
		if len(mappings) >= 256 {
			return nil, errPackage
		}
		first, err := decoder.Token()
		if err != nil || first != json.Delim('{') {
			return nil, errPackage
		}
		var tuple observationPreparationMapping
		seen := make(map[string]bool)
		for decoder.More() {
			token, err := decoder.Token()
			key, ok := token.(string)
			if err != nil || !ok || seen[key] {
				return nil, errPackage
			}
			seen[key] = true
			var destination *string
			switch key {
			case "device_identifier":
				destination = &tuple.deviceIdentifier
			case "unique_id":
				destination = &tuple.uniqueID
			case "alias":
				destination = &tuple.alias
			default:
				return nil, errPackage
			}
			if decoder.Decode(destination) != nil || strings.TrimSpace(*destination) == "" {
				return nil, errPackage
			}
		}
		last, err := decoder.Token()
		if err != nil || last != json.Delim('}') || len(seen) != 3 {
			return nil, errPackage
		}
		mappings = append(mappings, tuple)
	}
	last, err := decoder.Token()
	if err != nil || last != json.Delim(']') || len(mappings) == 0 {
		return nil, errPackage
	}
	return mappings, nil
}

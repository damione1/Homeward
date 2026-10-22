// Package integrationadmin is the administrative facade over the delivered149
// document owner. It owns neither storage nor activation or driver execution.
package integrationadmin

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/integrationadmin/v1"
)

var (
	ErrInvalid          = errors.New("integration administration: invalid request")
	ErrUnknownType      = errors.New("integration administration: unknown type")
	ErrNotFound         = errors.New("integration administration: instance not found")
	ErrAlreadyExists    = errors.New("integration administration: instance already exists")
	ErrWorkspaceInvalid = errors.New("integration administration: workspace requires full-document correction")
	ErrUnavailable      = errors.New("integration administration: unavailable")
)

// A descriptor declares ONLY shipped parsers and capability families. It is not
// an inferred device manifest or a claim that an executable implements a type.
type descriptor struct {
	id, fixed                     string
	fields, secrets, capabilities []string
	shape                         string
}

var descriptors = []descriptor{
	{id: "mqtt", fields: []string{"profile", "broker", "instance", "mode", "subscriptions", "command_topics", "transition_lights", "will"}, secrets: []string{"username_secret", "password_secret"}, capabilities: []string{contracts.BinarySchema, contracts.SwitchSchema, contracts.MeasurementSchema, contracts.LightSchema}, shape: "object"},
	{id: "ecobee", fields: []string{"instance", "address", "accessory_id"}, secrets: []string{"credential_secret"}, capabilities: []string{contracts.ClimateSchema}, shape: "object"},
	{id: "neviweb", fields: []string{"instance", "base_url", "account_id", "network_id", "poll_seconds", "daily_limit", "external_reserve", "day_zone", "bindings"}, secrets: []string{"username_secret", "password_secret", "session_secret"}, capabilities: []string{contracts.ClimateSchema, contracts.SwitchSchema, contracts.MeasurementSchema, contracts.BinarySchema}, shape: "object"},
	{id: "hilo", fields: []string{"instance", "account_id", "location_id", "location_hilo_id", "daily_limit", "day_zone", "lease_window_seconds", "energy_max_gap_seconds", "api_base_url", "platform_base_url", "bindings"}, secrets: []string{"credential_secret"}, capabilities: []string{contracts.ClimateSchema, contracts.SwitchSchema, contracts.MeasurementSchema, contracts.BinarySchema}, shape: "object"},
	{id: "solar", fixed: "homeward-sun-v1", fields: []string{"latitude", "longitude", "elevation_meters", "timezone"}, capabilities: []string{contracts.SolarSchema, contracts.SolarEventSchema}, shape: "object"},
	{id: "workday", fixed: "homeward-workday-v1", fields: []string{"timezone"}, capabilities: []string{contracts.BinarySchema}, shape: "object"},
	{id: "energy", fixed: "homeward-energy-v1", capabilities: []string{contracts.MeasurementSchema}, shape: "array"},
	{id: "derived_power", fixed: "homeward-derived-power-v1", capabilities: []string{contracts.MeasurementSchema}, shape: "string"},
}

func findDescriptor(id string) (descriptor, error) {
	for _, d := range descriptors {
		if d.id == id {
			return d, nil
		}
	}
	return descriptor{}, ErrUnknownType
}
func schemaVersion(d descriptor) string { return "homeward.integration/" + d.id + "/1" }
func descriptorWire(d descriptor) *wire.IntegrationType {
	// The structural schema describes the decoded public YAML fragment. Exact
	// semantic admission remains the existing strict operational parser.
	schema := map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": d.shape, "x-homeward-validator": "config.ParseOperationalWithEnv", "x-homeward-secret-reference-fields": d.secrets}
	if d.shape == "object" {
		fields := map[string]any{}
		for _, name := range d.fields {
			fields[name] = map[string]any{}
		}
		schema["properties"] = fields
		schema["additionalProperties"] = false
	}
	if d.id == "energy" {
		schema["maxItems"] = 3
		schema["items"] = map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"source": map[string]any{}, "output": map[string]any{}, "precision": map[string]any{}, "max_gap": map[string]any{}, "time_basis": map[string]any{}}, "required": []string{"source", "output", "precision", "max_gap", "time_basis"}}
	}
	if d.id == "derived_power" {
		schema["enum"] = []string{"mvp-v1"}
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil
	}
	hash := sha256.Sum256(raw)
	return &wire.IntegrationType{TypeId: d.id, ManifestVersion: "homeward.integration-manifest/1", SchemaVersion: schemaVersion(d), JsonSchema: raw, SchemaSha256: append([]byte(nil), hash[:]...), CapabilitySchemaIds: append([]string(nil), d.capabilities...)}
}

package integrationadmin

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/integrationadmin/v1"
	"github.com/damione1/Homeward/internal/config"
)

func TestNHA136AdminEcobeeUsesBoundSecretReferencesAndOperationalSelection(t *testing.T) {
	d, err := findDescriptor("ecobee")
	if err != nil {
		t.Fatal(err)
	}
	description := descriptorWire(d)
	if description == nil || len(description.CapabilitySchemaIds) != 1 || description.CapabilitySchemaIds[0] != contracts.ClimateSchema {
		t.Fatal("Ecobee advertised an unproved capability family")
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(description.JsonSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Properties) != 3 || schema.Properties["credential_secret"] != nil || schema.Properties["pin"] != nil {
		t.Fatal("public schema exposed a private credential field")
	}
	public := func() *wire.PublicConfiguration {
		return &wire.PublicConfiguration{
			SchemaVersion: "homeward.integration/ecobee/1",
			YamlSource:    []byte("instance: nha136-admin\naddress: '127.0.0.1:12345'\naccessory_id: 'CA:FE:00:11:22:33'\n"),
			Secrets:       []*wire.SecretReference{{Field: "credential_secret", IntegrationInstanceId: "nha136-admin", Name: "synthetic-pairing"}},
		}
	}
	node, err := privateFragment(d, "nha136-admin", public())
	if err != nil {
		t.Fatal(err)
	}
	item, err := publicEntry(entry{typ: "ecobee", id: "nha136-admin", node: node}, false)
	if err != nil || item == nil || item.GetEditable() == nil {
		t.Fatal("Ecobee public roundtrip failed", err)
	}
	editable := item.GetEditable()
	if len(editable.Secrets) != 1 || editable.Secrets[0].IntegrationInstanceId != "nha136-admin" || editable.Secrets[0].Name != "synthetic-pairing" || bytes.Contains(editable.YamlSource, []byte("credential_secret")) || bytes.Contains(editable.YamlSource, []byte("synthetic-pairing")) {
		t.Fatal("public configuration exposed or lost its bound credential locator")
	}
	root, err := admittedDocument([]byte("workday: {timezone: America/Toronto}\n"))
	if err != nil {
		t.Fatal(err)
	}
	setField(root, "ecobee", node)
	setDisabled(root, map[string]bool{"nha136-admin": true})
	raw, err := encodeNode(root)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := config.ParseOperationalWithEnv(raw, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if _, present := parsed.EcobeeSettings(); present {
		t.Fatal("admin disable retained the Ecobee pilot")
	}
	if _, present := parsed.WorkdaySettings(); !present {
		t.Fatal("admin mutation removed an unrelated module")
	}
	for name, change := range map[string]func(*wire.PublicConfiguration){
		"foreign-reference": func(p *wire.PublicConfiguration) { p.Secrets[0].IntegrationInstanceId = "foreign" },
		"inline-locator": func(p *wire.PublicConfiguration) {
			p.YamlSource = append(p.YamlSource, []byte("credential_secret: inline-locator\n")...)
		},
		"pairing-pin": func(p *wire.PublicConfiguration) {
			p.YamlSource = append(p.YamlSource, []byte("pin: synthetic-forbidden\n")...)
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := public()
			change(input)
			if _, err := privateFragment(d, "nha136-admin", input); err == nil {
				t.Fatal("credential boundary bypass admitted")
			}
		})
	}
}

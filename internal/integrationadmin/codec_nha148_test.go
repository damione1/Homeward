package integrationadmin

import (
	"bytes"
	wire "github.com/damione1/Homeward/gen/home/integrationadmin/v1"
	"go.yaml.in/yaml/v3"
	"testing"
)

func nha148Public() *wire.PublicConfiguration {
	return &wire.PublicConfiguration{SchemaVersion: "homeward.integration/mqtt/1", YamlSource: []byte("profile: '5'\nbroker: mqtt://127.0.0.1:1883\ninstance: nha148\nmode: passive\nsubscriptions: [homeassistant/#]\n"), Secrets: []*wire.SecretReference{{Field: "username_secret", IntegrationInstanceId: "nha148", Name: "user-ref"}, {Field: "password_secret", IntegrationInstanceId: "nha148", Name: "password-ref"}}}
}
func TestNHA148PublicCredentialSlotsHaveOnlyInstanceBoundReferences(t *testing.T) {
	d, _ := findDescriptor("mqtt")
	input := nha148Public()
	node, err := privateFragment(d, "nha148", input)
	if err != nil {
		t.Fatal(err)
	}
	item, err := publicEntry(entry{typ: "mqtt", id: "nha148", node: node}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(item.GetEditable().Secrets) != 2 || bytes.Contains(item.GetEditable().YamlSource, []byte("password_secret")) {
		t.Fatal("public config exposed scalar credential slot")
	}
	for _, change := range []func(*wire.PublicConfiguration){func(c *wire.PublicConfiguration) {
		c.YamlSource = append(c.YamlSource, []byte("password_secret: plaintext-canary\n")...)
	}, func(c *wire.PublicConfiguration) { c.Secrets[1].IntegrationInstanceId = "foreign" }, func(c *wire.PublicConfiguration) { c.SchemaVersion = "unknown" }, func(c *wire.PublicConfiguration) {
		c.YamlSource = append(c.YamlSource, []byte("password: plaintext-canary\n")...)
	}, func(c *wire.PublicConfiguration) {
		c.YamlSource = append(c.YamlSource, []byte("args: [plaintext-canary]\n")...)
	}} {
		c := nha148Public()
		change(c)
		if _, err := privateFragment(d, "nha148", c); err == nil {
			t.Fatal("credential bypass or foreign reference admitted")
		}
	}
	if _, err := findDescriptor("foreign-nha136"); err == nil {
		t.Fatal("unknown driver declared")
	}
	if _, err := findDescriptor("calendar"); err == nil {
		t.Fatal("WIP calendar declared")
	}
}
func TestNHA148NodeMutationPreservesAllUnrelatedSections(t *testing.T) {
	raw := []byte("listen: 127.0.0.1:0\npostgres:\n  password_env: NHA148_PRIVATE_SYNTHETIC_REFERENCE\npublic: {transport: tls}\nworkday: {timezone: America/Toronto}\n")
	root, err := admittedDocument(raw)
	if err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	for _, key := range []string{"listen", "postgres", "public", "workday"} {
		before[key], _ = yaml.Marshal(field(root, key))
	}
	d, _ := findDescriptor("mqtt")
	node, err := privateFragment(d, "nha148", nha148Public())
	if err != nil {
		t.Fatal(err)
	}
	setField(root, "mqtt", node)
	setDisabled(root, map[string]bool{"nha148": true})
	emitted, err := encodeNode(root)
	if err != nil {
		t.Fatal(err)
	}
	after, err := admittedDocument(emitted)
	if err != nil {
		t.Fatal(err)
	}
	for key, expected := range before {
		got, _ := yaml.Marshal(field(after, key))
		if !bytes.Equal(expected, got) {
			t.Fatal("unrelated subtree changed:", key)
		}
	}
	setField(after, "mqtt", nil)
	setDisabled(after, map[string]bool{})
	if _, err := encodeNode(after); err != nil {
		t.Fatal(err)
	}
}
func TestNHA148OpaqueWorkersDoNotInventManifestOrExposeArguments(t *testing.T) {
	root, err := decodeNode([]byte("external_workers:\n  - instance: opaque\n    executable: /usr/bin/true\n    args: [NHA148_PRIVATE_ARGUMENT]\n    secret_names: [credential-ref]\n"))
	if err != nil {
		t.Fatal(err)
	}
	list, err := entries(root)
	if err != nil || len(list) != 1 {
		t.Fatal(err)
	}
	item, err := publicEntry(list[0], true)
	if err != nil {
		t.Fatal(err)
	}
	if item.TypeId != "" || item.GetOpaque() == nil || len(item.GetOpaque().Secrets) != 1 || item.GetEditable() != nil {
		t.Fatal("opaque worker acquired an invented public manifest")
	}
}
func TestNHA148AmbiguousSourcesAndUnknownFieldsCannotBeTransformed(t *testing.T) {
	for _, raw := range []string{"mqtt: {}\nmqtt: {}\n", "mqtt: &shared {instance: a}\nother: *shared\n", "mqtt: {}\n---\nworkday: {}\n", "disabled_integrations: {x: true}\n"} {
		root, err := admittedDocument([]byte(raw))
		if err == nil {
			if _, err = disabled(root); err == nil {
				t.Fatal("ambiguous or invalid document admitted")
			}
		}
	}
}

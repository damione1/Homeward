package mcpkeys

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/damione1/Homeward/internal/accounts"
)

func TestNHA145CredentialFamilyRandomnessAndRedaction(t *testing.T) {
	a, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	text, _ := a.Text()
	other, _ := b.Text()
	if text == other || len(text) != 50 {
		t.Fatal("credential randomness/encoding")
	}
	parsed, err := ParseSecret(text)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := parsed.Digest()
	want, _ := a.Digest()
	if d != want {
		t.Fatal("digest roundtrip")
	}
	if _, err := accounts.ParseAccessSecret(text); err == nil {
		t.Fatal("MCP secret admitted as interactive access")
	}
	access, _ := accounts.NewAccessSecret()
	interactive, _ := access.Text()
	if _, err := ParseSecret(interactive); err == nil {
		t.Fatal("interactive admitted as MCP")
	}
	encoded, _ := json.Marshal(struct {
		Secret Secret
		Digest Digest
	}{a, d})
	for _, output := range []string{string(encoded), fmt.Sprintf("%+v %#v", a, d)} {
		if strings.Contains(output, text) || strings.Contains(output, text[len(Prefix):]) {
			t.Fatal("secret leaked in diagnostics")
		}
	}
	for _, bad := range []string{"", Prefix, Prefix + strings.Repeat("A", 43), text + "=", " " + text} {
		if _, err := ParseSecret(bad); err == nil {
			t.Fatal("malformed secret admitted")
		}
	}
	if _, err := (Secret{}).Digest(); err == nil {
		t.Fatal("zero secret admitted")
	}
	if _, err := (Digest{}).Bytes(); err == nil {
		t.Fatal("zero digest admitted")
	}
}

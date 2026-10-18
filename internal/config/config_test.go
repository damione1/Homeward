package config_test

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/damione1/Homeward/internal/config"
)

func TestEndpointParsingAndZero(t *testing.T) {
	for _, address := range []string{"127.0.0.1:0", "0.0.0.0:65535", "[::1]:8787", "[::]:0"} {
		c, err := config.ParseWithEnv([]byte("listen: '"+address+"'\n"+testPostgres), testLookup)
		if err != nil {
			t.Fatal(err)
		}
		endpoint, err := c.ListenEndpoint()
		if err != nil || endpoint.String() != address {
			t.Fatalf("%s: %s, %v", address, endpoint, err)
		}
		// Mutating a returned value cannot mutate the parsed configuration.
		endpoint = netip.AddrPortFrom(endpoint.Addr(), 123)
		again, _ := c.ListenEndpoint()
		if again.String() != address || endpoint.Port() != 123 {
			t.Fatal("configuration lost encapsulation")
		}
	}
	for _, data := range []string{"listen: '[fe80::1%en0]:42'", "listen: ':8787'", "listen: '127.0.0.1:65536'", "listen: '127.0.0.1:+1'", "listen: 'localhost:42'", "listen: null", "{}", "null", "listen: '127.0.0.1:0'\nlisten: '127.0.0.1:1'", "listen: '127.0.0.1:0'\n---\n"} {
		c, err := config.Parse([]byte(data))
		if err == nil {
			t.Fatalf("accepted %q", data)
		}
		if _, err := c.ListenEndpoint(); !errors.Is(err, config.ErrUninitialized) {
			t.Fatalf("failed parse exposed usable configuration: %q", data)
		}
	}
	if _, err := (config.Config{}).ListenEndpoint(); !errors.Is(err, config.ErrUninitialized) {
		t.Fatal("zero configuration accepted")
	}
}

const testPostgres = "postgres:\n  host: '127.0.0.1'\n  port: 5432\n  database: test\n  user: test\n  password_env: TEST_DB_PASSWORD\n  sslmode: disable\n"

func testLookup(string) (string, bool) { return "fake", true }

func TestNHA027SecretReferenceAndZero(t *testing.T) {
	const canary = "NHA027_private_canary"
	c, err := config.ParseWithEnv([]byte("listen: '127.0.0.1:0'\n"+testPostgres), func(string) (string, bool) { return canary, true })
	if err != nil {
		t.Fatal(err)
	}
	settings, err := c.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	for _, rendered := range []string{fmt.Sprintf("%+v", settings), fmt.Sprintf("%#v", settings), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c)} {
		if strings.Contains(rendered, canary) {
			t.Fatal("formatted config exposed secret")
		}
	}
	if _, err := (config.Config{}).PostgresSettings(); !errors.Is(err, config.ErrUninitialized) {
		t.Fatalf("zero config accepted: %v", err)
	}
	if _, err := config.ParseWithEnv([]byte("listen: '127.0.0.1:0'\n"+testPostgres), func(string) (string, bool) { return "", false }); err == nil || strings.Contains(err.Error(), canary) {
		t.Fatalf("missing secret accepted/leaked: %v", err)
	}
}

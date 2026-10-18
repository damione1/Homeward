package config_test

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/damione1/Homeward/internal/config"
)

func TestEndpointParsingAndZero(t *testing.T) {
	for _, address := range []string{"127.0.0.1:0", "0.0.0.0:65535", "[::1]:8787", "[::]:0"} {
		c, err := config.Parse([]byte("listen: '" + address + "'"))
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

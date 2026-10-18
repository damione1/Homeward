package config_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/config"
)

func TestNHA139PublicSettingsAdmission(t *testing.T) {
	defaultConfig, err := config.ParseWithEnv([]byte("listen: '127.0.0.1:0'\n"+testPostgres(t)), testLookup)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := defaultConfig.PublicTransport()
	if err != nil {
		t.Fatal(err)
	}
	if _, present := settings.CanonicalURL(); present {
		t.Fatal("absent canonical URL became present")
	}
	if settings.MaxRequestBytes() != 4<<20 || settings.MaxConcurrentRequests() != 64 ||
		settings.RequestTimeout() != 30*time.Second || settings.StreamTimeout() != 5*time.Minute ||
		settings.ShutdownTimeout() != 6*time.Second {
		t.Fatalf("unexpected defaults: %+v", settings)
	}
	if _, err := (config.Config{}).PublicTransport(); !errors.Is(err, config.ErrUninitialized) {
		t.Fatalf("zero config public settings: %v", err)
	}
	public := "public:\n  url: 'https://home.example:443/'\n  max_request_bytes: 128\n  max_concurrent_requests: 1\n  request_timeout: 80ms\n  stream_timeout: 2s\n  shutdown_timeout: 1s\n"
	parsed, err := config.ParseWithEnv([]byte("listen: '127.0.0.1:0'\n"+public+testPostgres(t)), testLookup)
	if err != nil {
		t.Fatal(err)
	}
	settings, err = parsed.PublicTransport()
	if err != nil {
		t.Fatal(err)
	}
	url, present := settings.CanonicalURL()
	if !present || url.String() != "https://home.example:443" ||
		settings.MaxRequestBytes() != 128 || settings.MaxConcurrentRequests() != 1 ||
		settings.RequestTimeout() != 80*time.Millisecond || settings.StreamTimeout() != 2*time.Second ||
		settings.ShutdownTimeout() != time.Second {
		t.Fatalf("public settings not retained: %+v, url=%q present=%t", settings, url.String(), present)
	}
	url.Host = "attacker.example"
	again, _ := settings.CanonicalURL()
	if again.Host != "home.example:443" {
		t.Fatal("caller mutated canonical URL")
	}
}

func TestNHA139PublicSettingsRejectInvalidBoundsAndURLs(t *testing.T) {
	for _, invalid := range []string{
		"public:\n  max_request_bytes: 0\n",
		"public:\n  max_request_bytes: 67108865\n",
		"public:\n  max_concurrent_requests: 0\n",
		"public:\n  max_concurrent_requests: 4097\n",
		"public:\n  request_timeout: 0s\n",
		"public:\n  request_timeout: 6m\n",
		"public:\n  stream_timeout: 25h\n",
		"public:\n  shutdown_timeout: 61s\n",
		"public:\n  url: 'http://home.example'\n",
		"public:\n  url: 'https://user:pass@home.example'\n",
		"public:\n  url: 'https://home.example/path'\n",
		"public:\n  url: 'https://home.example/?token=x'\n",
		"public:\n  url: 'https://home.example/#fragment'\n",
		"public:\n  url: 'https://home.example:0'\n",
		"public:\n  url: 'https://home.example:'\n",
		"public:\n  url: 'https://home.example:http'\n",
		"public:\n  url: ''\n",
	} {
		t.Run(fmt.Sprintf("%q", invalid), func(t *testing.T) {
			_, err := config.ParseWithEnv([]byte("listen: '127.0.0.1:0'\n"+invalid+testPostgres(t)), testLookup)
			if !errors.Is(err, config.ErrInvalidPublicSettings) {
				t.Fatalf("accepted invalid public settings: %v", err)
			}
		})
	}
	for _, valid := range []string{"http://127.0.0.1:8787", "http://[::1]:8787", "http://localhost:8787", "https://home.example"} {
		t.Run(valid, func(t *testing.T) {
			_, err := config.ParseWithEnv([]byte("listen: '127.0.0.1:0'\npublic:\n  url: '"+valid+"'\n"+testPostgres(t)), testLookup)
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

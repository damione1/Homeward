package config

import (
	"github.com/damione1/Homeward/internal/ecobee"
	"testing"
)

func TestNHA136SettingsAdmitOnlyExplicitIdentityAndSecretLocator(t *testing.T) {
	input := ecobee.Input{Instance: "nha136-synthetic", Address: "127.0.0.1:12345", AccessoryID: "CA:FE:00:11:22:33", CredentialSecret: "synthetic-pairing"}
	if _, err := ecobee.ParseSettings(input); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"", "localhost:12345", "0.0.0.0:12345", "224.0.0.1:12345", "127.0.0.1:0", "127.0.0.1:12345/path"} {
		in := input
		in.Address = address
		if _, err := ecobee.ParseSettings(in); err == nil {
			t.Fatalf("invalid endpoint admitted: %s", address)
		}
	}
	raw := []byte("ecobee:\n  instance: nha136-synthetic\n  address: '127.0.0.1:12345'\n  accessory_id: 'CA:FE:00:11:22:33'\n  credential_secret: synthetic-pairing\n")
	config, err := ParseOperationalWithEnv(raw, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	settings, ok := config.EcobeeSettings()
	if !ok || !settings.Valid() {
		t.Fatal("operational Ecobee lost")
	}
	unknown := append(append([]byte(nil), raw...), []byte("  pin: '111-22-333'\n")...)
	if _, err := ParseOperationalWithEnv(unknown, func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("PIN accepted in configuration")
	}
	if _, err := ParseBootstrapLaunchWithEnv(raw, func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("operational Ecobee accepted in launch sidecar")
	}
}

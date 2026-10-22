package config

import (
	"strings"
	"testing"
)

const nha136SelectionSource = "ecobee:\n  instance: nha136-ecobee\n  address: '127.0.0.1:12345'\n  accessory_id: 'CA:FE:00:11:22:33'\n  credential_secret: synthetic-pairing\n"

func TestNHA136DisabledEcobeeSelectionValidatesThenFilters(t *testing.T) {
	parse := func(raw string) (Config, error) {
		return ParseOperationalWithEnv([]byte(raw), func(string) (string, bool) {
			t.Fatal("operational selection resolved launch credentials")
			return "", false
		})
	}
	raw := nha136SelectionSource + "workday: {timezone: America/Toronto}\n"
	enabled, err := parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if settings, present := enabled.EcobeeSettings(); !present || !settings.Valid() {
		t.Fatal("enabled Ecobee was lost")
	}
	disabled, err := parse(raw + "disabled_integrations: [nha136-ecobee]\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, present := disabled.EcobeeSettings(); present {
		t.Fatal("disabled Ecobee reached operational composition")
	}
	if _, present := disabled.WorkdaySettings(); !present {
		t.Fatal("disabling Ecobee removed an unrelated module")
	}
	for name, source := range map[string]string{
		"invalid-disabled-address": strings.Replace(raw, "127.0.0.1:12345", "localhost:12345", 1) + "disabled_integrations: [nha136-ecobee]\n",
		"orphan":                   raw + "disabled_integrations: [foreign]\n",
		"duplicate-selection":      raw + "disabled_integrations: [nha136-ecobee, nha136-ecobee]\n",
		"duplicate-instance":       raw + "mqtt: {profile: '5', broker: 'mqtt://127.0.0.1:1883', instance: nha136-ecobee, mode: passive, username_secret: user-ref, password_secret: password-ref, subscriptions: ['homeassistant/#']}\ndisabled_integrations: [nha136-ecobee]\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parse(source); err == nil {
				t.Fatal("disabled selection hid invalid, orphan or duplicate input")
			}
		})
	}
}

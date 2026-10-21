package config

import (
	"strings"
	"testing"
)

const nha148MQTTSelection = "mqtt:\n  profile: '5'\n  broker: mqtt://127.0.0.1:1883\n  instance: nha148-mqtt\n  mode: passive\n  username_secret: user-ref\n  password_secret: password-ref\n  subscriptions: [homeassistant/#]\n"

func TestNHA148DisabledSelectionValidatesAllConfigurationsThenFilters(t *testing.T) {
	raw := nha148MQTTSelection + "disabled_integrations: [nha148-mqtt]\n"
	c, err := ParseOperationalWithEnv([]byte(raw), func(string) (string, bool) {
		t.Fatal("operational parser resolved launch credential")
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := c.MQTTSettings(); present {
		t.Fatal("disabled pilot remained in operational composition")
	}
	enabled, err := ParseOperationalWithEnv([]byte(nha148MQTTSelection), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if _, present := enabled.MQTTSettings(); !present {
		t.Fatal("omission did not preserve historic enabled behavior")
	}
	for _, bad := range []string{strings.Replace(raw, "profile: '5'", "profile: '3.1.1'", 1), nha148MQTTSelection + "disabled_integrations: [foreign]\n", nha148MQTTSelection + "disabled_integrations: [nha148-mqtt, nha148-mqtt]\n"} {
		if _, err := ParseOperationalWithEnv([]byte(bad), func(string) (string, bool) { return "", false }); err == nil {
			t.Fatal("disabled selection hid invalid, duplicate or orphan configuration")
		}
	}
}
func TestNHA148DisabledSelectionRetainsOtherConfiguredModules(t *testing.T) {
	c, err := ParseOperationalWithEnv([]byte(nha148MQTTSelection+"workday: {timezone: America/Toronto}\ndisabled_integrations: [nha148-mqtt]\n"), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if _, present := c.WorkdaySettings(); !present {
		t.Fatal("disabling mqtt removed an unrelated configured module")
	}
}

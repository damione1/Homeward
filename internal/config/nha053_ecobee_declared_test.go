package config

import "testing"

func TestNHA053DeclaredEcobeeRemainsInspectableWithoutReenablingRuntime(t *testing.T) {
	const source = "ecobee:\n  instance: nha053-private-ecobee\n  address: '127.0.0.1:12345'\n  accessory_id: 'CA:FE:00:11:22:33'\n  credential_secret: nha053-absent-pairing\ndisabled_integrations: [nha053-private-ecobee]\n"
	selected, err := ParseOperationalWithEnv([]byte(source), func(string) (string, bool) {
		t.Fatal("operational preview resolved environment")
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := selected.EcobeeSettings(); present {
		t.Fatal("disabled Ecobee reached runtime")
	}
	settings, present := selected.DeclaredOperationalSettings().EcobeeSettings()
	if !present || !settings.Valid() {
		t.Fatal("disabled declaration was hidden before reference admission")
	}
	if _, present := selected.EcobeeSettings(); present {
		t.Fatal("inspection changed runtime selection")
	}
}

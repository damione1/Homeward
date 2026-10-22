package config

import "testing"

func TestNHA053DeclaredOperationalViewPreservesFilteredRuntimeAndAdmission(t *testing.T) {
	raw := nha148MQTTSelection + "disabled_integrations: [nha148-mqtt]\n"
	selected, err := ParseOperationalWithEnv([]byte(raw), func(string) (string, bool) { t.Fatal("resolved environment"); return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if _, present := selected.MQTTSettings(); present {
		t.Fatal("declaration inspection reenabled runtime selection")
	}
	declaration := selected.DeclaredOperationalSettings()
	if _, present := declaration.MQTTSettings(); !present {
		t.Fatal("admitted disabled declaration was hidden from reference inspection")
	}
	// Inspecting values must never mutate the selected runtime configuration.
	if _, present := selected.MQTTSettings(); present {
		t.Fatal("inspection changed enabled integrations")
	}
	if _, err := ParseOperationalWithEnv([]byte(nha148MQTTSelection+"disabled_integrations: [unknown]\n"), func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("declaration view bypassed148 selection admission")
	}
}

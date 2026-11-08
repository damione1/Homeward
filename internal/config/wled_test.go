package config

import "testing"

func TestWLEDSelectionValidatesDisabledDeclarations(t *testing.T) {
	source := "wled:\n  instance: demo-wled\n  controllers:\n    - address: 192.168.50.20\n      mac: aabbccddeeff\n      routes:\n        - {alias: light.demo, label: Demo}\n"
	parse := func(raw string) (Config, error) {
		return ParseOperationalWithEnv([]byte(raw), func(string) (string, bool) { return "", false })
	}
	c, e := parse(source)
	if e != nil {
		t.Fatal(e)
	}
	s, ok := c.WLEDSettings()
	if !ok || len(s.Controllers()) != 1 {
		t.Fatal("missing selected native settings")
	}
	c, e = parse(source + "disabled_integrations: [demo-wled]\n")
	if e != nil {
		t.Fatal(e)
	}
	if _, ok = c.WLEDSettings(); ok {
		t.Fatal("disabled WLED is enabled")
	}
	if _, ok = c.DeclaredOperationalSettings().WLEDSettings(); !ok {
		t.Fatal("disabled declaration lost")
	}
	for _, raw := range []string{source + "disabled_integrations: [unknown]\n", source + "  arbitrary: unsafe\n", "wled: {instance: demo-wled, controllers: []}\ndisabled_integrations: [demo-wled]\n"} {
		if _, e = parse(raw); e == nil {
			t.Fatal("invalid declaration admitted")
		}
	}
}

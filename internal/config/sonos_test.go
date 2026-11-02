package config

import "testing"

func TestSonosSelectionValidatesDisabledDeclarations(t *testing.T) {
	source := "sonos:\n  instance: demo-sonos\n  players:\n    - {address: 192.168.50.10, uid: RINCON_00112233445501400, alias: media_player.demo, label: Demo}\n"
	parse := func(raw string) (Config, error) {
		return ParseOperationalWithEnv([]byte(raw), func(string) (string, bool) { return "", false })
	}
	c, e := parse(source)
	if e != nil {
		t.Fatal(e)
	}
	s, ok := c.SonosSettings()
	if !ok || len(s.Players()) != 1 {
		t.Fatal("missing selected native settings")
	}
	c, e = parse(source + "disabled_integrations: [demo-sonos]\n")
	if e != nil {
		t.Fatal(e)
	}
	if _, ok = c.SonosSettings(); ok {
		t.Fatal("disabled Sonos is enabled")
	}
	if _, ok = c.DeclaredOperationalSettings().SonosSettings(); !ok {
		t.Fatal("disabled declaration lost")
	}
	for _, raw := range []string{source + "disabled_integrations: [unknown]\n", source + "  arbitrary: unsafe\n", "sonos: {instance: demo-sonos, players: []}\ndisabled_integrations: [demo-sonos]\n"} {
		if _, e = parse(raw); e == nil {
			t.Fatal("invalid declaration admitted")
		}
	}
}

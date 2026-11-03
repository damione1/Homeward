package config

import "testing"

func TestAndroidTVSelectionValidatesDisabledDeclarations(t *testing.T) {
	source := "android_tv:\n  instance: demo-android_tv\n  players:\n    - {address: 192.168.50.10, mac: '02:22:33:44:55:66', credential_secret: pairing, alias: media_player.demo, label: Demo}\n"
	parse := func(raw string) (Config, error) {
		return ParseOperationalWithEnv([]byte(raw), func(string) (string, bool) { return "", false })
	}
	c, e := parse(source)
	if e != nil {
		t.Fatal(e)
	}
	s, ok := c.AndroidTVSettings()
	if !ok || len(s.Players()) != 1 {
		t.Fatal("missing selected native settings")
	}
	c, e = parse(source + "disabled_integrations: [demo-android_tv]\n")
	if e != nil {
		t.Fatal(e)
	}
	if _, ok = c.AndroidTVSettings(); ok {
		t.Fatal("disabled AndroidTV is enabled")
	}
	if _, ok = c.DeclaredOperationalSettings().AndroidTVSettings(); !ok {
		t.Fatal("disabled declaration lost")
	}
	for _, raw := range []string{source + "disabled_integrations: [unknown]\n", source + "  arbitrary: unsafe\n", "android_tv: {instance: demo-android_tv, players: []}\ndisabled_integrations: [demo-android_tv]\n"} {
		if _, e = parse(raw); e == nil {
			t.Fatal("invalid declaration admitted")
		}
	}
}

package config

import "testing"

func TestNHA052MCPConfigurationIsolation(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		settings, err := parsePublicSettings(&publicInput{MCP: &mcpInput{Enabled: enabled}})
		if err != nil || settings.MCPEnabled() != enabled || settings.Transport() != PublicTransportH2C || settings.MaxConcurrentRequests() != DefaultPublicSettings().MaxConcurrentRequests() {
			t.Fatalf("MCP configuration affects unrelated transport settings: enabled=%v err=%v", enabled, err)
		}
	}
	settings, err := parsePublicSettings(nil)
	if err != nil || !settings.MCPEnabled() {
		t.Fatal("MCP default unavailable", err)
	}
	if (PublicSettings{}).MCPEnabled() {
		t.Fatal("zero settings enabled MCP")
	}
}

package mqtt

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestNHA056MQTTDiagnosticRedactsAtEmission(t *testing.T) {
	var out bytes.Buffer
	m := &DiscoveryMapper{logger: slog.New(slog.NewJSONHandler(&out, nil))}
	topic, err := ParseTopic("homeassistant/sensor/NHA056_TOKEN_CANARY/config")
	if err != nil {
		t.Fatal(err)
	}
	m.diagnose(DiagnosticInvalidDiscovery, topic, "NHA056_LOCATION_CANARY")
	got := out.String()
	for _, secret := range []string{"NHA056_TOKEN_CANARY", "NHA056_LOCATION_CANARY"} {
		if strings.Contains(got, secret) {
			t.Errorf("diagnostic leaked canary %q", secret)
		}
	}
	if !strings.Contains(got, DiagnosticInvalidDiscovery.String()) {
		t.Fatal("structured diagnostic code was lost")
	}
}

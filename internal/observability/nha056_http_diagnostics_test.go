package observability

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/damione1/Homeward/contracts"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestNHA056HTTPDiagnosticsDiscardUntrustedBytes(t *testing.T) {
	for _, component := range []obs.LogComponent{obs.LogComponent_LOG_COMPONENT_PUBLIC_TRANSPORT, obs.LogComponent_LOG_COMPONENT_OPERATOR_TRANSPORT, obs.LogComponent_LOG_COMPONENT_WORKER_TRANSPORT} {
		epoch, cells, codec := nha056Fixture(t)
		journal, err := NewJournal(epoch, cells)
		if err != nil {
			t.Fatal(err)
		}
		writer, err := NewHTTPDiagnosticWriter(journal, component)
		if err != nil {
			t.Fatal(err)
		}
		const canary = "NHA056_HTTP_PASSWORD_TOKEN_COORDS_SIGNED_URL_CALENDAR_TOPIC_PANIC_CANARY"
		payload := bytes.Repeat([]byte(canary), 16384)
		n, err := writer.Write(payload)
		if err != nil || n != len(payload) {
			t.Fatal("diagnostic writer changed the transport result", n, err)
		}
		clear(payload)
		logger := log.New(writer, "", 0)
		logger.Print(canary)
		snapshot, err := journal.Snapshot()
		if err != nil || snapshot.Through() != 2 {
			t.Fatal("safe diagnostic lost", err)
		}
		cursor, err := codec.BeginLogs(nha056Binding(t, 1), snapshot.Through())
		if err != nil {
			t.Fatal(err)
		}
		size, _ := NewPageSize(100)
		request, err := NewLogPageRequest(size, cursor)
		if err != nil {
			t.Fatal(err)
		}
		page, err := journal.Read(request)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := page.Wire("")
		if err != nil || contracts.AdmitObservabilityResponse(wire) != nil {
			t.Fatal("diagnostic page not admitted", err)
		}
		encoded, err := protojson.Marshal(wire)
		if err != nil || strings.Contains(string(encoded), canary) {
			t.Fatal("HTTP diagnostic retained untrusted bytes", err)
		}
		for _, row := range wire.GetPage().Rows {
			event := row.Event
			if event.Code != obs.LogCode_LOG_CODE_TRANSPORT_INTERNAL_DIAGNOSTIC || event.Level != obs.LogLevel_LOG_LEVEL_WARNING || event.Component != component || event.Author.GetProcess() == nil || event.OccurredAt == nil {
				t.Fatal("safe positive diagnostic fields lost")
			}
		}
		for range 1030 {
			if _, err := writer.Write([]byte(canary)); err != nil {
				t.Fatal(err)
			}
		}
		snapshot, err = journal.Snapshot()
		if err != nil || snapshot.RetainedRows() > 1024 || snapshot.ChargedBytes() > 1<<20 || snapshot.EvictedTotal().GetExact() == 0 {
			t.Fatal("HTTP diagnostic ring unbounded", err)
		}
	}
}

func TestNHA056HTTPDiagnosticsRejectInvalidPortsAndComponents(t *testing.T) {
	epoch, cells, _ := nha056Fixture(t)
	journal, err := NewJournal(epoch, cells)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalidComponent := range []obs.LogComponent{0, obs.LogComponent(999), obs.LogComponent_LOG_COMPONENT_PROCESS, obs.LogComponent_LOG_COMPONENT_MQTT, obs.LogComponent_LOG_COMPONENT_SUPERVISOR} {
		if writer, err := NewHTTPDiagnosticWriter(journal, invalidComponent); err == nil || writer != nil {
			t.Fatal("nontransport component admitted")
		}
	}
	for _, invalidJournal := range []*Journal{nil, {}} {
		if writer, err := NewHTTPDiagnosticWriter(invalidJournal, obs.LogComponent_LOG_COMPONENT_PUBLIC_TRANSPORT); err == nil || writer != nil {
			t.Fatal("invalid Journal port admitted")
		}
	}
	var writer *httpDiagnosticWriter
	if n, err := writer.Write([]byte("NHA056_INVALID_WRITER_CANARY")); err == nil || n != 0 {
		t.Fatal("nil writer operationally admitted")
	}
}

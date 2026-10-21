package mqtt

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/damione1/Homeward/contracts"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/observability"
	"google.golang.org/protobuf/encoding/protojson"
)

type nha056Registry struct{ calls int }

func (r *nha056Registry) MatchMQTTSource(context.Context, domain.IntegrationInstanceID, []string, string) (integrations.SourceKey, error) {
	r.calls++
	return integrations.SourceKey{}, ErrInvalidSettings
}
func (r *nha056Registry) MQTTTopicSources(context.Context, domain.IntegrationInstanceID, string) ([]integrations.SourceKey, error) {
	r.calls++
	return nil, ErrInvalidSettings
}
func (r *nha056Registry) RememberMQTTTopics(context.Context, domain.IntegrationInstanceID, string, []integrations.SourceKey) error {
	r.calls++
	return ErrInvalidSettings
}
func (r *nha056Registry) Tombstone(context.Context, domain.EntityID) error {
	r.calls++
	return ErrInvalidSettings
}

func nha056Journal(t *testing.T) (*observability.Journal, *observability.CursorCodec) {
	t.Helper()
	epoch, err := observability.NewProcessObservationEpoch()
	if err != nil {
		t.Fatal(err)
	}
	cells, err := observability.NewCells(epoch)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := observability.NewJournal(epoch, cells)
	if err != nil {
		t.Fatal(err)
	}
	codec, err := observability.NewCursorCodec(epoch)
	if err != nil {
		t.Fatal(err)
	}
	return journal, codec
}
func nha056Logs(t *testing.T, journal *observability.Journal, codec *observability.CursorCodec) *obs.QueryRuntimeLogsResponse {
	t.Helper()
	snapshot, err := journal.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var digest [32]byte
	digest[0] = 1
	binding, err := observability.NewReadBinding(1, digest)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := codec.BeginLogs(binding, snapshot.Through())
	if err != nil {
		t.Fatal(err)
	}
	size, err := observability.NewPageSize(100)
	if err != nil {
		t.Fatal(err)
	}
	request, err := observability.NewLogPageRequest(size, cursor)
	if err != nil {
		t.Fatal(err)
	}
	page, err := journal.Read(request)
	if err != nil {
		t.Fatal(err)
	}
	response, err := page.Wire("")
	if err != nil || contracts.AdmitObservabilityResponse(response) != nil {
		t.Fatal("MQTT log page not admitted", err)
	}
	return response
}
func nha056Settings(t *testing.T) Settings {
	t.Helper()
	settings, err := ParseSettings(Input{Profile: "5", Broker: "mqtt://127.0.0.1:1883", Instance: "nha056-synthetic", Mode: "passive", UsernameSecret: "mqtt-username", PasswordSecret: "mqtt-password", Subscriptions: []string{"homeassistant/#", "synthetic/wait-only"}})
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

func TestNHA056MQTTJournalFactoriesRetainRealMappers(t *testing.T) {
	journal, codec := nha056Journal(t)
	settings := nha056Settings(t)
	repository := &nha056Registry{}
	filter, err := ParseTopicFilter("synthetic/wait-only")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := NewDiscoveryPilotFactoryWithJournal(settings, repository, journal)
	if err != nil {
		t.Fatal(err)
	}
	edges, err := NewDiscoveryPilotFactoryWithTriggerEdgesAndJournal(settings, repository, []TopicFilter{filter}, journal)
	if err != nil {
		t.Fatal(err)
	}
	first, ok := plain().(*discoveryPilot)
	if !ok || first == nil || first.Pilot == nil || first.mapper == nil {
		t.Fatal("plain factory lost its real pilot")
	}
	second, ok := plain().(*discoveryPilot)
	if !ok || second == nil || second.mapper == first.mapper {
		t.Fatal("factory shared a session mapper")
	}
	edge, ok := edges().(*discoveryPilot)
	if !ok || edge == nil || edge.Pilot == nil || edge.mapper == nil {
		t.Fatal("trigger factory lost its real pilot")
	}
	if first.Pilot.handler != first.mapper {
		t.Fatal("plain pilot bypassed its mapper")
	}
	handler, ok := edge.Pilot.handler.(*triggerEdgeHandler)
	if !ok || handler.mapper != edge.mapper {
		t.Fatal("trigger pilot bypassed the existing edge handler")
	}
	const topicCanary = "NHA056_MQTT_JOURNAL_TOKEN_CANARY"
	const componentCanary = "NHA056_MQTT_JOURNAL_CALENDAR_COMPONENT_CANARY"
	topic, err := ParseTopic("homeassistant/sensor/" + topicCanary + "/config")
	if err != nil {
		t.Fatal(err)
	}
	first.mapper.diagnose(DiagnosticInvalidDiscovery, topic, componentCanary)
	edge.mapper.diagnose(DiagnosticInvalidAction, topic, componentCanary)
	response := nha056Logs(t, journal, codec)
	if len(response.GetPage().Rows) != 2 {
		t.Fatal("true diagnose paths did not publish two safe events")
	}
	encoded, err := protojson.Marshal(response)
	if err != nil || bytes.Contains(encoded, []byte(topicCanary)) || bytes.Contains(encoded, []byte(componentCanary)) {
		t.Fatal("MQTT journal retained raw provenance", err)
	}
	for i, want := range []obs.LogCode{obs.LogCode_LOG_CODE_MQTT_INVALID_DISCOVERY, obs.LogCode_LOG_CODE_MQTT_INVALID_ACTION} {
		event := response.GetPage().Rows[i].Event
		if event.Code != want || event.Level != obs.LogLevel_LOG_LEVEL_WARNING || event.Component != obs.LogComponent_LOG_COMPONENT_MQTT || event.Author.GetProcess() == nil || event.OccurredAt == nil {
			t.Fatal("MQTT positive closed diagnostic fields lost")
		}
	}
	if repository.calls != 0 {
		t.Fatal("constructor or diagnostic performed repository I/O")
	}
	// Construction deliberately never calls Start: no broker connection is possible.
	invalidFilter, _ := ParseTopicFilter("unsubscribed/edge")
	if factory, err := NewDiscoveryPilotFactoryWithTriggerEdgesAndJournal(settings, repository, []TopicFilter{invalidFilter}, journal); err == nil || factory != nil {
		t.Fatal("wrapper bypassed existing subscription admission")
	}
	var typedNil *nha056Registry
	for _, bad := range []*observability.Journal{nil, {}} {
		if factory, err := NewDiscoveryPilotFactoryWithJournal(settings, repository, bad); err == nil || factory != nil {
			t.Fatal("invalid journal admitted")
		}
	}
	if factory, err := NewDiscoveryPilotFactoryWithJournal(settings, typedNil, journal); err == nil || factory != nil {
		t.Fatal("typed nil registry admitted")
	}
}

func TestNHA056MQTTDiagnosticConversionIsClosedAndEmissionSafe(t *testing.T) {
	journal, codec := nha056Journal(t)
	var output bytes.Buffer
	mapper := &DiscoveryMapper{logger: slog.New(slog.NewJSONHandler(&output, nil)), journal: journal}
	topic, err := ParseTopic("homeassistant/sensor/NHA056_RAW_TOPIC_CANARY/config")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		diagnostic DiscoveryDiagnostic
		code       obs.LogCode
	}{
		{DiagnosticUnknownDiscovery, obs.LogCode_LOG_CODE_MQTT_UNKNOWN_DISCOVERY},
		{DiagnosticInvalidDiscovery, obs.LogCode_LOG_CODE_MQTT_INVALID_DISCOVERY},
		{DiagnosticUnconfiguredSource, obs.LogCode_LOG_CODE_MQTT_UNCONFIGURED_SOURCE},
		{DiagnosticUnsupportedEffect, obs.LogCode_LOG_CODE_MQTT_UNSUPPORTED_EFFECT},
		{DiagnosticUnsupportedColorMode, obs.LogCode_LOG_CODE_MQTT_UNSUPPORTED_COLOR_MODE},
		{DiagnosticInvalidState, obs.LogCode_LOG_CODE_MQTT_INVALID_STATE},
		{DiagnosticAvailability, obs.LogCode_LOG_CODE_MQTT_INVALID_AVAILABILITY},
		{DiagnosticRetirement, obs.LogCode_LOG_CODE_MQTT_RETIREMENT_FAILED},
		{DiagnosticSnapshotOverflow, obs.LogCode_LOG_CODE_MQTT_SNAPSHOT_OVERFLOW},
		{DiagnosticUnconfiguredActionAnchor, obs.LogCode_LOG_CODE_MQTT_UNCONFIGURED_ACTION_ANCHOR},
		{DiagnosticAmbiguousActionDevice, obs.LogCode_LOG_CODE_MQTT_AMBIGUOUS_ACTION_DEVICE},
		{DiagnosticActionOverflow, obs.LogCode_LOG_CODE_MQTT_ACTION_OVERFLOW},
		{DiagnosticInvalidAction, obs.LogCode_LOG_CODE_MQTT_INVALID_ACTION},
	}
	for _, test := range cases {
		mapper.diagnose(test.diagnostic, topic, "NHA056_RAW_COMPONENT_CANARY")
	}
	mapper.diagnose(DiagnosticUnspecified, topic, "NHA056_RAW_COMPONENT_CANARY")
	mapper.diagnose(DiscoveryDiagnostic(255), topic, "NHA056_RAW_COMPONENT_CANARY")
	response := nha056Logs(t, journal, codec)
	if len(response.GetPage().Rows) != len(cases) {
		t.Fatal("closed conversion admitted an unknown or lost a real diagnostic")
	}
	for i, test := range cases {
		if response.GetPage().Rows[i].Event.Code != test.code {
			t.Fatal("discovery diagnostic mapped to the wrong closed code")
		}
	}
	if strings.Contains(output.String(), "NHA056_RAW_") || !strings.Contains(output.String(), DiagnosticInvalidDiscovery.String()) {
		t.Fatal("actual slog emission leaked provenance or lost the positive diagnostic")
	}
	if len(mapper.DiagnosticRecords()) != len(cases) {
		t.Fatal("closed admission changed bounded operational diagnostics")
	}
}

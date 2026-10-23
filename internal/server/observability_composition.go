package server

import (
	"io"
	"log"
	"time"

	"github.com/damione1/Homeward/contracts"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/observability"
	"github.com/damione1/Homeward/internal/recorder"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type observationRuntime struct {
	readOnly   *readOnlyObservationBinding
	epoch      observability.ProcessObservationEpoch
	cells      *observability.Cells
	health     *observability.HealthCell
	journal    *observability.Journal
	codec      *observability.CursorCodec
	captures   *observability.TraceCaptures
	sequence   *sequencer.Sequencer
	host       *integrations.Manager
	recording  *recorder.Recorder
	supervisor *integrations.Supervisor
	runs       *automationruns.Controller
	authority  *authorization.Authority
	store      *storage.Store
	public     *compiledPublic
	revision   observability.ActiveRevision
}

func newObservationRuntime() (*observationRuntime, error) {
	epoch, err := observability.NewProcessObservationEpoch()
	if err != nil {
		return nil, err
	}
	cells, err := observability.NewCells(epoch)
	if err != nil {
		return nil, err
	}
	health, err := observability.NewHealthCell(epoch)
	if err != nil {
		return nil, err
	}
	journal, err := observability.NewJournal(epoch, cells)
	if err != nil {
		return nil, err
	}
	codec, err := observability.NewCursorCodec(epoch)
	if err != nil {
		return nil, err
	}
	captures, err := observability.NewTraceCaptures(epoch, codec)
	if err != nil {
		return nil, err
	}
	return &observationRuntime{epoch: epoch, cells: cells, health: health, journal: journal, codec: codec, captures: captures}, nil
}
func (o *observationRuntime) emit(code obs.LogCode, level obs.LogLevel) {
	event, err := observability.NewEvent(code, level, obs.LogComponent_LOG_COMPONENT_PROCESS, time.Now().UTC(), observability.NewProcessAuthor())
	if err == nil {
		_ = o.journal.Emit(event)
	}
}
func observationErrorLog(journal *observability.Journal, component obs.LogComponent) *log.Logger {
	var writer io.Writer = io.Discard
	if journal != nil {
		if safe, err := observability.NewHTTPDiagnosticWriter(journal, component); err == nil {
			writer = safe
		}
	}
	return log.New(writer, "", 0)
}
func (o *observationRuntime) healthSnapshot() (*obs.GetHealthResponse, error) {
	facts, err := o.health.Snapshot()
	if err != nil {
		return nil, err
	}
	epoch, err := o.epoch.Wire()
	if err != nil {
		return nil, err
	}
	server, err := facts.Server()
	if err != nil {
		return nil, err
	}
	durable, err := facts.Durable()
	if err != nil {
		return nil, err
	}
	recording := &obs.RecorderHealth{Result: &obs.RecorderHealth_NotInstalled{NotInstalled: &emptypb.Empty{}}}
	if o.recording != nil {
		recording, err = o.recording.ObservationHealth()
		if err != nil {
			return nil, err
		}
	}
	pilots, err := o.supervisor.ObservationPhases()
	if err != nil {
		return nil, err
	}
	response := &obs.GetHealthResponse{Health: &obs.HealthSnapshot{ProcessEpoch: epoch, Liveness: obs.Liveness_LIVENESS_LIVE, Server: server, Durable: durable, Recorder: recording, Pilots: pilots}}
	return response, contracts.AdmitObservabilityResponse(response)
}
func (o *observationRuntime) metricsSnapshot() (*obs.GetMetricsResponse, error) {
	epoch, err := o.epoch.Wire()
	if err != nil {
		return nil, err
	}
	metrics := &obs.MetricsSnapshot{ProcessEpoch: epoch}
	cut := func(owner obs.MetricOwner) {
		metrics.Cuts = append(metrics.Cuts, &obs.ObservationCut{Owner: owner, CapturedAt: timestamppb.Now()})
	}
	metrics.Queues, metrics.QueueLags, err = o.sequence.ObservationMetrics()
	if err != nil {
		return nil, err
	}
	cut(obs.MetricOwner_METRIC_OWNER_SEQUENCER)
	metrics.Runs = &obs.RunMetrics{Result: &obs.RunMetrics_NotInstalled{NotInstalled: &emptypb.Empty{}}}
	if o.runs != nil {
		queues, runs, err := o.runs.ObservationMetrics()
		if err != nil {
			return nil, err
		}
		metrics.Queues = append(metrics.Queues, queues...)
		metrics.Runs = runs
	} else {
		for _, class := range []obs.QueueClass{obs.QueueClass_QUEUE_CLASS_AUTOMATION_PENDING, obs.QueueClass_QUEUE_CLASS_SCRIPT_PENDING} {
			metrics.Queues = append(metrics.Queues, &obs.QueueAggregate{QueueClass: class, Result: &obs.QueueAggregate_NotInstalled{NotInstalled: &emptypb.Empty{}}})
		}
	}
	cut(obs.MetricOwner_METRIC_OWNER_AUTOMATION_RUNS)
	for _, class := range []obs.QueueClass{obs.QueueClass_QUEUE_CLASS_MQTT_INBOUND, obs.QueueClass_QUEUE_CLASS_WORKER_COMMANDS, obs.QueueClass_QUEUE_CLASS_CLOUD_REQUESTS} {
		queue, err := o.cells.RuntimeQueue(class)
		if err != nil {
			return nil, err
		}
		metrics.Queues = append(metrics.Queues, queue)
	}
	cut(obs.MetricOwner_METRIC_OWNER_MQTT)
	cut(obs.MetricOwner_METRIC_OWNER_WORKER)
	cut(obs.MetricOwner_METRIC_OWNER_CLOUD_POLICY)
	metrics.Recorder = &obs.RecorderMetrics{Result: &obs.RecorderMetrics_NotInstalled{NotInstalled: &emptypb.Empty{}}}
	if o.recording != nil {
		metrics.Recorder, err = o.recording.ObservationMetrics()
		if err != nil {
			return nil, err
		}
	}
	cut(obs.MetricOwner_METRIC_OWNER_RECORDER)
	metrics.Actions, err = o.host.ObservationActions()
	if err != nil {
		return nil, err
	}
	cut(obs.MetricOwner_METRIC_OWNER_ACTIONS)
	journal, err := o.journal.Snapshot()
	if err != nil {
		return nil, err
	}
	metrics.Journal, err = journal.Metrics()
	if err != nil {
		return nil, err
	}
	cut(obs.MetricOwner_METRIC_OWNER_JOURNAL)
	response := &obs.GetMetricsResponse{Metrics: metrics}
	return response, contracts.AdmitObservabilityResponse(response)
}

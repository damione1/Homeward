package sequencer

import (
	wire "github.com/damione1/Homeward/gen/home/integrations/v1"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/observability"
	"google.golang.org/protobuf/types/known/emptypb"
)

func subscriptionObservationClass(class SubscriptionClass) (obs.QueueClass, error) {
	switch class {
	case SubscriptionUI:
		return obs.QueueClass_QUEUE_CLASS_UI, nil
	case SubscriptionEdge:
		return obs.QueueClass_QUEUE_CLASS_EDGE, nil
	case SubscriptionRecorder:
		return obs.QueueClass_QUEUE_CLASS_RECORDER, nil
	case SubscriptionView:
		return obs.QueueClass_QUEUE_CLASS_VIEW, nil
	default:
		return 0, ErrInvalidInput
	}
}

func (s *Sequencer) IntegrationHealth(instance domain.IntegrationInstanceID) (*obs.IntegrationReportPresence, error) {
	if s == nil || s.journal == nil {
		return nil, ErrInvalidInput
	}
	if _, err := instance.Value(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	report, present := s.health[instance]
	if !present {
		return &obs.IntegrationReportPresence{Result: &obs.IntegrationReportPresence_NoReport{NoReport: &emptypb.Empty{}}}, nil
	}
	status, _, err := report.Components()
	if err != nil {
		return nil, err
	}
	var converted wire.HealthStatus
	switch status {
	case integrations.HealthReady:
		converted = wire.HealthStatus_HEALTH_STATUS_READY
	case integrations.HealthDegraded:
		converted = wire.HealthStatus_HEALTH_STATUS_DEGRADED
	case integrations.HealthUnavailable:
		converted = wire.HealthStatus_HEALTH_STATUS_UNAVAILABLE
	default:
		return nil, ErrInvalidInput
	}
	return &obs.IntegrationReportPresence{Result: &obs.IntegrationReportPresence_Present{Present: &obs.IntegrationReport{Status: converted, AdmissionTime: &obs.IntegrationReport_NotRecorded{NotRecorded: &emptypb.Empty{}}}}}, nil
}

func (s *Sequencer) ObservationMetrics() ([]*obs.QueueAggregate, []*obs.QueueLag, error) {
	if s == nil || s.journal == nil || s.observation == nil {
		return nil, nil, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	queues := make([]*obs.QueueAggregate, 0, 4)
	lags := make([]*obs.QueueLag, 0, 4)
	for _, class := range []SubscriptionClass{SubscriptionUI, SubscriptionEdge, SubscriptionRecorder, SubscriptionView} {
		converted, _ := subscriptionObservationClass(class)
		var active, queued, capacity, degraded, bytes, byteCapacity, lag uint64
		invalidated := false
		epochMismatch := false
		for sub := range s.subs {
			if sub.class != class || sub.closed {
				continue
			}
			active++
			capacity += uint64(sub.capacity)
			count := len(sub.queue)
			if class == SubscriptionRecorder {
				count = sub.recorder.len()
			}
			queued += uint64(count)
			if sub.degraded {
				degraded++
			}
			if !sub.replayValid {
				invalidated = true
			}
			if class == SubscriptionRecorder || class == SubscriptionView {
				bytes += uint64(sub.queueBytes)
				if class == SubscriptionRecorder {
					byteCapacity += uint64(sub.maxBytes)
				} else {
					byteCapacity += MaxViewQueueBytes
				}
			}
			if count > 0 && sub.replayValid {
				head := Cursor{}
				if class == SubscriptionRecorder {
					head = sub.recorder.body[sub.recorder.head].cursor
				} else {
					head = sub.queue[0].cursor
				}
				if head.epoch != s.epoch {
					epochMismatch = true
				} else {
					if head.seq == 0 || head.seq > s.seq {
						return nil, nil, ErrInvalidInput
					}
					positions := s.seq - head.seq + 1
					if positions > lag {
						lag = positions
					}
				}
			}
		}
		q := &obs.QueueAggregate{QueueClass: converted}
		l := &obs.QueueLag{QueueClass: converted}
		if active == 0 {
			q.Result = &obs.QueueAggregate_NotInstalled{NotInstalled: &emptypb.Empty{}}
			l.Result = &obs.QueueLag_NotInstalled{NotInstalled: &emptypb.Empty{}}
		} else {
			var queuedBytes, capacityBytes *uint64
			if class == SubscriptionRecorder || class == SubscriptionView {
				queuedBytes, capacityBytes = &bytes, &byteCapacity
			}
			snapshot, err := observability.NewQueueSnapshot(active, queued, capacity, degraded, queuedBytes, capacityBytes)
			if err != nil {
				return nil, nil, err
			}
			value, err := snapshot.Wire()
			if err != nil {
				return nil, nil, err
			}
			counters, err := s.observation.QueueCounters(converted)
			if err != nil {
				return nil, nil, err
			}
			value.LostDeliveriesTotal = counters.Lost()
			if class == SubscriptionUI {
				value.CoalescedTotal = counters.Coalesced()
			}
			q.Result = &obs.QueueAggregate_Installed{Installed: value}
			measured := observability.SameEpochLag(lag)
			if epochMismatch {
				measured = observability.EpochMismatchLag()
			}
			if invalidated {
				measured = observability.InvalidatedLag()
			}
			l.Result = &obs.QueueLag_Installed{Installed: measured}
		}
		queues = append(queues, q)
		lags = append(lags, l)
	}
	return queues, lags, nil
}

func (s *Sequencer) ObservationCells() (*observability.Cells, error) {
	if s == nil || s.journal == nil || s.observation == nil {
		return nil, ErrInvalidInput
	}
	return s.observation, nil
}

func (s *Sequencer) ObservationProjectionReady() (bool, error) {
	if s == nil || s.journal == nil {
		return false, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.registryState == registryReady, nil
}

func (s *Sequencer) AttachObservationHealth(health *observability.HealthCell) error {
	if s == nil || health == nil {
		return ErrInvalidInput
	}
	facts, err := health.Snapshot()
	if err != nil {
		return err
	}
	epoch, err := facts.Epoch()
	if err != nil {
		return err
	}
	current, err := s.observation.Epoch()
	if err != nil || epoch != current {
		return ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seq != 0 || len(s.subs) != 0 {
		return ErrInvalidInput
	}
	s.observationHealth = health
	return nil
}

package scenes

import (
	"github.com/damione1/Homeward/internal/automationruns"
	"testing"
)

type nha186NilHistory struct{}

func (*nha186NilHistory) ObserveSceneInvocation(SceneInvocationObservation) {}
func (*nha186NilHistory) ObserveSceneMember(SceneMemberObservation)         {}

func TestNHA186SceneHistoryRejectsZeroAndTypedNilPorts(t *testing.T) {
	var sink *nha186NilHistory
	var absent *Service
	if err := absent.AttachHistoryObservationSink(sink, automationruns.FixtureProvenance()); err == nil {
		t.Fatal("nil service/sink admitted")
	}
	if err := (&Service{}).AttachHistoryObservationSink(sink, automationruns.FixtureProvenance()); err == nil {
		t.Fatal("typed nil sink admitted")
	}
	if err := (&Service{}).AttachHistoryObservationSink(&nha186NilHistory{}, automationruns.FixtureProvenance()); err == nil {
		t.Fatal("zero service admitted")
	}
}

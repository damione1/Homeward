package scriptcommands

import (
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"testing"
)

type nha186NilHistory struct{}

func (*nha186NilHistory) ObserveCommand(CommandObservation) {}

func TestNHA186CommandHistoryDoesNotInventConfirmation(t *testing.T) {
	for _, test := range []struct {
		status actions.ActionStatus
		want   CommandOutcome
	}{
		{actions.ActionStatus_ACTION_STATUS_ACCEPTED, CommandAccepted},
		{actions.ActionStatus_ACTION_STATUS_DISPATCHED, CommandDispatched},
		{actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED, CommandAcknowledged},
		{actions.ActionStatus_ACTION_STATUS_CONFIRMED, CommandConfirmed},
		{actions.ActionStatus_ACTION_STATUS_INDETERMINATE, CommandIndeterminate},
		{actions.ActionStatus_ACTION_STATUS_FAILED, CommandRefused},
		{actions.ActionStatus_ACTION_STATUS_CANCELLED, CommandRefused},
		{actions.ActionStatus_ACTION_STATUS_UNSPECIFIED, CommandOutcomeInvalid},
		{actions.ActionStatus(255), CommandOutcomeInvalid},
	} {
		if got := observedCommandOutcome(&actions.CommandResponse{Status: test.status}, nil); got != test.want {
			t.Fatalf("status%d became%d want%d", test.status, got, test.want)
		}
	}
	var typedNil *nha186NilHistory
	if !missing(typedNil) {
		t.Fatal("typed nil history accepted")
	}
	if _, err := New(Input{History: typedNil}); err == nil {
		t.Fatal("invalid command input admitted")
	}
	if got := observedCommandOutcome(nil, nil); got != CommandNotAttempted {
		t.Fatalf("no response fabricated%d", got)
	}
}

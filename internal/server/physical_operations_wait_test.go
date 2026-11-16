package server

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestWaitOperationPacesWithoutALiveProgressionAndStopsOnCancellation(t *testing.T) {
	handler := &coreRPCHandler{}
	name, err := apitypes.ParseOperationName("homes/synthetic/operations/cmd-0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if !handler.awaitPhysicalChange(context.Background(), name, 20*time.Millisecond) {
		t.Fatal("slice elapsed but the wait reported cancellation")
	}
	if time.Since(started) < 20*time.Millisecond {
		t.Fatal("slice returned early without a live change")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if handler.awaitPhysicalChange(cancelled, name, time.Second) {
		t.Fatal("cancelled context reported a change")
	}
	// Without owners the wait fails closed exactly like the read; it never
	// invents an Operation and never sleeps for a denied name.
	denied := &longrunningpb.WaitOperationRequest{Name: name.String(), Timeout: durationpb.New(time.Second)}
	started = time.Now()
	if _, err := handler.WaitOperation(context.Background(), denied); err == nil {
		t.Fatal("denied wait returned an operation")
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatal("denied wait slept")
	}
}

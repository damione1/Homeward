package ecobee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/storage"
)

func TestNHA136CredentialAdmitsEncryptedIdentityAndRejectsAbsence(t *testing.T) {
	adapter := newClimateAdapter(t, false)
	defer adapter.Close()
	admitted, err := decodeCredential(adapter.value, adapter.settings)
	if err != nil {
		t.Fatal(err)
	}
	if admitted.controller.DeviceID != adapter.fixture.Controller.DeviceID {
		t.Fatal("controller identity changed")
	}
	raw, err := adapter.value.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprintf("%#v", admitted), adapter.fixture.Controller.DeviceID) {
		t.Fatal("credential rendered")
	}
	var input PairingInput
	if err := json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	input.ControllerPrivateKey = make([]byte, 64)
	bad, _ := json.Marshal(input)
	value, _ := storage.NewSecretValue(bad)
	if _, err := decodeCredential(value, adapter.settings); err == nil {
		t.Fatal("invalid private/public pair admitted")
	}
	if _, err := decodeCredential(storage.SecretValue{}, adapter.settings); err == nil {
		t.Fatal("absent credential admitted")
	}
	input.ControllerPrivateKey = adapter.fixture.Controller.PrivateKey
	input.AccessoryID = "foreign"
	bad, _ = json.Marshal(input)
	value, _ = storage.NewSecretValue(bad)
	if _, err := decodeCredential(value, adapter.settings); err == nil {
		t.Fatal("foreign accessory admitted")
	}
}

func TestNHA136CredentialDuplicatesAndGoZeroAreRejected(t *testing.T) {
	t.Run("reply-and-done-ready", func(t *testing.T) {
		done := make(chan struct{})
		close(done)
		for attempt := 0; attempt < 256; attempt++ {
			reply := make(chan error, 1)
			reply <- ErrCommandUncertain
			if err := awaitCommandReply(context.Background(), reply, done); !errors.Is(err, ErrCommandUncertain) {
				t.Fatal("published command error lost when done ready", err)
			}
		}
	})
	t.Run("done-without-reply", func(t *testing.T) {
		done := make(chan struct{})
		close(done)
		if err := awaitCommandReply(context.Background(), make(chan error), done); !errors.Is(err, ErrUnavailable) {
			t.Fatal("stopped command without result did not fail", err)
		}
	})
	t.Run("reply-wait-canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := awaitCommandReply(ctx, make(chan error), make(chan struct{})); !errors.Is(err, context.Canceled) {
			t.Fatal("command cancellation changed", err)
		}
	})
	adapter := newClimateAdapter(t, false)
	defer adapter.Close()
	raw, _ := adapter.value.Bytes()
	duplicate := append([]byte(`{"version":1,`), raw[1:]...)
	value, _ := storage.NewSecretValue(duplicate)
	if _, err := decodeCredential(value, adapter.settings); err == nil {
		t.Fatal("duplicate credential field admitted")
	}
	var pilot Pilot
	if err := pilot.Stop(nil); err == nil {
		t.Fatal("zero pilot accepted")
	}
	var clock *nilClock
	if _, err := NewPilotFactoryWithClock(adapter.settings, clock); err == nil {
		t.Fatal("typed nil clock admitted")
	}
}

type nilClock struct{}

func (*nilClock) Now() time.Time                       { panic("nil clock invoked") }
func (*nilClock) After(time.Duration) <-chan time.Time { panic("nil clock invoked") }

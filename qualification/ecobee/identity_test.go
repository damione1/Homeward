package ecobee

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mctofu/homekit/client"
)

// A normal API request from a fresh, unpaired synthetic identity. No wire bytes,
// tags or protocol messages are changed. This is not the suspended old probe.
func TestNHA009UnpairedIdentityRejected(t *testing.T) {
	f := newOrdinaryFixture(t)
	paired := f.newClient()
	ctx, cancel := context.WithTimeout(f.ctx, operationTimeout)
	accessories, err := paired.api.Accessories(ctx)
	cancel()
	if err != nil {
		t.Fatal("paired identity positive accessory witness:", err)
	}
	ids := discoverThermostat(t, accessories)
	values := readValues(t, f, paired, ids["35"])
	if number(t, values[ids["35"].CharacteristicID]) != 21.5 {
		t.Fatal("paired identity positive characteristic witness missing")
	}
	pairedWire := paired.connection()
	if pairedWire == nil || pairedWire.encryptedWrites.Load() < 2 {
		t.Fatal("paired identity encrypted request witnesses missing")
	}
	positive := f.requestsFor(pairedWire.LocalAddr().String())
	if positive.verify < 2 || positive.accessoriesGET != 1 || positive.characteristicsGET != 1 || positive.characteristicsPUT != 0 {
		t.Fatal("positive witnesses did not reach the owned fixture through the paired connection")
	}

	unpairedIdentity, err := client.NewRandomControllerConfig()
	if err != nil {
		t.Fatal("new synthetic unpaired identity:", err)
	}
	if unpairedIdentity.DeviceID == f.controller.DeviceID {
		t.Fatal("new synthetic identity must differ from the paired controller")
	}
	unpaired := f.newClientWithIdentity(unpairedIdentity)
	badCtx, badCancel := context.WithTimeout(f.ctx, operationTimeout)
	_, err = unpaired.api.Accessories(badCtx)
	contextErr := badCtx.Err()
	badCancel()
	if err == nil || contextErr != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatal("unpaired identity requires an actual rejection before cancellation/deadline")
	}
	// The pinned client's normal API reports this fixture's explicit HTTP error
	// at the finish step; an EOF/panic/transport failure is not an auth witness.
	if !strings.Contains(err.Error(), "verifyFinishRequest: invalid status code 500:") {
		t.Fatal("unpaired identity did not receive the fixture's explicit pair-verify rejection")
	}
	badWire := unpaired.connection()
	if badWire == nil {
		t.Fatal("unpaired identity failed before reaching a real loopback connection")
	}
	negative := f.requestsFor(badWire.LocalAddr().String())
	if negative.verify == 0 || negative.accessoriesGET != 0 || negative.characteristicsGET != 0 || negative.characteristicsPUT != 0 {
		t.Fatal("unpaired identity must reach pair-verify and receive no accessory/characteristic access")
	}
	if f.characteristicPUT.Load() != 0 {
		t.Fatal("identity qualification unexpectedly issued a write")
	}
	f.close(t)
}

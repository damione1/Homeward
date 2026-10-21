package ecobee

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mctofu/homekit/client"
)

// Ordinary traffic only. This test does not reproduce the suspended probe's
// altered-frame cases, contact a device, discover services, or qualify ECB601.
func TestNHA009OrdinaryLifecycle(t *testing.T) {
	f := newOrdinaryFixture(t)
	c := f.newClient()
	ctx, cancel := context.WithTimeout(f.ctx, operationTimeout)
	accessories, err := c.api.Accessories(ctx)
	cancel()
	if err != nil {
		t.Fatal("ordinary pair-verify/accessories:", err)
	}
	ids := discoverThermostat(t, accessories)
	if f.verifyRequests.Load() < 2 {
		t.Fatal("pair-verify request witness missing")
	}

	initial := readValues(t, f, c, ids["11"], ids["10"], ids["35"], ids["33"], ids["F"], ids["36"], ids["AF"], ids["BF"])
	for typ, want := range map[string]float64{"11": 21.5, "10": 45, "35": 21.5, "33": 1, "F": 0, "36": 0, "AF": 0, "BF": 1} {
		if got := number(t, initial[ids[typ].CharacteristicID]); got != want {
			t.Fatalf("ordinary characteristic %s: got %v, want %v", typ, got, want)
		}
	}
	before := number(t, initial[ids["35"].CharacteristicID])
	const target = 22.5
	writeValue(t, f, c, ids["35"], target)
	if got := f.characteristicPUT.Load(); got != 1 {
		t.Fatalf("synthetic temperature write: received %d PUT requests, want 1", got)
	}
	observed := readValues(t, f, c, ids["35"])
	after := number(t, observed[ids["35"].CharacteristicID])
	if after != target || before == after || f.temperatureWrites.Load() != 1 {
		t.Fatalf("synthetic write not independently observed: before=%v after=%v callbacks=%d", before, after, f.temperatureWrites.Load())
	}
	// An interface containing integer zero must remain a present wire value.
	writeValue(t, f, c, ids["33"], 0)
	if got := f.characteristicPUT.Load(); got != 2 {
		t.Fatalf("synthetic mode write: received %d PUT requests, want 2 total", got)
	}
	zeros := readValues(t, f, c, ids["33"], ids["F"])
	if string(zeros[ids["33"].CharacteristicID].Value) != "0" || string(zeros[ids["F"].CharacteristicID].Value) != "0" || f.modeWrites.Load() != 1 {
		t.Fatal("synthetic mode zero and idle zero must remain present")
	}
	wire := c.connection()
	if wire == nil || wire.encryptedWrites.Load() < 4 {
		t.Fatal("ordinary encrypted-frame witnesses missing")
	}
	t.Logf("ordinary pairing/read/write/zero witnesses passed; encrypted writes=%d", wire.encryptedWrites.Load())

	// The pinned dialer keeps a closed connection. Reconnection is explicitly a
	// newly owned client using the existing synthetic pairing, without a PUT replay.
	if err := wire.Conn.Close(); err != nil {
		t.Fatal("simulate ordinary connection loss:", err)
	}
	lostCtx, lostCancel := context.WithTimeout(f.ctx, operationTimeout)
	_, err = c.api.Accessories(lostCtx)
	lostCancel()
	if err == nil {
		t.Fatal("closed synthetic connection remained usable")
	}
	// The deliberate raw close may surface a close error from the layered client;
	// the closure is owned and attempted exactly once, without assuming idempotence.
	_ = c.close()
	verifyBefore := f.verifyRequests.Load()
	reconnected := f.newClient()
	reconnectCtx, reconnectCancel := context.WithTimeout(f.ctx, operationTimeout)
	reconnectedAccessories, err := reconnected.api.Accessories(reconnectCtx)
	reconnectCancel()
	if err != nil {
		t.Fatal("new synthetic client pair-verify:", err)
	}
	newIDs := discoverThermostat(t, reconnectedAccessories)
	if newIDs["35"] != ids["35"] || f.verifyRequests.Load() < verifyBefore+2 {
		t.Fatal("new connection did not complete a fresh verify for the same synthetic accessory")
	}
	fresh := readValues(t, f, reconnected, newIDs["35"], newIDs["11"])
	if got := f.characteristicPUT.Load(); got != 2 {
		t.Fatalf("synthetic reconnect/read replayed a write: received %d PUT requests, want 2 total", got)
	}
	if number(t, fresh[newIDs["35"].CharacteristicID]) != target || f.temperatureWrites.Load() != 1 {
		t.Fatal("fresh GET after reconnect failed or synthetic PUT was replayed")
	}

	// The previous GET is a positive witness before blocking one ordinary read.
	f.blockNext.Store(true)
	cancelCtx, cancelRead := context.WithCancel(f.ctx)
	defer cancelRead()
	result := make(chan error, 1)
	f.workers.Add(1)
	go func() {
		defer f.workers.Done()
		_, readErr := reconnected.api.Characteristics(cancelCtx, &client.CharacteristicsReadRequest{
			Characteristics: []client.CharacteristicReadRequest{newIDs["11"]},
		})
		result <- readErr
	}()
	waitSignal(t, f.entered, "ordinary cancellable GET reached server")
	cancelRead()
	select {
	case readErr := <-result:
		if !errors.Is(readErr, context.Canceled) {
			t.Fatal("in-flight ordinary GET did not return context cancellation")
		}
	case <-time.After(operationTimeout):
		t.Fatal("ordinary GET cancellation timed out")
	}
	f.unblock()
	waitSignal(t, f.exited, "ordinary GET callback exited")
	f.close(t)
	if got := f.characteristicPUT.Load(); got != 2 {
		t.Fatalf("synthetic lifecycle replayed a write: received %d PUT requests, want 2 total", got)
	}
	t.Logf("ordinary reconnect/cancel/shutdown witnesses passed; GET requests=%d", f.characteristicGET.Load())
}

func discoverThermostat(t *testing.T, accessories []*client.RawAccessory) map[string]client.CharacteristicReadRequest {
	t.Helper()
	ids := make(map[string]client.CharacteristicReadRequest)
	for _, a := range accessories {
		for _, s := range a.Services {
			if s.Type != "4A" {
				continue
			}
			for _, ch := range s.Characteristics {
				if _, duplicate := ids[ch.Type]; duplicate {
					t.Fatal("ambiguous synthetic thermostat characteristic type:", ch.Type)
				}
				ids[ch.Type] = client.CharacteristicReadRequest{AccessoryID: a.ID, CharacteristicID: ch.ID}
			}
		}
	}
	for _, typ := range []string{"11", "10", "35", "33", "F", "36", "AF", "BF"} {
		id := ids[typ]
		if id.AccessoryID == 0 || id.CharacteristicID == 0 {
			t.Fatal("required synthetic thermostat characteristic missing:", typ)
		}
	}
	return ids
}

func readValues(t *testing.T, f *ordinaryFixture, c *ownedClient, ids ...client.CharacteristicReadRequest) map[uint64]*client.CharacteristicReadResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, operationTimeout)
	defer cancel()
	rows, err := c.api.Characteristics(ctx, &client.CharacteristicsReadRequest{
		Characteristics: ids, Metadata: true, Permissions: true, Type: true,
	})
	if err != nil {
		t.Fatal("ordinary synthetic GET:", err)
	}
	if len(rows) != len(ids) {
		t.Fatal("ordinary synthetic GET returned an unexpected row count")
	}
	values := make(map[uint64]*client.CharacteristicReadResponse)
	for _, row := range rows {
		if row == nil || len(row.Value) == 0 || string(row.Value) == "null" || (row.Status != nil && *row.Status != 0) {
			t.Fatal("ordinary synthetic GET did not return a present successful value")
		}
		if _, duplicate := values[row.CharacteristicID]; duplicate {
			t.Fatal("ordinary synthetic GET returned duplicate characteristic IDs")
		}
		values[row.CharacteristicID] = row
	}
	for _, id := range ids {
		row := values[id.CharacteristicID]
		if row == nil || row.AccessoryID != id.AccessoryID {
			t.Fatal("ordinary synthetic GET returned another characteristic")
		}
	}
	return values
}

func number(t *testing.T, row *client.CharacteristicReadResponse) float64 {
	t.Helper()
	if row == nil {
		t.Fatal("missing synthetic numeric characteristic")
	}
	v, err := row.Value.Float64()
	if err != nil {
		t.Fatal("synthetic numeric characteristic:", err)
	}
	return v
}

func writeValue(t *testing.T, f *ordinaryFixture, c *ownedClient, id client.CharacteristicReadRequest, value any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, operationTimeout)
	defer cancel()
	ack, err := c.api.SetCharacteristics(ctx, &client.CharacteristicsWriteRequest{
		Characteristics: []client.CharacteristicWriteRequest{{
			AccessoryID: id.AccessoryID, CharacteristicID: id.CharacteristicID, Value: value,
		}},
	})
	if err != nil {
		t.Fatal("ordinary synthetic PUT:", err)
	}
	if len(ack) != 1 || ack[0] == nil || ack[0].AccessoryID != id.AccessoryID || ack[0].CharacteristicID != id.CharacteristicID || (ack[0].Status != nil && *ack[0].Status != 0) {
		t.Fatal("ordinary synthetic PUT did not return a successful acknowledgement")
	}
}

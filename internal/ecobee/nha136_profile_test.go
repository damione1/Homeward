package ecobee

import (
	"context"
	"testing"
	"time"

	"github.com/mctofu/homekit/client"
	wire "github.com/mctofu/homekit/client/characteristic"
	"github.com/mctofu/homekit/client/service"
)

// These are parsed client objects, not protocol fault injection. Their IDs,
// bounds and optional fields deliberately differ from the localhost fixture.
// HAP descriptors are literal test data, independent of climateRules/bindings.
func profileObjects() []*client.RawAccessory {
	chars := []*wire.RawCharacteristic{
		{ID: 401, Type: "F", Format: "uint8", Permissions: []string{"pr"}},
		{ID: 402, Type: "33", Format: "uint8", ValidValues: []float64{0, 1, 2, 3}, Permissions: []string{"pr", "pw"}},
		{ID: 403, Type: "11", Format: "float", Unit: "celsius", Permissions: []string{"pr"}, MinValue: wire.Value("0"), MaxValue: wire.Value("100"), StepValue: wire.Value("0.1")},
		{ID: 404, Type: "35", Format: "float", Unit: "celsius", Permissions: []string{"pr", "pw"}, MinValue: wire.Value("7"), MaxValue: wire.Value("35"), StepValue: wire.Value("0.5")},
		{ID: 405, Type: "36", Format: "uint8", Permissions: []string{"pr"}},
		{ID: 406, Type: "10", Format: "float", Unit: "percentage", Permissions: []string{"pr"}, MinValue: wire.Value("0"), MaxValue: wire.Value("100"), StepValue: wire.Value("0.1")},
		{ID: 407, Type: "12", Format: "float", Unit: "celsius", Permissions: []string{"pr", "pw"}, MinValue: wire.Value("7"), MaxValue: wire.Value("35"), StepValue: wire.Value("0.5")},
		{ID: 408, Type: "D", Format: "float", Unit: "celsius", Permissions: []string{"pr", "pw"}, MinValue: wire.Value("7"), MaxValue: wire.Value("35"), StepValue: wire.Value("0.5")},
		{ID: 409, Type: "AF", Format: "uint8", Permissions: []string{"pr"}},
		{ID: 410, Type: "BF", Format: "uint8", ValidValues: []float64{0, 1}, Permissions: []string{"pr", "pw"}},
	}
	return []*client.RawAccessory{{ID: 73, Services: []*service.RawService{{ID: 17, Type: "4A", Characteristics: chars}}}}
}

func admittedTestProfile(t *testing.T) climateProfile {
	t.Helper()
	p, err := admitProfile(profileObjects())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func observationRows() []*client.CharacteristicReadResponse {
	return []*client.CharacteristicReadResponse{
		{AccessoryID: 73, CharacteristicID: 401, Value: wire.Value("0")},
		{AccessoryID: 73, CharacteristicID: 402, Value: wire.Value("3")},
		{AccessoryID: 73, CharacteristicID: 403, Value: wire.Value("21.5")},
		{AccessoryID: 73, CharacteristicID: 404, Value: wire.Value("22.5")},
		{AccessoryID: 73, CharacteristicID: 405, Value: wire.Value("1")},
		{AccessoryID: 73, CharacteristicID: 406, Value: wire.Value("45")},
		{AccessoryID: 73, CharacteristicID: 407, Value: wire.Value("20")},
		{AccessoryID: 73, CharacteristicID: 408, Value: wire.Value("24")},
		{AccessoryID: 73, CharacteristicID: 409, Value: wire.Value("0")},
		{AccessoryID: 73, CharacteristicID: 410, Value: wire.Value("1")},
	}
}

// Literal expected identity by semantic field. No value is read from the profile.
func expectedIID(field climateField) uint64 {
	switch field {
	case thermalState:
		return 401
	case requestedMode:
		return 402
	case currentTemperature:
		return 403
	case targetTemperature:
		return 404
	case displayUnits:
		return 405
	case relativeHumidity:
		return 406
	case heatThreshold:
		return 407
	case coolThreshold:
		return 408
	case observedFan:
		return 409
	case targetFan:
		return 410
	default:
		panic("unknown test field")
	}
}

func rowFor(rows []*client.CharacteristicReadResponse, field climateField) *client.CharacteristicReadResponse {
	for _, row := range rows {
		if row.AccessoryID == 73 && row.CharacteristicID == expectedIID(field) {
			return row
		}
	}
	return nil
}

func assertLiteralReadIDs(t *testing.T, request *client.CharacteristicsReadRequest) {
	t.Helper()
	if request == nil || len(request.Characteristics) != 10 {
		t.Error("one grouped read must contain ten literal HAP identities")
		return
	}
	want := map[uint64]bool{401: true, 402: true, 403: true, 404: true, 405: true, 406: true, 407: true, 408: true, 409: true, 410: true}
	for _, id := range request.Characteristics {
		if id.AccessoryID != 73 || !want[id.CharacteristicID] {
			t.Error("grouped read has an unexpected or duplicate literal AID/IID")
			return
		}
		delete(want, id.CharacteristicID)
	}
}

func TestNHA136ProfilePresenceActivityAndPrivateValues(t *testing.T) {
	p := admittedTestProfile(t)
	now := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	rows := observationRows()
	observation, err := p.observe(rows, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		thermal, fan string
		want         observedActivity
	}{{"1", "0", activityHeating}, {"2", "0", activityCooling}, {"0", "2", activityFanOnly}, {"0", "0", activityIdle}, {"0", "1", activityIdle}} {
		rowFor(rows, thermalState).Value = wire.Value(test.thermal)
		rowFor(rows, observedFan).Value = wire.Value(test.fan)
		o, e := p.observe(rows, now)
		if e != nil {
			t.Fatal(e)
		}
		if got, kind := o.activity(now); got != test.want || kind != valuePresent {
			t.Fatalf("activity=%v/%v", got, kind)
		}
	}
	for _, raw := range []string{"null", "", "99"} {
		rowFor(rows, thermalState).Value = wire.Value(raw)
		o, e := p.observe(rows, now)
		if e != nil {
			t.Fatal(e)
		}
		if activity, kind := o.activity(now); activity != activityUnknown || kind == valuePresent {
			t.Fatalf("%q invented activity", raw)
		}
	}
	if got := observation.value(currentTemperature, now.Add(45*time.Second-time.Nanosecond)); got.kind != valuePresent {
		t.Fatal("premature expiry")
	}
	if got := observation.value(currentTemperature, now.Add(45*time.Second)); got.kind != valueStale {
		t.Fatal("45s boundary was not stale")
	}
	if _, e := p.write(targetTemperature, 7.25); e == nil {
		t.Fatal("minStep bypassed")
	}
	if _, e := p.write(targetTemperature, 6.5); e == nil {
		t.Fatal("lower bound bypassed")
	}
	if _, e := p.write(requestedMode, 0); e != nil {
		t.Fatal("present zero mode refused", e)
	}
	values := observationRows()
	rowFor(values, relativeHumidity).Value = wire.Value("0")
	o, e := p.observe(values, now)
	if e != nil {
		t.Fatal(e)
	}
	value, e := o.domainValue(now)
	if e != nil {
		t.Fatal(e)
	}
	climate, ok := value.Climate()
	if !ok {
		t.Fatal("climate value absent")
	}
	parts, e := climate.Components()
	if e != nil || parts.Humidity == nil {
		t.Fatal("humidity zero lost")
	}
	if n, _ := parts.Humidity.Value(); n != 0 {
		t.Fatal("humidity differs")
	}
}

func TestNHA136ForeignAndOptionalDiscoveryFailClosed(t *testing.T) {
	// Independent literal HAP metadata: 20 is Manufacturer, 21 is Model.
	// A manufacturer named ECB601 cannot authorize a foreign or absent model.
	for _, test := range []struct {
		name         string
		manufacturer string
		models       []wire.Value
		valid        bool
	}{
		{"model-valid-manufacturer-unrelated", "Homeward qualification", []wire.Value{wire.Value(`"ECB601"`)}, true},
		{"manufacturer-spoof-other-model", "ECB601", []wire.Value{wire.Value(`"foreign"`)}, false},
		{"manufacturer-is-not-model", "ECB601", nil, false},
		{"foreign-model", "Homeward qualification", []wire.Value{wire.Value(`"ECB602"`)}, false},
		{"duplicate-model", "Homeward qualification", []wire.Value{wire.Value(`"ECB601"`), wire.Value(`"ECB601"`)}, false},
		{"null-model", "Homeward qualification", []wire.Value{wire.Value(`null`)}, false},
		{"non-string-model", "Homeward qualification", []wire.Value{wire.Value(`42`)}, false},
		{"model-case-sensitive", "Homeward qualification", []wire.Value{wire.Value(`"ecb601"`)}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fields := []*wire.RawCharacteristic{{ID: 2, Type: "20", Format: "string", Value: wire.Value(`"` + test.manufacturer + `"`)}}
			for index, model := range test.models {
				fields = append(fields, &wire.RawCharacteristic{ID: uint64(3 + index), Type: "21", Format: "string", Value: model})
			}
			accessories := []*client.RawAccessory{{ID: 73, Services: []*service.RawService{{ID: 1, Type: "3E", Characteristics: fields}, {ID: 17, Type: "4A"}}}}
			if err := admitECB601(accessories); (err == nil) != test.valid {
				t.Fatalf("model identity admission=%v wantvalid=%v", err, test.valid)
			}
		})
	}
	accessories := profileObjects()
	accessories[0].Services = append(accessories[0].Services, &service.RawService{ID: 999, Type: "40", Characteristics: []*wire.RawCharacteristic{{ID: 9001, Type: "BF", Format: "uint8", Permissions: []string{"pr", "pw"}}}})
	p, e := admitProfile(accessories)
	if e != nil {
		t.Fatal(e)
	}
	if p.bindings[targetFan].id.CharacteristicID != 410 {
		t.Fatal("foreign fan admitted")
	}
	accessories = profileObjects()
	accessories[0].Services[0].Characteristics = accessories[0].Services[0].Characteristics[:5]
	p, e = admitProfile(accessories)
	if e != nil {
		t.Fatal(e)
	}
	description := p.description().GetClimate()
	if description.ObservedHumidityPercent != nil || description.SetpointRangeCelsius != nil || len(description.FanTargetOptions) != 0 {
		t.Fatal("optional ability invented")
	}
	if _, e := p.write(targetFan, 0); e == nil {
		t.Fatal("missing fan became commandable")
	}
	rows := observationRows()[:5]
	rowFor(rows, thermalState).Value = wire.Value("0")
	o, e := p.observe(rows, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	if a, k := o.activity(time.Now()); a != activityUnknown || k == valuePresent {
		t.Fatal("F0 without AF invented idle")
	}
	accessories = append(profileObjects(), profileObjects()[0])
	if _, e := admitProfile(accessories); e == nil {
		t.Fatal("ambiguous thermostat admitted")
	}
}

func TestNHA136RealEncryptedFakeNetworkReadAndAllControls(t *testing.T) {
	f := newOrdinaryFixture(t)
	c := f.NewClient()
	ctx, cancel := context.WithTimeout(f.Ctx, 10*time.Second)
	defer cancel()
	accessories, e := c.API.Accessories(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = admitECB601(accessories); e != nil {
		t.Fatal(e)
	}
	p, e := admitProfile(accessories)
	if e != nil {
		t.Fatal(e)
	}
	for _, target := range []struct {
		field climateField
		value float64
	}{{targetTemperature, 22.5}, {requestedMode, 0}, {requestedMode, 1}, {requestedMode, 2}, {requestedMode, 3}, {heatThreshold, 20.5}, {coolThreshold, 24.5}, {targetFan, 0}, {targetFan, 1}} {
		// The network fake advertises uint8 bounds; no hardcoded AID/IID is used.
		write, e := p.write(target.field, target.value)
		if e != nil {
			t.Fatal(target.field, e)
		}
		before := f.CharacteristicPUT.Load()
		ack, e := c.API.SetCharacteristics(ctx, &client.CharacteristicsWriteRequest{Characteristics: []client.CharacteristicWriteRequest{write}})
		if e != nil || len(ack) != 1 {
			t.Fatal("PUT", e)
		}
		if f.CharacteristicPUT.Load() != before+1 {
			t.Fatal("PUT replay")
		}
		request, e := p.readRequest()
		if e != nil {
			t.Fatal(e)
		}
		rows, e := c.API.Characteristics(ctx, request)
		if e != nil {
			t.Fatal(e)
		}
		o, e := p.observe(rows, time.Now())
		if e != nil {
			t.Fatal(e)
		}
		if got := o.value(target.field, time.Now()); got.kind != valuePresent || got.number != target.value {
			t.Fatalf("readback=%+v", got)
		}
	}
}

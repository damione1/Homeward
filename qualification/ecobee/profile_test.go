package ecobee

import (
	"context"
	"math"
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
		{ID: 402, Type: "33", Format: "uint8", Permissions: []string{"pr", "pw"}},
		{ID: 403, Type: "11", Format: "float", Unit: "celsius", Permissions: []string{"pr"}, MinValue: wire.Value("0"), MaxValue: wire.Value("100"), StepValue: wire.Value("0.1")},
		{ID: 404, Type: "35", Format: "float", Unit: "celsius", Permissions: []string{"pr", "pw"}, MinValue: wire.Value("7"), MaxValue: wire.Value("35"), StepValue: wire.Value("0.5")},
		{ID: 405, Type: "36", Format: "uint8", Permissions: []string{"pr"}},
		{ID: 406, Type: "10", Format: "float", Unit: "percentage", Permissions: []string{"pr"}, MinValue: wire.Value("0"), MaxValue: wire.Value("100"), StepValue: wire.Value("0.1")},
		{ID: 407, Type: "12", Format: "float", Unit: "celsius", Permissions: []string{"pr", "pw"}, MinValue: wire.Value("7"), MaxValue: wire.Value("35"), StepValue: wire.Value("0.5")},
		{ID: 408, Type: "D", Format: "float", Unit: "celsius", Permissions: []string{"pr", "pw"}, MinValue: wire.Value("7"), MaxValue: wire.Value("35"), StepValue: wire.Value("0.5")},
		{ID: 409, Type: "AF", Format: "uint8", Permissions: []string{"pr"}},
		{ID: 410, Type: "BF", Format: "uint8", Permissions: []string{"pr", "pw"}},
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

func TestNHA009ProfileAdmission(t *testing.T) {
	objects := profileObjects()
	p, err := admitProfile(objects)
	if err != nil {
		t.Fatal(err)
	}
	request, err := p.readRequest()
	if err != nil {
		t.Fatal("one grouped read must contain every discovered supported field")
	}
	assertLiteralReadIDs(t, request)
	at := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	observation, err := p.observe(observationRows(), at)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []struct {
		field climateField
		value float64
	}{
		{thermalState, 0}, {requestedMode, 3}, {currentTemperature, 21.5},
		{targetTemperature, 22.5}, {displayUnits, 1}, {relativeHumidity, 45},
		{heatThreshold, 20}, {coolThreshold, 24}, {observedFan, 0}, {targetFan, 1},
	} {
		b, value := p.bindings[expected.field], observation.value(expected.field, at)
		if !b.present || b.id.AccessoryID != 73 || b.id.CharacteristicID != expectedIID(expected.field) || value.kind != valuePresent || value.number != expected.value {
			t.Fatalf("literal HAP field mapping/value mismatch: field=%d binding=%+v value=%+v", expected.field, b, value)
		}
	}
	write, err := p.write(targetTemperature, 22.5)
	if err != nil || write.AccessoryID != 73 || write.CharacteristicID != 404 {
		t.Fatal("write must use discovered AID/IID and bounds:", err)
	}
	for _, expected := range []struct {
		field climateField
		value float64
	}{{heatThreshold, 20.5}, {coolThreshold, 24.5}, {targetFan, 0}} {
		write, err := p.write(expected.field, expected.value)
		if err != nil || write.AccessoryID != 73 || write.CharacteristicID != expectedIID(expected.field) || write.Value != expected.value {
			t.Fatal("literal threshold/fan control identity/value mismatch:", err)
		}
	}
	for _, value := range []float64{6.5, 35.5, 22.25, math.NaN(), math.Inf(1)} {
		if _, err := p.write(targetTemperature, value); err == nil {
			t.Fatalf("unadmitted target value %v", value)
		}
	}
	for _, value := range []float64{0, 1, 2, 3} {
		if _, err := p.write(requestedMode, value); err != nil {
			t.Fatal("required mode missing:", err)
		}
	}
	for _, value := range []float64{4, 1.5} {
		if _, err := p.write(requestedMode, value); err == nil {
			t.Fatal("unknown or fractional requested mode admitted")
		}
	}
	if _, err := p.write(observedFan, 1); err == nil {
		t.Fatal("observed fan state admitted as a control")
	}
	// No slice from client metadata is retained by the admitted profile.
	objects[0].Services[0].Characteristics[targetTemperature].MinValue[0] = '9'
	objects[0].Services[0].Characteristics[targetTemperature].ID = 999
	if p.bindings[targetTemperature].min != 7 || p.bindings[targetTemperature].id.CharacteristicID != 404 {
		t.Fatal("profile aliases mutable discovery metadata")
	}
	for _, change := range []func([]*client.RawAccessory){
		func(a []*client.RawAccessory) {
			a[0].Services[0].Characteristics[targetTemperature].Permissions = []string{"pr"}
		},
		func(a []*client.RawAccessory) {
			a[0].Services[0].Characteristics[currentTemperature].Unit = "fahrenheit"
		},
		func(a []*client.RawAccessory) { a[0].Services[0].Characteristics[targetTemperature].StepValue = nil },
		func(a []*client.RawAccessory) {
			a[0].Services[0].Characteristics[targetTemperature].MaxValue = wire.Value("6")
		},
		func(a []*client.RawAccessory) {
			a[0].Services[0].Characteristics[requestedMode].ID = a[0].Services[0].Characteristics[thermalState].ID
		},
	} {
		a := profileObjects()
		change(a)
		if _, err := admitProfile(a); err == nil {
			t.Fatal("inconsistent control metadata admitted")
		}
	}
	if _, err := (climateProfile{}).readRequest(); err == nil {
		t.Fatal("zero profile admitted")
	}
}

func TestNHA009ProfileLinkedFanAndMissingOptional(t *testing.T) {
	a := profileObjects()
	thermostat := a[0].Services[0]
	fans := thermostat.Characteristics[observedFan:]
	thermostat.Characteristics = thermostat.Characteristics[:observedFan]
	a[0].Services = append(a[0].Services, &service.RawService{ID: 29, Type: "B7", Characteristics: fans})
	p, err := admitProfile(a)
	if err != nil || p.bindings[observedFan].present {
		t.Fatal("unlinked fan must not be attributed to the thermostat")
	}
	thermostat.Linked = []uint64{29}
	p, err = admitProfile(a)
	if err != nil || !p.bindings[observedFan].present {
		t.Fatal("linked fan not discovered:", err)
	}
	thermostat.Linked = nil
	p, err = admitProfile(a)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	o, err := p.observe(observationRows()[:8], at)
	activity, kind := o.activity(at)
	if err != nil || activity != activityUnknown || kind != valueAbsent {
		t.Fatal("missing observed fan was manufactured into idle")
	}
	if _, err := p.write(targetFan, 0); err == nil {
		t.Fatal("absent target fan admitted as writable")
	}
}

func TestNHA009ProfileValuesActivityAndFreshness(t *testing.T) {
	p := admittedTestProfile(t)
	at := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		thermal, fan, target string
		want                 observedActivity
		conflict             bool
	}{
		{"1", "0", "0", activityHeating, true},
		{"2", "2", "1", activityCooling, true},
		{"0", "2", "1", activityFanOnly, false},
		{"0", "0", "3", activityIdle, false},
		{"0", "1", "3", activityIdle, false},
	} {
		rows := observationRows()
		rowFor(rows, thermalState).Value = wire.Value(tc.thermal)
		rowFor(rows, observedFan).Value = wire.Value(tc.fan)
		rowFor(rows, requestedMode).Value = wire.Value(tc.target)
		o, err := p.observe(rows, at)
		activity, kind := o.activity(at)
		if err != nil || activity != tc.want || kind != valuePresent || o.targetConflict(at) != tc.conflict {
			t.Fatalf("activity must follow current thermal/fan state: %v %v %v", activity, kind, err)
		}
		if temp := o.value(currentTemperature, at); temp.kind != valuePresent || temp.number != 21.5 || o.value(displayUnits, at).number != 1 {
			t.Fatal("Fahrenheit display changed Celsius observation")
		}
		if _, kind := o.activity(at.Add(45*time.Second - time.Nanosecond)); kind != valuePresent {
			t.Fatal("fresh observation expired early")
		}
		if activity, kind := o.activity(at.Add(45 * time.Second)); activity != activityUnknown || kind != valueStale {
			t.Fatal("stale activity manufactured a valid zero/idle")
		}
	}
	for _, tc := range []struct {
		name string
		edit func(*client.CharacteristicReadResponse)
		want valueKind
	}{
		{"absent", func(r *client.CharacteristicReadResponse) { r.Value = nil }, valueAbsent},
		{"null", func(r *client.CharacteristicReadResponse) { r.Value = wire.Value("null") }, valueNull},
		{"device status", func(r *client.CharacteristicReadResponse) { status := -70402; r.Status = &status }, valueDeviceError},
		{"unknown state", func(r *client.CharacteristicReadResponse) { r.Value = wire.Value("7") }, valueUnknown},
		{"zero", func(r *client.CharacteristicReadResponse) { r.Value = wire.Value("0") }, valuePresent},
	} {
		rows := observationRows()
		tc.edit(rowFor(rows, thermalState))
		o, err := p.observe(rows, at)
		if err != nil || o.value(thermalState, at).kind != tc.want {
			t.Fatal(tc.name, "was conflated with another presence state:", err)
		}
	}
	rows := observationRows()
	wrongUnit := "fahrenheit"
	rowFor(rows, currentTemperature).Unit = &wrongUnit
	rowFor(rows, relativeHumidity).Value = wire.Value("101")
	rowFor(rows, targetTemperature).Value = wire.Value("1e999") // valid JSON numeric object outside float64, never sent on wire
	rowFor(rows, heatThreshold).Value = wire.Value("25")
	o, err := p.observe(rows, at)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []climateField{currentTemperature, relativeHumidity, targetTemperature, heatThreshold, coolThreshold} {
		if o.value(field, at).kind != valueInvalid {
			t.Fatal("incoherent unit/number/thresholds admitted:", field)
		}
	}
	rows = observationRows()[1:] // a missing response row remains absent
	o, err = p.observe(rows, at)
	if err != nil || o.value(thermalState, at).kind != valueAbsent {
		t.Fatal("missing response row did not remain absent")
	}
}

func TestNHA009ProfileDiscoveryOnDouble(t *testing.T) {
	f := newOrdinaryFixture(t)
	c := f.newClient()
	ctx, cancel := context.WithTimeout(f.ctx, operationTimeout)
	defer cancel()
	accessories, err := c.api.Accessories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p, err := admitProfile(accessories)
	if err != nil {
		t.Fatal("admit metadata actually returned by the synthetic double:", err)
	}
	// The existing fixture discovers these eight literal HAP types independently
	// of climateRules; optional humidity/fan mappings must not silently disappear.
	discovered := discoverThermostat(t, accessories)
	poller, err := newQualificationPoller(p, wallPollClock(), c.api.Characteristics)
	if err != nil {
		t.Fatal(err)
	}
	o, err := poller.poll(ctx)
	activity, kind := o.activity(time.Now())
	if err != nil || activity != activityIdle || kind != valuePresent || o.value(currentTemperature, time.Now()).number != 21.5 {
		t.Fatal("grouped read from admitted synthetic accessory failed:", err)
	}
	for _, expected := range []struct {
		field climateField
		typ   string
		value float64
	}{
		{thermalState, "F", 0}, {requestedMode, "33", 1}, {currentTemperature, "11", 21.5},
		{targetTemperature, "35", 21.5}, {displayUnits, "36", 0}, {relativeHumidity, "10", 45},
		{observedFan, "AF", 0}, {targetFan, "BF", 1},
	} {
		b, value := p.bindings[expected.field], o.value(expected.field, time.Now())
		if !b.present || b.id != discovered[expected.typ] || value.kind != valuePresent || value.number != expected.value {
			t.Fatal("synthetic network discovery/value mismatch for literal HAP type:", expected.typ)
		}
	}
	if p.bindings[heatThreshold].present || p.bindings[coolThreshold].present || o.value(heatThreshold, time.Now()).kind != valueAbsent || o.value(coolThreshold, time.Now()).kind != valueAbsent {
		t.Fatal("synthetic double does not expose threshold characteristics")
	}
	if f.characteristicGET.Load() != 1 || f.characteristicPUT.Load() != 0 {
		t.Fatal("profile polling must issue one grouped GET and no writes")
	}
}

package mqtt

import (
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/mqtt/testbroker"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestNHA212ColorModeSelectsObservationAndPresetsUseExactCatalog(t *testing.T) {
	profile := discoveryComponent{rgb: true, colorTemp: true, minMireds: 100, maxMireds: 500, presets: []string{"scene:night/7"}}
	for _, tc := range []struct {
		payload          string
		rgb, temp, valid bool
	}{
		{`{"state":"ON","color_mode":"rgb","color":{"r":0,"g":137,"b":255},"color_temp":300,"effect":"scene:night/7"}`, true, false, true},
		{`{"state":"ON","color_mode":"color_temp","color":{"r":0,"g":137,"b":255},"color_temp":300}`, false, true, true},
		{`{"state":"ON","color_mode":"brightness","color":{"r":0,"g":137,"b":255},"color_temp":300}`, false, false, true},
		{`{"state":"ON","color":{"r":0,"g":137,"b":255},"color_temp":300}`, false, false, false},
		{`{"state":"ON","color":{"r":0,"g":137.5,"b":255}}`, false, false, false},
		{`{"state":"ON","color":{"r":0,"g":137,"b":256}}`, false, false, false},
		{`{"state":"ON","color":{"r":0,"g":137}}`, false, false, false},
		{`{"state":"ON","effect":"Scene:night/7"}`, false, false, false},
	} {
		value, err := lightState(profile, []byte(tc.payload), nil)
		if (err == nil) != tc.valid {
			t.Fatalf("%s: %v", tc.payload, err)
		}
		if !tc.valid {
			continue
		}
		light, _ := value.Light()
		parts, err := light.Components()
		if err != nil || (parts.RGB != nil) != tc.rgb || (parts.ColorTemperatureMireds != nil) != tc.temp {
			t.Fatalf("wrong active color: %s %+v %v", tc.payload, parts, err)
		}
	}
}

func TestNHA212RGBPresetCorrelationRequiresCompleteDirectReport(t *testing.T) {
	r := newNHA180Rig(t, false)
	const discovery = `{"unique_id":"kitchen-driver","device":{"identifiers":["kitchen-device"]},"schema":"json","state_topic":"zigbee2mqtt/kitchen/state","command_topic":"zigbee2mqtt/kitchen/set","brightness":true,"brightness_scale":254,"supported_color_modes":["rgb"],"effect":true,"effect_list":["scene:night/7"]}`
	r.broker.Publish(t, nha178DiscoveryTopic, []byte(discovery), true)
	nha193Wait(t, func() bool {
		caps, e := r.manager.Capabilities(r.entity)
		return e == nil && len(caps) == 1 && caps[0].GetLight().GetRgb() && len(caps[0].GetLight().GetPresets().GetChoices()) == 1
	})
	producer := r.broker.Connect(t, testbroker.ProducerUser, testbroker.ProducerPassword, "nha212-correlated-color", true, 0, nil)
	full := []byte(`{"state":"ON","brightness":127,"color_mode":"rgb","color":{"r":0,"g":137,"b":255},"effect":"scene:night/7"}`)
	nha114PublishAndWait(t, r, producer, "zigbee2mqtt/kitchen/state", full, nil, false)
	packet := nha114DispatchPacket(t, r, "nha212-rgb-proof", &actions.SetLight{On: proto.Bool(true), Rgb: &registry.RgbColor{Red: proto.Uint32(0), Green: proto.Uint32(137), Blue: proto.Uint32(255)}, Preset: proto.String("scene:night/7")})
	nha114PublishAndWait(t, r, producer, packet.Properties.ResponseTopic, []byte(`{"state":"ON"}`), packet.Properties.CorrelationData, false)
	r.status(t, "nha212-rgb-proof", actions.ActionStatus_ACTION_STATUS_DISPATCHED)
	nha114PublishAndWait(t, r, producer, packet.Properties.ResponseTopic, full, packet.Properties.CorrelationData, false)
	r.status(t, "nha212-rgb-proof", actions.ActionStatus_ACTION_STATUS_CONFIRMED)
	if got := r.broker.PublishCount(r.settings.clientID); got != 1 {
		t.Fatalf("one atomic intention emitted %d commands", got)
	}
}

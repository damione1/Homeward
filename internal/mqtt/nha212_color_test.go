package mqtt

import "testing"

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

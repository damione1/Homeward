package neviweb

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strconv"

	"github.com/damione1/Homeward/internal/domain"
)

// Model rules and attribute names adapt sinope-130 v4.3.1 climate.py,
// switch.py and const.py at the pinned SHA in provenance.go (GPL-3.0).
type modelFamily uint8

const (
	familyUnknown modelFamily = iota
	familyZigbee
	familyWiFi
	familyZigbeeHeatCool
	familyHeatCool
	familySwitch
)

type modelInfo struct {
	family                   modelFamily
	minimum, maximum         float64
	bounded                  bool
	coolMinimum, coolMaximum float64
	coolBounded              bool
}
type attributeFrame struct {
	raw                 map[string]json.RawMessage
	model               modelInfo
	temperature, target *float64
	coolTarget          *float64
	coolMinimum         *float64
	coolMaximum         *float64
	mode                *domain.ClimateMode
	diagnostic          bool
	on                  *bool
}

// Ask only for fields used by this pilot and listed by the pinned update
// class for the admitted model. Neviweb may reject an entire request when one
// attribute is unsupported (DVCATTRNSPTD); an unknown model gets no query.
func requestedAttributes(kind BindingKind, model modelInfo) string {
	switch kind {
	case BindingSwitch:
		if model.family == familySwitch {
			return "onOff"
		}
	case BindingClimate:
		switch model.family {
		case familyZigbee:
			return "roomTemperature,roomSetpoint,roomSetpointMin,roomSetpointMax,systemMode"
		case familyWiFi:
			return "roomTemperature,roomSetpoint,roomSetpointMin,roomSetpointMax,setpointMode"
		case familyZigbeeHeatCool:
			return "roomTemperatureDisplay,roomSetpoint,roomSetpointMin,roomSetpointMax,coolSetpoint,coolSetpointMin,coolSetpointMax,systemMode"
		case familyHeatCool:
			return "roomTemperature,roomSetpoint,roomSetpointMin,roomSetpointMax,coolSetpoint,coolSetpointMin,coolSetpointMax,heatCoolMode"
		}
	}
	return ""
}

// The pinned client receives signature.model in the /api/devices inventory;
// attribute responses need not repeat it. Tests may pass an inline signature
// only when no separate inventory snapshot is available.
func frameFor(kind BindingKind, body []byte, inventorySignature ...json.RawMessage) (attributeFrame, error) {
	if len(inventorySignature) > 1 {
		return attributeFrame{}, ErrProtocol
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil || raw == nil {
		return attributeFrame{}, ErrProtocol
	}
	if err := providerFailure(body); err != nil {
		return attributeFrame{}, err
	}
	signature := raw["signature"]
	if len(inventorySignature) == 1 && len(inventorySignature[0]) != 0 {
		signature = inventorySignature[0]
	}
	model, err := parseModel(signature, kind)
	if err != nil {
		return attributeFrame{}, err
	}
	f := attributeFrame{raw: raw, model: model}
	if kind == BindingClimate {
		if model.family == familyZigbeeHeatCool {
			f.temperature, f.diagnostic = readOptionalNumber(raw["roomTemperatureDisplay"], false)
		} else {
			f.temperature, f.diagnostic = readOptionalNumber(raw["roomTemperature"], true)
			var measured map[string]json.RawMessage
			if json.Unmarshal(raw["roomTemperature"], &measured) == nil && len(measured["error"]) != 0 && !bytes.Equal(measured["error"], []byte("null")) {
				f.temperature = nil
				f.diagnostic = true
			}
		}
		var invalid bool
		f.target, invalid = readOptionalNumber(raw["roomSetpoint"], false)
		f.diagnostic = f.diagnostic || invalid
		min, invalid := readOptionalNumber(raw["roomSetpointMin"], false)
		f.diagnostic = f.diagnostic || invalid
		max, invalid := readOptionalNumber(raw["roomSetpointMax"], false)
		f.diagnostic = f.diagnostic || invalid
		if min != nil && max != nil && *min <= *max && validTemperature(*min) && validTemperature(*max) {
			f.model.minimum = *min
			f.model.maximum = *max
			f.model.bounded = true
		} else if min != nil && max != nil {
			f.diagnostic = true
		}
		if model.family == familyHeatCool || model.family == familyZigbeeHeatCool {
			f.coolTarget, invalid = readOptionalNumber(raw["coolSetpoint"], false)
			f.diagnostic = f.diagnostic || invalid
			f.coolMinimum, invalid = readOptionalNumber(raw["coolSetpointMin"], false)
			f.diagnostic = f.diagnostic || invalid
			f.coolMaximum, invalid = readOptionalNumber(raw["coolSetpointMax"], false)
			f.diagnostic = f.diagnostic || invalid
			if f.coolMinimum != nil && f.coolMaximum != nil {
				if *f.coolMinimum > *f.coolMaximum || !validTemperature(*f.coolMinimum) || !validTemperature(*f.coolMaximum) {
					f.coolMinimum, f.coolMaximum = nil, nil
					f.diagnostic = true
				} else {
					f.model.coolMinimum, f.model.coolMaximum = *f.coolMinimum, *f.coolMaximum
					f.model.coolBounded = true
				}
			}
		}
		f.mode, invalid = observedMode(model.family, raw)
		f.diagnostic = f.diagnostic || invalid
		if model.family == familyHeatCool || model.family == familyZigbeeHeatCool {
			modeKey := "heatCoolMode"
			if model.family == familyZigbeeHeatCool {
				modeKey = "systemMode"
			}
			var rawMode string
			_ = json.Unmarshal(raw[modeKey], &rawMode)
			switch rawMode {
			case "auto":
				if f.target == nil || f.coolTarget == nil || *f.target > *f.coolTarget {
					f.target, f.coolTarget = nil, nil
					f.diagnostic = true
				}
			case "cool":
				if f.coolTarget == nil {
					f.diagnostic = true
				}
			}
		}
		if f.temperature != nil && !validTemperature(*f.temperature) || f.target != nil && !validTemperature(*f.target) || f.coolTarget != nil && !validTemperature(*f.coolTarget) {
			return attributeFrame{}, ErrProtocol
		}
	} else if kind == BindingSwitch {
		var state string
		if json.Unmarshal(raw["onOff"], &state) == nil {
			switch state {
			case "on":
				v := true
				f.on = &v
			case "off":
				v := false
				f.on = &v
			}
		}
	}
	return f, nil
}

func validTemperature(value float64) bool {
	_, err := domain.NewCelsiusTemperature(value)
	return err == nil
}

// readOptionalNumber distinguishes an absent/null reading from a malformed
// present one. In particular, roomTemperature.value=null is not zero.
func readOptionalNumber(raw json.RawMessage, nested bool) (*float64, bool) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, false
	}
	if nested {
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil || object == nil {
			return nil, true
		}
		raw = object["value"]
		if len(raw) == 0 {
			return nil, true
		}
		if bytes.Equal(raw, []byte("null")) {
			return nil, false
		}
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&number) != nil {
		return nil, true
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, true
	}
	value, err := strconv.ParseFloat(string(number), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil, true
	}
	return &value, false
}

// The source models auto scheduling on Wi-Fi through setpointMode; an auto
// heat/cool range is a different mode and has no ClimateMode equivalent yet.
func observedMode(family modelFamily, raw map[string]json.RawMessage) (*domain.ClimateMode, bool) {
	attribute := "systemMode"
	if family == familyWiFi {
		attribute = "setpointMode"
	} else if family == familyHeatCool {
		attribute = "heatCoolMode"
	}
	value := raw[attribute]
	if len(value) == 0 || bytes.Equal(value, []byte("null")) {
		return nil, false
	}
	var source string
	if json.Unmarshal(value, &source) != nil {
		return nil, true
	}
	var mode domain.ClimateMode
	switch source {
	case "off":
		mode = domain.ClimateModeOff
	case "heat":
		if family == familyWiFi {
			return nil, true
		}
		mode = domain.ClimateModeHeat
	case "manual":
		if family != familyWiFi {
			return nil, true
		}
		mode = domain.ClimateModeHeat
	case "auto":
		if family != familyWiFi {
			return nil, true // HEAT_COOL range is not represented by the shared enum.
		}
		mode = domain.ClimateModeAuto
	case "cool":
		if family != familyHeatCool && family != familyZigbeeHeatCool {
			return nil, true
		}
		mode = domain.ClimateModeCool
	default:
		return nil, true
	}
	return &mode, false
}

func parseModel(raw json.RawMessage, kind BindingKind) (modelInfo, error) {
	var signature struct {
		Model    json.Number `json:"model"`
		Protocol string      `json:"protocol"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&signature) != nil {
		return modelInfo{}, ErrProtocol
	}
	model, err := strconv.Atoi(signature.Model.String())
	if err != nil || signature.Protocol == "miwi" {
		return modelInfo{}, ErrProtocol
	}
	info := modelInfo{}
	if kind == BindingSwitch {
		switch model {
		case 2151, 2152, 339:
			info.family = familySwitch
		}
	}
	if kind == BindingClimate {
		switch model {
		case 1123, 1124, 300, 737, 7372, 7373:
			info.family = familyZigbee
		case 1512:
			info.family = familyZigbeeHeatCool
		case 1510, 336, 348, 343, 350, 354, 738, 739, 742:
			info.family = familyWiFi
		case 6727, 6730, 6731, 6734:
			info.family = familyHeatCool
		}
	}
	if info.family == familyUnknown {
		return modelInfo{}, ErrProtocol
	}
	return info, nil
}

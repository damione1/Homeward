package hilo

import "strings"

// AliasKind is the single closed classification of the 58 retained Hilo IDs.
// A direct path is conditional on an admitted device and present wire field.
type AliasKind uint8

const (
	AliasUnknown AliasKind = iota
	AliasClimate
	AliasPower
	AliasTemperature
	AliasSwitch
	AliasGateway
	AliasNotifications
	AliasWeather
	AliasEnergy
	AliasDeferred
)

type aliasProfile struct {
	Alias  string
	Kind   AliasKind
	Source string // source-power alias for a calculated energy counter
}

var retainedProfile = []aliasProfile{
	{"climate.chambre_eugenie", AliasClimate, ""},
	{"climate.chambre_principale", AliasClimate, ""},
	{"climate.chambre_sous_sol", AliasClimate, ""},
	{"climate.living_room", AliasClimate, ""},
	{"climate.plancher_cuisine", AliasClimate, ""},
	{"climate.salle_a_manger", AliasClimate, ""},
	{"climate.salle_de_bain", AliasClimate, ""},
	{"climate.sous_sol", AliasClimate, ""},
	{"sensor.chambre_eugenie_hilo_energy", AliasEnergy, "sensor.chambre_eugenie_power"},
	{"sensor.chambre_eugenie_power", AliasPower, ""},
	{"sensor.chambre_eugenie_target_temperature", AliasTemperature, ""},
	{"sensor.chambre_eugenie_temperature", AliasTemperature, ""},
	{"sensor.chambre_principale_hilo_energy", AliasEnergy, "sensor.chambre_principale_power"},
	{"sensor.chambre_principale_power", AliasPower, ""},
	{"sensor.chambre_principale_target_temperature", AliasTemperature, ""},
	{"sensor.chambre_principale_temperature", AliasTemperature, ""},
	{"sensor.chambre_sous_sol_hilo_energy", AliasEnergy, "sensor.chambre_sous_sol_power"},
	{"sensor.chambre_sous_sol_power", AliasPower, ""},
	{"sensor.chambre_sous_sol_target_temperature", AliasTemperature, ""},
	{"sensor.chambre_sous_sol_temperature", AliasTemperature, ""},
	{"sensor.chauffe_eau_hilo_energy", AliasEnergy, "sensor.chauffe_eau_power"},
	{"sensor.chauffe_eau_power", AliasPower, ""},
	{"sensor.chauffe_eau_temperature", AliasTemperature, ""},
	{"sensor.defi_hilo", AliasDeferred, ""},
	{"sensor.hilo_cost_total", AliasDeferred, ""},
	{"sensor.hilo_energy_total", AliasEnergy, "sensor.meter00_power"},
	{"sensor.hilo_gateway", AliasGateway, ""},
	{"sensor.hilo_rate_access", AliasDeferred, ""},
	{"sensor.hilo_rate_current", AliasDeferred, ""},
	{"sensor.hilo_rate_low", AliasDeferred, ""},
	{"sensor.hilo_rate_low_threshold", AliasDeferred, ""},
	{"sensor.hilo_rate_medium", AliasDeferred, ""},
	{"sensor.hilo_rate_reward_rate", AliasDeferred, ""},
	{"sensor.living_room_hilo_energy", AliasEnergy, "sensor.living_room_power"},
	{"sensor.living_room_power", AliasPower, ""},
	{"sensor.living_room_target_temperature", AliasTemperature, ""},
	{"sensor.living_room_temperature", AliasTemperature, ""},
	{"sensor.meter00_power", AliasPower, ""},
	{"sensor.notifications_hilo", AliasNotifications, ""},
	{"sensor.outdoor_weather_hilo", AliasWeather, ""},
	{"sensor.plancher_cuisine_hilo_energy", AliasEnergy, "sensor.plancher_cuisine_power"},
	{"sensor.plancher_cuisine_power", AliasPower, ""},
	{"sensor.plancher_cuisine_target_temperature", AliasTemperature, ""},
	{"sensor.plancher_cuisine_temperature", AliasTemperature, ""},
	{"sensor.recompenses_hilo", AliasDeferred, ""},
	{"sensor.salle_a_manger_hilo_energy", AliasEnergy, "sensor.salle_a_manger_power"},
	{"sensor.salle_a_manger_power", AliasPower, ""},
	{"sensor.salle_a_manger_target_temperature", AliasTemperature, ""},
	{"sensor.salle_a_manger_temperature", AliasTemperature, ""},
	{"sensor.salle_de_bain_hilo_energy", AliasEnergy, "sensor.salle_de_bain_power"},
	{"sensor.salle_de_bain_power", AliasPower, ""},
	{"sensor.salle_de_bain_target_temperature", AliasTemperature, ""},
	{"sensor.salle_de_bain_temperature", AliasTemperature, ""},
	{"sensor.sous_sol_hilo_energy", AliasEnergy, "sensor.sous_sol_power"},
	{"sensor.sous_sol_power", AliasPower, ""},
	{"sensor.sous_sol_target_temperature", AliasTemperature, ""},
	{"sensor.sous_sol_temperature", AliasTemperature, ""},
	{"switch.chauffe_eau", AliasSwitch, ""},
}

func RetainedAliases() []string {
	out := make([]string, len(retainedProfile))
	for i, entry := range retainedProfile {
		out[i] = entry.Alias
	}
	return out
}

// RequiredDeviceAliases identifies the direct observations that need an
// operator-supplied Hilo device ID. Weather and notifications are location
// observations; calculated energy and deferred aliases have no device input.
func RequiredDeviceAliases() []string {
	out := make([]string, 0, 37)
	for _, entry := range retainedProfile {
		if entry.Kind.Direct() && entry.Kind != AliasNotifications && entry.Kind != AliasWeather {
			out = append(out, entry.Alias)
		}
	}
	return out
}

func classifyAlias(alias string) aliasProfile {
	for _, entry := range retainedProfile {
		if entry.Alias == alias {
			return entry
		}
	}
	return aliasProfile{}
}

func (k AliasKind) Direct() bool { return k >= AliasClimate && k <= AliasWeather }

func endpointFor(entry aliasProfile) string {
	switch entry.Kind {
	case AliasClimate:
		return "climate"
	case AliasPower:
		return "power"
	case AliasTemperature:
		if strings.Contains(entry.Alias, "target_temperature") {
			return "target_temperature"
		}
		return "temperature"
	case AliasSwitch:
		return "relay"
	case AliasGateway:
		return "connectivity"
	case AliasNotifications:
		return "notifications"
	case AliasWeather:
		return "weather"
	default:
		return ""
	}
}

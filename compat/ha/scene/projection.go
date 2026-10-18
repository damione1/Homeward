package scene

// These are fields present in the pinned HA scene exports. Their admission
// means only that the compiler has classified them: it does not infer a live
// capability from exported supported_features, device_id or names. Unknown
// fields fail the individual member instead of being silently discarded.
func knownAttribute(kind MemberKind, field string) bool {
	switch kind {
	case MemberLight:
		switch field {
		case "state", "brightness", "color_mode", "color_temp_kelvin", "effect",
			"color_temp", "hs_color", "rgb_color", "xy_color", "effect_list",
			"min_color_temp_kelvin", "max_color_temp_kelvin", "min_mireds", "max_mireds",
			"supported_color_modes", "supported_features", "device_id", "zone_id",
			"icon", "friendly_name":
			return true
		}
	case MemberSwitch:
		switch field {
		case "state", "icon", "friendly_name", "supported_features", "device_id", "zone_id":
			return true
		}
	case MemberFan:
		switch field {
		case "state", "percentage", "preset_mode", "preset_modes", "percentage_step",
			"supported_features", "device_id", "zone_id", "friendly_name", "icon":
			return true
		}
	case MemberClimate:
		switch field {
		case "state", "temperature", "humidity", "fan_mode", "preset_mode",
			"target_temp_high", "target_temp_low", "swing_mode", "swing_horizontal_mode",
			"aux_heat", "hvac_modes", "min_temp", "max_temp", "min_humidity",
			"max_humidity", "fan_modes", "preset_modes", "current_temperature",
			"current_humidity", "hvac_action", "friendly_name", "supported_features",
			"wattage", "cycle_length", "error_code", "heat_level", "pi_heating_demand",
			"temp_display_value", "second_display", "keypad", "backlight",
			"time_format", "temperature_format", "setpoint_max", "setpoint_min",
			"eco_status", "eco_optOut", "eco_setpoint", "eco_power_relative",
			"eco_power_absolute", "eco_setpoint_status", "eco_setpoint_delta",
			"hourly_kwh_count", "daily_kwh_count", "monthly_kwh_count", "hourly_kwh",
			"daily_kwh", "monthly_kwh", "rssi", "sku", "device_model",
			"device_model_cfg", "firmware", "activation", "id", "unit_of_measurement",
			"device_class", "icon":
			return true
		}
	}
	return false
}

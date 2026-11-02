package dashboard

import wire "github.com/damione1/Homeward/gen/home/dashboard/v1"

// A bounded icon name is presentation data, never executable SVG or an URL.
func nativeCardIcon(name string) wire.NativeIcon {
	switch name {
	case "", "mdi:information", "mdi:information-outline":
		return wire.NativeIcon_NATIVE_ICON_INFORMATION
	case "mdi:home", "mdi:home-outline":
		return wire.NativeIcon_NATIVE_ICON_HOME
	case "mdi:clock", "mdi:clock-outline":
		return wire.NativeIcon_NATIVE_ICON_CLOCK
	case "mdi:view-grid", "mdi:view-dashboard":
		return wire.NativeIcon_NATIVE_ICON_GRID
	case "mdi:sofa", "mdi:sofa-outline":
		return wire.NativeIcon_NATIVE_ICON_SOFA
	case "mdi:knife", "mdi:stove", "mdi:silverware-fork-knife":
		return wire.NativeIcon_NATIVE_ICON_KITCHEN
	case "mdi:bed", "mdi:bed-double", "mdi:bed-king", "mdi:bed-king-outline":
		return wire.NativeIcon_NATIVE_ICON_BED
	case "mdi:lightbulb", "mdi:lightbulb-group", "mdi:lightbulb-outline", "mdi:ceiling-light", "mdi:wall-sconce-round", "mdi:floor-lamp", "mdi:desk-lamp", "mdi:lamp", "mdi:coach-lamp", "mdi:string-lights", "mdi:alarm-light":
		return wire.NativeIcon_NATIVE_ICON_LIGHT
	case "mdi:fan", "mdi:fan-off":
		return wire.NativeIcon_NATIVE_ICON_FAN
	case "mdi:thermostat", "mdi:home-thermometer", "mdi:home-thermometer-outline", "mdi:thermometer", "mdi:fire":
		return wire.NativeIcon_NATIVE_ICON_THERMOSTAT
	case "mdi:music", "mdi:music-note", "mdi:speaker", "mdi:surround-sound", "mdi:television":
		return wire.NativeIcon_NATIVE_ICON_MUSIC
	case "mdi:garage", "mdi:garage-variant", "mdi:garage-open":
		return wire.NativeIcon_NATIVE_ICON_GARAGE
	case "mdi:lock", "mdi:lock-open":
		return wire.NativeIcon_NATIVE_ICON_LOCK
	case "mdi:shield-home", "mdi:shield-check", "mdi:shield", "mdi:alarm-light-outline":
		return wire.NativeIcon_NATIVE_ICON_SHIELD
	case "mdi:water", "mdi:water-alert", "mdi:water-alert-outline", "mdi:sprinkler", "mdi:sprinkler-variant":
		return wire.NativeIcon_NATIVE_ICON_WATER
	case "mdi:weather-sunny", "mdi:white-balance-sunny":
		return wire.NativeIcon_NATIVE_ICON_SUN
	case "mdi:tree", "mdi:pine-tree", "mdi:palm-tree":
		return wire.NativeIcon_NATIVE_ICON_TREE
	case "mdi:desk", "mdi:monitor", "mdi:desktop-classic":
		return wire.NativeIcon_NATIVE_ICON_OFFICE
	case "mdi:bathtub", "mdi:bathtub-outline", "mdi:shower":
		return wire.NativeIcon_NATIVE_ICON_BATH
	case "mdi:baby-face", "mdi:baby-face-outline", "mdi:teddy-bear":
		return wire.NativeIcon_NATIVE_ICON_CHILD
	case "mdi:cart", "mdi:cart-outline", "mdi:shopping":
		return wire.NativeIcon_NATIVE_ICON_SHOPPING
	case "mdi:lightbulb-off", "mdi:weather-night", "mdi:creation":
		return wire.NativeIcon_NATIVE_ICON_SCENE
	case "mdi:battery", "mdi:battery-outline":
		return wire.NativeIcon_NATIVE_ICON_BATTERY
	case "mdi:power", "mdi:power-plug":
		return wire.NativeIcon_NATIVE_ICON_POWER
	default:
		return wire.NativeIcon_NATIVE_ICON_INFORMATION
	}
}

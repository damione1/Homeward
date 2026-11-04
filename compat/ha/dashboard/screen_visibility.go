package dashboard

import (
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"regexp"
	"strconv"
)

// Deliberately not a CSS interpreter: no scripts, comma alternatives, device
// discovery, orientation or implicit mobile/tablet classes.
var screenQuery = regexp.MustCompile(`^\s*\(\s*(min-width|max-width)\s*:\s*([0-9]{1,5})px\s*\)(?:\s+and\s+\(\s*(min-width|max-width)\s*:\s*([0-9]{1,5})px\s*\))?\s*$`)

func parseScreenMediaQuery(query string) (uint32, uint32, bool) {
	if len(query) > 256 {
		return 0, 0, false
	}
	parts := screenQuery.FindStringSubmatch(query)
	if parts == nil {
		return 0, 0, false
	}
	var minimum, maximum uint32
	seen := map[string]bool{}
	for i := 1; i < 5; i += 2 {
		if parts[i] == "" {
			continue
		}
		n, err := strconv.ParseUint(parts[i+1], 10, 32)
		if err != nil || n > 65535 || seen[parts[i]] || (parts[i] == "max-width" && n == 0) {
			return 0, 0, false
		}
		seen[parts[i]] = true
		if parts[i] == "min-width" {
			minimum = uint32(n)
		} else {
			maximum = uint32(n)
		}
	}
	return minimum, maximum, maximum == 0 || minimum <= maximum
}
func nativeScreenVisibility(values []Visibility) (*wire.DashboardScreenVisibility, bool) {
	var result *wire.DashboardScreenVisibility
	for _, value := range values {
		if value.Kind() != VisibilityScreen {
			continue
		}
		minimum, maximum, valid := value.Viewport()
		if !valid || minimum > 65535 || maximum > 65535 {
			return nil, false
		}
		if result == nil {
			result = &wire.DashboardScreenVisibility{}
		}
		if int32(minimum) > result.MinWidth {
			result.MinWidth = int32(minimum)
		}
		if maximum != 0 && (result.MaxWidth == 0 || int32(maximum) < result.MaxWidth) {
			result.MaxWidth = int32(maximum)
		}
	}
	return result, result == nil || result.MaxWidth == 0 || result.MinWidth <= result.MaxWidth
}

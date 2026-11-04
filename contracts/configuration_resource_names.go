package contracts

import (
	"github.com/damione1/Homeward/internal/apitypes"
	"strings"
)

func configurationResourceName(value, collection string) bool {
	parent, id, ok := strings.Cut(value, "/"+collection+"/")
	if !ok || len(id) < 1 || len(id) > 63 || id[0] == '-' || id[len(id)-1] == '-' {
		return false
	}
	if _, err := apitypes.ParseHomeName(parent); err != nil {
		return false
	}
	for _, b := range []byte(id) {
		if !(b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-') {
			return false
		}
	}
	return true
}
func configurationHomeName(value string) bool {
	_, err := apitypes.ParseHomeName(value)
	return err == nil
}

func requestedPublicResourceID(value string) bool {
	if value == "" {
		return true
	}
	return value[0] >= 'a' && value[0] <= 'z' && configurationResourceName("homes/fixture/resources/"+value, "resources")
}

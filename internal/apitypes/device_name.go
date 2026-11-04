package apitypes

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/damione1/Homeward/internal/domain"
)

// DeviceName is an admitted canonical resource name, never a label or address.
type DeviceName struct{ value string }

func ParseDeviceName(value string) (DeviceName, error) {
	if err := resourceName(value, "homes", "devices"); err != nil {
		return DeviceName{}, err
	}
	return DeviceName{value}, nil
}
func (n DeviceName) String() string { return n.value }
func (n DeviceName) Valid() bool    { return n.value != "" }
func (n DeviceName) Home() HomeName {
	if !n.Valid() {
		return HomeName{}
	}
	return HomeName{n.value[:strings.LastIndex(n.value, "/devices/")]}
}

// DeviceNameForIdentity matches the registry SQL index's 192-bit native-ID hash.
// Neither aliases, display names, provider addresses nor normalization enter it.
func DeviceNameForIdentity(home HomeName, id domain.DeviceID) (DeviceName, error) {
	raw, err := id.Value()
	if err != nil || !home.Valid() {
		return DeviceName{}, ErrResourceName
	}
	digest := sha256.Sum256([]byte(raw))
	return ParseDeviceName(home.String() + "/devices/device-" + hex.EncodeToString(digest[:24]))
}

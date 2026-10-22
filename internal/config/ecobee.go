package config

import "github.com/damione1/Homeward/internal/ecobee"

func (c Config) EcobeeSettings() (ecobee.Settings, bool) {
	if c.ecobee == nil {
		return ecobee.Settings{}, false
	}
	return *c.ecobee, true
}

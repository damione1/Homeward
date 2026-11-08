package config

import "github.com/damione1/Homeward/internal/wled"

func (c Config) WLEDSettings() (wled.Settings, bool) {
	if c.wled == nil {
		return wled.Settings{}, false
	}
	return *c.wled, true
}

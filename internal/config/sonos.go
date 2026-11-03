package config

import "github.com/damione1/Homeward/internal/sonos"

func (c Config) SonosSettings() (sonos.Settings, bool) {
	if c.sonos == nil {
		return sonos.Settings{}, false
	}
	return *c.sonos, true
}

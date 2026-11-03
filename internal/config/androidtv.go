package config

import "github.com/damione1/Homeward/internal/androidtv"

func (c Config) AndroidTVSettings() (androidtv.Settings, bool) {
	if c.androidTV == nil {
		return androidtv.Settings{}, false
	}
	return *c.androidTV, true
}

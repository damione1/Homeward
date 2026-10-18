package config

import "github.com/damione1/Homeward/internal/hilo"

// HiloSettings returns the admitted, read-only cloud profile. An absent Hilo
// section leaves the integration disabled; the value is already validated by
// ParseWithEnv before Run opens PostgreSQL or starts a pilot.
func (c Config) HiloSettings() (hilo.Settings, bool) {
	if c.hilo == nil {
		return hilo.Settings{}, false
	}
	return *c.hilo, true
}

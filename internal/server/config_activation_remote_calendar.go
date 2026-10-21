package server

import (
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"reflect"
)

// Preparation builds the existing registry request and a dormant factory.
// It never binds a CalendarID, opens secrets, starts HTTP or mutates the cache.
func remoteCalendarBinding(configuration config.Config) (registry.BindingRequest, bool, error) {
	settings, enabled := configuration.RemoteCalendarSettings()
	if !enabled {
		return registry.BindingRequest{}, false, nil
	}
	instance, key, err := settings.Identity()
	if err != nil {
		return registry.BindingRequest{}, false, err
	}
	request, err := registry.NewBindingRequest(instance, key, remoteCalendarAlias)
	return request, true, err
}
func (c *configuredOperationalCompiler) prepareRemoteCalendar(old, next config.Config, bindings *registry.PreparedBindings, forced map[domain.IntegrationInstanceID]bool) (domain.IntegrationInstanceID, bool, bool, *integrations.PilotSpec, error) {
	previous, had := old.RemoteCalendarSettings()
	settings, enabled := next.RemoteCalendarSettings()
	instance, _ := domain.ParseIntegrationInstanceID("homeward-remote-calendar-v1")
	changed := had != enabled || !reflect.DeepEqual(previous, settings) || forced[instance]
	if !changed {
		return instance, had, false, nil, nil
	}
	if !enabled {
		return instance, had, true, nil, nil
	}
	if c.remoteCalendar == nil || bindings == nil {
		return instance, had, true, nil, configactivation.ErrOperationalComposition
	}
	resolved, err := bindings.LookupAlias(remoteCalendarAlias)
	if err != nil || resolved.Retired || !resolved.Current {
		return instance, had, true, nil, configactivation.ErrOperationalComposition
	}
	factory, err := c.remoteCalendar.factory(settings)
	if err != nil {
		return instance, had, true, nil, err
	}
	spec, err := integrations.NewPilotSpec(instance, factory)
	if err != nil {
		return instance, had, true, nil, err
	}
	return instance, had, true, &spec, nil
}

// Observation only; it confers no alternate activation or source authority.
type remoteCalendarActivationObserver func(*configactivation.Owner)

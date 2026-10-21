package config

import (
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/remotecalendar"
	"github.com/damione1/Homeward/internal/storage"
	"go.yaml.in/yaml/v3"
	"net/netip"
)

type remoteCalendarSettings = remotecalendar.Settings

func remoteCalendarMap(node yaml.Node, allowed ...string) (map[string]*yaml.Node, error) {
	if node.Kind != yaml.MappingNode || len(node.Content) == 0 || len(node.Content) > 32 || len(node.Content)%2 != 0 {
		return nil, ErrInvalidYAML
	}
	result := make(map[string]*yaml.Node, len(node.Content)/2)
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || result[key.Value] != nil || value.Kind == yaml.AliasNode {
			return nil, ErrInvalidYAML
		}
		admitted := false
		for _, name := range allowed {
			if key.Value == name {
				admitted = true
				break
			}
		}
		if !admitted {
			return nil, ErrInvalidYAML
		}
		result[key.Value] = value
	}
	return result, nil
}
func remoteCalendarString(node *yaml.Node) (string, error) {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!str" || node.Value == "" {
		return "", ErrInvalidYAML
	}
	return node.Value, nil
}
func remoteCalendarHandle(instance domain.IntegrationInstanceID, node *yaml.Node) (storage.SecretHandle, error) {
	raw, err := remoteCalendarString(node)
	if err != nil {
		return storage.SecretHandle{}, ErrInvalidYAML
	}
	name, err := storage.ParseSecretName(raw)
	if err != nil {
		return storage.SecretHandle{}, ErrInvalidYAML
	}
	handle, err := storage.NewSecretHandle(instance, name)
	if err != nil {
		return storage.SecretHandle{}, ErrInvalidYAML
	}
	return handle, nil
}
func parseRemoteCalendarInput(node yaml.Node) (*remoteCalendarSettings, error) {
	if node.Kind == 0 {
		return nil, nil
	}
	fields, err := remoteCalendarMap(node, "label", "url", "secret_url", "auth", "network", "floating_timezone", "view_timezone")
	if err != nil {
		return nil, ErrInvalidYAML
	}
	label, err := remoteCalendarString(fields["label"])
	if err != nil {
		return nil, ErrInvalidYAML
	}
	viewName, err := remoteCalendarString(fields["view_timezone"])
	if err != nil {
		return nil, ErrInvalidYAML
	}
	view, err := calendars.ParseZone(viewName)
	if err != nil {
		return nil, ErrInvalidYAML
	}
	instance, _ := domain.ParseIntegrationInstanceID("homeward-remote-calendar-v1")
	key, _ := integrations.NewSourceKey("remote-ics", "calendar.famille")
	var resource integrations.ConditionalResource
	switch {
	case fields["url"] != nil && fields["secret_url"] == nil:
		raw, e := remoteCalendarString(fields["url"])
		if e != nil {
			return nil, ErrInvalidYAML
		}
		resource, err = integrations.PublicConditionalResource(raw)
	case fields["secret_url"] != nil && fields["url"] == nil:
		handle, e := remoteCalendarHandle(instance, fields["secret_url"])
		if e != nil {
			return nil, ErrInvalidYAML
		}
		resource, err = integrations.SecretConditionalResource(handle)
	default:
		return nil, ErrInvalidYAML
	}
	if err != nil {
		return nil, ErrInvalidYAML
	}
	auth := integrations.NoConditionalAuth()
	if value := fields["auth"]; value != nil {
		values, e := remoteCalendarMap(*value, "bearer_secret", "basic_username_secret", "basic_password_secret")
		if e != nil {
			return nil, ErrInvalidYAML
		}
		switch {
		case len(values) == 1 && values["bearer_secret"] != nil:
			token, e := remoteCalendarHandle(instance, values["bearer_secret"])
			if e != nil {
				return nil, ErrInvalidYAML
			}
			auth, err = integrations.BearerConditionalAuth(token)
		case len(values) == 2 && values["basic_username_secret"] != nil && values["basic_password_secret"] != nil:
			user, e := remoteCalendarHandle(instance, values["basic_username_secret"])
			if e != nil {
				return nil, ErrInvalidYAML
			}
			password, e := remoteCalendarHandle(instance, values["basic_password_secret"])
			if e != nil {
				return nil, ErrInvalidYAML
			}
			auth, err = integrations.BasicConditionalAuth(user, password)
		default:
			return nil, ErrInvalidYAML
		}
		if err != nil {
			return nil, ErrInvalidYAML
		}
	}
	destination := integrations.PublicConditionalDestination()
	if value := fields["network"]; value != nil {
		values, e := remoteCalendarMap(*value, "origin", "prefixes")
		if e != nil || len(values) != 2 {
			return nil, ErrInvalidYAML
		}
		origin, e := remoteCalendarString(values["origin"])
		if e != nil {
			return nil, ErrInvalidYAML
		}
		prefixesNode := values["prefixes"]
		if prefixesNode == nil || prefixesNode.Kind != yaml.SequenceNode || len(prefixesNode.Content) == 0 || len(prefixesNode.Content) > 32 {
			return nil, ErrInvalidYAML
		}
		prefixes := make([]netip.Prefix, 0, len(prefixesNode.Content))
		for _, item := range prefixesNode.Content {
			raw, e := remoteCalendarString(item)
			if e != nil {
				return nil, ErrInvalidYAML
			}
			prefix, e := netip.ParsePrefix(raw)
			if e != nil || prefix.Bits() == 0 {
				return nil, ErrInvalidYAML
			}
			prefixes = append(prefixes, prefix)
		}
		destination, err = integrations.ExplicitConditionalDestination(origin, prefixes)
		if err != nil {
			return nil, ErrInvalidYAML
		}
	}
	projection := remotecalendar.NoFloatingProjection()
	if value := fields["floating_timezone"]; value != nil {
		name, e := remoteCalendarString(value)
		if e != nil {
			return nil, ErrInvalidYAML
		}
		zone, e := calendars.ParseZone(name)
		if e != nil {
			return nil, ErrInvalidYAML
		}
		projection, err = remotecalendar.WithFloatingProjection(zone)
		if err != nil {
			return nil, ErrInvalidYAML
		}
	}
	settings, err := remotecalendar.NewSettings(instance, key, label, resource, auth, destination, projection, view)
	if err != nil {
		return nil, ErrInvalidYAML
	}
	return &settings, nil
}
func (c Config) RemoteCalendarSettings() (remotecalendar.Settings, bool) {
	if c.remoteCalendar == nil {
		return remotecalendar.Settings{}, false
	}
	return *c.remoteCalendar, true
}

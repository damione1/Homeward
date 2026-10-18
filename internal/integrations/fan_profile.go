package integrations

import (
	"github.com/damione1/Homeward/contracts"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
)

// The opt-in is a private conversion policy attached to one qualified source,
// never a capability promise or an inference from the worker executable.
type fanProfileSnapshot struct {
	binding     Binding
	alias       domain.HAAlias
	description *registry.CapabilityDescription
}

func validCasetaHAFan4V1Description(descriptions []*registry.CapabilityDescription) bool {
	if len(descriptions) != 1 || descriptions[0] == nil {
		return false
	}
	d := descriptions[0]
	if d.SchemaId != contracts.FanSchema || d.Major != 1 || d.Endpoint != "speed" || d.GetFan() == nil || d.GetFan().SetOn {
		return false
	}
	options := d.GetFan().SpeedOptions
	want := [...]string{"Off", "Low", "Medium", "MediumHigh", "High"}
	if len(options) != len(want) {
		return false
	}
	for i, option := range want {
		if options[i] != option {
			return false
		}
	}
	return true
}

func (c Candidate) WithCasetaHAFan4V1(alias domain.HAAlias) (Candidate, error) {
	name, err := alias.Value()
	if err != nil || len(name) < len("fan.") || name[:len("fan.")] != "fan." ||
		c.lightEncoding != (LightEncoding{}) || c.casetaAlias != (domain.HAAlias{}) ||
		c.casetaFanAlias != (domain.HAAlias{}) || !validCasetaHAFan4V1Description(c.descriptions) {
		return Candidate{}, ErrInvalidInput
	}
	c.casetaFanAlias = alias
	return c, nil
}

func (s *fanProfileSnapshot) sameDiscovery(binding Binding, alias domain.HAAlias, description *registry.CapabilityDescription) bool {
	return s != nil && s.binding == binding && s.alias == alias && proto.Equal(s.description, description)
}

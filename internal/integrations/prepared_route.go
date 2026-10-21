package integrations

import (
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
)

// A small closed private sum lets both prepared light and fan pass through
// the existing router and ledgers. The original wire remains the fingerprint
// authority; this token contributes only captured conversion/fence evidence.
type preparedCommandKind uint8

const (
	preparedCommandInvalid preparedCommandKind = iota
	preparedCommandLight
	preparedCommandFan
	preparedCommandDirect
)

type preparedCommandRoute struct {
	kind   preparedCommandKind
	light  *preparedLightRoute
	fan    *preparedFanRoute
	direct *preparedDirectRoute
}

func preparedLightCommand(light *preparedLightRoute) *preparedCommandRoute {
	return &preparedCommandRoute{kind: preparedCommandLight, light: light}
}

func preparedFanCommand(fan *preparedFanRoute) *preparedCommandRoute {
	return &preparedCommandRoute{kind: preparedCommandFan, fan: fan}
}

func preparedDirectCommand(direct *preparedDirectRoute) *preparedCommandRoute {
	return &preparedCommandRoute{kind: preparedCommandDirect, direct: direct}
}

func (p *preparedCommandRoute) internal() *internalOccurrence {
	if p == nil {
		return nil
	}
	switch p.kind {
	case preparedCommandLight:
		if p.light != nil {
			return p.light.capture.internal
		}
	case preparedCommandFan:
		if p.fan != nil {
			return p.fan.capture.internal
		}
	case preparedCommandDirect:
		if p.direct != nil {
			return p.direct.capture.internal
		}
	}
	return nil
}

func (p *preparedCommandRoute) authorizationTarget() authorization.CommandTarget {
	if p == nil {
		return authorization.CommandTarget{}
	}
	var entity domain.EntityID
	var schema, endpoint string
	switch p.kind {
	case preparedCommandLight:
		if p.light == nil || p.light.capture.description == nil {
			return authorization.CommandTarget{}
		}
		entity, schema, endpoint = p.light.capture.entity, p.light.capture.description.SchemaId, p.light.capture.description.Endpoint
	case preparedCommandFan:
		if p.fan == nil || p.fan.capture.description == nil {
			return authorization.CommandTarget{}
		}
		entity, schema, endpoint = p.fan.capture.entity, p.fan.capture.description.SchemaId, p.fan.capture.description.Endpoint
	case preparedCommandDirect:
		if p.direct == nil || p.direct.capture.description == nil {
			return authorization.CommandTarget{}
		}
		entity, schema, endpoint = p.direct.capture.entity, p.direct.capture.description.SchemaId, p.direct.capture.description.Endpoint
	default:
		return authorization.CommandTarget{}
	}
	slot, err := authorization.NewCapabilitySlot(schema, endpoint)
	if err != nil {
		return authorization.CommandTarget{}
	}
	target, _ := authorization.NewCommandTarget(entity, slot)
	return target
}

func (p *preparedCommandRoute) matches(router *CommandRouter, actor domain.ActorID, request *actions.CommandRequest) bool {
	if p == nil || request == nil {
		return false
	}
	switch p.kind {
	case preparedCommandLight:
		return p.light != nil && p.fan == nil && p.direct == nil && p.light.capture.router == router &&
			p.light.actor == actor && p.light.request != nil && proto.Equal(p.light.request, request)
	case preparedCommandFan:
		return p.fan != nil && p.light == nil && p.direct == nil && p.fan.capture.router == router &&
			p.fan.actor == actor && p.fan.request != nil && proto.Equal(p.fan.request, request)
	case preparedCommandDirect:
		return p.direct != nil && p.light == nil && p.fan == nil && p.direct.capture.router == router &&
			p.direct.actor == actor && p.direct.request != nil && proto.Equal(p.direct.request, request)
	default:
		return false
	}
}

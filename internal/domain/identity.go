package domain

import (
	"errors"
	"unicode/utf8"
)

var (
	ErrInvalidDeviceID              = errors.New("device ID must be nonempty UTF-8")
	ErrInvalidIntegrationInstanceID = errors.New("integration instance ID must be nonempty UTF-8")
	ErrInvalidActionID              = errors.New("action ID must be nonempty UTF-8")
	ErrInvalidCommandID             = errors.New("command ID must be nonempty UTF-8")
	ErrInvalidActorID               = errors.New("actor ID must be nonempty UTF-8")
)

// These host identities are distinct even when their opaque source text happens
// to match. Parsing proves representation only; the registry establishes ownership.
type DeviceID struct{ device string }
type IntegrationInstanceID struct{ instance string }
type ActionID struct{ action string }
type CommandID struct{ command string }
type ActorID struct{ actor string }

func parseIdentity(value string, invalid error) (string, error) {
	if value == "" || !utf8.ValidString(value) {
		return "", invalid
	}
	return value, nil
}

func ParseDeviceID(value string) (DeviceID, error) {
	parsed, err := parseIdentity(value, ErrInvalidDeviceID)
	return DeviceID{device: parsed}, err
}

func ParseIntegrationInstanceID(value string) (IntegrationInstanceID, error) {
	parsed, err := parseIdentity(value, ErrInvalidIntegrationInstanceID)
	return IntegrationInstanceID{instance: parsed}, err
}

func ParseActionID(value string) (ActionID, error) {
	parsed, err := parseIdentity(value, ErrInvalidActionID)
	return ActionID{action: parsed}, err
}

func ParseCommandID(value string) (CommandID, error) {
	if len(value) > 256 {
		return CommandID{}, ErrInvalidCommandID
	}
	parsed, err := parseIdentity(value, ErrInvalidCommandID)
	return CommandID{command: parsed}, err
}

func ParseActorID(value string) (ActorID, error) {
	if len(value) > 256 {
		return ActorID{}, ErrInvalidActorID
	}
	parsed, err := parseIdentity(value, ErrInvalidActorID)
	return ActorID{actor: parsed}, err
}

func (id DeviceID) Value() (string, error) {
	return parseIdentity(id.device, ErrInvalidDeviceID)
}

func (id IntegrationInstanceID) Value() (string, error) {
	return parseIdentity(id.instance, ErrInvalidIntegrationInstanceID)
}

func (id ActionID) Value() (string, error) {
	return parseIdentity(id.action, ErrInvalidActionID)
}

func (id CommandID) Value() (string, error) {
	parsed, err := ParseCommandID(id.command)
	return parsed.command, err
}

func (id ActorID) Value() (string, error) {
	parsed, err := ParseActorID(id.actor)
	return parsed.actor, err
}

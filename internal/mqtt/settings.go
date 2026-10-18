// Package mqtt owns the MQTT 5 wire transport. Topic interpretation and entity
// discovery are supplied by the later mapping layer.
package mqtt

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
)

var (
	ErrInvalidSettings    = errors.New("mqtt: invalid settings")
	ErrUnsupportedProfile = errors.New("mqtt: requested protocol profile is not implemented; MQTT 5 is required")
	ErrInvalidTopic       = errors.New("mqtt: invalid or unauthorized topic")
	ErrPassiveMode        = errors.New("mqtt: passive mode forbids application publications")
	ErrNotConnected       = errors.New("mqtt: pilot is not connected")
	ErrObservationGap     = errors.New("mqtt: bounded observation queue overflow; session must reconnect")
	ErrNotAnEvent         = errors.New("mqtt: restored observation cannot become an edge event")
)

type Profile uint8

const (
	ProfileUnspecified Profile = iota
	ProfileMQTT5
)

type Mode uint8

const (
	ModeUnspecified Mode = iota
	ModePassive
	ModeActive
)

// Input is the untrusted YAML representation; only ParseSettings admits it.
type Input struct {
	Profile          string                 `yaml:"profile"`
	Broker           string                 `yaml:"broker"`
	Instance         string                 `yaml:"instance"`
	Mode             string                 `yaml:"mode"`
	UsernameSecret   string                 `yaml:"username_secret"`
	PasswordSecret   string                 `yaml:"password_secret"`
	Subscriptions    []string               `yaml:"subscriptions"`
	CommandTopics    []string               `yaml:"command_topics"`
	TransitionLights []TransitionLightInput `yaml:"transition_lights"`
	Will             *WillInput             `yaml:"will"`
}

// TransitionLightInput opts one already configured source into one pinned
// converter profile. Discovery must still prove the exact physical model.
type TransitionLightInput struct {
	SourceDevice string `yaml:"source_device"`
	SourceEntity string `yaml:"source_entity"`
	Profile      string `yaml:"profile"`
}

type transitionProfile uint8

const (
	transitionUnspecified transitionProfile = iota
	transitionIkeaDriver30W
)

const ikeaDriver30WProfile = "z2m-2.14.1-converters-26.105.0-ikea-driver-30w"

type WillInput struct {
	Topic   string `yaml:"topic"`
	Payload string `yaml:"payload"`
}

type Topic struct{ value string }
type TopicFilter struct{ value string }

func ParseTopic(raw string) (Topic, error) {
	if !validTopic(raw) || strings.ContainsAny(raw, "+#") {
		return Topic{}, ErrInvalidTopic
	}
	return Topic{raw}, nil
}

func ParseTopicFilter(raw string) (TopicFilter, error) {
	if !validTopic(raw) {
		return TopicFilter{}, ErrInvalidTopic
	}
	parts := strings.Split(raw, "/")
	for i, p := range parts {
		if strings.ContainsAny(p, "+#") && p != "+" && !(p == "#" && i == len(parts)-1) {
			return TopicFilter{}, ErrInvalidTopic
		}
	}
	return TopicFilter{raw}, nil
}

func validTopic(raw string) bool {
	if raw == "" || len(raw) > 256 || !utf8.ValidString(raw) || strings.ContainsAny(raw, "\x00\r\n") || strings.HasPrefix(raw, "$share/") {
		return false
	}
	for _, p := range strings.Split(raw, "/") {
		if p == "" {
			return false
		}
	}
	return true
}

func (t Topic) Value() (string, error) {
	parsed, err := ParseTopic(t.value)
	return parsed.value, err
}
func (t TopicFilter) Value() (string, error) {
	parsed, err := ParseTopicFilter(t.value)
	return parsed.value, err
}

func (f TopicFilter) Matches(t Topic) bool {
	if _, err := f.Value(); err != nil {
		return false
	}
	value, err := t.Value()
	if err != nil {
		return false
	}
	// MQTT reserves $ topics from subscriptions beginning with a wildcard.
	if strings.HasPrefix(value, "$") && !strings.HasPrefix(f.value, "$") {
		return false
	}
	filter := strings.Split(f.value, "/")
	topic := strings.Split(value, "/")
	for i, segment := range filter {
		if segment == "#" {
			return true
		}
		if i >= len(topic) || (segment != "+" && segment != topic[i]) {
			return false
		}
	}
	return len(filter) == len(topic)
}

type Settings struct {
	profile          Profile
	mode             Mode
	address          string
	tls              bool
	instance         domain.IntegrationInstanceID
	clientID         string
	username         storage.SecretHandle
	password         storage.SecretHandle
	subscriptions    []TopicFilter
	commands         []TopicFilter
	transitionLights map[integrations.SourceKey]transitionProfile
	willTopic        Topic
	willPayload      []byte
}

func ParseSettings(raw Input) (Settings, error) {
	if raw.Profile != "5" {
		if raw.Profile == "3.1.1" {
			return Settings{}, ErrUnsupportedProfile
		}
		return Settings{}, ErrInvalidSettings
	}
	var mode Mode
	switch raw.Mode {
	case "passive":
		mode = ModePassive
	case "active":
		mode = ModeActive
	default:
		return Settings{}, ErrInvalidSettings
	}
	u, err := url.Parse(raw.Broker)
	if err != nil || u == nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return Settings{}, ErrInvalidSettings
	}
	secure := false
	switch u.Scheme {
	case "mqtt":
	case "mqtts":
		secure = true
	default:
		return Settings{}, ErrInvalidSettings
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil || host == "" || strings.ContainsAny(host, "\x00\r\n") {
		return Settings{}, ErrInvalidSettings
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return Settings{}, ErrInvalidSettings
	}
	instance, err := domain.ParseIntegrationInstanceID(raw.Instance)
	if err != nil || len(raw.Instance) > 256 {
		return Settings{}, ErrInvalidSettings
	}
	usernameName, err := storage.ParseSecretName(raw.UsernameSecret)
	if err != nil {
		return Settings{}, ErrInvalidSettings
	}
	passwordName, err := storage.ParseSecretName(raw.PasswordSecret)
	if err != nil || usernameName == passwordName {
		return Settings{}, ErrInvalidSettings
	}
	username, err := storage.NewSecretHandle(instance, usernameName)
	if err != nil {
		return Settings{}, ErrInvalidSettings
	}
	password, err := storage.NewSecretHandle(instance, passwordName)
	if err != nil {
		return Settings{}, ErrInvalidSettings
	}
	if len(raw.Subscriptions) == 0 || len(raw.Subscriptions) > 16 || len(raw.CommandTopics) > 16 {
		return Settings{}, ErrInvalidSettings
	}
	s := Settings{profile: ProfileMQTT5, mode: mode, address: u.Host, tls: secure, instance: instance, username: username, password: password}
	digest := sha256.Sum256([]byte(raw.Instance))
	s.clientID = "homeward-" + hex.EncodeToString(digest[:12])
	seen := make(map[TopicFilter]bool)
	for _, value := range raw.Subscriptions {
		filter, err := ParseTopicFilter(value)
		if err != nil || seen[filter] {
			return Settings{}, ErrInvalidSettings
		}
		seen[filter] = true
		s.subscriptions = append(s.subscriptions, filter)
	}
	for _, value := range raw.CommandTopics {
		filter, err := ParseTopicFilter(value)
		if err != nil || seen[filter] {
			return Settings{}, ErrInvalidSettings
		}
		seen[filter] = true
		s.commands = append(s.commands, filter)
	}
	if len(raw.TransitionLights) > 16 || mode != ModeActive && len(raw.TransitionLights) != 0 {
		return Settings{}, ErrInvalidSettings
	}
	s.transitionLights = make(map[integrations.SourceKey]transitionProfile, len(raw.TransitionLights))
	for _, entry := range raw.TransitionLights {
		key, err := integrations.NewSourceKey(entry.SourceDevice, entry.SourceEntity)
		if err != nil || entry.Profile != ikeaDriver30WProfile {
			return Settings{}, ErrInvalidSettings
		}
		if _, duplicate := s.transitionLights[key]; duplicate {
			return Settings{}, ErrInvalidSettings
		}
		s.transitionLights[key] = transitionIkeaDriver30W
	}
	if mode == ModePassive {
		if raw.Will != nil || len(s.commands) != 0 {
			return Settings{}, ErrInvalidSettings
		}
	} else {
		if raw.Will == nil || len(raw.Will.Payload) == 0 || len(raw.Will.Payload) > 256 {
			return Settings{}, ErrInvalidSettings
		}
		topic, err := ParseTopic(raw.Will.Topic)
		if err != nil {
			return Settings{}, ErrInvalidSettings
		}
		for _, command := range s.commands {
			if command.Matches(topic) {
				return Settings{}, ErrInvalidSettings
			}
		}
		s.willTopic, s.willPayload = topic, []byte(raw.Will.Payload)
	}
	return s, nil
}

func (s Settings) Instance() (domain.IntegrationInstanceID, error) {
	if s.profile != ProfileMQTT5 || s.mode == ModeUnspecified {
		return domain.IntegrationInstanceID{}, ErrInvalidSettings
	}
	if _, err := s.instance.Value(); err != nil {
		return domain.IntegrationInstanceID{}, ErrInvalidSettings
	}
	return s.instance, nil
}

func (s Settings) String() string {
	return fmt.Sprintf("mqtt settings [REDACTED] profile=%d mode=%d", s.profile, s.mode)
}
func (s Settings) GoString() string { return s.String() }

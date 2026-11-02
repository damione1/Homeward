package mqtt

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/damione1/Homeward/internal/domain"
)

// Z2MReadInput admits a native device read, independently of command_topics.
// The exact state route must already be subscribed and admitted by discovery.
// Values cannot be configured: /get always carries empty property values.
type Z2MReadInput struct {
	StateTopic string   `yaml:"state_topic"`
	Properties []string `yaml:"properties"`
}

type z2mRead struct {
	state      Topic
	topic      Topic
	properties []string
	payload    []byte
}

func (s *Settings) admitZ2MReads(inputs []Z2MReadInput) error {
	if len(inputs) > 16 || len(inputs) != 0 && s.mode != ModeActive {
		return ErrInvalidSettings
	}
	seen := make(map[Topic]bool, len(inputs))
	for _, input := range inputs {
		state, err := ParseTopic(input.StateTopic)
		if err != nil || seen[state] || len(input.Properties) == 0 || len(input.Properties) > 3 {
			return ErrInvalidSettings
		}
		// Exclude the bridge API and ambiguous get/set routes. Friendly names
		// may contain spaces and slashes; neither is normalized or guessed.
		parts := strings.Split(input.StateTopic, "/")
		if len(parts) < 2 {
			return ErrInvalidSettings
		}
		for _, part := range parts {
			if part == "" || part == "bridge" || part == "get" || part == "set" {
				return ErrInvalidSettings
			}
		}
		subscribed := false
		for _, filter := range s.subscriptions {
			subscribed = subscribed || filter.Matches(state)
		}
		if !subscribed {
			return ErrInvalidSettings
		}
		fields := make(map[string]string, len(input.Properties))
		for _, property := range input.Properties {
			switch property {
			case "state", "brightness", "color_temp":
			default:
				return ErrInvalidSettings
			}
			if _, duplicate := fields[property]; duplicate {
				return ErrInvalidSettings
			}
			fields[property] = ""
		}
		if _, hasState := fields["state"]; !hasState {
			return ErrInvalidSettings
		}
		topic, err := ParseTopic(input.StateTopic + "/get")
		if err != nil {
			return ErrInvalidSettings
		}
		payload, err := json.Marshal(fields)
		if err != nil {
			return ErrInvalidSettings
		}
		seen[state] = true
		s.reads = append(s.reads, z2mRead{state: state, topic: topic, properties: append([]string(nil), input.Properties...), payload: payload})
	}
	return nil
}

type z2mReadDiscovery interface {
	z2mReadReady(z2mRead) bool
}

func (m *DiscoveryMapper) z2mReadReady(read z2mRead) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, runtime := range m.states[read.state] {
		profile := runtime.profile
		if profile.kind != componentSwitch && profile.kind != componentLight || runtime.effectiveAvailability() != domain.AvailabilityAvailable {
			continue
		}
		supported := true
		for _, property := range read.properties {
			if property == "brightness" && !profile.brightness || property == "color_temp" && !profile.colorTemp {
				supported = false
			}
		}
		if supported {
			return true
		}
	}
	return false
}

// runZ2MReads starts only after SUBACK and waits for the exact configured state
// route and availability to be admitted. A response arriving before discovery
// or availability would otherwise become a stale saved value. Reads do not publish state or
// command confirmation. Each route is attempted once per supervised session.
//
// Pinned upstream: Zigbee2MQTT 2.14.1 Publish.onMQTTMessage invokes convertGet,
// not convertSet. zigbee-herdsman-converters 26.105.0 on_off/light converters
// read genOnOff.onOff, genLevelCtrl.currentLevel and color temperature attrs.
// https://github.com/Koenkk/zigbee2mqtt/blob/2.14.1/lib/extension/publish.ts
// https://github.com/Koenkk/zigbee-herdsman-converters/blob/v26.105.0/src/converters/toZigbee.ts
func (p *Pilot) runZ2MReads(ctx context.Context, wake <-chan struct{}) error {
	discovery := p.handler.(z2mReadDiscovery) // NewPilot admits this capability.
	pending := append([]z2mRead(nil), p.settings.reads...)
	for len(pending) != 0 {
		for i := 0; i < len(pending); {
			read := pending[i]
			if !discovery.z2mReadReady(read) {
				i++
				continue
			}
			pending = append(pending[:i], pending[i+1:]...)
			if err := p.writePublication(ctx, read.topic, read.payload, nil); err != nil {
				return err
			}
		}
		if len(pending) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-clientDone(p):
			return nil
		case <-wake:
		}
	}
	return nil
}

package mqtt

import "github.com/damione1/Homeward/internal/integrations"

// CommandFeedbackInput is an operator declaration about one exact configured
// peer. Ordinary HA discovery does not promise MQTT5 response correlation.
type CommandFeedbackInput struct {
	SourceDevice string `yaml:"source_device"`
	SourceEntity string `yaml:"source_entity"`
	Mode         string `yaml:"mode"`
}

type commandFeedback uint8

const (
	feedbackSentUnverified commandFeedback = iota
	feedbackCorrelatedState
)

func (s *Settings) admitCommandFeedback(input []CommandFeedbackInput) error {
	if len(input) > 128 || s.mode != ModeActive && len(input) != 0 {
		return ErrInvalidSettings
	}
	s.commandFeedback = make(map[integrations.SourceKey]commandFeedback, len(input))
	for _, entry := range input {
		key, err := integrations.NewSourceKey(entry.SourceDevice, entry.SourceEntity)
		if err != nil || entry.Mode != "mqtt5_correlated_state" {
			return ErrInvalidSettings
		}
		if _, duplicate := s.commandFeedback[key]; duplicate {
			return ErrInvalidSettings
		}
		s.commandFeedback[key] = feedbackCorrelatedState
	}
	return nil
}

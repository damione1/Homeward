// Package nha006 runs a fixed synthetic fixture probe against pinned Gonja.
// It is a test harness; it does not install a template runtime in the server.
package nha006

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/nikolalohinski/gonja/v2"
	"github.com/nikolalohinski/gonja/v2/config"
	"github.com/nikolalohinski/gonja/v2/exec"
	"github.com/nikolalohinski/gonja/v2/loaders"
)

type diagnosticMode string

const (
	diagnosticLiteral diagnosticMode = "literal"
	diagnosticClock   diagnosticMode = "clock"
)

func (m *diagnosticMode) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	switch diagnosticMode(value) {
	case diagnosticLiteral, diagnosticClock:
		*m = diagnosticMode(value)
		return nil
	default:
		return fmt.Errorf("unknown diagnostic mode %q", value)
	}
}

type diagnosticInput struct {
	Mode     diagnosticMode `json:"mode"`
	Template string         `json:"template"`
}

type caseInput struct {
	ID          string            `json:"id"`
	Template    string            `json:"template"`
	Diagnostics []diagnosticInput `json:"diagnostics,omitempty"`
}

type syntheticState struct {
	EntityID   string          `json:"entity_id"`
	State      string          `json:"state"`
	Attributes json.RawMessage `json:"attributes"`
}

type fixtureSet struct {
	ClockUTC string           `json:"clock_utc"`
	Timezone string           `json:"timezone"`
	States   []syntheticState `json:"states"`
	Cases    []caseInput      `json:"cases"`
}

type outcomeKind string

const (
	outcomeOK          outcomeKind = "ok"
	outcomeParseError  outcomeKind = "parse_error"
	outcomeRenderError outcomeKind = "render_error"
)

type outcome struct {
	Kind  outcomeKind `json:"kind"`
	Text  string      `json:"text"`
	Error string      `json:"error,omitempty"`
}

type diagnosticResult struct {
	Mode     diagnosticMode `json:"mode"`
	Template string         `json:"template"`
	Outcome  outcome        `json:"outcome"`
}

type caseResult struct {
	ID          string             `json:"id"`
	Raw         outcome            `json:"raw"`
	Diagnostics []diagnosticResult `json:"diagnostics,omitempty"`
}

type probeResults struct {
	Cases []caseResult `json:"cases"`
}

// RunJSON evaluates each fixture with Gonja's raw environment. Diagnostics
// inject only the named synthetic value needed to isolate a specific behavior.
func RunJSON(input []byte) ([]byte, error) {
	var fixtures fixtureSet
	decoder := json.NewDecoder(strings.NewReader(string(input)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixtures); err != nil {
		return nil, fmt.Errorf("decode fixtures: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("fixture input has trailing data")
	}
	clock, err := time.Parse(time.RFC3339, fixtures.ClockUTC)
	if err != nil {
		return nil, fmt.Errorf("invalid synthetic clock: %w", err)
	}
	location, err := time.LoadLocation(fixtures.Timezone)
	if err != nil {
		return nil, fmt.Errorf("invalid fixture timezone: %w", err)
	}
	if len(fixtures.Cases) != 10 {
		return nil, fmt.Errorf("expected 10 cases, got %d", len(fixtures.Cases))
	}
	seen := make(map[string]struct{}, len(fixtures.Cases))
	results := probeResults{Cases: make([]caseResult, 0, len(fixtures.Cases))}
	for _, fixture := range fixtures.Cases {
		if !strings.HasPrefix(fixture.ID, "nha006-") || fixture.Template == "" {
			return nil, fmt.Errorf("invalid case id or template %q", fixture.ID)
		}
		if _, exists := seen[fixture.ID]; exists {
			return nil, fmt.Errorf("duplicate case id %q", fixture.ID)
		}
		seen[fixture.ID] = struct{}{}
		raw, err := render(fixture.Template, exec.EmptyContext())
		if err != nil {
			return nil, fmt.Errorf("probe setup for %q: %w", fixture.ID, err)
		}
		result := caseResult{ID: fixture.ID, Raw: raw}
		for _, diagnostic := range fixture.Diagnostics {
			if diagnostic.Template == "" {
				return nil, fmt.Errorf("empty diagnostic template in %q", fixture.ID)
			}
			context := exec.EmptyContext()
			switch diagnostic.Mode {
			case diagnosticLiteral:
			case diagnosticClock:
				// The only injected callable is the fixture clock. The date method
				// remains Gonja's method dispatch, not an emulated HA method.
				context = exec.NewContext(map[string]any{
					"now": func() time.Time { return clock.In(location) },
				})
			default:
				return nil, fmt.Errorf("invalid diagnostic mode %q", diagnostic.Mode)
			}
			measured, err := render(diagnostic.Template, context)
			if err != nil {
				return nil, fmt.Errorf("diagnostic setup for %q: %w", fixture.ID, err)
			}
			result.Diagnostics = append(result.Diagnostics, diagnosticResult{
				Mode: diagnostic.Mode, Template: diagnostic.Template, Outcome: measured,
			})
		}
		results.Cases = append(results.Cases, result)
	}
	return json.Marshal(results)
}

func render(source string, context *exec.Context) (outcome, error) {
	if context == nil {
		return outcome{}, errors.New("missing fixture context")
	}
	// MemoryLoader prevents fixture includes from reading the local filesystem.
	loader, err := loaders.NewMemoryLoader(map[string]string{"/fixture": source})
	if err != nil {
		return outcome{}, err
	}
	template, err := exec.NewTemplate("/fixture", config.New(), loader, gonja.DefaultEnvironment)
	if err != nil {
		return outcome{Kind: outcomeParseError, Error: err.Error()}, nil
	}
	text, err := template.ExecuteToString(context)
	if err != nil {
		return outcome{Kind: outcomeRenderError, Error: err.Error()}, nil
	}
	return outcome{Kind: outcomeOK, Text: text}, nil
}

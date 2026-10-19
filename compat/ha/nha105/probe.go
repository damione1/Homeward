// Package nha105 qualifies pure solar calculations with public synthetic data.
// It does not configure or start the Homeward server or any integration.
package nha105

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"strings"
	"time"

	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/solar"
)

const profile = "ha-2026.10/core-1"

type fixtureCase struct {
	ID               string   `json:"id"`
	Latitude         *float64 `json:"latitude"`
	Longitude        *float64 `json:"longitude"`
	ElevationMeters  *float64 `json:"elevation_m"`
	Timezone         string   `json:"timezone"`
	NowUTC           string   `json:"now_utc"`
	Date             string   `json:"date"`
	AfterEvent       string   `json:"after_event,omitempty"`
	AfterShiftMicros int64    `json:"after_shift_us,omitempty"`
}

type fixtureSet struct {
	Profile string        `json:"profile"`
	Cases   []fixtureCase `json:"cases"`
}

type occurrenceResult struct {
	Kind    string `json:"kind"`
	Instant string `json:"instant_utc,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

type eventResult struct {
	Event string           `json:"event"`
	Date  occurrenceResult `json:"date"`
	Next  occurrenceResult `json:"next"`
}

type sunResult struct {
	State           string             `json:"state"`
	Elevation       float64            `json:"elevation"`
	Azimuth         float64            `json:"azimuth"`
	Rising          bool               `json:"rising"`
	Phase           string             `json:"phase"`
	NextChange      string             `json:"next_change"`
	NextPosition    string             `json:"next_position,omitempty"`
	Next            []occurrenceResult `json:"next"`
	DateEvaluations int                `json:"date_evaluations"`
}

type caseResult struct {
	ID        string        `json:"id"`
	AfterUTC  string        `json:"after_utc"`
	Date      string        `json:"date"`
	AnchorUTC string        `json:"anchor_utc,omitempty"`
	Events    []eventResult `json:"events"`
	Sun       sunResult     `json:"sun"`
}

type probeResult struct {
	GoVersion      string       `json:"go_version"`
	Profile        string       `json:"profile"`
	AstralVersion  string       `json:"astral_version"`
	AstralCommit   string       `json:"astral_commit"`
	TimezoneDataID string       `json:"timezone_data_id"`
	Cases          []caseResult `json:"cases"`
}

func encodeOccurrence(value domain.SolarOccurrence) (occurrenceResult, error) {
	instant, reason, kind, err := value.Components()
	if err != nil {
		return occurrenceResult{}, err
	}
	switch kind {
	case domain.SolarOccurrencePresent:
		return occurrenceResult{Kind: "present", Instant: instant.Format(time.RFC3339Nano)}, nil
	case domain.SolarOccurrenceAbsent:
		switch reason {
		case domain.SolarNoTransitOnDate:
			return occurrenceResult{Kind: "absent", Reason: "no_transit_on_date"}, nil
		case domain.SolarNoEventWithinSearchHorizon:
			return occurrenceResult{Kind: "absent", Reason: "no_event_within_search_horizon"}, nil
		case domain.SolarAbsenceUnspecified:
			return occurrenceResult{}, errors.New("probe: invalid absence")
		default:
			return occurrenceResult{}, errors.New("probe: unknown absence")
		}
	case domain.SolarOccurrenceUnspecified:
		return occurrenceResult{}, errors.New("probe: invalid occurrence")
	default:
		return occurrenceResult{}, errors.New("probe: unknown occurrence")
	}
}

func calculateCase(ctx context.Context, input fixtureCase) (caseResult, error) {
	settings, err := solar.ParseSettings(solar.Input{Latitude: input.Latitude, Longitude: input.Longitude, ElevationMeters: input.ElevationMeters, Timezone: input.Timezone})
	if err != nil {
		return caseResult{}, err
	}
	calculator, err := solar.NewCalculator(settings)
	if err != nil {
		return caseResult{}, err
	}
	now, err := time.Parse(time.RFC3339Nano, input.NowUTC)
	if err != nil || input.AfterShiftMicros < -1 || input.AfterShiftMicros > 1 {
		return caseResult{}, errors.New("probe: invalid synthetic instant")
	}
	date, err := calendars.ParseDate(input.Date)
	if err != nil {
		return caseResult{}, errors.New("probe: invalid synthetic civil date")
	}
	result := caseResult{ID: input.ID, Date: date.String()}
	if input.AfterEvent != "" {
		event, err := domain.ParseSolarEventKind(input.AfterEvent)
		if err != nil {
			return caseResult{}, err
		}
		anchor, err := calculator.NextEvent(ctx, event, now)
		if err != nil {
			return caseResult{}, err
		}
		instant, _, kind, err := anchor.Components()
		if err != nil || kind != domain.SolarOccurrencePresent {
			return caseResult{}, errors.New("probe: equality fixture has no anchor")
		}
		result.AnchorUTC = instant.Format(time.RFC3339Nano)
		now = instant.Add(time.Duration(input.AfterShiftMicros) * time.Microsecond)
	} else if input.AfterShiftMicros != 0 {
		return caseResult{}, errors.New("probe: shift requires an event anchor")
	}
	result.AfterUTC = now.UTC().Format(time.RFC3339Nano)
	for _, event := range domain.SolarEventKinds() {
		name, _ := event.Name()
		onDate, err := calculator.EventAtDate(ctx, event, date)
		if err != nil {
			return caseResult{}, err
		}
		next, err := calculator.NextEvent(ctx, event, now)
		if err != nil {
			return caseResult{}, err
		}
		encodedDate, err := encodeOccurrence(onDate)
		if err != nil {
			return caseResult{}, err
		}
		encodedNext, err := encodeOccurrence(next)
		if err != nil {
			return caseResult{}, err
		}
		result.Events = append(result.Events, eventResult{Event: name, Date: encodedDate, Next: encodedNext})
	}
	computed, err := calculator.Compute(ctx, now)
	if err != nil {
		return caseResult{}, err
	}
	value, _, phase, nextChange, err := computed.Components()
	if err != nil {
		return caseResult{}, err
	}
	parts, err := value.Components()
	if err != nil {
		return caseResult{}, err
	}
	state := ""
	switch parts.Horizon {
	case domain.SolarAboveHorizon:
		state = "above_horizon"
	case domain.SolarBelowHorizon:
		state = "below_horizon"
	case domain.SolarHorizonUnspecified:
		return caseResult{}, errors.New("probe: missing horizon state")
	default:
		return caseResult{}, errors.New("probe: unknown horizon state")
	}
	phaseName, err := phase.Name()
	if err != nil {
		return caseResult{}, err
	}
	result.Sun = sunResult{State: state, Elevation: parts.ElevationDegrees, Azimuth: parts.AzimuthDegrees, Rising: parts.Rising, Phase: phaseName, NextChange: nextChange.Format(time.RFC3339Nano)}
	if next, present, err := computed.NextPosition(); err != nil {
		return caseResult{}, err
	} else if present {
		result.Sun.NextPosition = next.Format(time.RFC3339Nano)
	}
	result.Sun.DateEvaluations, err = computed.DateEvaluations()
	if err != nil {
		return caseResult{}, err
	}
	for _, next := range parts.Next {
		encoded, err := encodeOccurrence(next)
		if err != nil {
			return caseResult{}, err
		}
		result.Sun.Next = append(result.Sun.Next, encoded)
	}
	return result, nil
}

// RunJSON evaluates only bounded fixture input and returns computed values.
// Input coordinates are deliberately absent from the output representation.
func RunJSON(ctx context.Context, input []byte) ([]byte, error) {
	if len(input) == 0 || len(input) > 64<<10 {
		return nil, errors.New("probe: fixture size outside bounds")
	}
	var fixtures fixtureSet
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixtures); err != nil {
		return nil, errors.New("probe: invalid fixture JSON")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("probe: trailing fixture input")
	}
	if fixtures.Profile != profile || len(fixtures.Cases) < 1 || len(fixtures.Cases) > 64 {
		return nil, errors.New("probe: unsupported profile or fixture count")
	}
	result := probeResult{GoVersion: runtime.Version(), Profile: profile, AstralVersion: solar.AstralVersion, AstralCommit: solar.AstralCommit, TimezoneDataID: calendars.TimezoneDataID}
	seen := make(map[string]bool, len(fixtures.Cases))
	for _, fixture := range fixtures.Cases {
		if len(fixture.ID) > 128 || !strings.HasPrefix(fixture.ID, "nha105-") || seen[fixture.ID] {
			return nil, errors.New("probe: invalid or duplicate fixture identity")
		}
		seen[fixture.ID] = true
		outcome, err := calculateCase(ctx, fixture)
		if err != nil {
			return nil, err
		}
		result.Cases = append(result.Cases, outcome)
	}
	return json.Marshal(result)
}

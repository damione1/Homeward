// Package nha047 owns the bounded construction qualification used by migration
// reports. It never starts Homeward or connects to a household integration.
package nha047

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/damione1/Homeward/compat/ha/nha045"
)

const (
	Schema          = "nha047/v1"
	HAVersion       = "2026.10.1"
	GoVersion       = "go1.27.2"
	Image           = "ghcr.io/home-assistant/home-assistant@sha256:17100609af8bb79bf5390f5d514b9aaeb00d184ce758f37769cd80c450851db7"
	MaxInputBytes   = 4 << 20
	MaxCaptureBytes = 8 << 20
	MaxFacts        = 1024
	MaxCases        = 128
)

var (
	ErrInvalidInput   = errors.New("nha047: invalid qualification input")
	ErrInvalidCapture = errors.New("nha047: invalid capture")
	ErrDivergence     = errors.New("nha047: semantic divergence")
	ErrUnavailable    = errors.New("nha047: qualification unavailable")
)

// Construction is the single closed vocabulary shared with the report and
// exported to the HA worker by Manifest. Values identify semantics, not tests.
type Construction uint8

const (
	ConstructionInvalid Construction = iota
	AutomationSingle
	TriggerTime
	TriggerSunset
	ConditionState
	ClimateSetpoint
	SwitchTurnOn
	SceneLightOff
	SceneLightBrightness
	SceneLightCT
	SceneSwitch
	SceneFan
	SceneClimateUnsupported
	SceneMetadata
	LegacyServiceAlias
	ExcludedReference
	BlueprintSourceMissing
)

var constructionNames = [...]string{
	"", "automation_single", "trigger_time", "trigger_sunset", "condition_state",
	"climate_setpoint", "switch_turn_on", "scene_light_off", "scene_light_brightness",
	"scene_light_ct", "scene_switch", "scene_fan", "scene_climate_unsupported",
	"scene_metadata", "legacy_service_alias", "excluded_reference", "blueprint_source_missing",
}

func (c Construction) String() string {
	if c <= ConstructionInvalid || int(c) >= len(constructionNames) {
		return "invalid"
	}
	return constructionNames[c]
}

func parseConstruction(text string) (Construction, error) {
	for c := AutomationSingle; c <= BlueprintSourceMissing; c++ {
		if c.String() == text {
			return c, nil
		}
	}
	return ConstructionInvalid, ErrInvalidInput
}

// Feature retains the concrete parameters to which a construction proof
// applies. In particular a proof of brightness 128 does not cover brightness 3.
type Feature struct {
	construction Construction
	value        float64
	secondary    float64
}

func NewFeature(c Construction, value, secondary float64) (Feature, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || math.IsNaN(secondary) || math.IsInf(secondary, 0) {
		return Feature{}, ErrInvalidInput
	}
	valid := false
	switch c {
	case AutomationSingle, ConditionState, SwitchTurnOn, SceneLightOff, SceneMetadata, LegacyServiceAlias, ExcludedReference, BlueprintSourceMissing, SceneClimateUnsupported:
		valid = value == 0 && secondary == 0
	case TriggerTime:
		valid = value >= 0 && value < 86400 && math.Trunc(value) == value && secondary == 0
	case TriggerSunset:
		valid = value == -3600 && secondary == 0
	case ClimateSetpoint:
		valid = (value == 15 || value == 18 || value == 20) && secondary == 0
	case SceneLightBrightness:
		valid = value >= 0 && value <= 255 && math.Trunc(value) == value && secondary == 0
	case SceneLightCT:
		valid = value > 0 && value <= 255 && math.Trunc(value) == value && secondary > 0 && secondary <= 1000000 && math.Trunc(secondary) == secondary
	case SceneSwitch:
		valid = (value == 0 || value == 1) && secondary == 0
	case SceneFan:
		valid = (value == 0 || value == 75) && secondary == 0
	case ConstructionInvalid:
		valid = false
	default:
		valid = false
	}
	if !valid {
		return Feature{}, ErrInvalidInput
	}
	return Feature{c, value, secondary}, nil
}

func (f Feature) Construction() Construction { return f.construction }
func (f Feature) Value() float64             { return f.value }
func (f Feature) Secondary() float64         { return f.secondary }
func (f Feature) Valid() bool {
	_, err := NewFeature(f.construction, f.value, f.secondary)
	return err == nil
}

type FeatureWire struct {
	Construction string  `json:"construction"`
	Value        float64 `json:"value"`
	Secondary    float64 `json:"secondary"`
}

func (w *FeatureWire) UnmarshalJSON(data []byte) error {
	type fields FeatureWire
	var value fields
	if err := strictObjectJSON(data, &value, MaxInputBytes, "construction", "value", "secondary"); err != nil {
		return err
	}
	*w = FeatureWire(value)
	return nil
}

func (f Feature) wire() FeatureWire {
	return FeatureWire{f.construction.String(), f.value, f.secondary}
}

func featureFromWire(w FeatureWire) (Feature, error) {
	c, err := parseConstruction(w.Construction)
	if err != nil {
		return Feature{}, err
	}
	return NewFeature(c, w.Value, w.Secondary)
}

type EvidenceKind uint8

const (
	EvidenceInvalid EvidenceKind = iota
	DifferentialEquivalent
	DifferentialDivergence
	DifferentialRefusal
)

func (k EvidenceKind) String() string {
	switch k {
	case DifferentialEquivalent:
		return "differential_equivalent"
	case DifferentialDivergence:
		return "differential_divergence"
	case DifferentialRefusal:
		return "differential_refusal"
	case EvidenceInvalid:
		return "invalid"
	default:
		return "invalid"
	}
}

// ComparisonRole states which loaded source belongs to each runtime. It is
// derived from a registered case, never asserted by a capture producer.
type ComparisonRole uint8

const (
	RoleInvalid ComparisonRole = iota
	SameSource
	OriginalToCandidate
)

func (r ComparisonRole) String() string {
	switch r {
	case SameSource:
		return "same_source"
	case OriginalToCandidate:
		return "original_to_candidate"
	case RoleInvalid:
		return "invalid"
	default:
		return "invalid"
	}
}

type Digest [sha256.Size]byte

func Hash(data []byte) Digest   { return Digest(sha256.Sum256(data)) }
func (d Digest) String() string { return hex.EncodeToString(d[:]) }
func (d Digest) Valid() bool    { return d != (Digest{}) }
func ParseDigest(value string) (Digest, error) {
	var result Digest
	b, err := hex.DecodeString(value)
	if err != nil || len(b) != len(result) || hex.EncodeToString(b) != value {
		return result, ErrInvalidInput
	}
	copy(result[:], b)
	if !result.Valid() {
		return Digest{}, ErrInvalidInput
	}
	return result, nil
}

func strictJSON(data []byte, target any, maximum int) error {
	if len(data) == 0 || len(data) > maximum {
		return ErrInvalidInput
	}
	// Reject duplicate members before decoding the typed boundary.
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	depth := 0
	var value func() error
	value = func() error {
		depth++
		defer func() { depth-- }()
		if depth > 64 {
			return ErrInvalidInput
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			if token == nil {
				return ErrInvalidInput
			}
			return nil
		}
		switch delim {
		case '{':
			keys := map[string]bool{}
			for d.More() {
				token, err := d.Token()
				if err != nil {
					return err
				}
				key, ok := token.(string)
				if !ok || keys[key] {
					return ErrInvalidInput
				}
				keys[key] = true
				if err := value(); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := value(); err != nil {
					return err
				}
			}
		default:
			return ErrInvalidInput
		}
		_, err = d.Token()
		return err
	}
	if err := value(); err != nil {
		return ErrInvalidInput
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrInvalidInput
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return ErrInvalidInput
	}
	return nil
}

// Required fields include meaningful zero, false and empty values. Admission
// must not reconstruct an omitted wire member from a Go field's zero value.
// Concrete wire types declare their own shape; the typed decoder still rejects
// unknown fields, and strictJSON rejects duplicate members and nulls.
func strictObjectJSON(data []byte, target any, maximum int, required ...string) error {
	if len(data) == 0 || len(data) > maximum {
		return ErrInvalidInput
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || len(fields) != len(required) {
		return ErrInvalidInput
	}
	for _, name := range required {
		if _, present := fields[name]; !present {
			return ErrInvalidInput
		}
	}
	return strictJSON(data, target, maximum)
}

func validID(text string) bool {
	if len(text) == 0 || len(text) > 128 {
		return false
	}
	for _, c := range text {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.ContainsRune("._-/", c)) {
			return false
		}
	}
	return true
}

func Manifest() ([]byte, error) {
	type variant struct {
		Value uint8  `json:"value"`
		Name  string `json:"name"`
	}
	var variants []variant
	for c := AutomationSingle; c <= BlueprintSourceMissing; c++ {
		variants = append(variants, variant{uint8(c), c.String()})
	}
	cases, err := Corpus()
	if err != nil {
		return nil, err
	}
	type entry struct {
		ID     string `json:"id"`
		SHA256 string `json:"sha256"`
	}
	entries := make([]entry, 0, len(cases))
	for _, c := range cases {
		entries = append(entries, entry{c.ID(), c.Digest().String()})
	}
	sources, err := qualificationHASources()
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Schema        string          `json:"schema"`
		Image         string          `json:"image"`
		HAVersion     string          `json:"ha_version"`
		GoVersion     string          `json:"go_version"`
		Constructions []variant       `json:"constructions"`
		Cases         []entry         `json:"cases"`
		Demands       []demandVariant `json:"demands"`
		Python        string          `json:"python"`
		Jinja         string          `json:"jinja"`
		Sources       []SourceHash    `json:"sources"`
	}{Schema, Image, HAVersion, GoVersion, variants, entries, demandVariants(), nha045.HAPython, nha045.HAJinja, sources})
}

func inputError(kind string) error { return fmt.Errorf("%w: %s", ErrInvalidInput, kind) }

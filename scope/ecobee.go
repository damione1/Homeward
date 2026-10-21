package scope

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
)

var ErrInvalidEcobeeAmendment = errors.New("invalid Ecobee scope amendment")

const BaselineManifestSHA256 = "d2f15358239b29797906a31f9318d6ad7a8b81be76f9d39421b0aa6d22f52cfa"
const BaselineIDsSHA256 = "4cdcd750642c903a58195e404717042b9c7e2fa045799132f38f73291e069d31"
const ecobeeAmendmentSHA256 = "2780049835aa84e16ff6e17ee01c0dafdc6f0f838ff61273505f738886c65f1c"

// DocumentarySource locates an immutable input; it grants no runtime capability.
type DocumentarySource struct {
	File    string `json:"file"`
	Section string `json:"section"`
	Line    int    `json:"line"`
	SHA256  string `json:"sha256"`
}

type EcobeeQualification struct {
	Ticket        string `json:"ticket"`
	Commit        string `json:"commit"`
	Profile       string `json:"profile"`
	ProfileSHA256 string `json:"profile_sha256"`
}

// EcobeeField describes the qualified HAP harness, not the current action API.
// QualifiedControl must never be interpreted as an executable SetClimate field.
type EcobeeField struct {
	Name             string `json:"name"`
	HAPType          string `json:"hap_type"`
	Format           string `json:"format"`
	Unit             string `json:"unit"`
	Required         bool   `json:"required"`
	QualifiedControl bool   `json:"qualified_control"`
	Limitation       string `json:"limitation"`
}

type EcobeeConsumer struct {
	EntityID             string            `json:"entity_id"`
	Via                  []string          `json:"via"`
	DisabledActionInPath bool              `json:"disabled_action_in_path"`
	Source               DocumentarySource `json:"source"`
}

type EcobeeTechnicalConsumer struct {
	EntityID string            `json:"entity_id"`
	Via      []string          `json:"via"`
	Ticket   string            `json:"ticket"`
	Source   DocumentarySource `json:"source"`
}

type scopeEntity struct {
	EntityID string `json:"entity_id"`
	Source   string `json:"source"`
}

type rawEcobeeAmendment struct {
	Schema   string `json:"schema"`
	Revision string `json:"revision"`
	Baseline struct {
		Revision       string `json:"revision"`
		ManifestSHA256 string `json:"manifest_sha256"`
		IDsSHA256      string `json:"ids_sha256"`
	} `json:"baseline"`
	Decision      DocumentarySource   `json:"decision"`
	Qualification EcobeeQualification `json:"qualification"`
	Additions     []scopeEntity       `json:"additions"`
	Removals      []string            `json:"removals"`
	Aliases       []string            `json:"aliases"`
	Profile       struct {
		ThermostatService string        `json:"thermostat_service"`
		LinkedFanServices []string      `json:"linked_fan_services"`
		Fields            []EcobeeField `json:"fields"`
		Limits            []string      `json:"limits"`
	} `json:"profile"`
	Consumers          []EcobeeConsumer          `json:"consumers"`
	TechnicalConsumers []EcobeeTechnicalConsumer `json:"technical_consumers"`
}

// EcobeeAmendment has a private, admitted representation. Getters return copies;
// mutating documentary output cannot change registry admission or its lineage.
type EcobeeAmendment struct {
	raw   rawEcobeeAmendment
	valid bool
}

func (a EcobeeAmendment) Valid() bool { return a.valid }
func (a EcobeeAmendment) Digest() ([sha256.Size]byte, bool) {
	if !a.valid {
		return [sha256.Size]byte{}, false
	}
	return sha256.Sum256(ecobeeMVPV1), true
}
func (a EcobeeAmendment) Decision() DocumentarySource        { return a.raw.Decision }
func (a EcobeeAmendment) Qualification() EcobeeQualification { return a.raw.Qualification }
func (a EcobeeAmendment) ThermostatService() (string, bool) {
	return a.raw.Profile.ThermostatService, a.valid
}
func (a EcobeeAmendment) LinkedFanServices() []string {
	return append([]string(nil), a.raw.Profile.LinkedFanServices...)
}
func (a EcobeeAmendment) Fields() []EcobeeField {
	return append([]EcobeeField(nil), a.raw.Profile.Fields...)
}
func (a EcobeeAmendment) Limits() []string { return append([]string(nil), a.raw.Profile.Limits...) }
func (a EcobeeAmendment) AddedIDs() []string {
	ids := make([]string, 0, len(a.raw.Additions))
	for _, item := range a.raw.Additions {
		ids = append(ids, item.EntityID)
	}
	return ids
}
func (a EcobeeAmendment) SourceFor(id string) (string, bool) {
	if !a.valid {
		return "", false
	}
	for _, item := range a.raw.Additions {
		if item.EntityID == id {
			return item.Source, true
		}
	}
	return "", false
}
func (a EcobeeAmendment) RemovedIDs() []string        { return append([]string{}, a.raw.Removals...) }
func (a EcobeeAmendment) AdditionalAliases() []string { return append([]string{}, a.raw.Aliases...) }
func (a EcobeeAmendment) Consumers() []EcobeeConsumer {
	rows := append([]EcobeeConsumer(nil), a.raw.Consumers...)
	for i := range rows {
		rows[i].Via = append([]string{}, rows[i].Via...)
	}
	return rows
}
func (a EcobeeAmendment) TechnicalConsumers() []EcobeeTechnicalConsumer {
	rows := append([]EcobeeTechnicalConsumer(nil), a.raw.TechnicalConsumers...)
	for i := range rows {
		rows[i].Via = append([]string{}, rows[i].Via...)
	}
	return rows
}

func LoadEcobeeMVPV1() (EcobeeAmendment, error) {
	return ParseEcobeeAmendment(EcobeeMVPV1Bytes(), ManifestV1())
}

// ParseEcobeeAmendment admits this published revision's exact semantics, rather
// than treating an arbitrary well-formed diff as authorization. The pinned asset
// is the sole profile/consumer authority; member order and whitespace may differ.
// Baseline v1 parsing is intentionally independent and is not relaxed here.
func ParseEcobeeAmendment(data, baseline []byte) (EcobeeAmendment, error) {
	if hexDigest(sha256.Sum256(baseline)) != BaselineManifestSHA256 || hexDigest(sha256.Sum256(ecobeeMVPV1)) != ecobeeAmendmentSHA256 {
		return EcobeeAmendment{}, ErrInvalidEcobeeAmendment
	}
	want, err := strictEcobeeJSON(ecobeeMVPV1)
	if err != nil {
		return EcobeeAmendment{}, ErrInvalidEcobeeAmendment
	}
	got, err := strictEcobeeJSON(data)
	if err != nil || !reflect.DeepEqual(got, want) {
		return EcobeeAmendment{}, ErrInvalidEcobeeAmendment
	}
	var raw rawEcobeeAmendment
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&raw); err != nil || raw.Baseline.ManifestSHA256 != BaselineManifestSHA256 || raw.Baseline.IDsSHA256 != BaselineIDsSHA256 {
		return EcobeeAmendment{}, ErrInvalidEcobeeAmendment
	}
	return EcobeeAmendment{raw: raw, valid: true}, nil
}

func hexDigest(d [sha256.Size]byte) string { return hex.EncodeToString(d[:]) }

// The structural pass rejects duplicates, missing/extra members, null, trailing
// values, wrong types and excessive nesting before constructing admitted values.
func strictEcobeeJSON(data []byte) (any, error) {
	if len(data) == 0 || len(data) > 64<<10 {
		return nil, ErrInvalidEcobeeAmendment
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var consume func(int) (any, error)
	consume = func(depth int) (any, error) {
		if depth > 16 {
			return nil, ErrInvalidEcobeeAmendment
		}
		token, err := d.Token()
		if err != nil || token == nil {
			return nil, ErrInvalidEcobeeAmendment
		}
		delimiter, structured := token.(json.Delim)
		if !structured {
			return token, nil
		}
		switch delimiter {
		case '{':
			values := map[string]any{}
			for d.More() {
				key, err := d.Token()
				name, ok := key.(string)
				if err != nil || !ok {
					return nil, ErrInvalidEcobeeAmendment
				}
				if _, exists := values[name]; exists {
					return nil, ErrInvalidEcobeeAmendment
				}
				value, err := consume(depth + 1)
				if err != nil {
					return nil, err
				}
				values[name] = value
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return nil, ErrInvalidEcobeeAmendment
			}
			return values, nil
		case '[':
			values := []any{}
			for d.More() {
				value, err := consume(depth + 1)
				if err != nil {
					return nil, err
				}
				values = append(values, value)
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return nil, ErrInvalidEcobeeAmendment
			}
			return values, nil
		default:
			return nil, ErrInvalidEcobeeAmendment
		}
	}
	value, err := consume(0)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, ErrInvalidEcobeeAmendment
	}
	return value, nil
}

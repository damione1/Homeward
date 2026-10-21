package report

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/nha047"
	"github.com/damione1/Homeward/compat/ha/parser"
	registryv1 "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/scope"
	"google.golang.org/protobuf/encoding/protojson"
)

// OfflineReport consumes caller-supplied documentary declarations and concrete
// comparison bundles. No route, entity discovery or replacement is inferred.
// The digest identifies these supplied configuration bytes, never an active house.
func OfflineReport(data []byte) (Report, error) {
	if len(data) == 0 || len(data) > 64<<20 {
		return Report{}, ErrLimit
	}
	var wire struct {
		Schema        string  `json:"schema"`
		ScopeRevision *string `json:"scope_revision"`
		Configuration struct {
			ID       string `json:"id"`
			Revision uint64 `json:"revision"`
		} `json:"configuration"`
		Entities []struct {
			ID       string               `json:"id"`
			Status   ir.ReferenceStatus   `json:"status"`
			Supports []ir.DeclaredSupport `json:"supports"`
		} `json:"entities"`
		Declarations []struct {
			ID          string          `json:"id"`
			Description json.RawMessage `json:"description"`
			Conversion  Conversion      `json:"conversion"`
		} `json:"declarations"`
		Evidence   json.RawMessage `json:"evidence"`
		Control045 json.RawMessage `json:"control_045"`
		Candidates []struct {
			ID       string `json:"id"`
			Original struct {
				Path string `json:"path"`
				YAML string `json:"yaml"`
			} `json:"original"`
			Candidate struct {
				Path string `json:"path"`
				YAML string `json:"yaml"`
			} `json:"candidate"`
		} `json:"candidates"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return Report{}, ErrInvalidInput
	}
	if err := decoder.Decode(new(any)); err != io.EOF || wire.Schema != Schema || wire.Entities == nil || wire.Declarations == nil || wire.Candidates == nil || len(wire.Candidates) > 17 {
		return Report{}, ErrInvalidInput
	}
	// The structural decoder rejects duplicate members throughout the supplied
	// JSON tree, including configuration and candidates, before any admission.
	if err := validateOfflineJSON(data); err != nil {
		return Report{}, err
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return Report{}, ErrInvalidInput
	}
	for name := range members {
		if strings.EqualFold(name, "scope_revision") && name != "scope_revision" {
			return Report{}, ErrInvalidInput
		}
	}
	revision := scope.BaselineV1()
	if wire.ScopeRevision != nil {
		var err error
		revision, err = scope.ParseRevision(*wire.ScopeRevision)
		if err != nil {
			return Report{}, ErrInvalidInput
		}
	} else {
		// Absence uses the documented baseline; explicit null is not absence.
		if _, present := members["scope_revision"]; present {
			return Report{}, ErrInvalidInput
		}
	}
	manifest, err := registry.LoadManifest(revision)
	if err != nil {
		return Report{}, err
	}
	counts, ok := manifest.Counts()
	if !ok || len(wire.Entities) > counts.Entities || len(wire.Declarations) > counts.Entities {
		return Report{}, ErrInvalidInput
	}
	configurationBytes, err := json.Marshal(struct {
		Configuration any `json:"configuration"`
		Entities      any `json:"entities"`
		Declarations  any `json:"declarations"`
	}{wire.Configuration, wire.Entities, wire.Declarations})
	if err != nil {
		return Report{}, err
	}
	configuration, err := NewConfigurationRef(wire.Configuration.ID, wire.Configuration.Revision, nha047.Hash(configurationBytes))
	if err != nil {
		return Report{}, err
	}
	var entities []ir.Entity
	for _, row := range wire.Entities {
		entity, e := ir.NewEntity(row.ID, row.Status, row.Supports...)
		if e != nil {
			return Report{}, ErrInvalidInput
		}
		entities = append(entities, entity)
	}
	catalogue, err := ir.NewCatalogue(entities, nil, nil)
	if err != nil {
		return Report{}, ErrInvalidInput
	}
	var declarations []Declaration
	for _, row := range wire.Declarations {
		alias, e := domain.ParseHAAlias(row.ID)
		if e != nil {
			return Report{}, ErrInvalidInput
		}
		description := new(registryv1.CapabilityDescription)
		if e = protojson.Unmarshal(row.Description, description); e != nil {
			return Report{}, ErrInvalidInput
		}
		declaration, e := NewDeclaration(alias, description, row.Conversion)
		if e != nil {
			return Report{}, e
		}
		declarations = append(declarations, declaration)
	}
	evidence, err := nha047.DecodeEvidence(wire.Evidence)
	if err != nil {
		return Report{}, err
	}
	if len(wire.Control045) != 0 && !bytes.Equal(wire.Control045, []byte("null")) {
		proof, e := nha047.ControlProof045(wire.Control045)
		if e != nil {
			return Report{}, e
		}
		evidence, e = nha047.NewEvidence(append(evidence.Proofs(), proof))
		if e != nil {
			return Report{}, e
		}
	}
	var candidates []Candidate
	for _, row := range wire.Candidates {
		alias, e := domain.ParseHAAlias(row.ID)
		if e != nil {
			return Report{}, ErrInvalidInput
		}
		original, e := parser.NewSourceFile(row.Original.Path, []byte(row.Original.YAML))
		if e != nil {
			return Report{}, ErrInvalidInput
		}
		candidate, e := parser.NewSourceFile(row.Candidate.Path, []byte(row.Candidate.YAML))
		if e != nil {
			return Report{}, ErrInvalidInput
		}
		admitted, e := NewCandidatePair(alias, original, candidate)
		if e != nil {
			return Report{}, e
		}
		candidates = append(candidates, admitted)
	}
	return BuildMVPWithManifest(manifest, catalogue, configuration, declarations, evidence, candidates, nil)
}

func validateOfflineJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var consume func(int) error
	consume = func(depth int) error {
		if depth > 64 {
			return ErrInvalidInput
		}
		token, err := d.Token()
		if err != nil {
			return ErrInvalidInput
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, e := d.Token()
				if e != nil {
					return ErrInvalidInput
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return ErrInvalidInput
				}
				seen[name] = true
				if e = consume(depth + 1); e != nil {
					return e
				}
			}
		case '[':
			for d.More() {
				if e := consume(depth + 1); e != nil {
					return e
				}
			}
		default:
			return ErrInvalidInput
		}
		_, err = d.Token()
		return err
	}
	if err := consume(0); err != nil {
		return ErrInvalidInput
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrInvalidInput
	}
	return nil
}

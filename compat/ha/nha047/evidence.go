package nha047

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/damione1/Homeward/compat/ha/nha045"
	"github.com/damione1/Homeward/compat/ha/parser"
)

type SourceHash struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func (w *SourceHash) UnmarshalJSON(data []byte) error {
	type fields SourceHash
	var value fields
	if err := strictObjectJSON(data, &value, MaxCaptureBytes, "path", "sha256"); err != nil {
		return err
	}
	*w = SourceHash(value)
	return nil
}

type captureWire struct {
	Schema        string       `json:"schema"`
	Scenario      string       `json:"scenario"`
	FixtureSHA256 string       `json:"fixture_sha256"`
	InputSHA256   string       `json:"input_sha256"`
	Runtime       string       `json:"runtime"`
	Version       string       `json:"version"`
	Image         string       `json:"image"`
	Sources       []SourceHash `json:"sources"`
	Terminal      bool         `json:"terminal"`
	Evicted       int          `json:"evicted"`
	LoopbackOnly  bool         `json:"loopback_only"`
	Demands       []DemandWire `json:"demands"`
}

type Capture struct {
	wire    captureWire
	demands []Demand
	digest  Digest
}

// EncodeCapture is for the instrumented producers. Admission is repeated on
// the encoded boundary. consumed must be the snapshot passed to the loader;
// its observed digest is deliberately independent of the expected case role.
// Source hashes record provenance, not an activation right.
func EncodeCapture(c Case, runtime, version string, consumed parser.SourceFile, sources []SourceHash, demands []Demand, loopbackOnly, terminal bool) ([]byte, error) {
	if !c.Valid() || consumed.Path() == "" || consumed.ByteLen() == 0 || !Digest(consumed.SHA256()).Valid() {
		return nil, ErrInvalidCapture
	}
	w := captureWire{Schema: Schema, Scenario: c.ID(), FixtureSHA256: c.Digest().String(), InputSHA256: Digest(consumed.SHA256()).String(), Runtime: runtime, Version: version, Image: Image, Sources: append([]SourceHash(nil), sources...), Terminal: terminal, LoopbackOnly: loopbackOnly, Demands: []DemandWire{}}
	for _, d := range demands {
		w.Demands = append(w.Demands, d.wire())
	}
	data, err := json.Marshal(w)
	if err != nil {
		return nil, err
	}
	if _, err := DecodeCapture(data); err != nil {
		return nil, err
	}
	return data, nil
}

func DecodeCapture(data []byte) (Capture, error) {
	var w captureWire
	if err := strictObjectJSON(data, &w, MaxCaptureBytes, "schema", "scenario", "fixture_sha256", "input_sha256", "runtime", "version", "image", "sources", "terminal", "evicted", "loopback_only", "demands"); err != nil {
		return Capture{}, ErrInvalidCapture
	}
	if w.Schema != Schema || !validID(w.Scenario) || !w.Terminal || w.Evicted != 0 || !w.LoopbackOnly || w.Image != Image || w.Demands == nil || len(w.Demands) > MaxFacts {
		return Capture{}, ErrInvalidCapture
	}
	if _, err := ParseDigest(w.FixtureSHA256); err != nil {
		return Capture{}, ErrInvalidCapture
	}
	if _, err := ParseDigest(w.InputSHA256); err != nil {
		return Capture{}, ErrInvalidCapture
	}
	if w.Runtime != "ha" && w.Runtime != "go" || w.Runtime == "ha" && w.Version != HAVersion || w.Runtime == "go" && w.Version != GoVersion {
		return Capture{}, ErrInvalidCapture
	}
	if len(w.Sources) == 0 || len(w.Sources) > 32 {
		return Capture{}, ErrInvalidCapture
	}
	seen := map[string]bool{}
	for _, source := range w.Sources {
		if !validID(source.Path) || seen[source.Path] {
			return Capture{}, ErrInvalidCapture
		}
		if _, err := ParseDigest(source.SHA256); err != nil {
			return Capture{}, ErrInvalidCapture
		}
		seen[source.Path] = true
	}
	if w.Runtime == "go" && (len(seen) != 1 || !seen["probe"]) {
		return Capture{}, ErrInvalidCapture
	}
	if w.Runtime == "ha" && !seen["homeassistant.core"] {
		return Capture{}, ErrInvalidCapture
	}
	result := Capture{wire: w, digest: Hash(data), demands: make([]Demand, 0, len(w.Demands))}
	for _, item := range w.Demands {
		d, err := demandFromWire(item)
		if err != nil {
			return Capture{}, err
		}
		result.demands = append(result.demands, d)
	}
	return result, nil
}

type Difference struct {
	Side     string `json:"side"`
	Index    int    `json:"index"`
	Expected string `json:"expected"`
	Observed string `json:"observed"`
}

type Comparison struct {
	Schema          string       `json:"schema"`
	Equivalent      bool         `json:"equivalent"`
	ExpectedRefusal bool         `json:"expected_refusal"`
	Differences     []Difference `json:"differences"`
}

// Proof has no public construction from a test name, boolean or feature list.
// It exists only after both concrete captures satisfy a registered case oracle.
type Proof struct {
	id       string
	kind     EvidenceKind
	fixture  Digest
	ha       Digest
	goSide   Digest
	role     ComparisonRole
	features []Feature
	sources  []SourceHash
}

func (p Proof) ID() string            { return p.id }
func (p Proof) Kind() EvidenceKind    { return p.kind }
func (p Proof) Fixture() Digest       { return p.fixture }
func (p Proof) HACapture() Digest     { return p.ha }
func (p Proof) GoCapture() Digest     { return p.goSide }
func (p Proof) Role() ComparisonRole  { return p.role }
func (p Proof) Features() []Feature   { return append([]Feature(nil), p.features...) }
func (p Proof) Sources() []SourceHash { return append([]SourceHash(nil), p.sources...) }
func (p Proof) valid() bool {
	return validID(p.id) && p.kind >= DifferentialEquivalent && p.kind <= DifferentialRefusal && p.fixture.Valid() && p.ha.Valid() && p.goSide.Valid() && (p.role == SameSource || p.role == OriginalToCandidate) && len(p.features) != 0
}

func demandRows(input []Demand) []string {
	rows := make([]string, 0, len(input))
	for _, item := range input {
		encoded, _ := json.Marshal(item.wire())
		rows = append(rows, string(encoded))
	}
	return rows
}

// validateHASources is shared by every public route that can produce a Proof.
// These pins establish declared source provenance, not remote attestation.
func validateHASources(sources []SourceHash) error {
	pins, err := qualificationHASources()
	if err != nil {
		return err
	}
	if len(sources) != len(pins) {
		return ErrInvalidCapture
	}
	actual := make(map[string]string, len(sources))
	for _, row := range sources {
		if _, duplicate := actual[row.Path]; duplicate {
			return ErrInvalidCapture
		}
		actual[row.Path] = row.SHA256
	}
	for _, pin := range pins {
		if actual[pin.Path] != pin.SHA256 {
			return ErrInvalidCapture
		}
	}
	return nil
}

func Compare(c Case, ha, goSide Capture) (Comparison, Proof, error) {
	result := Comparison{Schema: Schema, Differences: []Difference{}}
	if !c.Valid() || !ha.digest.Valid() || !goSide.digest.Valid() || ha.wire.Runtime != "ha" || goSide.wire.Runtime != "go" || ha.wire.Scenario != c.ID() || goSide.wire.Scenario != c.ID() || ha.wire.FixtureSHA256 != c.Digest().String() || goSide.wire.FixtureSHA256 != c.Digest().String() {
		return result, Proof{}, ErrInvalidCapture
	}
	if err := validateHASources(ha.wire.Sources); err != nil {
		return result, Proof{}, err
	}
	haInput, haErr := c.expectedInput("ha")
	goInput, goErr := c.expectedInput("go")
	if haErr != nil || goErr != nil || ha.wire.InputSHA256 != haInput.String() || goSide.wire.InputSHA256 != goInput.String() {
		return result, Proof{}, ErrInvalidCapture
	}
	// Scene members are independent; only their cross-member order is ignored.
	// Multiplicity remains intact. Other scenarios keep their native order.
	rows := func(input []Demand) []string {
		values := demandRows(input)
		if c.Scenario() == SceneProjection {
			sort.Strings(values)
		}
		return values
	}
	check := func(side string, expected, actual []Demand) {
		a, b := rows(expected), rows(actual)
		n := max(len(a), len(b))
		for i := 0; i < n; i++ {
			left, right := "<absent>", "<absent>"
			if i < len(a) {
				left = a[i]
			}
			if i < len(b) {
				right = b[i]
			}
			if left != right {
				result.Differences = append(result.Differences, Difference{side, i, left, right})
			}
		}
	}
	check("ha", c.haExpected, ha.demands)
	check("go", c.goExpected, goSide.demands)
	if len(result.Differences) != 0 {
		return result, Proof{}, ErrDivergence
	}
	left, right := rows(ha.demands), rows(goSide.demands)
	result.Equivalent = len(left) == len(right)
	if result.Equivalent {
		for i := range left {
			if left[i] != right[i] {
				result.Equivalent = false
				break
			}
		}
	}
	kind := DifferentialEquivalent
	if !result.Equivalent {
		kind = DifferentialDivergence
		result.ExpectedRefusal = true
		for i := 0; i < max(len(left), len(right)); i++ {
			a, b := "<absent>", "<absent>"
			if i < len(left) {
				a = left[i]
			}
			if i < len(right) {
				b = right[i]
			}
			if a != b {
				result.Differences = append(result.Differences, Difference{"ha/go", i, a, b})
			}
		}
	}
	if len(goSide.demands) != 0 && goSide.demands[0].Kind() == Refusal {
		kind = DifferentialRefusal
		result.Equivalent = false
		result.ExpectedRefusal = true
	}
	sources := append(append([]SourceHash(nil), ha.wire.Sources...), goSide.wire.Sources...)
	features := c.Features()
	// A false condition proves suppression and trigger handling, never the
	// action behind it. Keep registered fixture bytes/hashes unchanged; derive
	// applicability from the actual requests admitted by both case oracles.
	covered := make([]Feature, 0, len(features))
	for _, feature := range features {
		if feature.Construction() == ClimateSetpoint {
			observed := func(demands []Demand) bool {
				for _, demand := range demands {
					if demand.Kind() == ClimateTemperature && demand.wire().Target == "climate.nha047_value" && demand.wire().Value == feature.Value() {
						return true
					}
				}
				return false
			}
			if !observed(ha.demands) || !observed(goSide.demands) {
				continue
			}
		}
		covered = append(covered, feature)
	}
	proof := Proof{id: c.ID(), kind: kind, fixture: c.Digest(), ha: ha.digest, goSide: goSide.digest, role: c.Role(), features: covered, sources: sources}
	return result, proof, nil
}

type Evidence struct {
	proofs []Proof
	valid  bool
}

func NewEvidence(proofs []Proof) (Evidence, error) {
	if len(proofs) > MaxCases {
		return Evidence{}, ErrInvalidInput
	}
	result := Evidence{proofs: make([]Proof, 0, len(proofs)), valid: true}
	seen := map[string]bool{}
	for _, p := range proofs {
		if !p.valid() || seen[p.id] {
			return Evidence{}, ErrInvalidInput
		}
		seen[p.id] = true
		p.features = p.Features()
		p.sources = p.Sources()
		result.proofs = append(result.proofs, p)
	}
	sort.Slice(result.proofs, func(i, j int) bool { return result.proofs[i].id < result.proofs[j].id })
	return result, nil
}

func (e Evidence) Valid() bool { return e.valid }
func (e Evidence) Proofs() []Proof {
	if !e.valid {
		return nil
	}
	result := make([]Proof, len(e.proofs))
	for i, p := range e.proofs {
		p.features = p.Features()
		p.sources = p.Sources()
		result[i] = p
	}
	return result
}

func (e Evidence) Cover(feature Feature) []Proof {
	if !e.valid || !feature.Valid() {
		return nil
	}
	var result []Proof
	for _, p := range e.Proofs() {
		for _, f := range p.features {
			if f == feature {
				result = append(result, p)
				break
			}
		}
	}
	return result
}

type bundleWire struct {
	Fixture json.RawMessage `json:"fixture"`
	HA      json.RawMessage `json:"ha"`
	Go      json.RawMessage `json:"go"`
}

func CompareInput(data []byte) (Comparison, Proof, error) {
	var input bundleWire
	if err := strictJSON(data, &input, 2*MaxCaptureBytes+MaxInputBytes); err != nil {
		return Comparison{}, Proof{}, err
	}
	c, err := DecodeCase(input.Fixture)
	if err != nil {
		return Comparison{}, Proof{}, err
	}
	ha, err := DecodeCapture(input.HA)
	if err != nil {
		return Comparison{}, Proof{}, err
	}
	goSide, err := DecodeCapture(input.Go)
	if err != nil {
		return Comparison{}, Proof{}, err
	}
	return Compare(c, ha, goSide)
}

func DecodeEvidence(data []byte) (Evidence, error) {
	var input struct {
		Schema      string            `json:"schema"`
		Comparisons []json.RawMessage `json:"comparisons"`
	}
	if err := strictJSON(data, &input, 64<<20); err != nil || input.Schema != Schema || input.Comparisons == nil || len(input.Comparisons) > MaxCases {
		return Evidence{}, ErrInvalidInput
	}
	proofs := make([]Proof, 0, len(input.Comparisons))
	for _, data := range input.Comparisons {
		_, p, err := CompareInput(data)
		if err != nil {
			return Evidence{}, fmt.Errorf("%w: comparison", err)
		}
		proofs = append(proofs, p)
	}
	return NewEvidence(proofs)
}

// ControlProof045 reuses the delivered busy-lane differential without extending
// its switch-only payload or asserting that it exercises time/climate/scenes.
// Both captures and the exact current registered fixture are required.
func ControlProof045(data []byte) (Proof, error) {
	comparison, err := nha045.CompareInput(data)
	if err != nil || !comparison.Equivalent {
		return Proof{}, ErrInvalidCapture
	}
	var raw struct {
		Fixture json.RawMessage `json:"fixture"`
		HA      json.RawMessage `json:"ha"`
		Go      json.RawMessage `json:"go"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return Proof{}, ErrInvalidCapture
	}
	fixture, err := nha045.DecodeFixture(raw.Fixture)
	if err != nil || fixture.ID() != "03-single-busy" {
		return Proof{}, ErrInvalidCapture
	}
	corpus, err := nha045.Corpus()
	if err != nil {
		return Proof{}, err
	}
	matched := false
	for _, c := range corpus {
		if c.ID() == fixture.ID() && c.SHA256() == fixture.SHA256() {
			matched = true
			break
		}
	}
	if !matched {
		return Proof{}, ErrInvalidCapture
	}
	f, err := ParseDigest(fixture.SHA256())
	if err != nil {
		return Proof{}, err
	}
	feature, _ := NewFeature(AutomationSingle, 0, 0)
	return Proof{id: "nha045/03-single-busy", kind: DifferentialEquivalent, fixture: f, ha: Hash(raw.HA), goSide: Hash(raw.Go), role: SameSource, features: []Feature{feature}, sources: []SourceHash{{"nha045/comparison_bundle", Hash(data).String()}}}, nil
}

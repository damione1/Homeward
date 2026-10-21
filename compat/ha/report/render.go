package report

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/logicselection"
)

type proofWire struct {
	Case            string `json:"case"`
	Kind            string `json:"kind"`
	Role            string `json:"role"`
	FixtureSHA256   string `json:"fixture_sha256"`
	HACaptureSHA256 string `json:"ha_capture_sha256"`
	GoCaptureSHA256 string `json:"go_capture_sha256"`
}

type provenanceWire struct {
	Origin string `json:"origin"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func renderProvenance(source SourceProvenance) *provenanceWire {
	if !source.Valid() {
		return nil
	}
	return &provenanceWire{source.Origin().String(), source.Path(), source.Digest().String()}
}

type sourceWire struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
	Path   string `json:"path"`
}

type constructionWire struct {
	Construction string      `json:"construction"`
	Value        float64     `json:"value"`
	Secondary    float64     `json:"secondary"`
	Source       sourceWire  `json:"source"`
	References   []string    `json:"references"`
	Coverage     string      `json:"coverage"`
	Refusal      string      `json:"refusal"`
	Proofs       []proofWire `json:"proofs"`
}

type objectWire struct {
	ID                string             `json:"id"`
	Path              string             `json:"source_path"`
	SHA256            string             `json:"source_sha256"`
	SourceDocument    string             `json:"source_document"`
	DecisionReference string             `json:"selection_decision"`
	Verdict           string             `json:"verdict"`
	Refusal           string             `json:"refusal"`
	Reason            string             `json:"reason"`
	Activation        string             `json:"activation"`
	Activatable       bool               `json:"activatable"`
	CandidateSHA256   string             `json:"candidate_sha256"`
	OriginalSource    *provenanceWire    `json:"original_source,omitempty"`
	CandidateSource   *provenanceWire    `json:"candidate_source,omitempty"`
	CompilerCode      string             `json:"compiler_code"`
	Constructions     []constructionWire `json:"constructions"`
}

type reportWire struct {
	Schema              string       `json:"schema"`
	Profile             string       `json:"profile"`
	BaselineSHA256      string       `json:"baseline_logic_sha256"`
	SelectionSHA256     string       `json:"selection_sha256"`
	SelectionDigestKind string       `json:"selection_digest_kind"`
	ManifestIDsSHA256   string       `json:"manifest_ids_sha256"`
	Configuration       string       `json:"configuration"`
	Revision            uint64       `json:"configuration_revision"`
	ConfigurationSHA256 string       `json:"configuration_sha256"`
	Objects             []objectWire `json:"objects"`
	Deferred            []string     `json:"deferred"`
	Limits              []string     `json:"limits"`
}

func (r Report) wire() (reportWire, error) {
	if !r.Valid() {
		return reportWire{}, ErrInvalidReport
	}
	w := reportWire{Schema: Schema, Profile: parser.Profile, BaselineSHA256: r.baseline.String(), SelectionSHA256: r.selection.String(), SelectionDigestKind: "admitted_partition_and_definition_metadata", ManifestIDsSHA256: r.manifest.String(), Configuration: r.configuration.id, Revision: r.configuration.revision, ConfigurationSHA256: r.configuration.digest.String(), Objects: []objectWire{}, Deferred: []string{}, Limits: []string{
		"Construction-level compatibility under the explicit catalogue and declared conversion profiles; not whole-object runtime equivalence.",
		"No live device, driver route, physical action, business activation or authorization is qualified by this report.",
		"Deferred objects have no semantic verdict; all 89 baseline objects remain inactive.",
		"Native tests and expected refusals are not successful HA equivalence proofs.",
		"An explicitly supplied original is an input to this analysis, not an attested historical household configuration.",
	}}
	for _, o := range r.objects {
		id, err := o.ID().Value()
		if err != nil {
			return reportWire{}, ErrInvalidReport
		}
		decision := ""
		switch o.definition.Decision() {
		case logicselection.DecisionQ32:
			decision = "Q32"
		case logicselection.DecisionQ33:
			decision = "Q33"
		case logicselection.DecisionQ34:
			decision = "Q34"
		case logicselection.DecisionQ37:
			decision = "Q37"
		case logicselection.DecisionUnspecified:
			return reportWire{}, ErrInvalidReport
		default:
			return reportWire{}, ErrInvalidReport
		}
		item := objectWire{ID: id, Path: o.definition.Path(), SHA256: fmt.Sprintf("%x", o.definition.Digest()), SourceDocument: o.definition.SourceDocument(), DecisionReference: decision, Verdict: o.verdict.String(), Refusal: o.refusal.String(), Reason: o.refusal.Reason(), Activation: o.activation.String(), Activatable: false, CompilerCode: o.compilerCode, Constructions: []constructionWire{}}
		item.OriginalSource = renderProvenance(o.originalSource)
		item.CandidateSource = renderProvenance(o.candidateSource)
		if o.candidate.Valid() {
			item.CandidateSHA256 = o.candidate.String()
		}
		for _, node := range o.nodes {
			s := node.source
			c := constructionWire{Construction: node.feature.Construction().String(), Value: node.feature.Value(), Secondary: node.feature.Secondary(), Source: sourceWire{s.File, s.Line, s.Column, s.Path}, References: []string{}, Coverage: node.coverage.String(), Refusal: node.refusal.String(), Proofs: []proofWire{}}
			for _, ref := range node.references {
				value, err := ref.Value()
				if err != nil {
					return reportWire{}, ErrInvalidReport
				}
				c.References = append(c.References, value)
			}
			for _, p := range node.proofs {
				c.Proofs = append(c.Proofs, proofWire{Case: p.ID(), Kind: p.Kind().String(), Role: p.Role().String(), FixtureSHA256: p.Fixture().String(), HACaptureSHA256: p.HACapture().String(), GoCaptureSHA256: p.GoCapture().String()})
			}
			item.Constructions = append(item.Constructions, c)
		}
		w.Objects = append(w.Objects, item)
	}
	for _, id := range r.deferred {
		value, err := id.Value()
		if err != nil {
			return reportWire{}, ErrInvalidReport
		}
		w.Deferred = append(w.Deferred, value)
	}
	return w, nil
}

func JSON(r Report) ([]byte, error) {
	w, err := r.wire()
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return nil, err
	}
	if len(data) > MaxReportBytes {
		return nil, ErrLimit
	}
	return append(data, '\n'), nil
}

func RenderText(writer io.Writer, r Report) error {
	if writer == nil {
		return ErrInvalidInput
	}
	w, err := r.wire()
	if err != nil {
		return err
	}
	data, err := JSON(r)
	if err != nil {
		return err
	}
	// The JSON size gate bounds the text before writing anything to the caller.
	_ = data
	if _, err = fmt.Fprintf(writer, "Homeward — rapport de compatibilité\nProfil : %s ; configuration : %s / %d\n17 définitions ; 72 objets reportés ; aucune activation.\n\n", w.Profile, w.Configuration, w.Revision); err != nil {
		return err
	}
	for _, o := range w.Objects {
		label := "refusé"
		switch o.Verdict {
		case AcceptedUnchanged.String():
			label = "accepté inchangé"
		case AcceptedAdapted.String():
			label = "accepté après adaptation validée"
		case Refused.String():
		default:
			return ErrInvalidReport
		}
		if _, err = fmt.Fprintf(writer, "%s : %s ; %s\n  Source documentaire 158 : %s (%s)\n", o.ID, label, o.Activation, o.Path, o.SHA256); err != nil {
			return err
		}
		if source := o.OriginalSource; source != nil {
			label := "Original analysé depuis la source 158"
			if source.Origin == ExplicitImportPair.String() {
				label = "Original fourni pour cette analyse"
			}
			if _, err = fmt.Fprintf(writer, "  %s : %s (%s)\n", label, source.Path, source.SHA256); err != nil {
				return err
			}
		}
		if source := o.CandidateSource; source != nil {
			if _, err = fmt.Fprintf(writer, "  Candidate fournie : %s (%s)\n", source.Path, source.SHA256); err != nil {
				return err
			}
		}
		if o.Refusal != RefusalNone.String() {
			if _, err = fmt.Fprintf(writer, "  Refus : %s — %s\n", o.Refusal, o.Reason); err != nil {
				return err
			}
		}
		for _, c := range o.Constructions {
			if _, err = fmt.Fprintf(writer, "  %s (%g, %g) : %s — %s:%d %s\n", c.Construction, c.Value, c.Secondary, c.Coverage, c.Source.File, c.Source.Line, c.Source.Path); err != nil {
				return err
			}
			for _, proof := range c.Proofs {
				if _, err = fmt.Fprintf(writer, "    %s : %s ; rôle %s ; fixture %s\n", proof.Case, proof.Kind, proof.Role, proof.FixtureSHA256); err != nil {
					return err
				}
			}
		}
	}
	_, err = fmt.Fprintf(writer, "\nLes 72 autres objets restent reportés et inactifs. Ce rapport ne qualifie ni pilote ni installation réelle.\n")
	return err
}

package report_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/damione1/Homeward/compat/ha/nha047"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/compat/ha/report"
)

func TestNHA047CandidatePairProvenanceIsExplicitImmutableAndZeroInvalid(t *testing.T) {
	definition, original, canonical := pairSources(t)
	pair, err := report.NewCandidatePair(definition.ID(), original, canonical)
	if err != nil {
		t.Fatal(err)
	}
	left, right := pair.OriginalSource(), pair.CandidateSource()
	if !left.Valid() || left.Origin() != report.ExplicitImportPair || left.Path() != original.Path() || left.Digest() != nha047.Digest(original.SHA256()) {
		t.Fatal("explicit original provenance was lost")
	}
	if !right.Valid() || right.Origin() != report.ExplicitCandidate || right.Path() != canonical.Path() || right.Digest() != nha047.Digest(canonical.SHA256()) {
		t.Fatal("candidate provenance was lost")
	}
	left, right = report.SourceProvenance{}, report.SourceProvenance{}
	if left.Valid() || right.Valid() || !(pair.OriginalSource().Valid()) || !(pair.CandidateSource().Valid()) {
		t.Fatal("provenance zero or copy changed the admitted pair")
	}
	if (report.Candidate{}).OriginalSource().Valid() || (report.Candidate{}).CandidateSource().Valid() {
		t.Fatal("zero candidate acquired provenance")
	}
	frozen, err := report.NewCandidate(definition.ID(), nha047.Digest(definition.Digest()), canonical)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.OriginalSource().Origin() != report.Frozen158 || frozen.OriginalSource().Digest() != nha047.Digest(definition.Digest()) {
		t.Fatal("frozen original became a supplied source pair")
	}
}

func TestNHA047CandidatePairBudgetChargesBothSnapshotsBeforeSourceFS(t *testing.T) {
	definition, _, _ := pairSources(t)
	file, err := parser.NewSourceFile(definition.Path(), bytes.Repeat([]byte{'x'}, report.MaxSourceBytes))
	if err != nil {
		t.Fatal(err)
	}
	pair, err := report.NewCandidatePair(definition.ID(), file, file)
	if err != nil {
		t.Fatal(err)
	}
	input := budgetInput(t)
	input.Blueprints = []parser.SourceFile{file, file, file}
	input.Candidates = []report.Candidate{pair}
	source := &countedReportFS{}
	input.Sources = source
	if _, err := report.Build(input); !errors.Is(err, report.ErrLimit) {
		t.Fatalf("both snapshots must be charged: %v", err)
	}
	if source.opens != 0 {
		t.Fatalf("over-budget pair reached source FS: %d", source.opens)
	}
}

package report_test

import (
	"bytes"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/damione1/Homeward/compat/ha/nha047"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/compat/ha/report"
)

func budgetInput(t *testing.T) report.Input {
	t.Helper()
	empty, err := nha047.NewEvidence(nil)
	if err != nil {
		t.Fatal(err)
	}
	return fanInput(t, empty)
}

func budgetSnapshot(t *testing.T, size int) parser.SourceFile {
	t.Helper()
	file, err := parser.NewSourceFile("snapshot.yaml", bytes.Repeat([]byte{'x'}, size))
	if err != nil {
		t.Fatal(err)
	}
	if file.ByteLen() != size {
		t.Fatalf("snapshot length: got %d, want %d", file.ByteLen(), size)
	}
	return file
}

func TestNHA047SourceBudgetAdmitsSnapshotsBeforeOpeningFS(t *testing.T) {
	file := budgetSnapshot(t, report.MaxSourceBytes)
	for _, kind := range []string{"blueprints", "candidates", "combined"} {
		t.Run(kind, func(t *testing.T) {
			input := budgetInput(t)
			source := &countedReportFS{}
			input.Sources = source
			if kind != "candidates" {
				count := 5
				if kind == "combined" {
					count = 4
				}
				for range count {
					input.Blueprints = append(input.Blueprints, file)
				}
			}
			if kind != "blueprints" {
				count := 5
				if kind == "combined" {
					count = 1
				}
				for _, definition := range input.Selection.Retained() {
					id, _ := definition.ID().Value()
					if !strings.HasPrefix(id, "automation.") {
						continue
					}
					candidate, err := report.NewCandidate(definition.ID(), nha047.Digest(definition.Digest()), file)
					if err != nil {
						t.Fatal(err)
					}
					input.Candidates = append(input.Candidates, candidate)
					if len(input.Candidates) == count {
						break
					}
				}
				if len(input.Candidates) != count {
					t.Fatal("precondition: missing selected automation")
				}
			}
			if _, err := report.Build(input); !errors.Is(err, report.ErrLimit) {
				t.Fatalf("want snapshot aggregate ErrLimit, got %v", err)
			}
			if source.opens != 0 {
				t.Fatalf("over-budget snapshots reached source FS: opens=%d", source.opens)
			}
		})
	}
}

func TestNHA047SourceBudgetUsesRemainingAggregateForUnreliableStream(t *testing.T) {
	input := budgetInput(t)
	file := budgetSnapshot(t, report.MaxSourceBytes-2)
	input.Blueprints = []parser.SourceFile{file, file, file, file}
	// Four immutable snapshots leave eight bytes, even though one file may
	// ordinarily consume a MiB. The generated stream needs no backing buffer.
	source := &countedReportFS{declared: 0, stream: 10}
	input.Sources = source
	if _, err := report.Build(input); !errors.Is(err, report.ErrLimit) {
		t.Fatalf("want remaining-aggregate ErrLimit, got %v", err)
	}
	if source.opens != 1 || source.closes != 1 || source.bytesRead != 9 {
		t.Fatalf("remaining budget exceeded its sentinel: opens=%d closes=%d bytes=%d", source.opens, source.closes, source.bytesRead)
	}
}

func TestNHA047SourceBudgetAllowsExactFileAndAggregateBoundaries(t *testing.T) {
	input := budgetInput(t)
	file := budgetSnapshot(t, report.MaxSourceBytes)
	input.Blueprints = []parser.SourceFile{file, file, file}
	source := &countedReportFS{declared: report.MaxSourceBytes, stream: report.MaxSourceBytes}
	input.Sources = source
	if _, err := report.Build(input); !errors.Is(err, report.ErrLimit) {
		t.Fatalf("want the next file's aggregate ErrLimit, got %v", err)
	}
	// The first exact-MiB source is read; metadata rejects the next file when
	// the total has reached exactly four MiB, before another read.
	if source.opens != 2 || source.stats != 2 || source.closes != 2 || source.bytesRead != report.MaxSourceBytes {
		t.Fatalf("exact boundary was rejected early or exceeded: opens=%d stats=%d closes=%d bytes=%d", source.opens, source.stats, source.closes, source.bytesRead)
	}
}

type budgetFailureFS struct {
	countedReportFS
	stage string
}

func (source *budgetFailureFS) Open(name string) (fs.File, error) {
	file, err := source.countedReportFS.Open(name)
	if err != nil {
		return nil, err
	}
	return budgetFailureFile{File: file, stage: source.stage}, nil
}

type budgetFailureFile struct {
	fs.File
	stage string
}

var errBudgetIO = errors.New("synthetic source I/O failure")

func (file budgetFailureFile) Stat() (fs.FileInfo, error) {
	info, err := file.File.Stat()
	if file.stage == "stat" {
		return nil, errBudgetIO
	}
	return info, err
}

func (file budgetFailureFile) Read(destination []byte) (int, error) {
	if file.stage == "read" {
		return 0, errBudgetIO
	}
	return file.File.Read(destination)
}

func (file budgetFailureFile) Close() error {
	err := file.File.Close()
	if file.stage == "close" {
		return errBudgetIO
	}
	return err
}

func TestNHA047SourceReadFailuresCloseEveryOpenedFileAndRefuse(t *testing.T) {
	for _, stage := range []string{"stat", "read", "close"} {
		t.Run(stage, func(t *testing.T) {
			input := budgetInput(t)
			source := &budgetFailureFS{countedReportFS: countedReportFS{declared: 1, stream: 1}, stage: stage}
			input.Sources = source
			r, err := report.Build(input)
			if err != nil {
				t.Fatal(err)
			}
			if source.opens != 17 || source.closes != source.opens {
				t.Fatalf("opened source leaked: opens=%d closes=%d", source.opens, source.closes)
			}
			if stage == "stat" && source.reads != 0 {
				t.Fatal("read after failed metadata admission")
			}
			for _, object := range r.Objects() {
				if object.Verdict() != report.Refused || object.Refusal() != report.SourceUnavailable {
					t.Fatalf("I/O failure accepted or misclassified: %s", object.Refusal())
				}
			}
		})
	}
}

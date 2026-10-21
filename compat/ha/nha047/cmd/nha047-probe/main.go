package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/damione1/Homeward/compat/ha/nha047"
	"github.com/damione1/Homeward/compat/ha/report"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	mode := flag.String("mode", "run", "contract, corpus, run, project-ha or compare")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	write := func(data []byte, err error) error {
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(append(data, '\n'))
		return err
	}
	switch *mode {
	case "contract":
		return write(nha047.Manifest())
	case "corpus":
		corpus, err := nha047.Corpus()
		if err != nil {
			return err
		}
		type row struct {
			ID      string `json:"id"`
			SHA256  string `json:"sha256"`
			Fixture string `json:"fixture"`
		}
		rows := make([]row, 0, len(corpus))
		for _, c := range corpus {
			data, err := c.JSON()
			if err != nil {
				return err
			}
			rows = append(rows, row{c.ID(), c.Digest().String(), string(data)})
		}
		return json.NewEncoder(os.Stdout).Encode(rows)
	case "report-json", "report-text":
		data, err := io.ReadAll(io.LimitReader(os.Stdin, (64<<20)+1))
		if err != nil {
			return err
		}
		if len(data) > 64<<20 {
			return report.ErrLimit
		}
		value, err := report.OfflineReport(data)
		if err != nil {
			return err
		}
		if *mode == "report-text" {
			return report.RenderText(os.Stdout, value)
		}
		return write(report.JSON(value))
	case "run", "project-ha", "compare":
		maximum := nha047.MaxInputBytes
		if *mode == "project-ha" {
			maximum += nha047.MaxCaptureBytes
		}
		if *mode == "compare" {
			maximum += 2 * nha047.MaxCaptureBytes
		}
		data, err := io.ReadAll(io.LimitReader(os.Stdin, int64(maximum)+1))
		if err != nil {
			return err
		}
		if len(data) > maximum {
			return nha047.ErrInvalidInput
		}
		switch *mode {
		case "run":
			fixture, err := nha047.DecodeCase(data)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			capture, err := nha047.Run(ctx, fixture)
			if err != nil {
				return err
			}
			return write(capture.JSON())
		case "project-ha":
			capture, err := nha047.ProjectInput(data)
			if err != nil {
				return err
			}
			return write(capture.JSON())
		case "compare":
			comparison, _, comparisonErr := nha047.CompareInput(data)
			if err := json.NewEncoder(os.Stdout).Encode(comparison); err != nil {
				return err
			}
			return comparisonErr
		}
	}
	return fmt.Errorf("unknown qualification mode")
}

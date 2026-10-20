package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/damione1/Homeward/compat/ha/nha045"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	mode := flag.String("mode", "run", "run, corpus, contract, project-ha or compare")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	switch *mode {
	case "contract":
		data, e := nha045.Manifest()
		if e != nil {
			return e
		}
		_, e = os.Stdout.Write(append(data, '\n'))
		return e
	case "corpus":
		all, e := nha045.Corpus()
		if e != nil {
			return e
		}
		type row struct {
			ID      string `json:"id"`
			SHA256  string `json:"sha256"`
			Fixture string `json:"fixture"`
		}
		rows := make([]row, 0, len(all))
		for _, f := range all {
			data, e := f.JSON()
			if e != nil {
				return e
			}
			rows = append(rows, row{f.ID(), f.SHA256(), string(data)})
		}
		return json.NewEncoder(os.Stdout).Encode(rows)
	case "run":
		data, e := io.ReadAll(io.LimitReader(os.Stdin, nha045.MaxInputBytes+1))
		if e != nil {
			return e
		}
		f, e := nha045.DecodeFixture(data)
		if e != nil {
			return e
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		capture, e := nha045.Run(ctx, f)
		if e != nil {
			return e
		}
		out, e := capture.JSON()
		if e != nil {
			return e
		}
		_, e = os.Stdout.Write(append(out, '\n'))
		return e
	case "compare":
		return compare()
	case "project-ha":
		data, e := io.ReadAll(io.LimitReader(os.Stdin, nha045.MaxCaptureBytes+nha045.MaxInputBytes+1))
		if e != nil {
			return e
		}
		capture, e := nha045.ProjectInput(data)
		if e != nil {
			return e
		}
		out, e := capture.JSON()
		if e != nil {
			return e
		}
		_, e = os.Stdout.Write(append(out, '\n'))
		return e
	default:
		return fmt.Errorf("unknown mode: %s", *mode)
	}
}
func compare() error {
	data, e := io.ReadAll(io.LimitReader(os.Stdin, 2*nha045.MaxCaptureBytes+nha045.MaxInputBytes+1))
	if e != nil {
		return e
	}
	result, comparisonError := nha045.CompareInput(data)
	if e = json.NewEncoder(os.Stdout).Encode(result); e != nil {
		return e
	}
	return comparisonError
}

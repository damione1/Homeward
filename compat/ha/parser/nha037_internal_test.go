package parser

import (
	"errors"
	"testing"
)

func TestNHA037UnknownYAMLPosition(t *testing.T) {
	pos := yamlErrorPosition("configuration.yaml", errors.New("parser failure without location"))
	if pos.File != "configuration.yaml" || pos.Line != 0 || pos.Column != 0 {
		t.Fatalf("unreported source location was invented: %+v", pos)
	}
}

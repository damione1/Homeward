package server

import (
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/configplans"
	"strings"
)

func configPlanSourceFile(family configplans.Family, path string, raw []byte) (parser.SourceFile, error) {
	if family == configplans.Ownership && strings.HasSuffix(path, ".json") {
		return parser.NewJSONSourceFile(path, raw)
	}
	return parser.NewSourceFile(path, raw)
}

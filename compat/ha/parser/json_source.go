package parser

import (
	"crypto/sha256"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// NewJSONSourceFile owns an exact JSON source snapshot. It does not admit JSON
// to LoadSources: that loader retains its closed YAML grammar and paths.
func NewJSONSourceFile(path string, data []byte) (SourceFile, error) {
	if path == "" || len(path) > maxPathBytes || !utf8.ValidString(path) || strings.IndexByte(path, 0) >= 0 || !filepath.IsLocal(path) || filepath.Clean(path) != path || filepath.Ext(path) != ".json" || len(data) > maxFileBytes {
		return SourceFile{}, diagnostic(CodeInvalidEntry, Position{File: path, Line: 1, Column: 1}, "$", "invalid JSON source snapshot")
	}
	owned := append([]byte(nil), data...)
	return SourceFile{path: path, data: owned, hash: sha256.Sum256(owned)}, nil
}

// CloneSourceFile preserves the closed snapshot format and validates its seal.
// It supplies no filesystem read authority and does not select a parser.
func CloneSourceFile(file SourceFile) (SourceFile, error) {
	var copy SourceFile
	var err error
	if filepath.Ext(file.path) == ".json" {
		copy, err = NewJSONSourceFile(file.path, file.data)
	} else {
		copy, err = NewSourceFile(file.path, file.data)
	}
	if err != nil {
		return SourceFile{}, err
	}
	if copy.hash != file.hash {
		return SourceFile{}, diagnostic(CodeInvalidEntry, Position{File: file.path, Line: 1, Column: 1}, "$", "source snapshot hash mismatch")
	}
	return copy, nil
}

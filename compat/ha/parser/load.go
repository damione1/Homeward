package parser

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"go.yaml.in/yaml/v3"
)

const (
	maxFileBytes     = 1 << 20
	maxBundleBytes   = 4 << 20
	maxIncludeDepth  = 8
	maxAliasDepth    = 16
	maxSyntaxDepth   = 64
	maxKeyBytes      = 256
	maxPathBytes     = 4096
	maxExpandedNodes = 20_000
	maxRetainedBytes = 16 << 20
)

type loader struct {
	root     *os.Root
	bytes    int64
	nodes    int
	retained int64
	active   map[string]bool
}

// Load parses a single YAML document inside bundleRoot. The entry and every
// !include are opened through os.Root, so symlink replacement cannot turn a
// checked path into a read outside the bundle. Files are size checked before
// allocation and limited again while streaming.
func Load(bundleRoot, entry string) (*Document, error) {
	if !safeName(entry) {
		return nil, diagnostic(CodeInvalidEntry, Position{File: entry, Line: 1, Column: 1}, "$", "entry must be a local YAML file")
	}
	root, err := os.OpenRoot(bundleRoot)
	if err != nil {
		return nil, diagnostic(CodeReadFile, Position{File: entry, Line: 1, Column: 1}, "$", "bundle root cannot be opened")
	}
	defer root.Close()
	l := loader{root: root, active: make(map[string]bool)}
	n, d := l.parseFile(filepath.Clean(entry), "$", nil, 0, 0, Position{File: entry, Line: 1, Column: 1})
	if d != nil {
		return nil, d
	}
	if d = validateProfile(n); d != nil {
		return nil, d
	}
	return &Document{root: n}, nil
}

func safeName(name string) bool {
	if name == "" || strings.HasSuffix(name, string(filepath.Separator)) || !filepath.IsLocal(name) {
		return false
	}
	ext := filepath.Ext(name)
	return ext == ".yaml" || ext == ".yml"
}

func (l *loader) parseFile(name, path string, refs []Reference, depth, syntaxDepth int, site Position) (Node, *Diagnostic) {
	if depth > maxIncludeDepth {
		return Node{}, diagnostic(CodeIncludeDepth, site, path, "include depth limit exceeded")
	}
	if l.active[name] {
		return Node{}, diagnostic(CodeIncludeCycle, site, path, "include cycle")
	}
	if !safeName(name) {
		return Node{}, diagnostic(CodeIncludeOutside, site, path, "include path is outside the supported bundle profile")
	}
	// This excludes symlinks altogether. os.Root remains the confinement
	// boundary if a component is replaced between Lstat and Open.
	parts := strings.Split(name, string(filepath.Separator))
	for i := range parts {
		component := filepath.Join(parts[:i+1]...)
		info, err := l.root.Lstat(component)
		if err != nil {
			return Node{}, diagnostic(CodeReadFile, site, path, "included file cannot be inspected")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return Node{}, diagnostic(CodeIncludeOutside, site, path, "symlink include is outside the supported bundle profile")
		}
		if i == len(parts)-1 && !info.Mode().IsRegular() {
			return Node{}, diagnostic(CodeReadFile, site, path, "include is not a regular file")
		}
	}
	// O_NONBLOCK prevents a raced replacement with a FIFO from hanging
	// before the descriptor can be checked. Regular files are unaffected.
	f, err := l.root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		// A raced symlink can only fail here: Root prevents escape.
		return Node{}, diagnostic(CodeReadFile, site, path, "included file cannot be opened")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return Node{}, diagnostic(CodeReadFile, site, path, "include is not a regular file")
	}
	if info.Size() > maxFileBytes || l.bytes+info.Size() > maxBundleBytes {
		return Node{}, diagnostic(CodeSizeLimit, site, path, "YAML size limit exceeded")
	}
	remaining := int64(maxBundleBytes) - l.bytes
	limit := int64(maxFileBytes) + 1
	if remaining+1 < limit {
		limit = remaining + 1
	}
	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return Node{}, diagnostic(CodeReadFile, site, path, "YAML file cannot be read")
	}
	if len(data) > maxFileBytes || int64(len(data)) > remaining {
		return Node{}, diagnostic(CodeSizeLimit, site, path, "YAML size limit exceeded")
	}
	l.bytes += int64(len(data))
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		return Node{}, diagnostic(CodeYAML, yamlErrorPosition(name, err), path, "invalid YAML")
	}
	var second yaml.Node
	if err := dec.Decode(&second); !errors.Is(err, io.EOF) {
		if err != nil {
			return Node{}, diagnostic(CodeYAML, yamlErrorPosition(name, err), path, "invalid YAML")
		}
		return Node{}, diagnostic(CodeMultipleDocs, Position{File: name, Line: second.Line, Column: second.Column}, path, "multiple YAML documents are unsupported")
	}
	if len(doc.Content) != 1 {
		return Node{}, diagnostic(CodeUnsupportedForm, Position{File: name, Line: 1, Column: 1}, path, "empty YAML document")
	}
	l.active[name] = true
	defer delete(l.active, name)
	return l.build(doc.Content[0], name, path, refs, depth, make(map[*yaml.Node]bool), 0, syntaxDepth)
}

func yamlErrorPosition(name string, err error) Position {
	// yaml.v3 exposes parse line only in error text, not as structured data.
	// This bounded extraction is for a source coordinate, never policy.
	at := Position{File: name}
	var line int
	if _, scanErr := fmt.Sscanf(err.Error(), "yaml: line %d:", &line); scanErr == nil && line > 0 {
		at.Line = line
	}
	return at
}

func (l *loader) build(raw *yaml.Node, file, path string, refs []Reference, includeDepth int, aliases map[*yaml.Node]bool, aliasDepth, syntaxDepth int) (Node, *Diagnostic) {
	l.nodes++
	pos := Position{File: file, Line: raw.Line, Column: raw.Column}
	l.retained += int64(192 + len(path) + len(refs)*48)
	if l.nodes > maxExpandedNodes || l.retained > maxRetainedBytes || len(path) > maxPathBytes {
		return Node{}, diagnostic(CodeSyntaxLimit, pos, path, "expanded syntax budget exceeded")
	}
	if syntaxDepth > maxSyntaxDepth {
		return Node{}, diagnostic(CodeSyntaxLimit, pos, path, "syntax depth limit exceeded")
	}
	if raw.Kind == yaml.AliasNode {
		if raw.Alias == nil || aliases[raw.Alias] {
			return Node{}, diagnostic(CodeAliasCycle, pos, path, "alias cycle")
		}
		if aliasDepth >= maxAliasDepth {
			return Node{}, diagnostic(CodeAliasLimit, pos, path, "alias depth limit exceeded")
		}
		aliases[raw.Alias] = true
		defer delete(aliases, raw.Alias)
		aliasRefs := append(append([]Reference(nil), refs...), Reference{Kind: ReferenceAlias, At: pos})
		return l.build(raw.Alias, file, path, aliasRefs, includeDepth, aliases, aliasDepth+1, syntaxDepth+1)
	}
	if raw.Tag == "!include" {
		if raw.Kind != yaml.ScalarNode || strings.TrimSpace(raw.Value) == "" {
			return Node{}, diagnostic(CodeUnsupportedForm, pos, path, "include requires a file path")
		}
		if filepath.IsAbs(raw.Value) {
			return Node{}, diagnostic(CodeIncludeOutside, pos, path, "include path escapes bundle")
		}
		name := filepath.Clean(filepath.Join(filepath.Dir(file), raw.Value))
		if !safeName(name) {
			return Node{}, diagnostic(CodeIncludeOutside, pos, path, "include path escapes bundle")
		}
		includeRefs := append(append([]Reference(nil), refs...), Reference{Kind: ReferenceInclude, At: pos})
		return l.parseFile(name, path, includeRefs, includeDepth+1, syntaxDepth+1, pos)
	}
	n := Node{source: pos, path: path, references: append([]Reference(nil), refs...)}
	if raw.Tag == "!secret" {
		if raw.Kind != yaml.ScalarNode || strings.TrimSpace(raw.Value) == "" {
			return Node{}, diagnostic(CodeUnsupportedForm, pos, path, "secret requires a reference name")
		}
		n.kind, n.secretKey = KindSecretRef, raw.Value
		return n, nil
	}
	if strings.HasPrefix(raw.Tag, "!") && !strings.HasPrefix(raw.Tag, "!!") {
		return Node{}, diagnostic(CodeUnsupportedTag, pos, path, "custom YAML tag outside profile")
	}
	switch raw.Kind {
	case yaml.ScalarNode:
		kind, ok := scalarKind(raw.Tag)
		if !ok {
			return Node{}, diagnostic(CodeUnsupportedTag, pos, path, "scalar tag outside profile")
		}
		n.kind, n.scalar = KindScalar, Scalar{kind: kind, text: raw.Value}
	case yaml.SequenceNode:
		if raw.Tag != "!!seq" {
			return Node{}, diagnostic(CodeUnsupportedTag, pos, path, "sequence tag outside profile")
		}
		n.kind = KindSequence
		for i, child := range raw.Content {
			childPath := fmt.Sprintf("%s[%d]", path, i)
			if len(childPath) > maxPathBytes {
				return Node{}, diagnostic(CodeSyntaxLimit, pos, path, "syntax path limit exceeded")
			}
			value, d := l.build(child, file, childPath, refs, includeDepth, aliases, aliasDepth, syntaxDepth+1)
			if d != nil {
				return Node{}, d
			}
			n.items = append(n.items, value)
		}
	case yaml.MappingNode:
		if raw.Tag != "!!map" {
			return Node{}, diagnostic(CodeUnsupportedTag, pos, path, "mapping tag outside profile")
		}
		n.kind = KindMapping
		seen := make(map[string]bool, len(raw.Content)/2)
		for i := 0; i < len(raw.Content); i += 2 {
			key := raw.Content[i]
			keyPos := Position{File: file, Line: key.Line, Column: key.Column}
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return Node{}, diagnostic(CodeUnsupportedForm, keyPos, path, "mapping key must be a string")
			}
			if len(key.Value) > maxKeyBytes || len(path)+1+len(key.Value) > maxPathBytes {
				return Node{}, diagnostic(CodeSyntaxLimit, keyPos, path, "syntax key or path limit exceeded")
			}
			childPath := path + "." + key.Value
			if seen[key.Value] {
				return Node{}, diagnostic(CodeDuplicateKey, keyPos, childPath, "duplicate mapping key")
			}
			seen[key.Value] = true
			value, d := l.build(raw.Content[i+1], file, childPath, refs, includeDepth, aliases, aliasDepth, syntaxDepth+1)
			if d != nil {
				return Node{}, d
			}
			n.entries = append(n.entries, Entry{Key: key.Value, KeySource: keyPos, Value: value})
		}
	default:
		return Node{}, diagnostic(CodeUnsupportedForm, pos, path, "YAML node kind outside profile")
	}
	return n, nil
}

func scalarKind(tag string) (ScalarKind, bool) {
	switch tag {
	case "!!str":
		return ScalarString, true
	case "!!bool":
		return ScalarBool, true
	case "!!int":
		return ScalarInteger, true
	case "!!float":
		return ScalarFloat, true
	case "!!null":
		return ScalarNull, true
	default:
		return ScalarInvalid, false
	}
}

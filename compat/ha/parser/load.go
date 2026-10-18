package parser

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

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
	maxSourceFiles   = 256
)

type loader struct {
	root              *os.Root
	sources           map[string]SourceFile
	used              []string
	profile           SyntaxProfile
	bytes             int64
	nodes             int
	retained          int64
	active            map[string]bool
	trace             *[]string
	prechargedSources bool
}

// Load parses a single YAML document inside bundleRoot. The entry and every
// !include are opened through os.Root, so symlink replacement cannot turn a
// checked path into a read outside the bundle. Files are size checked before
// allocation and limited again while streaming.
func Load(bundleRoot, entry string) (*Document, error) {
	return LoadProfile(bundleRoot, entry, SyntaxProfileCore)
}

// LoadProfile uses the same confined loader with an explicitly selected grammar.
func LoadProfile(bundleRoot, entry string, profile SyntaxProfile) (*Document, error) {
	if !safeName(entry) {
		return nil, diagnostic(CodeInvalidEntry, Position{File: entry, Line: 1, Column: 1}, "$", "entry must be a local YAML file")
	}
	if profile.String() == "" {
		return nil, diagnostic(CodeUnsupportedForm, Position{File: entry, Line: 1, Column: 1}, "$", "unknown syntax profile")
	}
	root, err := os.OpenRoot(bundleRoot)
	if err != nil {
		return nil, diagnostic(CodeReadFile, Position{File: entry, Line: 1, Column: 1}, "$", "bundle root cannot be opened")
	}
	defer root.Close()
	l := loader{root: root, sources: make(map[string]SourceFile), profile: profile, active: make(map[string]bool)}
	return l.load(entry)
}

// NewSourceFile creates a detached, exact source snapshot for persistence or
// restoration. An uninitialized SourceFile is rejected by LoadSources.
func NewSourceFile(path string, data []byte) (SourceFile, error) {
	if len(path) > maxPathBytes || !safeName(path) || filepath.Clean(path) != path || len(data) > maxFileBytes {
		return SourceFile{}, diagnostic(CodeInvalidEntry, Position{File: path, Line: 1, Column: 1}, "$", "invalid source snapshot")
	}
	copyData := append([]byte(nil), data...)
	return SourceFile{path: path, data: copyData, hash: sha256.Sum256(copyData)}, nil
}

// LoadSources recompiles persisted exact bytes through the same grammar and
// include expansion. It never accesses the filesystem.
func LoadSources(entry string, files []SourceFile, profile SyntaxProfile) (*Document, error) {
	l, err := snapshotLoader(entry, files, profile)
	if err != nil {
		return nil, err
	}
	doc, err := l.load(entry)
	if err != nil {
		return nil, err
	}
	if len(doc.sources) != len(files) {
		return nil, diagnostic(CodeUnsupportedForm, Position{File: entry, Line: 1, Column: 1}, "$", "source snapshot contains unused files")
	}
	return doc, nil
}

func snapshotLoader(entry string, files []SourceFile, profile SyntaxProfile) (*loader, error) {
	if !safeName(entry) {
		return nil, diagnostic(CodeInvalidEntry, Position{File: entry, Line: 1, Column: 1}, "$", "entry must be a local YAML file")
	}
	if profile.String() == "" {
		return nil, diagnostic(CodeUnsupportedForm, Position{File: entry, Line: 1, Column: 1}, "$", "unknown syntax profile")
	}
	if len(files) > maxSourceFiles {
		return nil, diagnostic(CodeSizeLimit, Position{File: entry, Line: 1, Column: 1}, "$", "source snapshot file limit exceeded")
	}
	l := &loader{sources: make(map[string]SourceFile, len(files)), profile: profile, active: make(map[string]bool)}
	var totalBytes int64
	var retainedInput int64
	for _, file := range files {
		if len(file.path) > maxPathBytes || !safeName(file.path) || filepath.Clean(file.path) != file.path || len(file.data) > maxFileBytes {
			return nil, diagnostic(CodeInvalidEntry, Position{File: file.path, Line: 1, Column: 1}, "$", "invalid source snapshot")
		}
		totalBytes += int64(len(file.data))
		retainedInput += int64(len(file.data) + len(file.path) + 128)
		if totalBytes > maxBundleBytes {
			return nil, diagnostic(CodeSizeLimit, Position{File: file.path, Line: 1, Column: 1}, "$", "source snapshot size limit exceeded")
		}
		if retainedInput > maxRetainedBytes {
			return nil, diagnostic(CodeSizeLimit, Position{File: file.path, Line: 1, Column: 1}, "$", "source snapshot retained budget exceeded")
		}
		if sha256.Sum256(file.data) != file.hash {
			return nil, diagnostic(CodeInvalidEntry, Position{File: file.path, Line: 1, Column: 1}, "$", "source snapshot hash mismatch")
		}
		if _, exists := l.sources[file.path]; exists {
			return nil, diagnostic(CodeDuplicateKey, Position{File: file.path, Line: 1, Column: 1}, "$", "duplicate source snapshot")
		}
		// SourceFile's bytes are private and NewSourceFile already detached
		// caller-owned input, so a value copy is an immutable snapshot.
		l.sources[file.path] = file
	}
	return l, nil
}

func (l *loader) load(entry string) (*Document, error) {
	n, d := l.parseFile(filepath.Clean(entry), "$", nil, 0, 0, Position{File: entry, Line: 1, Column: 1})
	if d != nil {
		return nil, d
	}
	if d = validateProfile(n, l.profile); d != nil {
		return nil, d
	}
	sources := make([]SourceFile, 0, len(l.used))
	for _, name := range l.used {
		sources = append(sources, l.sources[name])
	}
	return &Document{root: n, profile: l.profile, sources: sources}, nil
}

func safeName(name string) bool {
	if name == "" || len(name) > maxPathBytes || !utf8.ValidString(name) || strings.IndexByte(name, 0) >= 0 || strings.HasSuffix(name, string(filepath.Separator)) || !filepath.IsLocal(name) {
		return false
	}
	ext := filepath.Ext(name)
	return ext == ".yaml" || ext == ".yml"
}

func (l *loader) parseFile(name, path string, refs []Reference, depth, syntaxDepth int, site Position) (Node, *Diagnostic) {
	if l.trace != nil && !containsString(*l.trace, name) {
		*l.trace = append(*l.trace, name)
	}
	if depth > maxIncludeDepth {
		return Node{}, diagnostic(CodeIncludeDepth, site, path, "include depth limit exceeded")
	}
	if l.active[name] {
		return Node{}, diagnostic(CodeIncludeCycle, site, path, "include cycle")
	}
	if !safeName(name) {
		return Node{}, diagnostic(CodeIncludeOutside, site, path, "include path is outside the supported bundle profile")
	}
	file, exists := l.sources[name]
	if !exists {
		if l.root == nil {
			return Node{}, diagnostic(CodeReadFile, site, path, "source snapshot is missing")
		}
		if len(l.sources) >= maxSourceFiles {
			return Node{}, diagnostic(CodeSizeLimit, site, path, "source file limit exceeded")
		}
		data, d := l.readFile(name, path, site)
		if d != nil {
			return Node{}, d
		}
		file = SourceFile{path: name, data: data, hash: sha256.Sum256(data)}
		l.sources[name] = file
		l.used = append(l.used, name)
		l.retained += int64(len(data) + len(name) + 128)
		if l.retained > maxRetainedBytes {
			return Node{}, diagnostic(CodeSyntaxLimit, site, path, "retained syntax and source budget exceeded")
		}
	} else if l.root == nil && !containsString(l.used, name) {
		l.used = append(l.used, name)
		if !l.prechargedSources {
			l.retained += int64(len(file.data) + len(name) + 128)
		}
		if l.retained > maxRetainedBytes {
			return Node{}, diagnostic(CodeSyntaxLimit, site, path, "retained syntax and source budget exceeded")
		}
	}
	if len(file.data) > maxFileBytes || l.bytes+int64(len(file.data)) > maxBundleBytes {
		return Node{}, diagnostic(CodeSizeLimit, site, path, "YAML size limit exceeded")
	}
	l.bytes += int64(len(file.data))
	dec := yaml.NewDecoder(bytes.NewReader(file.data))
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

func containsString(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}

func (l *loader) readFile(name, path string, site Position) ([]byte, *Diagnostic) {
	// This excludes symlinks altogether. os.Root remains the confinement
	// boundary if a component is replaced between Lstat and Open.
	parts := strings.Split(name, string(filepath.Separator))
	for i := range parts {
		component := filepath.Join(parts[:i+1]...)
		info, err := l.root.Lstat(component)
		if err != nil {
			return nil, diagnostic(CodeReadFile, site, path, "included file cannot be inspected")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, diagnostic(CodeIncludeOutside, site, path, "symlink include is outside the supported bundle profile")
		}
		if i == len(parts)-1 && !info.Mode().IsRegular() {
			return nil, diagnostic(CodeReadFile, site, path, "include is not a regular file")
		}
	}
	// O_NONBLOCK prevents a raced replacement with a FIFO from hanging
	// before the descriptor can be checked. Regular files are unaffected.
	f, err := l.root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		// A raced symlink can only fail here: Root prevents escape.
		return nil, diagnostic(CodeReadFile, site, path, "included file cannot be opened")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, diagnostic(CodeReadFile, site, path, "include is not a regular file")
	}
	if info.Size() > maxFileBytes || l.bytes+info.Size() > maxBundleBytes {
		return nil, diagnostic(CodeSizeLimit, site, path, "YAML size limit exceeded")
	}
	remaining := int64(maxBundleBytes) - l.bytes
	limit := int64(maxFileBytes) + 1
	if remaining+1 < limit {
		limit = remaining + 1
	}
	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return nil, diagnostic(CodeReadFile, site, path, "YAML file cannot be read")
	}
	if len(data) > maxFileBytes || int64(len(data)) > remaining {
		return nil, diagnostic(CodeSizeLimit, site, path, "YAML size limit exceeded")
	}
	return data, nil
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
	if raw.Tag == "!input" && l.profile == SyntaxProfileBlueprintDefinition {
		if raw.Kind != yaml.ScalarNode || !validInputName(raw.Value) {
			return Node{}, diagnostic(CodeBlueprintInputReference, pos, path, "input reference requires a nonempty bounded name")
		}
		n.kind, n.scalar = KindInputRef, Scalar{kind: ScalarString, text: raw.Value}
		return n, nil
	}
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

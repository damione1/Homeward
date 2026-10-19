package dashboard

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"strings"

	"github.com/damione1/Homeward/compat/ha/parser"
)

const (
	maxViews       = 128
	maxSections    = 256
	maxCards       = 2048
	maxCardDepth   = 16
	maxConditions  = 2048
	maxDiagnostics = 512
)

type compiler struct {
	result     Result
	cards      int
	sections   int
	conditions int
	overflow   bool
}

// Import reads the entry and includes once through the shared confined loader.
func Import(bundleRoot string, source Source) (Result, error) {
	if !source.valid() {
		return Result{}, ErrInvalidInput
	}
	doc, err := parser.LoadProfile(bundleRoot, source.entry, parser.SyntaxProfileLovelace)
	if err != nil {
		return syntaxFailure(err), nil
	}
	return Compile(source, doc)
}

// Restore never touches the filesystem. LoadSources verifies every detached
// snapshot hash and rejects missing, duplicate or unused included files.
func Restore(source Source, files []parser.SourceFile) (Result, error) {
	if !source.valid() {
		return Result{}, ErrInvalidInput
	}
	doc, err := parser.LoadSources(source.entry, files, parser.SyntaxProfileLovelace)
	if err != nil {
		return syntaxFailure(err), nil
	}
	return Compile(source, doc)
}

func syntaxFailure(err error) Result {
	var parsed *parser.Diagnostic
	if errors.As(err, &parsed) {
		return Result{status: StatusFatal, diagnostics: []Diagnostic{{code: CodeSyntax, syntaxCode: parsed.Code, location: Location{at: parsed.At, path: parsed.Path}, reason: parsed.Message}}}
	}
	return Result{status: StatusFatal, diagnostics: []Diagnostic{{code: CodeSyntax, reason: err.Error()}}}
}

// Compile accepts only a document returned by the Lovelace syntax profile.
// Every unknown semantic subtree is retained and cannot become executable.
func Compile(source Source, doc *parser.Document) (Result, error) {
	if !source.valid() || doc == nil || doc.Profile() != parser.LovelaceProfile {
		return Result{}, ErrInvalidInput
	}
	files := doc.Sources()
	if len(files) == 0 || files[0].Path() != source.entry {
		return Result{}, ErrInvalidInput
	}
	c := compiler{result: Result{status: StatusSupported, source: source, sources: files}}
	digest := bundleDigest(source, files)
	definition := Definition{source: source, profile: Profile, digest: digest}
	root := doc.Root()
	if root.Kind() != parser.KindMapping {
		c.fatal(CodeInvalidValue, root, "dashboard root must be a mapping")
		return c.result, nil
	}
	views, hasViews := root.Lookup("views")
	_, hasStrategy := root.Lookup("strategy")
	if !hasViews && !hasStrategy {
		c.fatal(CodeMissingField, root, "views or strategy is required")
		return c.result, nil
	}
	if hasViews && views.Kind() != parser.KindSequence {
		c.fatal(CodeInvalidValue, views, "views must be a sequence")
		return c.result, nil
	}
	if hasViews && len(views.Items()) > maxViews {
		c.fatal(CodeInvalidValue, views, "view count exceeds dashboard budget")
		return c.result, nil
	}
	if hasViews && hasStrategy {
		c.fatal(CodeInvalidValue, root, "views and runtime strategy are mutually exclusive")
		return c.result, nil
	}
	rootBlocked := false
	for _, entry := range root.Entries() {
		switch entry.Key {
		case "views":
		case "strategy":
			c.unsupported(CodeUnsupportedVariant, entry.Value, "runtime strategy is not compiled")
			rootBlocked = true
		default:
			c.unknown(entry, "dashboard property outside profile")
			rootBlocked = true
		}
	}
	if hasViews {
		seen := make(map[string]bool)
		for _, node := range views.Items() {
			view := c.view(node)
			if view.path != "" {
				if seen[view.path] {
					c.fatal(CodeInvalidValue, node, "duplicate stable view path")
					return c.result, nil
				}
				seen[view.path] = true
			}
			definition.views = append(definition.views, view)
			if c.overflow {
				c.fatal(CodeInvalidValue, node, "dashboard projection budget exceeded")
				return c.result, nil
			}
		}
	}
	if rootBlocked {
		for i := range definition.views {
			disableView(&definition.views[i])
		}
	}
	if c.overflow {
		c.fatal(CodeInvalidValue, root, "dashboard projection budget exceeded")
		return c.result, nil
	}
	definition.requirements = collectRequirements(definition.views)
	c.result.definition = definition
	c.result.hasDefinition = true
	if len(c.result.diagnostics) > 0 {
		c.result.status = StatusUnsupported
	}
	return c.result, nil
}

func (c *compiler) fatal(code Code, node parser.Node, reason string) {
	c.result.status = StatusFatal
	c.result.hasDefinition = false
	if len(c.result.diagnostics) >= maxDiagnostics {
		c.result.diagnostics[maxDiagnostics-1] = Diagnostic{code: code, location: locate(node), reason: reason}
		return
	}
	c.result.diagnostics = append(c.result.diagnostics, Diagnostic{code: code, location: locate(node), reason: reason})
}

func (c *compiler) addDiagnostic(code Code, node parser.Node, reason string) {
	if len(c.result.diagnostics) >= maxDiagnostics {
		c.overflow = true
		return
	}
	c.result.diagnostics = append(c.result.diagnostics, Diagnostic{code: code, location: locate(node), reason: reason})
}

func (c *compiler) unsupported(code Code, node parser.Node, reason string) {
	c.addDiagnostic(code, node, reason)
	if len(c.result.unsupported) >= maxDiagnostics {
		c.overflow = true
		return
	}
	c.result.unsupported = append(c.result.unsupported, Unsupported{code: code, location: locate(node), node: node})
}

func (c *compiler) unknown(entry parser.Entry, reason string) {
	beforeDiagnostics := len(c.result.diagnostics)
	beforeUnsupported := len(c.result.unsupported)
	c.unsupported(CodeUnknownField, entry.Value, reason)
	if len(c.result.diagnostics) > beforeDiagnostics {
		c.result.diagnostics[beforeDiagnostics].location.at = entry.KeySource
	}
	if len(c.result.unsupported) > beforeUnsupported {
		c.result.unsupported[beforeUnsupported].location.at = entry.KeySource
	}
}

func locate(node parser.Node) Location {
	return Location{at: node.Source(), path: node.Path(), references: node.References()}
}

func validSegment(value string, limit int) bool {
	if len(value) == 0 || len(value) > limit {
		return false
	}
	for i := 0; i < len(value); i++ {
		b := value[i]
		if (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || (i > 0 && (b == '_' || b == '-')) {
			continue
		}
		return false
	}
	return true
}

func validNavigation(value string) bool {
	if len(value) < 2 || len(value) > 1024 || strings.HasPrefix(value, "//") {
		return false
	}
	if value[0] != '/' && value[0] != '#' {
		return false
	}
	for _, r := range value {
		if r <= 0x20 || r == 0x7f || r == '\\' {
			return false
		}
	}
	return true
}

func bundleDigest(source Source, files []parser.SourceFile) [sha256.Size]byte {
	h := sha256.New()
	writePart(h, []byte("homeward-lovelace-bundle-v1"))
	writePart(h, []byte(parser.LovelaceProfile))
	writePart(h, []byte(source.entry))
	for _, file := range files {
		writePart(h, []byte(file.Path()))
		sum := file.SHA256()
		writePart(h, sum[:])
	}
	var digest [sha256.Size]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

func writePart(h hash.Hash, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = h.Write(length[:])
	_, _ = h.Write(value)
}

func disableView(view *View) {
	view.status = StatusUnsupported
	for i := range view.visibility {
		view.visibility[i].status = StatusUnsupported
	}
	for i := range view.sections {
		disableSection(&view.sections[i])
	}
}

func disableSection(section *Section) {
	section.status = StatusUnsupported
	for i := range section.visibility {
		section.visibility[i].status = StatusUnsupported
	}
	for i := range section.cards {
		disableCard(&section.cards[i])
	}
}

func disableCard(card *Card) {
	card.status = StatusUnsupported
	for i := range card.actions {
		card.actions[i].status = StatusUnsupported
	}
	for i := range card.visibility {
		card.visibility[i].status = StatusUnsupported
	}
	for i := range card.children {
		disableCard(&card.children[i])
	}
}

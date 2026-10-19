package parser

// lovelaceRule checks only the document envelope. The semantic compiler owns
// the smaller native profile and records every unknown Lovelace subtree. Using
// closed nested syntax rules here would reject the first custom card before
// its position, source and siblings could be projected.
func lovelaceRule() *rule {
	root := mapping(map[string]*rule{
		"views":    sequence(&rule{open: true}),
		"strategy": &rule{open: true},
	})
	root.dynamic = &rule{open: true}
	return root
}

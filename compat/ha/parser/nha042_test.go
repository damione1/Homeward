package parser_test

import (
	"testing"

	"github.com/damione1/Homeward/compat/ha/parser"
)

func TestNHA042ScriptGrammarIsAdditiveAndHistoricalProfileStaysClosed(t *testing.T) {
	file, err := parser.NewSourceFile("scripts.yaml", []byte("script:\n  nha042_wait:\n    sequence:\n    - wait_template: '{{ true }}'\n      timeout: {seconds: 0}\n    - delay: {milliseconds: 1}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.LoadSources("scripts.yaml", []parser.SourceFile{file}, parser.SyntaxProfileLogic); err != nil {
		t.Fatal(err)
	}
	if _, err := parser.LoadSources("scripts.yaml", []parser.SourceFile{file}, parser.SyntaxProfileCore); err == nil {
		t.Fatal("historical grammar widened")
	}
}

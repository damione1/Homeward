package template

import (
	"context"
	"testing"
	"time"
)

// FuzzNHA043Admission probes the untrusted lexer/parser boundary and the
// admitted evaluator. All errors are valid outcomes; a panic or hang is not.
func FuzzNHA043Admission(f *testing.F) {
	for _, seed := range []string{
		"{{ missing | default('fallback') }}",
		"{{ now().date() }}",
		"{{ value_json[\"battery\"] }}",
		"{% include '/etc/passwd' %}",
		"{{ (((((1))))) }}",
		"{{ -----1 }}",
		"{{ 'unterminated }}",
		"{{ [1, 2, none] }}",
		"{{ ''.replace('', 'x') }}",
		"\xff{{ 1 }}",
	} {
		f.Add(seed)
	}
	input, err := NewInputs(nil, nil, time.Date(2026, 3, 8, 4, 30, 0, 0, time.UTC), time.UTC)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, source string) {
		program, err := Compile(source)
		if err == nil {
			_, _ = Evaluate(context.Background(), program, input, DefaultLimits())
		}
	})
}

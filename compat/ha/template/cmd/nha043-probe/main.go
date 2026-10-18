// Command nha043-probe evaluates bounded synthetic differential fixtures.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/domain"
)

type fixture struct {
	ClockUTC string `json:"clock_utc"`
	Timezone string `json:"timezone"`
	States   []struct {
		EntityID   string                     `json:"entity_id"`
		State      string                     `json:"state"`
		Attributes map[string]json.RawMessage `json:"attributes"`
	} `json:"states"`
	Variables map[string]json.RawMessage `json:"variables"`
	Cases     []struct {
		ID       string `json:"id"`
		Template string `json:"template"`
	} `json:"cases"`
}

type outcome struct {
	Text   string `json:"text"`
	Native any    `json:"native"`
}

func shape(value template.Value, rendered string) (any, error) {
	kind, err := value.Kind()
	if err != nil {
		return nil, err
	}
	switch kind {
	case template.KindNone:
		return map[string]any{"type": "none", "value": nil}, nil
	case template.KindBool:
		v, _ := value.Bool()
		return map[string]any{"type": "bool", "value": v}, nil
	case template.KindInt:
		v, _ := value.Int()
		return map[string]any{"type": "int", "value": v}, nil
	case template.KindFloat:
		v, _ := value.Float()
		return map[string]any{"type": "float", "value": v}, nil
	case template.KindString:
		v, _ := value.StringValue()
		return map[string]any{"type": "str", "value": v}, nil
	case template.KindList:
		items, _ := value.Items()
		result := make([]any, len(items))
		for i, item := range items {
			result[i], err = shape(item, "")
			if err != nil {
				return nil, err
			}
		}
		return map[string]any{"type": "list", "category": "list", "render_result": rendered, "value": result}, nil
	case template.KindUndefined, template.KindObject, template.KindDate, template.KindDateTime, template.KindDuration, template.KindStateRef:
		return nil, fmt.Errorf("unsupported native fixture kind %v", kind)
	default:
		return nil, fmt.Errorf("invalid native fixture kind %v", kind)
	}
}

func run(input []byte) (any, error) {
	if len(input) == 0 || len(input) > 65536 {
		return nil, fmt.Errorf("fixture size")
	}
	var f fixture
	if err := json.Unmarshal(input, &f); err != nil {
		return nil, err
	}
	if len(f.Cases) < 10 || len(f.Cases) > 64 {
		return nil, fmt.Errorf("fixture count")
	}
	now, err := time.Parse(time.RFC3339, f.ClockUTC)
	if err != nil {
		return nil, err
	}
	zone, err := time.LoadLocation(f.Timezone)
	if err != nil {
		return nil, err
	}
	states := make([]template.State, 0, len(f.States))
	for _, raw := range f.States {
		alias, err := domain.ParseHAAlias(raw.EntityID)
		if err != nil {
			return nil, err
		}
		attrs := make([]template.Binding, 0, len(raw.Attributes))
		for key, bytes := range raw.Attributes {
			value, err := template.DecodeJSON(bytes)
			if err != nil {
				return nil, err
			}
			attrs = append(attrs, template.Binding{Name: key, Value: value})
		}
		state, err := template.NewState(alias, raw.State, attrs)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	variables := make([]template.Binding, 0, len(f.Variables))
	for key, bytes := range f.Variables {
		value, err := template.DecodeJSON(bytes)
		if err != nil {
			return nil, err
		}
		variables = append(variables, template.Binding{Name: key, Value: value})
	}
	inputs, err := template.NewInputs(states, variables, now, zone)
	if err != nil {
		return nil, err
	}
	results := make([]map[string]any, 0, len(f.Cases))
	for _, item := range f.Cases {
		program, err := template.Compile(item.Template)
		if err != nil {
			return nil, fmt.Errorf("%s compile: %w", item.ID, err)
		}
		result, err := template.Evaluate(context.Background(), program, inputs, template.DefaultLimits())
		if err != nil {
			return nil, fmt.Errorf("%s evaluate: %w", item.ID, err)
		}
		native, err := shape(result.Native(), result.Text())
		if err != nil {
			return nil, err
		}
		results = append(results, map[string]any{"id": item.ID, "raw": outcome{Text: result.Text(), Native: native}})
	}
	return map[string]any{"cases": results}, nil
}

func main() {
	input, err := io.ReadAll(io.LimitReader(os.Stdin, 65537))
	if err != nil {
		panic(err)
	}
	result, err := run(input)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		panic(err)
	}
}

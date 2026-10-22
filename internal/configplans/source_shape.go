package configplans

import (
	"bytes"
	"encoding/json"
	"io"

	"go.yaml.in/yaml/v3"
)

// These structural guards borrow the existing strictJSON token pattern used by
// compat/ha/nha045. They do not admit a business field or replace any compiler.
// All failures remain fixed diagnostics; source keys/values never enter errors.
func uniqueJSON(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	nodes := 0
	var visit func(int) bool
	visit = func(depth int) bool {
		nodes++
		if depth > 64 || nodes > 65536 {
			return false
		}
		token, e := decoder.Token()
		if e != nil {
			return false
		}
		delimiter, isDelimiter := token.(json.Delim)
		if !isDelimiter {
			return true
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, e := decoder.Token()
				if e != nil {
					return false
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return false
				}
				seen[name] = true
				if !visit(depth + 1) {
					return false
				}
			}
			end, e := decoder.Token()
			return e == nil && end == json.Delim('}')
		case '[':
			for decoder.More() {
				if !visit(depth + 1) {
					return false
				}
			}
			end, e := decoder.Token()
			return e == nil && end == json.Delim(']')
		default:
			return false
		}
	}
	if !visit(0) {
		return false
	}
	_, e := decoder.Token()
	return e == io.EOF
}

// A shared expansion budget bounds both the shape guard and subsequent
// yamlValue traversal. Alias DAGs consume their expanded occurrence count;
// depth and active-path checks remain independent of repeated-node sharing.
const yamlShapeNodeBudget = 65536

func uniqueYAML(node *yaml.Node, depth int, active map[*yaml.Node]bool) bool {
	remaining := yamlShapeNodeBudget
	return uniqueYAMLWithinBudget(node, depth, active, &remaining)
}

func uniqueYAMLWithinBudget(node *yaml.Node, depth int, active map[*yaml.Node]bool, remaining *int) bool {
	if node == nil || depth > 64 || active[node] || *remaining <= 0 {
		return false
	}
	*remaining -= 1
	active[node] = true
	defer delete(active, node)
	switch node.Kind {
	case yaml.MappingNode:
		if len(node.Content)%2 != 0 {
			return false
		}
		seen := map[string]bool{}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key == nil || key.Kind != yaml.ScalarNode || seen[key.Value] || *remaining <= 0 {
				return false
			}
			*remaining -= 1
			seen[key.Value] = true
			if !uniqueYAMLWithinBudget(node.Content[i+1], depth+1, active, remaining) {
				return false
			}
		}
		return true
	case yaml.SequenceNode:
		for _, child := range node.Content {
			if !uniqueYAMLWithinBudget(child, depth+1, active, remaining) {
				return false
			}
		}
		return true
	case yaml.AliasNode:
		return uniqueYAMLWithinBudget(node.Alias, depth+1, active, remaining)
	case yaml.ScalarNode:
		return true
	case yaml.DocumentNode:
		if len(node.Content) != 1 {
			return false
		}
		return uniqueYAMLWithinBudget(node.Content[0], depth+1, active, remaining)
	default:
		return false
	}
}

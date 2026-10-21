package integrationadmin

import (
	"bytes"
	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/integrationadmin/v1"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/storage"
	"go.yaml.in/yaml/v3"
	"io"
	"sort"
)

func decodeNode(raw []byte) (*yaml.Node, error) {
	if len(raw) < 1 || len(raw) > 1<<20 {
		return nil, ErrInvalid
	}
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	if dec.Decode(&doc) != nil || len(doc.Content) != 1 {
		return nil, ErrInvalid
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, ErrInvalid
	}
	count := 0
	var admit func(*yaml.Node, int) bool
	admit = func(n *yaml.Node, depth int) bool {
		count++
		if n == nil || depth > 64 || count > 65536 || n.Alias != nil || n.Anchor != "" || n.Kind == yaml.AliasNode {
			return false
		}
		if n.Kind == yaml.MappingNode {
			if len(n.Content)%2 != 0 {
				return false
			}
			seen := map[string]bool{}
			for i := 0; i < len(n.Content); i += 2 {
				k := n.Content[i]
				if k.Kind != yaml.ScalarNode || k.Tag != "!!str" || k.Value == "<<" || seen[k.Value] {
					return false
				}
				seen[k.Value] = true
			}
		}
		for _, child := range n.Content {
			if !admit(child, depth+1) {
				return false
			}
		}
		return true
	}
	if !admit(&doc, 0) {
		return nil, ErrInvalid
	}
	return doc.Content[0], nil
}

// clonePublicNode is used only for the public projection. The protected tree
// keeps every comment for unrelated targeted149 transformations and replay.
func clonePublicNode(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	copy := *n
	copy.HeadComment, copy.LineComment, copy.FootComment = "", "", ""
	copy.Content = make([]*yaml.Node, len(n.Content))
	for i, c := range n.Content {
		copy.Content[i] = clonePublicNode(c)
	}
	return &copy
}
func field(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}
func setField(n *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			if value == nil {
				n.Content = append(n.Content[:i], n.Content[i+2:]...)
			} else {
				n.Content[i+1] = value
			}
			return
		}
	}
	if value != nil {
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
	}
}
func scalar(text string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: text}
}
func encodeNode(n *yaml.Node) ([]byte, error) {
	raw, err := yaml.Marshal(n)
	if err != nil || len(raw) > 1<<20 {
		return nil, ErrInvalid
	}
	return raw, nil
}

// Active bytes already passed029 admission and are authenticated by replay.
// Listing them must not stat or re-open an opaque worker executable.
func activeDocument(raw []byte) (*yaml.Node, error) {
	root, err := decodeNode(raw)
	if err != nil || root.Kind != yaml.MappingNode {
		return nil, ErrUnavailable
	}
	return root, nil
}
func admittedDocument(raw []byte) (*yaml.Node, error) {
	root, err := decodeNode(raw)
	if err != nil || root.Kind != yaml.MappingNode {
		return nil, ErrWorkspaceInvalid
	}
	if _, err = config.ParseOperationalWithEnv(raw, func(string) (string, bool) { return "", false }); err != nil {
		return nil, ErrWorkspaceInvalid
	}
	return root, nil
}

type entry struct {
	typ, id, key string
	node         *yaml.Node
	workerIndex  int
}

func entries(root *yaml.Node) ([]entry, error) {
	result := []entry{}
	seen := map[string]bool{}
	add := func(e entry) error {
		if !contracts.IntegrationAdminID(e.id) || seen[e.id] {
			return ErrWorkspaceInvalid
		}
		seen[e.id] = true
		result = append(result, e)
		return nil
	}
	for _, d := range descriptors {
		node := field(root, d.id)
		if node == nil || node.Tag == "!!null" || d.id == "energy" && len(node.Content) == 0 {
			continue
		}
		id := d.fixed
		if id == "" {
			n := field(node, "instance")
			if n == nil || n.Tag != "!!str" {
				return nil, ErrWorkspaceInvalid
			}
			id = n.Value
		}
		if err := add(entry{typ: d.id, id: id, key: d.id, node: node, workerIndex: -1}); err != nil {
			return nil, err
		}
	}
	if workers := field(root, "external_workers"); workers != nil && !(workers.Kind == yaml.ScalarNode && workers.Tag == "!!null") {
		if workers.Kind != yaml.SequenceNode {
			return nil, ErrWorkspaceInvalid
		}
		for i, node := range workers.Content {
			id := field(node, "instance")
			if id == nil || id.Tag != "!!str" {
				return nil, ErrWorkspaceInvalid
			}
			if err := add(entry{id: id.Value, key: "external_workers", node: node, workerIndex: i}); err != nil {
				return nil, err
			}
		}
	}
	if len(result) > 1024 {
		return nil, ErrUnavailable
	}
	sort.Slice(result, func(i, j int) bool { return result[i].id < result[j].id })
	return result, nil
}
func disabled(root *yaml.Node) (map[string]bool, error) {
	off := map[string]bool{}
	if n := field(root, "disabled_integrations"); n != nil && !(n.Kind == yaml.ScalarNode && n.Tag == "!!null") {
		if n.Kind != yaml.SequenceNode || len(n.Content) > 1024 {
			return nil, ErrWorkspaceInvalid
		}
		for _, v := range n.Content {
			if v.Kind != yaml.ScalarNode || v.Tag != "!!str" || off[v.Value] {
				return nil, ErrWorkspaceInvalid
			}
			off[v.Value] = true
		}
	}
	return off, nil
}
func setDisabled(root *yaml.Node, off map[string]bool) {
	names := make([]string, 0, len(off))
	for id, v := range off {
		if v {
			names = append(names, id)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		setField(root, "disabled_integrations", nil)
		return
	}
	n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, id := range names {
		n.Content = append(n.Content, scalar(id))
	}
	setField(root, "disabled_integrations", n)
}
func publicEntry(e entry, off bool) (*wire.IntegrationInstance, error) {
	state := wire.DesiredState_DESIRED_STATE_ENABLED
	if off {
		state = wire.DesiredState_DESIRED_STATE_DISABLED
	}
	out := &wire.IntegrationInstance{TypeId: e.typ, IntegrationInstanceId: e.id, DesiredState: state}
	if e.typ == "" {
		opaque := &wire.OpaqueConfiguration{}
		if names := field(e.node, "secret_names"); names != nil && !(names.Kind == yaml.ScalarNode && names.Tag == "!!null") {
			if names.Kind != yaml.SequenceNode {
				return nil, ErrWorkspaceInvalid
			}
			for _, name := range names.Content {
				if name.Tag != "!!str" {
					return nil, ErrWorkspaceInvalid
				}
				opaque.Secrets = append(opaque.Secrets, &wire.SecretReference{Field: "secret_names", IntegrationInstanceId: e.id, Name: name.Value})
			}
		}
		out.Configuration = &wire.IntegrationInstance_Opaque{Opaque: opaque}
		return out, nil
	}
	d, err := findDescriptor(e.typ)
	if err != nil {
		return nil, err
	}
	node := clonePublicNode(e.node)
	c := &wire.PublicConfiguration{SchemaVersion: schemaVersion(d)}
	for _, key := range d.secrets {
		n := field(node, key)
		if n == nil || n.Tag != "!!str" {
			return nil, ErrWorkspaceInvalid
		}
		c.Secrets = append(c.Secrets, &wire.SecretReference{Field: key, IntegrationInstanceId: e.id, Name: n.Value})
		setField(node, key, nil)
	}
	raw, err := encodeNode(node)
	if err != nil || len(raw) > 64<<10 {
		return nil, ErrUnavailable
	}
	c.YamlSource = raw
	out.Configuration = &wire.IntegrationInstance_Editable{Editable: c}
	return out, nil
}
func privateFragment(d descriptor, id string, c *wire.PublicConfiguration) (*yaml.Node, error) {
	if c == nil || c.SchemaVersion != schemaVersion(d) || len(c.YamlSource) < 1 || len(c.YamlSource) > 64<<10 || len(c.Secrets) != len(d.secrets) {
		return nil, ErrInvalid
	}
	node, err := decodeNode(c.YamlSource)
	if err != nil {
		return nil, ErrInvalid
	}
	if d.shape == "object" && node.Kind != yaml.MappingNode || d.shape == "array" && node.Kind != yaml.SequenceNode || d.shape == "string" && (node.Kind != yaml.ScalarNode || node.Tag != "!!str") {
		return nil, ErrInvalid
	}
	// Public credential fields have NO scalar form. Only typed reference slots
	// are accepted; even a plausible secret name in YAML is rejected here.
	if node.Kind == yaml.MappingNode {
		allowed := map[string]bool{}
		for _, key := range d.fields {
			allowed[key] = true
		}
		for i := 0; i < len(node.Content); i += 2 {
			if !allowed[node.Content[i].Value] {
				return nil, ErrInvalid
			}
		}
	}
	refs := map[string]bool{}
	for _, ref := range c.Secrets {
		if ref == nil || ref.IntegrationInstanceId != id || refs[ref.Field] {
			return nil, ErrInvalid
		}
		allowed := false
		for _, name := range d.secrets {
			if name == ref.Field {
				allowed = true
			}
		}
		if !allowed {
			return nil, ErrInvalid
		}
		if _, err := storage.ParseSecretName(ref.Name); err != nil {
			return nil, ErrInvalid
		}
		refs[ref.Field] = true
		setField(node, ref.Field, scalar(ref.Name))
	}
	if d.fixed != "" {
		if id != d.fixed {
			return nil, ErrInvalid
		}
	} else {
		n := field(node, "instance")
		if n == nil || n.Tag != "!!str" || n.Value != id {
			return nil, ErrInvalid
		}
	}
	return node, nil
}

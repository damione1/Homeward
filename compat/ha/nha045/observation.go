package nha045

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Wire structs are boundary representations only. Captures retain admitted
// private values; callers receive encoded snapshots instead of mutable slices.
type runWire struct {
	Key      string  `json:"key"`
	Raw      string  `json:"raw"`
	Parent   *string `json:"parent"`
	Callsite *string `json:"callsite"`
	Object   string  `json:"object"`
}
type waitWire struct {
	Completed   *bool        `json:"completed"`
	RemainingMS *int64       `json:"remaining_ms"`
	Trigger     *triggerWire `json:"trigger"`
}
type triggerWire struct {
	ID       string `json:"id"`
	Index    int    `json:"index"`
	Platform string `json:"platform"`
}
type dataWire struct {
	Outcome    *string   `json:"outcome,omitempty"`
	Entity     *string   `json:"entity,omitempty"`
	Service    *string   `json:"service,omitempty"`
	State      *string   `json:"state,omitempty"`
	Brightness *float64  `json:"brightness,omitempty"`
	Wait       *waitWire `json:"wait,omitempty"`
}
type factWire struct {
	Key    string   `json:"key"`
	Kind   string   `json:"kind"`
	Run    *string  `json:"run"`
	AtMS   int64    `json:"at_ms"`
	Object string   `json:"object"`
	Node   string   `json:"node"`
	Data   dataWire `json:"data"`
}
type edgeWire struct {
	Before string `json:"before"`
	After  string `json:"after"`
}
type streamWire struct {
	Scope string   `json:"scope"`
	Keys  []string `json:"keys"`
}
type runtimeWire struct {
	Name         string       `json:"name"`
	Version      string       `json:"version"`
	Python       *string      `json:"python"`
	Jinja        *string      `json:"jinja"`
	SourceSHA256 []sourceWire `json:"source_sha256"`
}
type sourceWire struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type captureWire struct {
	Schema        string          `json:"schema"`
	Scenario      string          `json:"scenario"`
	FixtureSHA256 string          `json:"fixture_sha256"`
	Runtime       runtimeWire     `json:"runtime"`
	Runs          []runWire       `json:"runs"`
	Facts         []factWire      `json:"facts"`
	Edges         []edgeWire      `json:"edges"`
	Orders        []streamWire    `json:"orders"`
	Evicted       uint64          `json:"evicted"`
	Terminal      *bool           `json:"terminal"`
	Raw           json.RawMessage `json:"raw"`
}
type runBinding struct {
	key      runKey
	raw      rawRunID
	object   objectID
	parent   optionalRunKey
	callsite optionalCallsiteID
}
type fact struct {
	key     factKey
	kind    FactKind
	run     optionalRawRunID
	tick    int64
	object  objectID
	node    nodeKey
	payload admittedPayload
}
type Capture struct {
	wire     captureWire
	scenario scenarioID
	runs     []runBinding
	facts    []fact
	valid    bool
}

func ptr[T any](v T) *T { return &v }
func admittedSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'f' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
func syntheticEntity(s string) bool {
	for _, prefix := range []string{"switch.nha045_", "light.nha045_", "sensor.nha045_"} {
		if len(s) > len(prefix) && len(s) <= 128 && len(s) > 0 && s[:min(len(s), len(prefix))] == prefix {
			for _, c := range s[len(prefix):] {
				if !(c == '_' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
					return false
				}
			}
			return true
		}
	}
	return false
}
func validateData(kind FactKind, d dataWire) error {
	outcome := OutcomeInvalid
	if d.Outcome != nil {
		var err error
		outcome, err = parseOutcome(*d.Outcome)
		if err != nil {
			return err
		}
	}
	switch kind {
	case Admission:
		if outcome != Accepted && outcome != Queued && outcome != SingleBusy && outcome != MaxExceeded {
			return fmt.Errorf("invalid admission outcome")
		}
	case Started, Cancellation:
		if d.Outcome != nil {
			return fmt.Errorf("unexpected stage outcome")
		}
	case Terminal:
		if outcome != Finished && outcome != Cancelled && outcome != TimeoutStopped && outcome != CommandFailed {
			return fmt.Errorf("invalid terminal outcome")
		}
	case Action:
		if d.Entity == nil || !syntheticEntity(*d.Entity) || d.Service == nil || *d.Service != "switch.turn_on" || outcome != Completed && outcome != CommandFailed {
			return fmt.Errorf("invalid synthetic action")
		}
	case State:
		if d.Outcome != nil {
			return fmt.Errorf("state cannot carry an execution outcome")
		}
		if d.Entity == nil || !syntheticEntity(*d.Entity) || d.State == nil {
			return fmt.Errorf("state/entity absent")
		}
		if *d.State != "on" && *d.State != "off" && *d.State != "unknown" {
			return fmt.Errorf("invalid synthetic state")
		}
	case Wait:
		if d.Wait == nil || d.Wait.Completed == nil || outcome != Completed && outcome != TimeoutStopped && outcome != Cancelled {
			return fmt.Errorf("invalid wait observation")
		}
		if d.Wait.RemainingMS != nil && (*d.Wait.RemainingMS < 0 || *d.Wait.RemainingMS > 86400000) {
			return fmt.Errorf("wait remaining outside bound")
		}
		if t := d.Wait.Trigger; t != nil {
			if t.ID != "release" || t.Index != 0 || t.Platform != "event" {
				return fmt.Errorf("unknown trigger projection")
			}
		}
	case FactInvalid:
		return fmt.Errorf("invalid kind")
	default:
		return fmt.Errorf("unhandled kind")
	}
	if kind != Action && kind != State && (d.Entity != nil || d.Service != nil || d.State != nil || d.Brightness != nil) {
		return fmt.Errorf("unexpected entity payload")
	}
	if kind != Action && d.Service != nil {
		return fmt.Errorf("unexpected service")
	}
	if kind != State && (d.State != nil || d.Brightness != nil) {
		return fmt.Errorf("unexpected state payload")
	}
	if kind != Wait && d.Wait != nil {
		return fmt.Errorf("unexpected wait payload")
	}
	if d.Brightness != nil && (math.IsNaN(*d.Brightness) || math.IsInf(*d.Brightness, 0) || *d.Brightness < 0 || *d.Brightness > 255) {
		return fmt.Errorf("invalid brightness")
	}
	return nil
}

func DecodeCapture(data []byte) (Capture, error) {
	// Check the raw boundary before decoding non-nullable numbers into Go
	// zero values. A present numeric zero is valid; JSON null is not.
	var document json.RawMessage
	if err := strictJSON(data, &document, MaxCaptureBytes); err != nil {
		return Capture{}, fail(InvalidObservation, err.Error())
	}
	if err := requireFields(data, "schema", "scenario", "fixture_sha256", "runtime", "runs", "facts", "edges", "orders", "evicted", "terminal", "raw"); err != nil {
		return Capture{}, fail(InvalidObservation, err.Error())
	}
	if err := requireCaptureScalars(data, "evicted"); err != nil {
		return Capture{}, fail(InvalidObservation, err.Error())
	}
	var envelope struct {
		Runtime json.RawMessage   `json:"runtime"`
		Runs    []json.RawMessage `json:"runs"`
		Facts   []json.RawMessage `json:"facts"`
		Edges   []json.RawMessage `json:"edges"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return Capture{}, fail(InvalidObservation, err.Error())
	}
	if err := requireFields(envelope.Runtime, "name", "version", "python", "jinja", "source_sha256"); err != nil {
		return Capture{}, fail(InvalidObservation, err.Error())
	}
	for _, r := range envelope.Runs {
		if err := requireFields(r, "key", "raw", "parent", "callsite", "object"); err != nil {
			return Capture{}, fail(InvalidObservation, err.Error())
		}
	}
	for _, f := range envelope.Facts {
		if err := requireFields(f, "key", "kind", "run", "at_ms", "object", "node", "data"); err != nil {
			return Capture{}, fail(InvalidObservation, err.Error())
		}
		if err := requireCaptureScalars(f, "at_ms"); err != nil {
			return Capture{}, fail(InvalidObservation, err.Error())
		}
		var row struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(f, &row); err != nil {
			return Capture{}, fail(InvalidObservation, err.Error())
		}
		if err := requireFields(row.Data); err != nil {
			return Capture{}, fail(InvalidObservation, err.Error())
		}
		var payload struct {
			Wait json.RawMessage `json:"wait"`
		}
		if err := json.Unmarshal(row.Data, &payload); err != nil {
			return Capture{}, fail(InvalidObservation, err.Error())
		}
		if len(payload.Wait) > 0 {
			if err := requireFields(payload.Wait, "completed", "remaining_ms", "trigger"); err != nil {
				return Capture{}, fail(InvalidObservation, err.Error())
			}
			var wait struct {
				Trigger json.RawMessage `json:"trigger"`
			}
			if err := json.Unmarshal(payload.Wait, &wait); err != nil {
				return Capture{}, fail(InvalidObservation, err.Error())
			}
			if len(wait.Trigger) > 0 && string(wait.Trigger) != "null" {
				if err := requireFields(wait.Trigger, "id", "index", "platform"); err != nil {
					return Capture{}, fail(InvalidObservation, err.Error())
				}
				if err := requireCaptureScalars(wait.Trigger, "index"); err != nil {
					return Capture{}, fail(InvalidObservation, err.Error())
				}
			}
		}
	}
	for _, e := range envelope.Edges {
		if err := requireFields(e, "before", "after"); err != nil {
			return Capture{}, fail(InvalidObservation, err.Error())
		}
	}
	var w captureWire
	if err := strictJSON(data, &w, MaxCaptureBytes); err != nil {
		return Capture{}, fail(InvalidObservation, err.Error())
	}
	return admitCapture(w)
}

func requireCaptureScalars(data []byte, names ...string) error {
	if err := requireFields(data, names...); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, name := range names {
		if strings.TrimSpace(string(fields[name])) == "null" {
			return fmt.Errorf("capture scalar %s cannot be null", name)
		}
	}
	return nil
}
func admitCapture(w captureWire) (Capture, error) {
	bad := func(s string) (Capture, error) { return Capture{}, fail(InvalidObservation, s) }
	if w.Schema != Schema || !admittedSHA(w.FixtureSHA256) || w.Terminal == nil || !*w.Terminal || w.Evicted != 0 {
		return bad("schema, fixture hash, eviction or terminal invalid")
	}
	if _, e := parseID(w.Scenario); e != nil {
		return bad(e.Error())
	}
	if len(w.Runs) == 0 || len(w.Runs) > MaxRuns || len(w.Facts) == 0 || len(w.Facts) > MaxFacts || len(w.Edges) > MaxEdges || len(w.Raw) > MaxCaptureBytes/2 {
		return bad("capture cardinality outside bound")
	}
	if w.Runtime.Name != "go" && w.Runtime.Name != "ha" {
		return bad("unknown runtime")
	}
	if w.Runtime.Name == "ha" && (w.Runtime.Version != HAVersion || w.Runtime.Python == nil || *w.Runtime.Python != HAPython || w.Runtime.Jinja == nil || *w.Runtime.Jinja != HAJinja) {
		return Capture{}, fail(VersionMismatch, "HA runtime drift")
	}
	if w.Runtime.Name == "go" && (w.Runtime.Version != GoVersion || w.Runtime.Python != nil || w.Runtime.Jinja != nil) {
		return Capture{}, fail(VersionMismatch, "Go runtime drift")
	}
	if e := validateSources(w.Runtime); e != nil {
		return Capture{}, e
	}
	scenario, _ := parseID(w.Scenario)
	c := Capture{wire: w, scenario: scenarioID{scenario}, valid: true}
	rawIDs := make(map[string]string)
	keys := make(map[string]string)
	objectsByRaw := make(map[string]string)
	parents := make(map[string]string)
	for _, r := range w.Runs {
		key, e := parseID(r.Key)
		if e != nil {
			return bad(e.Error())
		}
		raw, e := parseID(r.Raw)
		if e != nil {
			return bad(e.Error())
		}
		object, e := parseID(r.Object)
		if e != nil {
			return bad(e.Error())
		}
		parent, e := optional(r.Parent)
		if e != nil {
			return bad(e.Error())
		}
		callsite, e := optional(r.Callsite)
		if e != nil {
			return bad(e.Error())
		}
		if parent.present != callsite.present {
			return bad("child parent/callsite presence mismatch")
		}
		if !strings.HasPrefix(r.Object, "script.nha045_") && !strings.HasPrefix(r.Object, "automation.nha045_") {
			return bad("non-synthetic run object")
		}
		objectsByRaw[r.Raw] = r.Object
		if r.Parent != nil {
			parents[r.Key] = *r.Parent
		}
		if _, ok := rawIDs[r.Raw]; ok {
			return bad("raw run ID reused or merged")
		}
		if _, ok := keys[r.Key]; ok {
			return bad("logical run ID repeated")
		}
		rawIDs[r.Raw] = r.Key
		keys[r.Key] = r.Raw
		c.runs = append(c.runs, runBinding{runKey{key}, rawRunID{raw}, objectID{object}, optionalRunKey{runKey{parent.id}, parent.present}, optionalCallsiteID{callsiteID{callsite.id}, callsite.present}})
	}
	for _, r := range c.runs {
		if r.parent.present {
			if _, ok := keys[r.parent.value.value]; !ok || r.parent.value.value == r.key.value {
				return bad("orphan/self parent")
			}
		}
	}
	for root := range parents {
		seen := make(map[string]bool)
		for key := root; key != ""; key = parents[key] {
			if seen[key] {
				return bad("run parent cycle")
			}
			seen[key] = true
		}
	}
	factIDs := make(map[string]bool)
	for _, v := range w.Facts {
		key, e := parseID(v.Key)
		if e != nil {
			return bad(e.Error())
		}
		kind, e := parseFactKind(v.Kind)
		if e != nil {
			return bad(e.Error())
		}
		run, e := optional(v.Run)
		if e != nil {
			return bad(e.Error())
		}
		object, e := parseID(v.Object)
		if e != nil {
			return bad(e.Error())
		}
		node, e := parseID(v.Node)
		if e != nil {
			return bad(e.Error())
		}
		if factIDs[v.Key] || v.AtMS < 0 || v.AtMS > 86400000 {
			return bad("duplicate fact or invalid tick")
		}
		factIDs[v.Key] = true
		if run.present {
			if _, ok := rawIDs[run.id.value]; !ok {
				return bad("orphan raw run reference")
			}
		} else if kind != State && kind != Admission {
			return bad("required run reference absent")
		}
		if kind == State && v.Object != "house" {
			return bad("state object must be house")
		}
		if kind != State && v.Run != nil && objectsByRaw[*v.Run] != v.Object {
			return bad("fact object differs from owning run")
		}
		payload, e := admitPayload(kind, v.Data)
		if e != nil {
			return bad(e.Error())
		}
		c.facts = append(c.facts, fact{factKey{key}, kind, optionalRawRunID{rawRunID{run.id}, run.present}, v.AtMS, objectID{object}, nodeKey{node}, payload})
	}
	if _, e := reachability(w.Edges, factIDs); e != nil {
		return bad(e.Error())
	}
	if e := validateStreams(w.Orders, factIDs); e != nil {
		return bad(e.Error())
	}
	if e := validateCoverage(c, w.Orders); e != nil {
		return bad(e.Error())
	}
	// A copy through encoding owns every nested pointer/slice; callers cannot
	// mutate the admitted capture by retaining a wire value.
	owned, e := json.Marshal(w)
	if e != nil {
		return bad(e.Error())
	}
	if e = strictJSON(owned, &c.wire, MaxCaptureBytes); e != nil {
		return bad(e.Error())
	}
	return c, nil
}
func (c Capture) boundary() (captureWire, error) {
	if !c.valid {
		return captureWire{}, fail(InvalidObservation, "zero capture")
	}
	w := c.wire
	w.Scenario = c.scenario.value
	w.Runs = make([]runWire, 0, len(c.runs))
	w.Facts = make([]factWire, 0, len(c.facts))
	for _, r := range c.runs {
		row := runWire{Key: r.key.value, Raw: r.raw.value, Object: r.object.value}
		if r.parent.present {
			row.Parent = ptr(r.parent.value.value)
		}
		if r.callsite.present {
			row.Callsite = ptr(r.callsite.value.value)
		}
		w.Runs = append(w.Runs, row)
	}
	for _, f := range c.facts {
		payload, e := payloadWire(f.payload)
		if e != nil {
			return captureWire{}, fail(InvalidObservation, e.Error())
		}
		kind, e := f.kind.token()
		if e != nil {
			return captureWire{}, e
		}
		row := factWire{Key: f.key.value, Kind: kind, AtMS: f.tick, Object: f.object.value, Node: f.node.value, Data: payload}
		if f.run.present {
			row.Run = ptr(f.run.value.value)
		}
		w.Facts = append(w.Facts, row)
	}
	return w, nil
}
func (c Capture) JSON() ([]byte, error) {
	w, e := c.boundary()
	if e != nil {
		return nil, e
	}
	return json.MarshalIndent(w, "", "  ")
}

func reachability(edges []edgeWire, facts map[string]bool) (map[string]map[string]bool, error) {
	graph := make(map[string][]string, len(facts))
	seen := make(map[edgeWire]bool)
	for _, e := range edges {
		if e.Before == e.After || !facts[e.Before] || !facts[e.After] || seen[e] {
			return nil, fmt.Errorf("invalid/duplicate edge %s -> %s", e.Before, e.After)
		}
		seen[e] = true
		graph[e.Before] = append(graph[e.Before], e.After)
	}
	result := make(map[string]map[string]bool, len(facts))
	for root := range facts {
		visited := make(map[string]bool)
		stack := append([]string(nil), graph[root]...)
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if n == root {
				return nil, fmt.Errorf("causal cycle")
			}
			if visited[n] {
				continue
			}
			visited[n] = true
			stack = append(stack, graph[n]...)
		}
		result[root] = visited
	}
	return result, nil
}

type Comparison struct {
	Schema      string       `json:"schema"`
	Equivalent  bool         `json:"equivalent"`
	Differences []Difference `json:"differences"`
}
type Difference struct {
	Key   string `json:"key"`
	Field string `json:"field"`
	HA    string `json:"ha"`
	Go    string `json:"go"`
}

func Compare(ha, goCapture Capture) (Comparison, error) {
	result := Comparison{Schema: Schema, Differences: []Difference{}}
	if !ha.valid || !goCapture.valid || ha.wire.Runtime.Name != "ha" || goCapture.wire.Runtime.Name != "go" {
		return result, fail(InvalidObservation, "expected admitted HA and Go captures")
	}
	if _, e := ha.boundary(); e != nil {
		return result, e
	}
	if _, e := goCapture.boundary(); e != nil {
		return result, e
	}
	add := func(key, field, a, b string) {
		result.Differences = append(result.Differences, Difference{key, field, a, b})
	}
	if ha.wire.Scenario != goCapture.wire.Scenario {
		add("fixture", "scenario", ha.wire.Scenario, goCapture.wire.Scenario)
	}
	if ha.wire.FixtureSHA256 != goCapture.wire.FixtureSHA256 {
		add("fixture", "sha256", ha.wire.FixtureSHA256, goCapture.wire.FixtureSHA256)
	}
	normalize := func(c Capture) (map[string]string, map[string]string) {
		runs := make(map[string]string)
		for _, r := range c.runs {
			runs[r.raw.value] = r.key.value
		}
		out := make(map[string]string)
		boundary, _ := c.boundary()
		for _, v := range boundary.Facts {
			if v.Run != nil {
				v.Run = ptr(runs[*v.Run])
			}
			b, _ := json.Marshal(v)
			out[v.Key] = string(b)
		}
		return out, runs
	}
	a, _ := normalize(ha)
	b, _ := normalize(goCapture)
	runRows := func(c Capture) map[string]string {
		r := make(map[string]string)
		boundary, _ := c.boundary()
		for _, v := range boundary.Runs {
			v.Raw = "<opaque>"
			j, _ := json.Marshal(v)
			r[v.Key] = string(j)
		}
		return r
	}
	compareRows := func(field string, a, b map[string]string) {
		keys := make(map[string]bool)
		for k := range a {
			keys[k] = true
		}
		for k := range b {
			keys[k] = true
		}
		ordered := make([]string, 0, len(keys))
		for k := range keys {
			ordered = append(ordered, k)
		}
		sort.Strings(ordered)
		for _, k := range ordered {
			if a[k] != b[k] {
				add(k, field, a[k], b[k])
			}
		}
	}
	compareRows("run_binding", runRows(ha), runRows(goCapture))
	compareRows("fact", a, b)
	graphRows := func(c Capture) map[string]string {
		facts := make(map[string]bool)
		for _, v := range c.facts {
			facts[v.key.value] = true
		}
		reach, _ := reachability(c.wire.Edges, facts)
		rows := make(map[string]string)
		for from, targets := range reach {
			for to := range targets {
				rows[from+" -> "+to] = "causal"
			}
		}
		return rows
	}
	compareRows("order", graphRows(ha), graphRows(goCapture))
	result.Equivalent = len(result.Differences) == 0
	if !result.Equivalent {
		return result, fail(SemanticDivergence, fmt.Sprintf("%d differences", len(result.Differences)))
	}
	return result, nil
}

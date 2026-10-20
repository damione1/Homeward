package nha045

import (
	"encoding/json"
	"reflect"
	"strconv"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/automationruns"
)

// These owned boundary records retain the immutable native payload. position is
// the sole ordering input of the local observation: Ordinal deliberately does
// not reread the payload's original position after a boundary copy is changed.
// No ordinal can be allocated here; acquisition uses only the native getters.
type nativeDecisionObservation struct {
	automationruns.Trace
	position automationruns.TraceOrdinal
}

func (r *rig) actionOrigin(execution automationruns.CommandExecution, parts []string) (nativeActionOrigin, error) {
	if r == nil || r.controller == nil || r.interpreter == nil || len(parts) != 5 {
		return nativeActionOrigin{}, fail(InvalidObservation, "action producer absent")
	}
	generation, ge := strconv.ParseUint(parts[0], 10, 64)
	run, re := strconv.ParseUint(parts[1], 10, 64)
	call, ce := strconv.ParseUint(parts[2], 10, 64)
	if ge != nil || re != nil || ce != nil || generation == 0 || run == 0 || call == 0 {
		return nativeActionOrigin{}, fail(InvalidObservation, "action identity not admitted")
	}
	nodes, loss := r.interpreter.NodeTraces()
	if loss != 0 || len(nodes) > automationruns.MaxNodeTraces {
		return nativeActionOrigin{}, fail(CaptureOverflow, "action entry witness evicted or oversized")
	}
	source := execution.Source()
	if parts[3] != source.Object() || parts[4] != source.Path() || source.Object() == "" || source.Path() == "" {
		return nativeActionOrigin{}, fail(InvalidObservation, "command source does not match its native causal identity")
	}
	// At the real port there must be exactly one still-open entry for this
	// source/run. Completed iterations are removed by their exact path, rather
	// than selecting the first or last node with a matching source string.
	active := make(map[nativeEntryPath]automationruns.NodeTrace)
	for _, node := range nodes {
		if uint64(node.RunID()) != run || uint64(node.Generation()) != generation || node.Kind() != ir.ActionCall || node.Source().Object() != source.Object() || node.Source().Path() != source.Path() || node.Source().At() != source.At() {
			continue
		}
		branches, iterations := node.Branches(), node.Iterations()
		if len(branches) > automationruns.MaxBranchDepth || len(iterations) > automationruns.MaxBranchDepth {
			return nativeActionOrigin{}, fail(InvalidObservation, "command entry path oversized")
		}
		path := nativeEntryPath{branch: nativeBranchKey{generation: node.Generation(), run: node.RunID(), depth: len(branches)}, depth: len(iterations)}
		copy(path.branch.path[:], branches)
		copy(path.iterations[:], iterations)
		switch node.Stage() {
		case automationruns.NodeEntered:
			if _, present := active[path]; present {
				return nativeActionOrigin{}, fail(InvalidObservation, "ambiguous open command entry")
			}
			active[path] = node
		case automationruns.NodeCompleted, automationruns.NodeFailed:
			delete(active, path)
		}
	}
	if len(active) != 1 {
		return nativeActionOrigin{}, fail(InvalidObservation, "exact active command entry absent or ambiguous")
	}
	for _, node := range active {
		if _, valid := node.Ordinal().Value(); !valid {
			return nativeActionOrigin{}, fail(InvalidObservation, "action entry has no native position")
		}
		return nativeActionOrigin{entry: node.Ordinal(), source: source, run: node.RunID(), generation: node.Generation(), call: call, branches: node.Branches(), iterations: node.Iterations(), valid: true}, nil
	}
	return nativeActionOrigin{}, fail(InvalidObservation, "command entry absent")
}

type nativeCallKey struct {
	generation automationruns.Generation
	run        automationruns.RunID
	ordinal    uint64
}
type nativeBranchKey struct {
	generation automationruns.Generation
	run        automationruns.RunID
	path       [automationruns.MaxBranchDepth]int
	depth      int
}
type nativeEntryPath struct {
	branch     nativeBranchKey
	iterations [automationruns.MaxBranchDepth]int
	depth      int
}

func actionIssue(action observedAction, nodes []nativeNodeObservation, used map[nativeCallKey]bool) (int, error) {
	origin := action.origin
	if _, valid := origin.entry.Value(); !valid || !origin.valid || origin.run == 0 || origin.generation == 0 || origin.call == 0 || used == nil || len(nodes) > automationruns.MaxNodeTraces || len(origin.branches) > automationruns.MaxBranchDepth || len(origin.iterations) > automationruns.MaxBranchDepth || action.Raw != rawID(origin.generation, origin.run) || action.Source != origin.source.Path() {
		return 0, fail(InvalidObservation, "action origin absent or foreign")
	}
	callKey := nativeCallKey{origin.generation, origin.run, origin.call}
	if used[callKey] {
		return 0, fail(InvalidObservation, "native command occurrence reused")
	}
	used[callKey] = true
	entry := -1
	for index, node := range nodes {
		if node.NodeTrace.Ordinal() == origin.entry {
			if entry != -1 {
				return 0, fail(InvalidObservation, "action entry repeated")
			}
			entry = index
		}
	}
	if entry < 0 {
		return 0, fail(InvalidObservation, "action entry not retained by final native snapshot")
	}
	entered := nodes[entry]
	if entered.Stage() != automationruns.NodeEntered || entered.Kind() != ir.ActionCall || entered.RunID() != origin.run || entered.Generation() != origin.generation || entered.Source().Object() != origin.source.Object() || entered.Source().Path() != origin.source.Path() || entered.Source().At() != origin.source.At() || !reflect.DeepEqual(entered.Branches(), origin.branches) || !reflect.DeepEqual(entered.Iterations(), origin.iterations) {
		return 0, fail(InvalidObservation, "action entry does not belong to the actual command")
	}
	issue := -1
	for index := entry + 1; index < len(nodes); index++ {
		node := nodes[index]
		if node.RunID() != origin.run || node.Generation() != origin.generation || node.Kind() != ir.ActionCall || node.Source().Object() != origin.source.Object() || node.Source().Path() != origin.source.Path() || node.Source().At() != origin.source.At() || !reflect.DeepEqual(node.Branches(), origin.branches) || !reflect.DeepEqual(node.Iterations(), origin.iterations) {
			continue
		}
		if node.Stage() != automationruns.NodeCompleted && node.Stage() != automationruns.NodeFailed {
			continue
		}
		if issue != -1 {
			return 0, fail(InvalidObservation, "ambiguous action issue for its occurrence")
		}
		issue = index
	}
	if issue < 0 || action.Failed && (nodes[issue].Stage() != automationruns.NodeFailed || nodes[issue].Failure() != automationruns.ExecutionCommandError) || !action.Failed && (nodes[issue].Stage() != automationruns.NodeCompleted || nodes[issue].Failure() != automationruns.ExecutionErrorInvalid) {
		return 0, fail(InvalidObservation, "actual command result lacks its typed native issue")
	}
	return issue, nil
}

func (o nativeDecisionObservation) Ordinal() automationruns.TraceOrdinal { return o.position }

type nativeNodeObservation struct {
	automationruns.NodeTrace
	position automationruns.TraceOrdinal
}

func (o nativeNodeObservation) Ordinal() automationruns.TraceOrdinal { return o.position }

// This is a local observation boundary, not a replacement Controller. The two
// identities are the actual bound producers. Their rings are copied and bounded;
// Trace and NodeTrace expose only immutable payload values/copying accessors.
// Completeness is qualified before projection; positions select only the
// explicit causal relationships, never a total order between siblings.
type nativeObservation struct {
	controller      *automationruns.Controller
	interpreter     *automationruns.Interpreter
	decisions       []nativeDecisionObservation
	nodes           []nativeNodeObservation
	decisionEvicted uint64
	nodeEvicted     uint64
	state           automationruns.TraceOrderState
	joined          bool
	valid           bool
}

func channelJoined(channel <-chan struct{}) bool {
	if channel == nil {
		return false
	}
	select {
	case <-channel:
		return true
	default:
		return false
	}
}

func (r *rig) observeNative() (nativeObservation, error) {
	if r == nil || r.controller == nil || r.interpreter == nil {
		return nativeObservation{}, fail(InvalidObservation, "native producers absent")
	}
	traces, decisionEvicted := r.controller.Traces()
	nodes, nodeEvicted := r.interpreter.NodeTraces()
	if decisionEvicted != 0 || nodeEvicted != 0 {
		return nativeObservation{}, fail(CaptureOverflow, "trace loss")
	}
	if len(traces) > automationruns.MaxTraces || len(nodes) > automationruns.MaxNodeTraces {
		return nativeObservation{}, fail(CaptureOverflow, "native snapshot oversized")
	}
	observation := nativeObservation{
		controller: r.controller, interpreter: r.interpreter,
		decisions:       make([]nativeDecisionObservation, 0, len(traces)),
		nodes:           make([]nativeNodeObservation, 0, len(nodes)),
		decisionEvicted: decisionEvicted, nodeEvicted: nodeEvicted,
		state: r.controller.TraceOrderState(), joined: channelJoined(r.controller.Done()),
	}
	for _, trace := range traces {
		if _, admitted := trace.Ordinal().Value(); !admitted {
			return nativeObservation{}, fail(InvalidObservation, "native decision ordinal absent")
		}
		observation.decisions = append(observation.decisions, nativeDecisionObservation{trace, trace.Ordinal()})
	}
	for _, node := range nodes {
		if _, admitted := node.Ordinal().Value(); !admitted {
			return nativeObservation{}, fail(InvalidObservation, "native node ordinal absent")
		}
		observation.nodes = append(observation.nodes, nativeNodeObservation{node, node.Ordinal()})
	}
	observation.valid = true
	return observation, nil
}

func (o nativeObservation) copy() nativeObservation {
	if !o.valid || len(o.decisions) > automationruns.MaxTraces || len(o.nodes) > automationruns.MaxNodeTraces {
		return nativeObservation{}
	}
	o.decisions = append([]nativeDecisionObservation(nil), o.decisions...)
	o.nodes = append([]nativeNodeObservation(nil), o.nodes...)
	return o
}

func (r *rig) qualifyNative(o nativeObservation, joined bool) error {
	if r == nil || !o.valid || o.controller == nil || o.controller != r.controller || o.interpreter == nil || o.interpreter != r.interpreter || o.state != automationruns.TraceOrderHealthy || o.controller.TraceOrderState() != automationruns.TraceOrderHealthy || o.decisionEvicted != 0 || o.nodeEvicted != 0 || len(o.decisions) == 0 || len(o.decisions) > automationruns.MaxTraces || len(o.nodes) == 0 || len(o.nodes) > automationruns.MaxNodeTraces {
		return fail(InvalidObservation, "incomplete or foreign native ordering space")
	}
	if joined && (!o.joined || !channelJoined(o.controller.Done()) || !r.closed || r.triggers == nil || !channelJoined(r.triggers.Done()) || r.reactive == nil || !channelJoined(r.reactive.Done()) || r.session == nil || !channelJoined(r.session.Done())) {
		return fail(InvalidObservation, "native producers not joined")
	}
	// A boundary copy may vary admitted positions for the corruption oracles,
	// but its immutable native records must still belong to these exact producers.
	actualDecisions, decisionLoss := r.controller.Traces()
	actualNodes, nodeLoss := r.interpreter.NodeTraces()
	if decisionLoss != 0 || nodeLoss != 0 || joined && (len(actualDecisions) != len(o.decisions) || len(actualNodes) != len(o.nodes)) {
		return fail(InvalidObservation, "native snapshot is not complete for its producer")
	}
	decisions := make(map[automationruns.TraceOrdinal]automationruns.Trace, len(actualDecisions))
	for _, trace := range actualDecisions {
		decisions[trace.Ordinal()] = trace
	}
	for _, trace := range o.decisions {
		native, present := decisions[trace.Trace.Ordinal()]
		if !present || !reflect.DeepEqual(native, trace.Trace) {
			return fail(InvalidObservation, "foreign Controller record")
		}
		delete(decisions, trace.Trace.Ordinal())
	}
	nodes := make(map[automationruns.TraceOrdinal]automationruns.NodeTrace, len(actualNodes))
	for _, node := range actualNodes {
		nodes[node.Ordinal()] = node
	}
	branchPositions := make(map[nativeBranchKey]uint64)
	for _, node := range o.nodes {
		native, present := nodes[node.NodeTrace.Ordinal()]
		if !present || !reflect.DeepEqual(native, node.NodeTrace) {
			return fail(InvalidObservation, "foreign interpreter record")
		}
		delete(nodes, node.NodeTrace.Ordinal())
		path := node.Branches()
		if len(path) > automationruns.MaxBranchDepth {
			return fail(InvalidObservation, "native branch path oversized")
		}
		branch := nativeBranchKey{generation: node.Generation(), run: node.RunID(), depth: len(path)}
		copy(branch.path[:], path)
		value, valid := node.NodeTrace.Ordinal().Value()
		if !valid || value <= branchPositions[branch] {
			return fail(InvalidObservation, "native branch payload sequence reordered")
		}
		branchPositions[branch] = value
	}
	seen := make(map[uint64]bool, len(o.decisions)+len(o.nodes))
	check := func(position automationruns.TraceOrdinal, previous *uint64) error {
		value, valid := position.Value()
		if !valid || seen[value] || value <= *previous {
			return fail(InvalidObservation, "native ordinal absent, reused or ring reordered")
		}
		seen[value] = true
		*previous = value
		return nil
	}
	var decisionPosition, nodePosition uint64
	for _, trace := range o.decisions {
		if e := check(trace.Ordinal(), &decisionPosition); e != nil {
			return e
		}
	}
	for _, node := range o.nodes {
		if e := check(node.Ordinal(), &nodePosition); e != nil {
			return e
		}
	}
	return nil
}

func (r *rig) businessTerminals(o nativeObservation) error {
	if e := r.qualifyNative(o, false); e != nil {
		return e
	}
	if len(r.identities) == 0 || len(r.identities) > MaxRuns {
		return fail(InvalidObservation, "native business runs absent")
	}
	for _, id := range r.identities {
		count := 0
		for _, trace := range o.decisions {
			if trace.RunID == id.id && (trace.Decision == automationruns.DecisionFinished || trace.Decision == automationruns.DecisionCancelled || trace.Decision == automationruns.DecisionExecutionError) {
				count++
			}
		}
		if count != 1 {
			return fail(InvalidObservation, "semantic terminal absent or repeated before cleanup")
		}
	}
	for _, handle := range r.handles {
		if handle != nil && !channelJoined(handle.Done()) {
			return fail(InvalidObservation, "script handle not semantically joined")
		}
	}
	return nil
}

func stableNativePrefix(before, after nativeObservation) error {
	if !before.valid || !after.valid || !after.joined || before.state != automationruns.TraceOrderHealthy || after.state != automationruns.TraceOrderHealthy || before.decisionEvicted != 0 || before.nodeEvicted != 0 || after.decisionEvicted != 0 || after.nodeEvicted != 0 || before.controller == nil || before.controller != after.controller || before.interpreter == nil || before.interpreter != after.interpreter || len(before.decisions) != len(after.decisions) || len(before.nodes) != len(after.nodes) || !reflect.DeepEqual(before.decisions, after.decisions) || !reflect.DeepEqual(before.nodes, after.nodes) {
		return fail(InvalidObservation, "cleanup changed the completed native observation prefix")
	}
	return nil
}

// Boundary only. The admitted proof below retains typed keys and positions,
// never nullable payload unions or arbitrary JSON domain values.
type orderPointWire struct {
	Key     string `json:"key"`
	Ordinal uint64 `json:"ordinal"`
}
type stateCauseWire struct {
	State  string `json:"state"`
	Action string `json:"action"`
}
type orderProofWire struct {
	Producer        string           `json:"producer"`
	Healthy         bool             `json:"healthy"`
	Joined          bool             `json:"joined"`
	DecisionEvicted uint64           `json:"decision_evicted"`
	NodeEvicted     uint64           `json:"node_evicted"`
	Points          []orderPointWire `json:"points"`
	StateCauses     []stateCauseWire `json:"state_causes"`
}
type emissionPosition struct{ value uint64 }
type stateCause struct{ state, action factKey }
type nativeProducerID struct{ identifier }
type nativeOrderProof struct {
	producer  nativeProducerID
	positions map[factKey]emissionPosition
	causes    []stateCause
	valid     bool
}
type nativeCommandWire struct {
	Key        string `json:"key"`
	Run        string `json:"run"`
	Object     string `json:"object"`
	Source     string `json:"source"`
	Call       uint64 `json:"call"`
	Entry      uint64 `json:"entry"`
	Issue      uint64 `json:"issue"`
	Branches   []int  `json:"branches"`
	Iterations []int  `json:"iterations"`
}

func decodeOrderProof(data []byte, c Capture) (nativeOrderProof, error) {
	if !c.valid {
		return nativeOrderProof{}, fail(InvalidObservation, "native proof has no admitted capture")
	}
	var document json.RawMessage
	if e := strictJSON(data, &document, MaxCaptureBytes/2); e != nil {
		return nativeOrderProof{}, fail(InvalidObservation, e.Error())
	}
	var envelope struct {
		Order    json.RawMessage `json:"order"`
		Producer string          `json:"producer"`
	}
	if e := json.Unmarshal(data, &envelope); e != nil {
		return nativeOrderProof{}, fail(InvalidObservation, e.Error())
	}
	body := envelope.Order
	if e := requireCaptureScalars(body, "producer", "healthy", "joined", "decision_evicted", "node_evicted", "points", "state_causes"); e != nil {
		return nativeOrderProof{}, fail(InvalidObservation, e.Error())
	}
	var records struct {
		Points []json.RawMessage `json:"points"`
		Causes []json.RawMessage `json:"state_causes"`
	}
	if e := json.Unmarshal(body, &records); e != nil {
		return nativeOrderProof{}, fail(InvalidObservation, e.Error())
	}
	for _, record := range records.Points {
		if e := requireCaptureScalars(record, "key", "ordinal"); e != nil {
			return nativeOrderProof{}, fail(InvalidObservation, e.Error())
		}
	}
	for _, record := range records.Causes {
		if e := requireCaptureScalars(record, "state", "action"); e != nil {
			return nativeOrderProof{}, fail(InvalidObservation, e.Error())
		}
	}
	var wire orderProofWire
	if e := strictJSON(body, &wire, MaxCaptureBytes/2); e != nil {
		return nativeOrderProof{}, fail(InvalidObservation, e.Error())
	}
	if envelope.Producer == "" || envelope.Producer != wire.Producer {
		return nativeOrderProof{}, fail(InvalidObservation, "native proof belongs to another producer")
	}
	return admitOrderProof(wire, c)
}

func admitOrderProof(w orderProofWire, c Capture) (nativeOrderProof, error) {
	bad := func(message string) (nativeOrderProof, error) {
		return nativeOrderProof{}, fail(InvalidObservation, message)
	}
	producer, e := parseID(w.Producer)
	if e != nil || !c.valid || !w.Healthy || !w.Joined || w.DecisionEvicted != 0 || w.NodeEvicted != 0 || len(w.Points) == 0 || len(w.Points) > MaxFacts || len(w.StateCauses) > MaxFacts {
		return bad("invalid native order proof envelope")
	}
	proof := nativeOrderProof{producer: nativeProducerID{producer}, positions: make(map[factKey]emissionPosition, len(w.Points)), causes: make([]stateCause, 0, len(w.StateCauses))}
	facts := make(map[factKey]fact, len(c.facts))
	for _, f := range c.facts {
		facts[f.key] = f
	}
	seen := make(map[uint64]bool, len(w.Points))
	var previous uint64
	for _, point := range w.Points {
		id, e := parseID(point.Key)
		key := factKey{id}
		f, present := facts[key]
		if e != nil || !present || f.kind == Admission || point.Ordinal == 0 || point.Ordinal == ^uint64(0) || seen[point.Ordinal] {
			return bad("absent, duplicate or foreign native position")
		}
		if _, duplicate := proof.positions[key]; duplicate {
			return bad("repeated native fact witness")
		}
		if c.wire.Runtime.Name == "ha" && point.Ordinal <= previous {
			return bad("HA callback stream reordered")
		}
		previous = point.Ordinal
		seen[point.Ordinal] = true
		proof.positions[key] = emissionPosition{point.Ordinal}
	}
	for _, f := range c.facts {
		switch f.kind {
		case Started, Terminal, Action, Wait:
			if _, present := proof.positions[f.key]; !present {
				return bad("native fact lacks an emission witness")
			}
		}
	}
	for _, stream := range c.wire.Orders {
		var previous uint64
		for _, name := range stream.Keys {
			id, _ := parseID(name)
			position, present := proof.positions[factKey{id}]
			if !present {
				continue
			}
			if position.value <= previous {
				return bad("native stream contradicts its emission positions")
			}
			previous = position.value
		}
	}
	caused := make(map[factKey]bool, len(w.StateCauses))
	for _, row := range w.StateCauses {
		stateID, se := parseID(row.State)
		actionID, ae := parseID(row.Action)
		stateKey, actionKey := factKey{stateID}, factKey{actionID}
		state, statePresent := facts[stateKey]
		action, actionPresent := facts[actionKey]
		stateValue, stateValid := state.payload.(statePayload)
		actionValue, actionValid := action.payload.(actionPayload)
		if se != nil || ae != nil || !statePresent || !actionPresent || !stateValid || !actionValid || actionValue.outcome != Completed || !state.run.present || state.run != action.run || stateValue.entity != actionValue.entity || caused[stateKey] {
			return bad("effect state lacks its unique actual action cause")
		}
		caused[stateKey] = true
		proof.causes = append(proof.causes, stateCause{stateKey, actionKey})
	}
	for _, f := range c.facts {
		if f.kind == State && f.run.present && !caused[f.key] {
			return bad("effect state cause absent")
		}
	}
	proof.valid = true
	return proof, nil
}

package nha045

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

func validateStreams(streams []streamWire, facts map[string]bool) error {
	if len(streams) == 0 || len(streams) > MaxFacts {
		return fmt.Errorf("native order streams absent or oversized")
	}
	scopes := make(map[string]bool)
	count := 0
	for _, stream := range streams {
		if _, e := parseID(stream.Scope); e != nil || scopes[stream.Scope] || len(stream.Keys) == 0 {
			return fmt.Errorf("invalid/duplicate native stream")
		}
		if stream.Scope != "control" && !strings.HasPrefix(stream.Scope, "node/") {
			return fmt.Errorf("unknown native stream")
		}
		scopes[stream.Scope] = true
		seen := make(map[string]bool)
		for _, key := range stream.Keys {
			count++
			if count > MaxEdges || !facts[key] || seen[key] {
				return fmt.Errorf("invalid native order reference")
			}
			seen[key] = true
		}
	}
	if !scopes["control"] {
		return fmt.Errorf("native control stream absent")
	}
	return nil
}

func validateCoverage(c Capture, streams []streamWire) error {
	facts := make(map[string]fact)
	logical := make(map[string]string)
	for _, r := range c.runs {
		logical[r.raw.value] = r.key.value
	}
	for _, f := range c.facts {
		facts[f.key.value] = f
	}
	seen := make(map[string]int)
	for _, stream := range streams {
		for _, key := range stream.Keys {
			f := facts[key]
			seen[key]++
			if seen[key] > 1 {
				return fmt.Errorf("fact repeated across native streams")
			}
			if stream.Scope == "control" {
				if f.kind != Started && f.kind != Terminal && f.kind != Cancellation && (f.kind != State || f.run.present) {
					return fmt.Errorf("non-control fact in control stream")
				}
				continue
			}
			if f.kind != Action && f.kind != Wait || !f.run.present {
				return fmt.Errorf("non-node fact in native branch stream")
			}
			prefix := "node/" + logical[f.run.value.value] + "/root"
			if !strings.HasPrefix(stream.Scope, prefix) {
				return fmt.Errorf("native stream belongs to another run")
			}
			suffix := strings.TrimPrefix(stream.Scope, prefix)
			if suffix != "" {
				parts := strings.Split(suffix, "/")
				if parts[0] != "" || len(parts) > 9 {
					return fmt.Errorf("native branch depth exceeded")
				}
				for _, part := range parts[1:] {
					n, e := strconv.Atoi(part)
					if e != nil || n < 0 || n >= 32 || strconv.Itoa(n) != part {
						return fmt.Errorf("invalid native branch index")
					}
				}
			}
		}
	}
	for _, f := range c.facts {
		switch f.kind {
		case Started, Terminal, Cancellation, Action, Wait:
			if seen[f.key.value] != 1 {
				return fmt.Errorf("native order witness absent: %s", f.key.value)
			}
		}
	}
	return nil
}

// Native per-branch streams establish sequence; native Controller/Script hooks
// establish cancellation joins. Independent streams acquire no collection-order
// edges. Fixture barriers select which cross-stream relationship is compared,
// and the observed stream decides its direction (including an inversion).
func causalEdges(w captureWire, f Fixture) []edgeWire {
	present := make(map[string]bool)
	for _, v := range w.Facts {
		present[v.Key] = true
	}
	seen := make(map[edgeWire]bool)
	out := []edgeWire{}
	edge := func(a, b string) {
		e := edgeWire{a, b}
		if a != b && present[a] && present[b] && !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	control := make(map[string]int)
	for _, stream := range w.Orders {
		if stream.Scope == "control" {
			for index, key := range stream.Keys {
				control[key] = index
			}
			continue
		}
		for i := 1; i < len(stream.Keys); i++ {
			edge(stream.Keys[i-1], stream.Keys[i])
		}
	}
	observed := func(a, b string) {
		i, ok := control[a]
		j, other := control[b]
		if !ok || !other {
			return
		}
		if i < j {
			edge(a, b)
		} else {
			edge(b, a)
		}
	}
	for _, run := range w.Runs {
		observed(run.Key+"/start", run.Key+"/terminal")
		if run.Parent != nil {
			observed(*run.Parent+"/start", run.Key+"/start")
			if f.ID() == "08-direct-parent-cancel" {
				observed(*run.Parent+"/cancel", run.Key+"/terminal")
				observed(run.Key+"/terminal", *run.Parent+"/terminal")
			}
		}
		observed(run.Key+"/cancel", run.Key+"/terminal")
	}
	switch f.ID() {
	case "01-restart-delay", "02-queued-max":
		observed("a/terminal", "b/start")
	case "09-detached-parent-cancel":
		observed("a/terminal", "state/switch.nha045_gate/2")
		observed("state/switch.nha045_gate/2", "a/child/1/terminal")
	}
	prior := make(map[string]string)
	for _, fact := range w.Facts {
		if fact.Kind == factName(State) && fact.Data.Entity != nil && fact.Run == nil {
			entity := *fact.Data.Entity
			edge(prior[entity], fact.Key)
			prior[entity] = fact.Key
		}
	}
	return out
}

func observedCausalEdges(w captureWire, f Fixture, proof nativeOrderProof) ([]edgeWire, error) {
	if !proof.valid {
		return nil, fail(InvalidObservation, "native order proof not admitted")
	}
	edges := causalEdges(w, f)
	selected := make(map[edgeWire]bool, len(edges))
	for _, edge := range edges {
		selected[edge] = true
	}
	appendEdge := func(before, after string) {
		edge := edgeWire{before, after}
		if !selected[edge] {
			selected[edge] = true
			edges = append(edges, edge)
		}
	}
	observed := func(a, b string) error {
		ai, ae := parseID(a)
		bi, be := parseID(b)
		left, lok := proof.positions[factKey{ai}]
		right, rok := proof.positions[factKey{bi}]
		if ae != nil || be != nil || !lok || !rok || left.value == right.value {
			return fail(InvalidObservation, "selected relation has no distinct native witnesses")
		}
		if left.value < right.value {
			appendEdge(a, b)
		} else {
			appendEdge(b, a)
		}
		return nil
	}
	for _, stream := range w.Orders {
		if stream.Scope == "control" {
			continue
		}
		for _, run := range w.Runs {
			root := "node/" + run.Key + "/root"
			if stream.Scope != root && !strings.HasPrefix(stream.Scope, root+"/") {
				continue
			}
			if e := observed(run.Key+"/start", stream.Keys[0]); e != nil {
				return nil, e
			}
			if e := observed(stream.Keys[len(stream.Keys)-1], run.Key+"/terminal"); e != nil {
				return nil, e
			}
		}
	}
	for _, cause := range proof.causes {
		appendEdge(cause.action.value, cause.state.value)
	}
	return edges, nil
}

// ProjectHA applies the same graph authority to HA's native observation streams.
// It never supplies observations, order or expected outcomes on HA's behalf.
func ProjectHA(f Fixture, data []byte) (Capture, error) {
	capture, e := DecodeCapture(data)
	if e != nil {
		return Capture{}, e
	}
	if !f.valid || capture.wire.Runtime.Name != "ha" || capture.wire.Scenario != f.ID() || capture.wire.FixtureSHA256 != f.SHA256() {
		return Capture{}, fail(InvalidObservation, "HA projection fixture mismatch")
	}
	w, e := capture.boundary()
	if e != nil {
		return Capture{}, e
	}
	proof, e := decodeOrderProof(w.Raw, capture)
	if e != nil {
		return Capture{}, e
	}
	w.Edges, e = observedCausalEdges(w, f, proof)
	if e != nil {
		return Capture{}, e
	}
	encoded, e := json.Marshal(w)
	if e != nil {
		return Capture{}, e
	}
	return DecodeCapture(encoded)
}

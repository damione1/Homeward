package nha045

import "encoding/json"

func CompareInput(data []byte) (Comparison, error) {
	var input struct {
		Fixture json.RawMessage `json:"fixture"`
		HA      json.RawMessage `json:"ha"`
		Go      json.RawMessage `json:"go"`
	}
	empty := Comparison{Schema: Schema, Differences: []Difference{}}
	if e := strictJSON(data, &input, 2*MaxCaptureBytes+MaxInputBytes); e != nil {
		return empty, fail(InvalidObservation, e.Error())
	}
	if e := requireFields(data, "fixture", "ha", "go"); e != nil {
		return empty, fail(InvalidObservation, e.Error())
	}
	f, e := DecodeFixture(input.Fixture)
	if e != nil {
		return empty, e
	}
	ha, e := DecodeCapture(input.HA)
	if e != nil {
		return empty, e
	}
	g, e := DecodeCapture(input.Go)
	if e != nil {
		return empty, e
	}
	result, e := Compare(ha, g)
	if e != nil {
		return result, e
	}
	if e = ValidateExpected(f, ha); e != nil {
		result.Equivalent = false
		result.Differences = append(result.Differences, Difference{f.ID(), "expected", e.Error(), "not evaluated"})
		return result, e
	}
	if e = ValidateExpected(f, g); e != nil {
		result.Equivalent = false
		result.Differences = append(result.Differences, Difference{f.ID(), "expected", "satisfied", e.Error()})
		return result, e
	}
	return result, nil
}
func ProjectInput(data []byte) (Capture, error) {
	var input struct {
		Fixture json.RawMessage `json:"fixture"`
		HA      json.RawMessage `json:"ha"`
	}
	if e := strictJSON(data, &input, MaxCaptureBytes+MaxInputBytes); e != nil {
		return Capture{}, fail(InvalidObservation, e.Error())
	}
	if e := requireFields(data, "fixture", "ha"); e != nil {
		return Capture{}, fail(InvalidObservation, e.Error())
	}
	f, e := DecodeFixture(input.Fixture)
	if e != nil {
		return Capture{}, e
	}
	return ProjectHA(f, input.HA)
}

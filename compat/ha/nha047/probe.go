package nha047

import "encoding/json"

func (c Capture) JSON() ([]byte, error) {
	if !c.digest.Valid() {
		return nil, ErrInvalidCapture
	}
	return json.Marshal(c.wire)
}

func ProjectInput(data []byte) (Capture, error) {
	var input struct {
		Fixture json.RawMessage `json:"fixture"`
		HA      json.RawMessage `json:"ha"`
	}
	if err := strictObjectJSON(data, &input, MaxInputBytes+MaxCaptureBytes, "fixture", "ha"); err != nil {
		return Capture{}, err
	}
	fixture, err := DecodeCase(input.Fixture)
	if err != nil {
		return Capture{}, err
	}
	capture, err := DecodeCapture(input.HA)
	if err != nil {
		return Capture{}, err
	}
	expected, err := fixture.expectedInput("ha")
	if err != nil || capture.wire.Runtime != "ha" || capture.wire.Scenario != fixture.ID() || capture.wire.FixtureSHA256 != fixture.Digest().String() || capture.wire.InputSHA256 != expected.String() {
		return Capture{}, ErrInvalidCapture
	}
	return capture, nil
}

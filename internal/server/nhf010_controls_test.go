package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

type nhf010ControlAction string

const (
	nhf010ControlNominal nhf010ControlAction = "nominal"
	nhf010ControlStale   nhf010ControlAction = "stale"
	nhf010ControlUnknown nhf010ControlAction = "unknown"
	nhf010ControlAbsent  nhf010ControlAction = "absent"
	nhf010ControlMissing nhf010ControlAction = "missing"
	nhf010ControlRevoke  nhf010ControlAction = "revoke-details"
	nhf010ControlAllow   nhf010ControlAction = "allow-details"
	nhf010ControlParis   nhf010ControlAction = "profile-paris"
	nhf010ControlToronto nhf010ControlAction = "profile-toronto"
	nhf010ControlOutage  nhf010ControlAction = "outage"
	nhf010ControlNetwork nhf010ControlAction = "restore-network"
)

func (a *nhf010ControlAction) UnmarshalText(raw []byte) error {
	switch value := nhf010ControlAction(raw); value {
	case nhf010ControlNominal, nhf010ControlStale, nhf010ControlUnknown, nhf010ControlAbsent, nhf010ControlMissing, nhf010ControlRevoke, nhf010ControlAllow, nhf010ControlParis, nhf010ControlToronto, nhf010ControlOutage, nhf010ControlNetwork:
		*a = value
		return nil
	default:
		return errors.New("unknown environmental fixture control")
	}
}

type nhf010Control struct {
	sequence uint64
	action   nhf010ControlAction
}

func nhf010ReadControl(directory string, previous uint64) (*nhf010Control, error) {
	file, err := os.Open(filepath.Join(directory, "command.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return nil, err
	}
	if len(raw) > 4096 {
		return nil, errors.New("environment control exceeds bound")
	}
	var value struct {
		Sequence uint64              `json:"sequence"`
		Action   nhf010ControlAction `json:"action"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing environment control")
	}
	if value.Sequence == 0 || value.Sequence > 128 || value.Action == "" {
		return nil, errors.New("invalid environment control")
	}
	if value.Sequence == previous {
		return nil, nil
	}
	if value.Sequence != previous+1 {
		return nil, errors.New("out of order environment control")
	}
	return &nhf010Control{value.Sequence, value.Action}, nil
}

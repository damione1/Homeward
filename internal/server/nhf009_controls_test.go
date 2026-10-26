package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

type nhf009ControlAction string

const (
	nhf009DenyWrite           nhf009ControlAction = "deny-write"
	nhf009AllowWrite          nhf009ControlAction = "allow-write"
	nhf009WithdrawCover       nhf009ControlAction = "withdraw-cover"
	nhf009WithdrawLock        nhf009ControlAction = "withdraw-lock"
	nhf009WithdrawAlarm       nhf009ControlAction = "withdraw-alarm"
	nhf009Rediscover          nhf009ControlAction = "rediscover"
	nhf009DropObservations    nhf009ControlAction = "drop-observations"
	nhf009RestoreObservations nhf009ControlAction = "restore-observations"
	nhf009ConfirmLast         nhf009ControlAction = "confirm-last"
	nhf009ObserveLast         nhf009ControlAction = "observe-last"
	nhf009Outage              nhf009ControlAction = "outage"
	nhf009RestoreNetwork      nhf009ControlAction = "restore-network"
)

func (a *nhf009ControlAction) UnmarshalJSON(raw []byte) error {
	var input string
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	switch value := nhf009ControlAction(input); value {
	case nhf009DenyWrite, nhf009AllowWrite, nhf009WithdrawCover, nhf009WithdrawLock, nhf009WithdrawAlarm, nhf009Rediscover, nhf009DropObservations, nhf009RestoreObservations, nhf009ConfirmLast, nhf009ObserveLast, nhf009Outage, nhf009RestoreNetwork:
		*a = value
		return nil
	default:
		return errors.New("unknown NHF009 synthetic control")
	}
}

type nhf009Control struct {
	sequence uint64
	action   nhf009ControlAction
	command  *domain.CommandID
}

func nhf009ReadControl(directory string, previous uint64) (*nhf009Control, error) {
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
		return nil, errors.New("synthetic control exceeds bound")
	}
	var value struct {
		Sequence uint64              `json:"sequence"`
		Action   nhf009ControlAction `json:"action"`
		Command  string              `json:"command_id,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing synthetic control")
	}
	if value.Sequence == 0 || value.Sequence > 64 || value.Action == "" {
		return nil, errors.New("invalid synthetic control")
	}
	result := &nhf009Control{sequence: value.Sequence, action: value.Action}
	switch value.Action {
	case nhf009ConfirmLast, nhf009ObserveLast:
		id, err := domain.ParseCommandID(value.Command)
		if err != nil {
			return nil, err
		}
		result.command = &id
	case nhf009DenyWrite, nhf009AllowWrite, nhf009WithdrawCover, nhf009WithdrawLock, nhf009WithdrawAlarm, nhf009Rediscover, nhf009DropObservations, nhf009RestoreObservations, nhf009Outage, nhf009RestoreNetwork:
		if value.Command != "" {
			return nil, errors.New("unexpected synthetic command identity")
		}
	default:
		return nil, errors.New("unparsed synthetic control")
	}
	if value.Sequence == previous {
		return nil, nil
	}
	if value.Sequence != previous+1 {
		return nil, errors.New("out of order synthetic control")
	}
	return result, nil
}

// This receipt exists only in the opt-in fake driver. A filesystem instruction
// must name the exact admitted command; an observation never supplies proof.
type nhf009ProtocolReceipt struct{ command domain.CommandID }

func (p nhf009ProtocolReceipt) QualifyProtocolConfirmation(command integrations.AdmittedCommand) error {
	id, _, err := command.RouteIdentity()
	if err != nil {
		return err
	}
	if p.command != id {
		return errors.New("synthetic protocol receipt names another command")
	}
	return nil
}
func (d *nhf008Driver) nhf009Last(expected *domain.CommandID) (*integrations.AdmittedCommand, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if expected == nil || d.last == nil {
		return nil, errors.New("missing exact synthetic command")
	}
	id, _, err := d.last.RouteIdentity()
	if err != nil {
		return nil, err
	}
	if id != *expected {
		return nil, errors.New("synthetic command identity changed")
	}
	command := *d.last
	return &command, nil
}

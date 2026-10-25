package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/damione1/Homeward/internal/helpers"
)

type nhf008ControlAction string

const (
	nhf008DenyWrite           nhf008ControlAction = "deny-write"
	nhf008AllowWrite          nhf008ControlAction = "allow-write"
	nhf008WithdrawThermostat  nhf008ControlAction = "withdraw-thermostat"
	nhf008Rediscover          nhf008ControlAction = "rediscover"
	nhf008DropObservations    nhf008ControlAction = "drop-observations"
	nhf008RestoreObservations nhf008ControlAction = "restore-observations"
	nhf008SQLRace             nhf008ControlAction = "sql-race-counter"
	nhf008SQLRaceNumber       nhf008ControlAction = "sql-race-number"
	nhf008ReadSQL             nhf008ControlAction = "read-sql"
)

func (a *nhf008ControlAction) UnmarshalJSON(raw []byte) error {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return err
	}
	value := nhf008ControlAction(text)
	switch value {
	case nhf008DenyWrite, nhf008AllowWrite, nhf008WithdrawThermostat, nhf008Rediscover, nhf008DropObservations, nhf008RestoreObservations, nhf008SQLRace, nhf008SQLRaceNumber, nhf008ReadSQL:
		*a = value
		return nil
	default:
		return errors.New("unknown synthetic NHF008 control")
	}
}

type nhf008Control struct {
	Sequence uint64              `json:"sequence"`
	Action   nhf008ControlAction `json:"action"`
}

func nhf008ReadControl(directory string, previous uint64) (*nhf008Control, error) {
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
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var command nhf008Control
	if err := decoder.Decode(&command); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing synthetic control")
	}
	if command.Sequence == 0 || command.Sequence > 32 || command.Action == "" {
		return nil, errors.New("invalid synthetic control")
	}
	if command.Sequence == previous {
		return nil, nil
	}
	if command.Sequence != previous+1 {
		return nil, errors.New("out of order synthetic control")
	}
	return &command, nil
}

type nhf008Helper struct {
	definition helpers.Definition
	handle     helpers.Handle
}

func nhf008HelperSnapshots(ctx context.Context, repository *helpers.Repository, entries map[string]nhf008Helper) (map[string]any, error) {
	result := make(map[string]any)
	for alias, entry := range entries {
		snapshot, err := repository.Restore(ctx, entry.handle, entry.definition)
		if err != nil {
			return nil, err
		}
		revision, ok := snapshot.Revision().Number()
		if !ok {
			return nil, errors.New("helper SQL revision missing")
		}
		value, present := snapshot.Value()
		row := map[string]any{"revision": revision, "present": present, "kind": snapshot.Kind()}
		if present {
			if number, ok := value.Number(); ok {
				row["number"] = number
			}
			if boolean, ok := value.Boolean(); ok {
				row["boolean"] = boolean
			}
			if counter, ok := value.Counter(); ok {
				row["counter"] = counter
			}
			if text, ok := value.Text(); ok {
				row["text"] = text
			}
			if selected, ok := value.Select(); ok {
				row["selected"] = selected
			}
			if date, ok := value.Date(); ok {
				year, month, day, err := date.Components()
				if err != nil {
					return nil, err
				}
				row["date"] = fmt.Sprintf("%04d-%02d-%02d", year, month, day)
			}
			if clock, ok := value.Time(); ok {
				hour, minute, second, err := clock.Components()
				if err != nil {
					return nil, err
				}
				row["time"] = fmt.Sprintf("%02d:%02d:%02d", hour, minute, second)
			}
			if civil, ok := value.DateTime(); ok {
				date, clock, err := civil.Components()
				if err != nil {
					return nil, err
				}
				year, month, day, err := date.Components()
				if err != nil {
					return nil, err
				}
				hour, minute, second, err := clock.Components()
				if err != nil {
					return nil, err
				}
				row["datetime"] = fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d", year, month, day, hour, minute, second)
			}
		}
		result[alias] = row
	}
	return result, nil
}

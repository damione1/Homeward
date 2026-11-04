package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
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
	request  *apitypes.RequestID
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
		Request  string              `json:"request_id,omitempty"`
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
		id, err := apitypes.ParseRequestID(value.Request)
		if err != nil {
			return nil, err
		}
		result.request = &id
	case nhf009DenyWrite, nhf009AllowWrite, nhf009WithdrawCover, nhf009WithdrawLock, nhf009WithdrawAlarm, nhf009Rediscover, nhf009DropObservations, nhf009RestoreObservations, nhf009Outage, nhf009RestoreNetwork:
		if value.Request != "" {
			return nil, errors.New("unexpected synthetic request identity")
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

// A fixture control carries only the public request UUID. Bind it through the
// existing SQL provenance to the native driver's last occurrence. Never invert
// an operation name or construct a native command from the public request UUID.
func nhf009ResolveControlCommand(ctx context.Context, store *storage.Store, actor domain.ActorID, home apitypes.HomeName, request *apitypes.RequestID, driver *nhf008Driver) (domain.CommandID, error) {
	if store == nil || driver == nil || request == nil || !request.Valid() || !home.Valid() {
		return domain.CommandID{}, errors.New("missing synthetic request binding")
	}
	driver.mu.Lock()
	if driver.last == nil {
		driver.mu.Unlock()
		return domain.CommandID{}, errors.New("missing admitted synthetic occurrence")
	}
	last := *driver.last
	driver.mu.Unlock()
	native, _, err := last.RouteIdentity()
	if err != nil {
		return domain.CommandID{}, err
	}
	nativeText, err := native.Value()
	if err != nil {
		return domain.CommandID{}, err
	}
	expected, err := apitypes.ParseOperationName(home.String() + "/operations/cmd-" + nativeText)
	if err != nil {
		return domain.CommandID{}, err
	}
	matches, err := store.FindAPICommandsByRequest(ctx, actor, home, *request, time.Now())
	if err != nil {
		return domain.CommandID{}, err
	}
	if len(matches) != 1 || matches[0] != expected {
		return domain.CommandID{}, errors.New("synthetic request does not name the last admitted occurrence")
	}
	record, found, err := store.CommandIntent(ctx, native)
	if err != nil {
		return domain.CommandID{}, err
	}
	metadata, present := record.APIMetadata()
	key, keyed := metadata.RequestID()
	if !found || !present || !keyed || key != *request || record.OriginKind() != storage.IntentOriginExternalActor {
		return domain.CommandID{}, errors.New("synthetic occurrence lacks exact API provenance")
	}
	return native, nil
}

package view

import (
	"encoding/hex"

	"github.com/damione1/Homeward/contracts"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	statewire "github.com/damione1/Homeward/gen/home/state/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// EncodePage translates the library result to the additive v1 schema. The
// future common Connect server owns authentication and calls this conversion.
func EncodePage(page Page) (*statewire.OpenViewResponse, error) {
	if len(page.SnapshotToken) != 64 || page.ExpiresAt.IsZero() || len(page.Items) > MaxPageSize {
		return nil, ErrInvalidInput
	}
	decoded, err := hex.DecodeString(page.SnapshotToken)
	if err != nil || allZero(decoded) {
		return nil, ErrInvalidInput
	}
	if page.NextPageToken != "" {
		if len(page.NextPageToken) != 64 {
			return nil, ErrInvalidInput
		}
		decoded, err := hex.DecodeString(page.NextPageToken)
		if err != nil || allZero(decoded) {
			return nil, ErrInvalidInput
		}
	}
	sequence, config, authorizationRevision, filterHash, err := page.Cursor.Components()
	if err != nil {
		return nil, err
	}
	epoch, seq, err := sequence.Components()
	if err != nil {
		return nil, err
	}
	configRevision, configHash, err := config.Components()
	if err != nil {
		return nil, err
	}
	response := &statewire.OpenViewResponse{
		SnapshotToken: page.SnapshotToken,
		Cursor:        &statewire.ViewCursor{Epoch: epoch, GlobalSequence: seq, ConfigRevision: configRevision, AuthorizationRevision: authorizationRevision, FilterSha256: filterHash[:], ConfigSha256: configHash[:]},
		NextPageToken: page.NextPageToken,
		ExpiresAt:     timestamppb.New(page.ExpiresAt.UTC()),
		Entities:      make([]*statewire.ViewEntity, 0, len(page.Items)),
	}
	if err := response.ExpiresAt.CheckValid(); err != nil {
		return nil, ErrInvalidInput
	}
	for _, item := range page.Items {
		device, entity, err := item.record.IDs()
		if err != nil || item.record.Retired() {
			return nil, ErrInvalidInput
		}
		deviceText, _ := device.Value()
		entityText, _ := entity.Value()
		original, current, aliases, err := item.record.Aliases()
		if err != nil {
			return nil, err
		}
		originalText, _ := original.Value()
		currentText, _ := current.Value()
		wire := &statewire.ViewEntity{EntityId: entityText, DeviceId: deviceText, OriginalAlias: originalText, CurrentAlias: currentText, Capabilities: item.Capabilities()}
		for _, alias := range aliases {
			value, _ := alias.Value()
			if value != currentText {
				wire.HistoricalAliases = append(wire.HistoricalAliases, value)
			}
		}
		if item.hasState {
			encoded, err := contracts.EncodeObservedState(item.state)
			if err != nil {
				return nil, err
			}
			wire.State = encoded
			response.States = append(response.States, proto.Clone(encoded).(*statewire.ObservedState))
		}
		for _, description := range wire.Capabilities {
			response.Capabilities = append(response.Capabilities, proto.Clone(description).(*registrywire.CapabilityDescription))
		}
		response.Entities = append(response.Entities, wire)
	}
	return response, nil
}

func allZero(bytes []byte) bool {
	for _, b := range bytes {
		if b != 0 {
			return false
		}
	}
	return true
}

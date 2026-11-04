package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// CommandAPIMetadata is immutable provenance attached to the native ledger row.
// It is not an independent intention, execution owner, or permission.
type CommandAPIMetadata struct {
	capability  apitypes.CapabilityName
	request     apitypes.RequestID
	fingerprint [sha256.Size]byte
}

func NewCommandAPIMetadata(capability apitypes.CapabilityName, request *apitypes.RequestID, fingerprint [sha256.Size]byte) (CommandAPIMetadata, error) {
	if !capability.Valid() || fingerprint == ([sha256.Size]byte{}) || request != nil && !request.Valid() {
		return CommandAPIMetadata{}, &Error{kind: InvalidSettings}
	}
	value := CommandAPIMetadata{capability: capability, fingerprint: fingerprint}
	if request != nil {
		value.request = *request
	}
	return value, nil
}
func (m CommandAPIMetadata) Capability() apitypes.CapabilityName { return m.capability }
func (m CommandAPIMetadata) RequestID() (apitypes.RequestID, bool) {
	return m.request, m.request.Valid()
}
func (m CommandAPIMetadata) Fingerprint() [sha256.Size]byte { return m.fingerprint }
func (r IntentRecord) APIMetadata() (CommandAPIMetadata, bool) {
	return r.api, r.api.capability.Valid()
}
func (r IntentRecord) AcceptedAt() time.Time { return r.acceptedAt }

func parseCommandAPIMetadata(capability, request, fingerprint *string) (CommandAPIMetadata, error) {
	if capability == nil && request == nil && fingerprint == nil {
		return CommandAPIMetadata{}, nil
	}
	if capability == nil || fingerprint == nil {
		return CommandAPIMetadata{}, &Error{kind: InconsistentSchema}
	}
	name, err := apitypes.ParseCapabilityName(*capability)
	if err != nil {
		return CommandAPIMetadata{}, &Error{kind: InconsistentSchema}
	}
	var key *apitypes.RequestID
	if request != nil {
		value, err := apitypes.ParseRequestID(*request)
		if err != nil {
			return CommandAPIMetadata{}, &Error{kind: InconsistentSchema}
		}
		key = &value
	}
	bytes, err := hex.DecodeString(strings.TrimPrefix(*fingerprint, `\x`))
	if err != nil || len(bytes) != sha256.Size {
		return CommandAPIMetadata{}, &Error{kind: InconsistentSchema}
	}
	var digest [sha256.Size]byte
	copy(digest[:], bytes)
	return NewCommandAPIMetadata(name, key, digest)
}

// AcceptAPICommandIntent reserves API provenance in the same transaction and
// same row as the existing durable intention, before any physical effect.
func (s *Store) AcceptAPICommandIntent(ctx context.Context, id domain.CommandID, actor domain.ActorID, epoch string, fingerprint []byte, target authorization.CommandTarget, metadata CommandAPIMetadata) (IntentRecord, bool, TransactionOutcome, error) {
	if _, _, err := target.Values(); err != nil || !metadata.capability.Valid() {
		return IntentRecord{}, false, TransactionNotCommitted, &Error{kind: InvalidSettings}
	}
	actorText, err := actor.Value()
	if err != nil {
		return IntentRecord{}, false, TransactionNotCommitted, &Error{kind: InvalidSettings}
	}
	return s.acceptCommandIntent(ctx, id, commandOrigin{kind: IntentOriginExternalActor, actor: actorText, api: &metadata}, epoch, fingerprint, &target)
}

// AppendCommandAPITerminal records one final native tracker result. A known
// result cannot be changed by a later read, timeout, restart or observation.
func (s *Store) AppendCommandAPITerminal(ctx context.Context, id domain.CommandID, response []byte) (TransactionOutcome, error) {
	key, err := id.Value()
	if err != nil || len(response) == 0 || len(response) > 65536 {
		return TransactionNotCommitted, &Error{kind: InvalidSettings}
	}
	return s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE public.command_intents SET api_terminal_response=$2 WHERE command_id=$1 AND api_capability_name IS NOT NULL AND state='result' AND api_terminal_response IS NULL`, key, response)
		if err != nil || tag.RowsAffected() == 1 {
			return err
		}
		var existing []byte
		if err := tx.QueryRow(ctx, `SELECT api_terminal_response FROM public.command_intents WHERE command_id=$1 AND api_capability_name IS NOT NULL AND state='result'`, key).Scan(&existing); err != nil {
			return err
		}
		if !bytes.Equal(existing, response) {
			return &Error{kind: InconsistentSchema}
		}
		return nil
	})
}

// Recovery has no tracker from the former process. Preserve the recorded
// dispatch ACK as explicitly unverified instead of inventing confirmation.
func (s *Store) RecoverCommandAPITerminals(ctx context.Context) (TransactionOutcome, error) {
	return s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE public.command_intents SET api_terminal_response=COALESCE(api_acknowledged_response,response) WHERE api_capability_name IS NOT NULL AND state='result' AND api_terminal_response IS NULL`)
		return err
	})
}
func (r IntentRecord) APITerminalResponse() []byte { return append([]byte(nil), r.apiTerminal...) }

// AppendCommandAPIAcknowledgement persists a native ACK fact without ending the
// confirmation tracker. The exact persisted target and command ID bind proof
// to the admitted occurrence; neither this method nor reads dispatch effects.
func (s *Store) AppendCommandAPIAcknowledgement(ctx context.Context, id domain.CommandID, target authorization.CommandTarget, response []byte) (TransactionOutcome, error) {
	key, err := id.Value()
	entity, slot, targetErr := target.Values()
	entityText, entityErr := entity.Value()
	schema, endpoint, slotErr := slot.Values()
	proof := &actions.CommandResponse{}
	if err != nil || targetErr != nil || entityErr != nil || slotErr != nil || len(response) == 0 || len(response) > 65536 || proto.Unmarshal(response, proof) != nil || proof.CommandId != key || proof.Status != actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED || proof.Error != nil || len(proof.ProtoReflect().GetUnknown()) != 0 {
		return TransactionNotCommitted, &Error{kind: InvalidSettings}
	}
	canonical, err := proto.Marshal(proof)
	if err != nil {
		return TransactionNotCommitted, &Error{kind: InvalidSettings}
	}
	return s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE public.command_intents SET api_acknowledged_response=$2 WHERE command_id=$1 AND api_capability_name IS NOT NULL AND state='result' AND authorization_entity_id=$3 AND authorization_schema_id=$4 AND authorization_endpoint=$5 AND api_acknowledged_response IS NULL`, key, canonical, entityText, schema, endpoint)
		if err != nil || tag.RowsAffected() == 1 {
			return err
		}
		var existing []byte
		if err := tx.QueryRow(ctx, `SELECT api_acknowledged_response FROM public.command_intents WHERE command_id=$1 AND api_capability_name IS NOT NULL AND state='result' AND authorization_entity_id=$2 AND authorization_schema_id=$3 AND authorization_endpoint=$4`, key, entityText, schema, endpoint).Scan(&existing); err != nil {
			return err
		}
		if !bytes.Equal(existing, canonical) {
			return &Error{kind: InconsistentSchema}
		}
		return nil
	})
}
func (r IntentRecord) APIAcknowledgedResponse() []byte {
	return append([]byte(nil), r.apiAcknowledged...)
}

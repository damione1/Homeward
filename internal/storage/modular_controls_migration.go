package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"

	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/recorderevidence"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// SQL31's only legacy decoder. Runtime state admission never accepts this
// enum. Untouched fields retain their original wire bytes, including unknowns.
func migrateLegacyFanPayload(raw []byte) ([]byte, bool, error) {
	out := make([]byte, 0, len(raw)+16)
	changed := false
	for remaining := raw; len(remaining) > 0; {
		num, typ, n := protowire.ConsumeTag(remaining)
		if n < 0 {
			return nil, false, ErrRecorderInvalid
		}
		size := protowire.ConsumeFieldValue(num, typ, remaining[n:])
		if size < 0 {
			return nil, false, ErrRecorderInvalid
		}
		field := remaining[:n+size]
		if num != 14 {
			out = append(out, field...)
			remaining = remaining[n+size:]
			continue
		}
		if typ != protowire.BytesType || changed {
			return nil, false, ErrRecorderInvalid
		}
		value, k := protowire.ConsumeBytes(remaining[n:])
		if k < 0 {
			return nil, false, ErrRecorderInvalid
		}
		var speed uint64
		seen := false
		extra := []byte{}
		for len(value) > 0 {
			fn, ft, tn := protowire.ConsumeTag(value)
			if tn < 0 {
				return nil, false, ErrRecorderInvalid
			}
			count := protowire.ConsumeFieldValue(fn, ft, value[tn:])
			if count < 0 {
				return nil, false, ErrRecorderInvalid
			}
			if fn == 1 {
				if ft != protowire.VarintType || seen {
					return nil, false, ErrRecorderInvalid
				}
				speed, k = protowire.ConsumeVarint(value[tn:])
				if k < 0 || speed < 1 || speed > 5 {
					return nil, false, ErrRecorderInvalid
				}
				seen = true
			} else {
				// New fields cannot pre-exist in the authenticated old schema.
				if fn == 2 || fn == 3 || fn == 4 {
					return nil, false, ErrRecorderInvalid
				}
				extra = append(extra, value[:tn+count]...)
			}
			value = value[tn+count:]
		}
		if !seen {
			return nil, false, ErrRecorderInvalid
		}
		keys := [...]string{"", "Off", "Low", "Medium", "MediumHigh", "High"}
		fan := protowire.AppendTag(nil, 2, protowire.VarintType)
		on := uint64(1)
		if speed == 1 {
			on = 0
		}
		fan = protowire.AppendVarint(fan, on)
		fan = protowire.AppendTag(fan, 3, protowire.BytesType)
		fan = protowire.AppendString(fan, keys[speed])
		fan = append(fan, extra...)
		out = protowire.AppendTag(out, 14, protowire.BytesType)
		out = protowire.AppendBytes(out, fan)
		changed = true
		remaining = remaining[n+size:]
	}
	return out, changed, nil
}

func fanMigrationObserved(w *state.ObservedState, payload []byte) (recorderObserved, error) {
	if w == nil || w.Entity.GetEntityId() == "" || w.Quality == nil || w.GetFan() == nil || w.ReceivedAt.CheckValid() != nil || w.LastChanged.CheckValid() != nil || w.LastUpdated.CheckValid() != nil || w.LastReported.CheckValid() != nil {
		return recorderObserved{}, ErrRecorderInvalid
	}
	kind := int16(4)
	o := recorderObserved{entity: w.Entity.GetEntityId(), revision: w.Revision, sourceSession: w.SourceSession, sourceSequence: w.SourceSeq,
		availability: int16(w.Quality.Availability), freshness: int16(w.Quality.Freshness), provenance: int16(w.Quality.Provenance), valueKind: &kind,
		receivedAt: w.ReceivedAt.AsTime().UTC(), lastChanged: w.LastChanged.AsTime().UTC(), lastUpdated: w.LastUpdated.AsTime().UTC(), lastReported: w.LastReported.AsTime().UTC(), payload: payload, payloadDigest: sha256.Sum256(payload)}
	if w.ObservedAt != nil {
		if w.ObservedAt.CheckValid() != nil {
			return recorderObserved{}, ErrRecorderInvalid
		}
		at := w.ObservedAt.AsTime().UTC()
		o.observedAt = &at
	}
	return o, nil
}

// Read at most 64 rows into memory, close the cursor, then update in the same
// exclusive migration transaction. A bad payload or digest rolls back all rows.
func migrateFanObservations31(ctx context.Context, tx pgx.Tx) error {
	for _, table := range []string{"state_history", "recorder_snapshots"} {
		cursor := "homeward_fan31_" + table
		query := `SELECT epoch,sequence::text,entity_id,payload,payload_digest,record_digest FROM public.state_history WHERE value_kind=4 ORDER BY epoch,sequence`
		if table == "recorder_snapshots" {
			query = `SELECT epoch,boundary_sequence::text,entity_id,payload,payload_digest,record_digest FROM public.recorder_snapshots ORDER BY epoch,boundary_sequence,entity_digest`
		}
		if _, err := tx.Exec(ctx, "DECLARE "+cursor+" NO SCROLL CURSOR FOR "+query); err != nil {
			return err
		}
		for {
			rows, err := tx.Query(ctx, "FETCH FORWARD 64 FROM "+cursor)
			if err != nil {
				return err
			}
			type entry struct {
				epoch                                []byte
				sequence, entity                     string
				payload, payloadDigest, recordDigest []byte
			}
			batch := []entry{}
			for rows.Next() {
				var r entry
				if err = rows.Scan(&r.epoch, &r.sequence, &r.entity, &r.payload, &r.payloadDigest, &r.recordDigest); err != nil {
					rows.Close()
					return err
				}
				batch = append(batch, r)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			if len(batch) == 0 {
				break
			}
			for _, r := range batch {
				next, changed, err := migrateLegacyFanPayload(r.payload)
				if err != nil {
					return err
				}
				if !changed {
					continue
				}
				sum := sha256.Sum256(r.payload)
				if !bytes.Equal(sum[:], r.payloadDigest) {
					return ErrRecorderInvalid
				}
				old := &state.ObservedState{}
				if err = proto.Unmarshal(r.payload, old); err != nil {
					return err
				}
				if old.Entity.GetEntityId() != r.entity {
					return ErrRecorderInvalid
				}
				observed, err := fanMigrationObserved(old, r.payload)
				if err != nil {
					return err
				}
				var digest [32]byte
				if table == "state_history" {
					epoch, err := ParseRecorderEpoch(hex.EncodeToString(r.epoch))
					if err != nil {
						return err
					}
					sequence, err := strconv.ParseUint(r.sequence, 10, 64)
					if err != nil {
						return err
					}
					if _, err = verifyHistoricalRecord(ctx, tx, epoch, sequence, old, r.payload); err != nil {
						return err
					}
					var row RecorderRecord
					var seconds int64
					var nanos int32
					var evidence []byte
					err = tx.QueryRow(ctx, `SELECT instance_id,admitted_unix_seconds,admitted_nanos,causation_id,correlation_id,admission_evidence FROM public.state_history WHERE epoch=$1 AND sequence=$2::numeric`, r.epoch, r.sequence).Scan(&row.instance, &seconds, &nanos, &row.causation, &row.correlation, &evidence)
					if err != nil {
						return err
					}
					if evidence != nil {
						row.evidence, err = recorderevidence.Parse(evidence)
						if err != nil {
							return err
						}
					}
					row.sequence = sequence
					row.admittedAt = time.Unix(seconds, int64(nanos)).UTC()
					observed.payload = next
					observed.payloadDigest = sha256.Sum256(next)
					row.observed = observed
					digest = digestRecorderRecord(row)
					_, err = tx.Exec(ctx, `UPDATE public.state_history SET payload=$3,payload_digest=$4,record_digest=$5 WHERE epoch=$1 AND sequence=$2::numeric`, r.epoch, r.sequence, next, observed.payloadDigest[:], digest[:])
					if err != nil {
						return err
					}
				} else {
					prior := digestRecorderSnapshot(RecorderSnapshotRecord{observed: observed})
					if !bytes.Equal(prior[:], r.recordDigest) {
						return ErrRecorderInvalid
					}
					observed.payload = next
					observed.payloadDigest = sha256.Sum256(next)
					digest = digestRecorderSnapshot(RecorderSnapshotRecord{observed: observed})
					_, err = tx.Exec(ctx, `UPDATE public.recorder_snapshots SET payload=$4,payload_digest=$5,record_digest=$6 WHERE epoch=$1 AND boundary_sequence=$2::numeric AND entity_id=$3`, r.epoch, r.sequence, r.entity, next, observed.payloadDigest[:], digest[:])
					if err != nil {
						return err
					}
				}
			}
		}
		if _, err := tx.Exec(ctx, "CLOSE "+cursor); err != nil {
			return err
		}
	}
	// Historical batch receipts and command ledgers are left intact. This
	// migration converts observations only and never dispatches an action.
	return nil
}

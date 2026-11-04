package mediasessions

import (
	"context"
	"errors"
	"time"

	"github.com/damione1/Homeward/internal/apimutations"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
)

const recordColumns = `session_id,actor_id,credential_binding,source_entity_id,source_schema,source_endpoint,content_kind,source_binding,artwork_reference,event_revision,create_time,expire_time,retire_time,discard_time`

var errNoOriginalResult = errors.New("no original API result")

type rowScanner interface{ Scan(...any) error }

func scanRecord(row rowScanner) (Record, error) {
	var r Record
	var id, actor, entity, schema, endpoint string
	var credential, digest []byte
	var kind int16
	var retired *time.Time
	err := row.Scan(&id, &actor, &credential, &entity, &schema, &endpoint, &kind, &digest, &r.reference, &r.eventRevision, &r.created, &r.expires, &retired, &r.discard)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, failure(NotFound)
	}
	if err != nil {
		return Record{}, err
	}
	r.id, err = ParseID(id)
	if err != nil {
		return Record{}, failure(Unavailable)
	}
	r.actor, err = domain.ParseActorID(actor)
	if err != nil {
		return Record{}, failure(Unavailable)
	}
	r.binding.entity, err = domain.ParseEntityID(entity)
	if err != nil {
		return Record{}, failure(Unavailable)
	}
	r.binding.slot, err = authorization.NewCapabilitySlot(schema, endpoint)
	if err != nil {
		return Record{}, failure(Unavailable)
	}
	if len(credential) != 32 || len(digest) != 32 {
		return Record{}, failure(Unavailable)
	}
	copy(r.credential[:], credential)
	copy(r.binding.digest[:], digest)
	r.binding.kind = Kind(kind)
	if retired != nil {
		r.retired = *retired
	}
	if !r.valid() {
		return Record{}, failure(Unavailable)
	}
	return r, nil
}

// transaction establishes SQL authority before the shared request-result gate
// and before any MediaSession capacity/row locks. It never calls Authority while
// the transaction holds account or policy locks.
func (s *Service) transaction(ctx context.Context, a *Admission, fn func(context.Context, pgx.Tx) (Record, error)) (Record, error) {
	if ctx == nil || fn == nil || !s.validAdmission(a) {
		return Record{}, failure(Denied)
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var result Record
	outcome, err := s.store.InTransactionOutcome(bounded, func(tx pgx.Tx) error {
		if err := authorization.FenceSQL(bounded, tx, a.evidence); err != nil {
			return err
		}
		var err error
		result, err = apimutations.Execute[Record](bounded, tx, func() (Record, error) { return fn(bounded, tx) })
		return err
	})
	if outcome == storage.TransactionIndeterminate {
		return Record{}, failure(CommitUnknown)
	}
	if err != nil {
		if errors.Is(err, errNoOriginalResult) {
			return Record{}, errNoOriginalResult
		}
		if apimutations.IsResultError(err) {
			return Record{}, err
		}
		var own *Error
		if errors.As(err, &own) {
			return Record{}, own
		}
		if errors.Is(err, authorization.ErrDenied) {
			return Record{}, failure(Denied)
		}
		if ctx.Err() != nil {
			return Record{}, ctx.Err()
		}
		return Record{}, failure(Unavailable)
	}
	if outcome != storage.TransactionCommitted {
		return Record{}, failure(Unavailable)
	}
	if !a.Ready() {
		return Record{}, failure(CommittedResponseWithheld)
	}
	return result, nil
}
func (s *Service) lookup(ctx context.Context, id ID, actor domain.ActorID, credential [32]byte) (Locator, error) {
	actorText, err := actor.Value()
	if err != nil {
		return Locator{}, failure(Denied)
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var result Locator
	// This identity-only lookup exposes no source content or result metadata. The
	// exact original slot must subsequently be admitted and SQL-fenced for reads.
	outcome, err := s.store.InTransactionOutcome(bounded, func(tx pgx.Tx) error {
		var entity, schema, endpoint string
		var kind int16
		err := tx.QueryRow(bounded, `SELECT source_entity_id,source_schema,source_endpoint,content_kind FROM public.api_media_sessions WHERE session_id=$1 AND actor_id=$2 AND credential_binding=$3 AND discard_time>$4`, id.String(), actorText, credential[:], s.clock.Now()).Scan(&entity, &schema, &endpoint, &kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return failure(NotFound)
		}
		if err != nil {
			return err
		}
		parsedEntity, err := domain.ParseEntityID(entity)
		if err != nil {
			return failure(Unavailable)
		}
		slot, err := authorization.NewCapabilitySlot(schema, endpoint)
		if err != nil || !Kind(kind).Valid() {
			return failure(Unavailable)
		}
		result = Locator{id, actor, credential, parsedEntity, slot, Kind(kind)}
		return nil
	})
	if err != nil {
		var own *Error
		if errors.As(err, &own) {
			return Locator{}, own
		}
		if ctx.Err() != nil {
			return Locator{}, ctx.Err()
		}
		return Locator{}, failure(Unavailable)
	}
	if outcome != storage.TransactionCommitted {
		return Locator{}, failure(Unavailable)
	}
	return result, nil
}
func ownedRecord(ctx context.Context, tx pgx.Tx, a *Admission, id ID, write bool) (Record, error) {
	actor, err := a.actor.Value()
	if err != nil {
		return Record{}, failure(Denied)
	}
	query := `SELECT ` + recordColumns + ` FROM public.api_media_sessions WHERE session_id=$1 AND actor_id=$2 AND credential_binding=$3`
	if write {
		query += ` FOR UPDATE`
	} else {
		query += ` FOR SHARE`
	}
	record, err := scanRecord(tx.QueryRow(ctx, query, id.String(), actor, a.credential[:]))
	if err != nil {
		return Record{}, err
	}
	if record.binding.entity != a.entity || record.binding.slot != a.slot {
		return Record{}, failure(Denied)
	}
	return record, nil
}

// Create reserves both retained metadata and live-lease capacity before insert.
// SQL is the sole lease owner; reads and replay never extend its timestamps.
func (s *Service) Create(ctx context.Context, a *Admission, input Input) (Record, error) {
	if !input.valid || !s.validAdmission(a) || input.binding.entity != a.entity || input.binding.slot != a.slot {
		return Record{}, failure(Denied)
	}
	id, err := NewID()
	if err != nil {
		return Record{}, failure(Unavailable)
	}
	now := s.clock.Now().UTC().Truncate(time.Microsecond)
	if now.IsZero() {
		return Record{}, failure(Unavailable)
	}
	record := Record{id: id, actor: a.actor, credential: a.credential, binding: input.binding, reference: append([]byte(nil), input.reference...), eventRevision: input.eventRevision, created: now, expires: now.Add(input.lifetime), discard: now.Add(input.lifetime).Add(TombstoneRetention)}
	if !record.valid() {
		return Record{}, failure(Invalid)
	}
	return s.transaction(ctx, a, func(ctx context.Context, tx pgx.Tx) (Record, error) {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(1735553389,203)`); err != nil {
			return Record{}, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM public.api_media_sessions WHERE discard_time<=$1`, now); err != nil {
			return Record{}, err
		}
		actor, _ := record.actor.Value()
		var total, owned, active, ownedActive int64
		if err := tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE actor_id=$1),count(*) FILTER(WHERE retire_time IS NULL AND expire_time>$2),count(*) FILTER(WHERE actor_id=$1 AND retire_time IS NULL AND expire_time>$2) FROM public.api_media_sessions`, actor, now).Scan(&total, &owned, &active, &ownedActive); err != nil {
			return Record{}, err
		}
		if total >= MaxMetadataGlobal || owned >= MaxMetadataPerActor {
			return Record{}, failure(RetentionCapacity)
		}
		if active >= MaxActiveGlobal || ownedActive >= MaxActivePerActor {
			return Record{}, failure(ActiveCapacity)
		}
		entity, _ := record.binding.entity.Value()
		schema, endpoint, _ := record.binding.slot.Values()
		var reference []byte
		if input.binding.kind == Artwork {
			reference = record.reference
		}
		_, err := tx.Exec(ctx, `INSERT INTO public.api_media_sessions (`+recordColumns+`) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NULL,$13)`, id.String(), actor, record.credential[:], entity, schema, endpoint, int16(record.binding.kind), record.binding.digest[:], reference, record.eventRevision, record.created, record.expires, record.discard)
		return record, err
	})
}

// Get checks original ownership, exact current source generation and lifetime
// under the current admitted source's account/policy SQL fence.
func (s *Service) Get(ctx context.Context, a *Admission, id ID, current SourceBinding) (Record, error) {
	if !id.Valid() || !current.Valid() {
		return Record{}, failure(Invalid)
	}
	return s.transaction(ctx, a, func(ctx context.Context, tx pgx.Tx) (Record, error) {
		record, err := ownedRecord(ctx, tx, a, id, false)
		if err != nil {
			return Record{}, err
		}
		if !record.Active(s.clock.Now()) {
			return Record{}, failure(NotFound)
		}
		if !record.MatchesSource(current) {
			return Record{}, failure(SourceChanged)
		}
		return record, nil
	})
}

// Retire preserves a bounded non-authoritative tombstone for exact original
// source reauthorization of request-result replays. It cancels and joins any
// registered stream even if COMMIT's outcome cannot be established.
func (s *Service) Retire(ctx context.Context, a *Admission, id ID, current SourceBinding) (Record, error) {
	if !id.Valid() || !current.Valid() {
		return Record{}, failure(Invalid)
	}
	record, err := s.transaction(ctx, a, func(ctx context.Context, tx pgx.Tx) (Record, error) {
		record, err := ownedRecord(ctx, tx, a, id, true)
		if err != nil {
			return Record{}, err
		}
		now := s.clock.Now().UTC().Truncate(time.Microsecond)
		if !record.Active(now) {
			return Record{}, failure(NotFound)
		}
		// Retirement only removes access to this original slot. A new
		// configuration digest cannot prevent the owner from releasing its
		// non-readable old lease; no content is admitted by this path.
		if record.binding.entity != current.entity || record.binding.slot != current.slot || record.binding.kind != current.kind {
			return Record{}, failure(SourceChanged)
		}
		if _, err := tx.Exec(ctx, `UPDATE public.api_media_sessions SET retire_time=$2 WHERE session_id=$1`, id.String(), now); err != nil {
			return Record{}, err
		}
		record.retired = now
		return record, nil
	})
	var resultError *apimutations.Error
	replayed := errors.As(err, &resultError) && resultError.Code() == apimutations.Replayed
	if err == nil || replayed || IsCode(err, CommitUnknown) || IsCode(err, CommittedResponseWithheld) {
		// Stream sends already observe permit/caller cancellation; join remains
		// bounded even when an unrelated native producer violates cancellation.
		join, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if joinErr := s.cancelOperations(join, id); joinErr != nil && err == nil {
			return Record{}, failure(CommittedResponseWithheld)
		}
	}
	return record, err
}

// OwnedLocators returns only the bounded identities needed to reauthorize this
// credential's live sessions. The caller must acquire each exact Source permit
// and call Get before publishing any session metadata.
func (s *Service) OwnedLocators(ctx context.Context) ([]Locator, error) {
	if ctx == nil {
		return nil, failure(Invalid)
	}
	if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	_, actor, credential, err := s.principal(ctx)
	if err != nil {
		return nil, err
	}
	actorText, err := actor.Value()
	if err != nil {
		return nil, failure(Denied)
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var out []Locator
	outcome, err := s.store.InTransactionOutcome(bounded, func(tx pgx.Tx) error {
		rows, err := tx.Query(bounded, `SELECT session_id,source_entity_id,source_schema,source_endpoint,content_kind FROM public.api_media_sessions WHERE actor_id=$1 AND credential_binding=$2 AND retire_time IS NULL AND expire_time>$3 ORDER BY session_id LIMIT $4`, actorText, credential[:], s.clock.Now(), MaxActivePerActor+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw, entity, schema, endpoint string
			var kind int16
			if err := rows.Scan(&raw, &entity, &schema, &endpoint, &kind); err != nil {
				return err
			}
			id, err := ParseID(raw)
			if err != nil {
				return failure(Unavailable)
			}
			parsed, err := domain.ParseEntityID(entity)
			if err != nil {
				return failure(Unavailable)
			}
			slot, err := authorization.NewCapabilitySlot(schema, endpoint)
			if err != nil || !Kind(kind).Valid() {
				return failure(Unavailable)
			}
			out = append(out, Locator{id, actor, credential, parsed, slot, Kind(kind)})
			if len(out) > MaxActivePerActor {
				return failure(Unavailable)
			}
		}
		return rows.Err()
	})
	if outcome != storage.TransactionCommitted || err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, failure(Unavailable)
	}
	return out, nil
}

// RetireVerified is native lifecycle cleanup, never a public mutation or a
// content read. Its only input is an immutable, owner-authenticated Record;
// expiry/source/credential cannot be chosen or extended by a caller. Cleanup
// needs no user permission after revocation and acquires no account/policy lock.
func (s *Service) RetireVerified(record Record) error {
	if s == nil || s.store == nil || !record.valid() {
		return failure(Invalid)
	}
	actor, err := record.actor.Value()
	if err != nil {
		return failure(Invalid)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	outcome, err := s.store.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE public.api_media_sessions SET retire_time=$5 WHERE session_id=$1 AND actor_id=$2 AND credential_binding=$3 AND source_binding=$4 AND retire_time IS NULL`, record.id.String(), actor, record.credential[:], record.binding.digest[:], s.clock.Now().UTC().Truncate(time.Microsecond))
		return err
	})
	// Both known and uncertain cleanup stop and join all local executions.
	_ = s.cancelOperations(ctx, record.id)
	if outcome == storage.TransactionIndeterminate {
		return failure(CommitUnknown)
	}
	if err != nil || outcome != storage.TransactionCommitted {
		return failure(Unavailable)
	}
	return nil
}

// ProbeReplay performs no owner effect or result reservation. It allows a
// request with an original result to finish after its source reference expires,
// while still requiring the original source's current authority fence. A miss
// aborts its transaction; Create/Retire perform the authoritative race-safe
// result gate again in their own mutation transaction after native validation.
func (s *Service) ProbeReplay(ctx context.Context, a *Admission) error {
	_, err := s.transaction(ctx, a, func(context.Context, pgx.Tx) (Record, error) { return Record{}, errNoOriginalResult })
	if errors.Is(err, errNoOriginalResult) {
		return nil
	}
	return err
}

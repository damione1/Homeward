package storage

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// IntentState is the storage lifecycle. An unrecognized database value fails
// closed; no caller may turn it into permission to dispatch.
type IntentState uint8

const (
	IntentUnknown IntentState = iota
	IntentAccepted
	IntentDispatching
	IntentResult
	IntentIndeterminate
)

type IntentError uint8

const (
	IntentErrorUnknown IntentError = iota
	IntentNoError
	IntentRouteBusy
	IntentRouteUnavailable
	IntentCanceled
	IntentDeadline
	IntentOtherError
	IntentOutcomeIndeterminate
)

type IntentRecord struct {
	state       IntentState
	fingerprint []byte
	response    []byte
	errorKind   IntentError
}

func (r IntentRecord) State() IntentState     { return r.state }
func (r IntentRecord) Fingerprint() []byte    { return bytes.Clone(r.fingerprint) }
func (r IntentRecord) Response() []byte       { return bytes.Clone(r.response) }
func (r IntentRecord) ErrorKind() IntentError { return r.errorKind }

func scanIntent(row pgx.Row) (IntentRecord, error) {
	var state string
	var errorText string
	var fingerprint, response []byte
	if err := row.Scan(&state, &fingerprint, &response, &errorText); err != nil {
		return IntentRecord{}, err
	}
	var parsed IntentState
	switch state {
	case "accepted":
		parsed = IntentAccepted
	case "dispatching":
		parsed = IntentDispatching
	case "result":
		parsed = IntentResult
	case "indeterminate":
		parsed = IntentIndeterminate
	default:
		return IntentRecord{}, &Error{kind: InconsistentSchema}
	}
	var errorKind IntentError
	switch errorText {
	case "none":
		errorKind = IntentNoError
	case "route_busy":
		errorKind = IntentRouteBusy
	case "route_unavailable":
		errorKind = IntentRouteUnavailable
	case "canceled":
		errorKind = IntentCanceled
	case "deadline":
		errorKind = IntentDeadline
	case "unknown":
		errorKind = IntentOtherError
	case "indeterminate":
		errorKind = IntentOutcomeIndeterminate
	default:
		return IntentRecord{}, &Error{kind: InconsistentSchema}
	}
	return IntentRecord{parsed, bytes.Clone(fingerprint), bytes.Clone(response), errorKind}, nil
}

func (s *Store) CommandIntent(ctx context.Context, id domain.CommandID) (IntentRecord, bool, error) {
	if s == nil || s.pool == nil || ctx == nil {
		return IntentRecord{}, false, &Error{kind: InvalidSettings}
	}
	text, err := id.Value()
	if err != nil {
		return IntentRecord{}, false, err
	}
	record, err := scanIntent(s.pool.QueryRow(ctx, `SELECT state::text, fingerprint, response, result_error::text FROM public.command_intents WHERE command_id=$1`, text))
	if errors.Is(err, pgx.ErrNoRows) {
		return IntentRecord{}, false, nil
	}
	if err != nil {
		return IntentRecord{}, false, &Error{kind: Unavailable}
	}
	return record, true, nil
}

// AcceptCommandIntent serializes duplicate IDs through the primary key. A
// successful new INSERT is committed before its caller may attempt dispatch.
func (s *Store) AcceptCommandIntent(ctx context.Context, id domain.CommandID, actor domain.ActorID, epoch string, fingerprint []byte) (IntentRecord, bool, TransactionOutcome, error) {
	idText, idErr := id.Value()
	actorText, actorErr := actor.Value()
	if idErr != nil || actorErr != nil || epoch == "" || len(fingerprint) == 0 {
		return IntentRecord{}, false, TransactionNotCommitted, &Error{kind: InvalidSettings}
	}
	var record IntentRecord
	var created bool
	outcome, err := s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO public.command_intents(command_id,actor_id,admission_epoch,fingerprint,state)
			VALUES ($1,$2,$3,$4,'accepted') ON CONFLICT (command_id) DO NOTHING`, idText, actorText, epoch, fingerprint)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			created = true
			record = IntentRecord{state: IntentAccepted, fingerprint: bytes.Clone(fingerprint), errorKind: IntentNoError}
			return nil
		}
		record, err = scanIntent(tx.QueryRow(ctx, `SELECT state::text,fingerprint,response,result_error::text FROM public.command_intents WHERE command_id=$1`, idText))
		return err
	})
	if outcome != TransactionCommitted || err != nil {
		return IntentRecord{}, false, outcome, err
	}
	return record, created, outcome, nil
}

// MoveCommandIntent is a compare-and-swap transition; zero affected rows means
// another terminal state won. A COMMIT without an ACK is never permission to send.
func (s *Store) MoveCommandIntent(ctx context.Context, id domain.CommandID, from, to IntentState, response []byte, resultError IntentError) (bool, TransactionOutcome, error) {
	idText, err := id.Value()
	if err != nil {
		return false, TransactionNotCommitted, err
	}
	fromText, okFrom := intentStateText(from)
	toText, okTo := intentStateText(to)
	errorText, okError := intentErrorText(resultError)
	if !okFrom || !okTo || !okError || (to == IntentDispatching && resultError != IntentNoError) || (to == IntentDispatching && from != IntentAccepted) ||
		(to == IntentResult && from != IntentAccepted && from != IntentDispatching) ||
		(to == IntentIndeterminate && from != IntentDispatching) ||
		(to != IntentDispatching && to != IntentResult && to != IntentIndeterminate) ||
		(to == IntentDispatching && response != nil) ||
		(to != IntentDispatching && len(response) == 0) ||
		(to == IntentIndeterminate && resultError != IntentOutcomeIndeterminate) ||
		(to == IntentResult && resultError == IntentOutcomeIndeterminate) {
		return false, TransactionNotCommitted, &Error{kind: InvalidSettings}
	}
	var moved bool
	outcome, err := s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE public.command_intents SET state=$2::public.command_intent_state,response=$3,result_error=$4::public.command_result_error,updated_at=now()
			WHERE command_id=$1 AND state=$5::public.command_intent_state`, idText, toText, response, errorText, fromText)
		if err == nil {
			moved = tag.RowsAffected() == 1
		}
		return err
	})
	return moved && outcome == TransactionCommitted, outcome, err
}

func intentErrorText(kind IntentError) (string, bool) {
	switch kind {
	case IntentNoError:
		return "none", true
	case IntentRouteBusy:
		return "route_busy", true
	case IntentRouteUnavailable:
		return "route_unavailable", true
	case IntentCanceled:
		return "canceled", true
	case IntentDeadline:
		return "deadline", true
	case IntentOtherError:
		return "unknown", true
	case IntentOutcomeIndeterminate:
		return "indeterminate", true
	case IntentErrorUnknown:
		return "", false
	default:
		return "", false
	}
}

func intentStateText(state IntentState) (string, bool) {
	switch state {
	case IntentAccepted:
		return "accepted", true
	case IntentDispatching:
		return "dispatching", true
	case IntentResult:
		return "result", true
	case IntentIndeterminate:
		return "indeterminate", true
	case IntentUnknown:
		return "", false
	default:
		return "", false
	}
}

// CommandRuntime holds a database-wide session advisory lock while this
// process may dispatch durable commands. A replacement runtime cannot recover
// dispatching rows until the old session is gone.
type CommandRuntime struct {
	mu    sync.Mutex
	conn  *pgxpool.Conn
	owner *Store
}

func (s *Store) AcquireCommandRuntime(ctx context.Context) (*CommandRuntime, error) {
	if s == nil || s.pool == nil || ctx == nil {
		return nil, &Error{kind: InvalidSettings}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{kind: Unavailable}
	}
	var locked bool
	err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(481316, 24)`).Scan(&locked)
	if err != nil {
		physical := conn.Hijack()
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = physical.Close(closeCtx)
		return nil, &Error{kind: Unavailable}
	}
	if !locked {
		conn.Release()
		return nil, &Error{kind: CommandRuntimeBusy}
	}
	return &CommandRuntime{conn: conn, owner: s}, nil
}

func (r *CommandRuntime) Ping(ctx context.Context) error {
	if r == nil || ctx == nil {
		return &Error{kind: InvalidSettings}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pingLocked(ctx)
}

func (r *CommandRuntime) pingLocked(ctx context.Context) error {
	if r.conn == nil {
		return &Error{kind: Unavailable}
	}
	var one int
	if err := r.conn.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil || one != 1 {
		return &Error{kind: Unavailable}
	}
	return nil
}

func (r *CommandRuntime) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Hijack removes the session from the pool before closing it. Even an
	// uncertain close can never return a possibly locked connection to callers.
	physical := r.conn.Hijack()
	_ = physical.Close(ctx)
	_ = physical.PgConn().Conn().Close()
	r.conn = nil
}

// RecoverCommandIntents only runs while the runtime session guard is held.
// It never invokes a driver and leaves accepted intentions for consultation.
func (s *Store) RecoverCommandIntents(ctx context.Context, runtime *CommandRuntime, response []byte) (TransactionOutcome, error) {
	if len(response) == 0 || runtime == nil || runtime.owner != s {
		return TransactionNotCommitted, &Error{kind: InvalidSettings}
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if err := runtime.pingLocked(ctx); err != nil {
		return TransactionNotCommitted, err
	}
	return s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE public.command_intents SET state='indeterminate', response=$1, result_error='indeterminate', updated_at=now()
			WHERE state='dispatching'`, response)
		return err
	})
}

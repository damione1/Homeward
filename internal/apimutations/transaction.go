package apimutations

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"github.com/jackc/pgx/v5"
)

// Execute runs after the owner's account/policy SQL fence and before its data
// locks. A single bounded ledger gate serializes reservation/capacity decisions
// across domains. No authority lock, external call or goroutine enters this
// section. Replayed results abort this read-only transaction through a typed
// outcome; they never invoke fn or alter the original result/expiry.
func Execute[T any](ctx context.Context, tx pgx.Tx, fn func() (T, error)) (T, error) {
	var zero T
	bound, present := ctx.Value(requestKey[T]{}).(binding[T])
	if !present {
		return fn()
	}
	request := bound.request
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(1735553389,202)`); err != nil {
		return zero, err
	}
	// At most MaxRecords expired rows exist; cleanup is bounded by admission.
	if _, err := tx.Exec(ctx, `DELETE FROM public.api_mutation_results WHERE expire_time <= clock_timestamp()`); err != nil {
		return zero, err
	}
	var fingerprint, result, resultHash []byte
	err := tx.QueryRow(ctx, `SELECT fingerprint,result,result_sha256 FROM public.api_mutation_results WHERE actor_id=$1 AND method=$2 AND request_id=$3::uuid`, request.actor, request.method, request.id).Scan(&fingerprint, &result, &resultHash)
	if err == nil {
		if !bytes.Equal(fingerprint, request.fingerprint[:]) {
			return zero, &Error{code: RequestIDReused}
		}
		hash := sha256.Sum256(result)
		if len(result) > MaxResultBytes || !bytes.Equal(hash[:], resultHash) {
			return zero, &Error{code: StoredResultCorrupt}
		}
		return zero, &Error{code: Replayed, result: bytes.Clone(result)}
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return zero, err
	}
	var count, actorCount, stored, actorStored int64
	if err := tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE actor_id=$1),coalesce(sum(octet_length(result)),0),coalesce(sum(octet_length(result)) FILTER(WHERE actor_id=$1),0) FROM public.api_mutation_results`, request.actor).Scan(&count, &actorCount, &stored, &actorStored); err != nil {
		return zero, err
	}
	// Reserve the maximum bounded result charge before invoking a mutation.
	if count >= MaxRecords || actorCount >= MaxActorRecords || stored > MaxStoredBytes-MaxResultBytes || actorStored > MaxActorStoredBytes-MaxResultBytes {
		return zero, &Error{code: Capacity}
	}
	value, err := fn()
	if err != nil {
		return zero, err
	}
	result, err = bound.encode(value)
	if err != nil {
		return zero, err
	}
	if len(result) > MaxResultBytes {
		return zero, &Error{code: Capacity}
	}
	resultDigest := sha256.Sum256(result)
	if _, err := tx.Exec(ctx, `INSERT INTO public.api_mutation_results(actor_id,method,request_id,fingerprint,result,result_sha256,create_time,expire_time) VALUES($1,$2,$3::uuid,$4,$5,$6,statement_timestamp(),statement_timestamp()+interval '24 hours')`, request.actor, request.method, request.id, request.fingerprint[:], result, resultDigest[:]); err != nil {
		return zero, err
	}
	return value, nil
}

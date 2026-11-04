package apimutations

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Probe reads a previously committed result after the owner's current
// account/policy fence and before any diagnostic or owner effect. It uses the
// same ledger gate, identity, fingerprint and digest checks as Execute, but
// never cleans up rows, allocates a resource, calls an encoder or invokes fn.
// A miss is not an execution capability: Execute must atomically recheck after
// any read-only preparation performed outside this transaction.
func Probe[T any](ctx context.Context, tx pgx.Tx) ([]byte, bool, error) {
	if ctx == nil || tx == nil {
		return nil, false, ErrInvalidRequest
	}
	bound, present := ctx.Value(requestKey[T]{}).(binding[T])
	if !present {
		return nil, false, nil
	}
	request := bound.request
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(1735553389,202)`); err != nil {
		return nil, false, err
	}
	var fingerprint, result, resultHash []byte
	err := tx.QueryRow(ctx, `SELECT fingerprint,result,result_sha256 FROM public.api_mutation_results
		WHERE actor_id=$1 AND method=$2 AND request_id=$3::uuid AND expire_time>clock_timestamp()`, request.actor, request.method, request.id).Scan(&fingerprint, &result, &resultHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !bytes.Equal(fingerprint, request.fingerprint[:]) {
		return nil, false, &Error{code: RequestIDReused}
	}
	digest := sha256.Sum256(result)
	if len(result) > MaxResultBytes || !bytes.Equal(resultHash, digest[:]) {
		return nil, false, &Error{code: StoredResultCorrupt}
	}
	return bytes.Clone(result), true, nil
}

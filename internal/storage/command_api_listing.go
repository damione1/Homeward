package storage

import (
	"context"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/domain"
	"time"
)

// ListAPICommandNames returns only this actor's public command candidates.
// Native target authorization remains mandatory before a caller can see a name.
// Next is the final scanned candidate, independent of the authorized results.
func (s *Store) ListAPICommandNames(ctx context.Context, actor domain.ActorID, home apitypes.HomeName, after *apitypes.OperationName, limit uint32, cutoff time.Time) ([]apitypes.OperationName, *apitypes.OperationName, error) {
	owner, err := actor.Value()
	if cutoff.IsZero() || err != nil || !home.Valid() || limit == 0 || limit > 500 || after != nil && (!after.Valid() || after.Home() != home) {
		return nil, nil, &Error{kind: InvalidSettings}
	}
	lower := ""
	if after != nil {
		lower = after.ID()
	}
	rows, err := s.pool.Query(ctx, `SELECT command_id FROM public.command_intents WHERE actor_id=$1 AND origin_kind='external_actor' AND api_capability_name LIKE $2 AND 'cmd-'||command_id>$3 AND accepted_at<=$5 ORDER BY command_id LIMIT $4`, owner, home.String()+"/%", lower, int(limit)+1, cutoff)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	values := make([]apitypes.OperationName, 0, limit+1)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, nil, err
		}
		name, err := apitypes.ParseOperationName(home.String() + "/operations/cmd-" + id)
		if err != nil {
			return nil, nil, &Error{kind: InconsistentSchema}
		}
		values = append(values, name)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(values) > int(limit) {
		next := values[limit-1]
		return values[:limit], &next, nil
	}
	return values, nil, nil
}

// A public UUID identifies at most one native command per actor, enforced by
// SQL. The caller must still authorize the returned historical native target.
func (s *Store) FindAPICommandsByRequest(ctx context.Context, actor domain.ActorID, home apitypes.HomeName, key apitypes.RequestID, cutoff time.Time) ([]apitypes.OperationName, error) {
	owner, err := actor.Value()
	if cutoff.IsZero() || err != nil || !home.Valid() || !key.Valid() {
		return nil, &Error{kind: InvalidSettings}
	}
	rows, err := s.pool.Query(ctx, `SELECT command_id FROM public.command_intents WHERE actor_id=$1 AND api_request_id=$2 AND api_capability_name LIKE $3 AND accepted_at<=$4 ORDER BY command_id LIMIT 100`, owner, key.String(), home.String()+"/%", cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []apitypes.OperationName
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		name, err := apitypes.ParseOperationName(home.String() + "/operations/cmd-" + id)
		if err != nil {
			return nil, &Error{kind: InconsistentSchema}
		}
		values = append(values, name)
	}
	return values, rows.Err()
}

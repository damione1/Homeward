package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"github.com/damione1/Homeward/internal/configcompilation"
	"github.com/jackc/pgx/v5"
)

// migrateCompilationContexts33 updates only compiler-input bytes and their
// digest. Generation/source/manifest identities and all receipts remain exact.
// ALTER TABLE holds its DDL lock against concurrent writers until commit.
// The immutable generation trigger is restored before publishing SQL33.
func migrateCompilationContexts33(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `ALTER TABLE public.config_generations DISABLE TRIGGER config_generation_immutable`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DECLARE homeward_context33 NO SCROLL CURSOR FOR SELECT revision,CASE WHEN octet_length(compilation_context) BETWEEN 1 AND 8388608 THEN compilation_context END,compilation_context_hash FROM public.config_generations WHERE compilation_context IS NOT NULL ORDER BY revision`); err != nil {
		return err
	}
	for {
		rows, err := tx.Query(ctx, `FETCH FORWARD 8 FROM homeward_context33`)
		if err != nil {
			return err
		}
		type row struct {
			revision  int64
			raw, hash []byte
		}
		batch := []row{}
		for rows.Next() {
			var r row
			if err = rows.Scan(&r.revision, &r.raw, &r.hash); err != nil {
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
			oldHash := sha256.Sum256(r.raw)
			if len(r.raw) == 0 || !bytes.Equal(r.hash, oldHash[:]) {
				return &Error{kind: ConfigIntegrity}
			}
			next, changed, err := configcompilation.MigrateFanContextSQL33(r.raw)
			if err != nil {
				return &Error{kind: ConfigIntegrity, cause: err}
			}
			if !changed {
				continue
			}
			digest := next.SHA256()
			result, err := tx.Exec(ctx, `UPDATE public.config_generations SET compilation_context=$1,compilation_context_hash=$2 WHERE revision=$3 AND compilation_context_hash=$4`, next.Bytes(), digest[:], r.revision, r.hash)
			if err != nil {
				return err
			}
			if result.RowsAffected() != 1 {
				return &Error{kind: ConfigIntegrity}
			}
		}
	}
	if _, err := tx.Exec(ctx, `CLOSE homeward_context33`); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `ALTER TABLE public.config_generations ENABLE TRIGGER config_generation_immutable`)
	return err
}

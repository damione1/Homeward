package lists

import (
	"context"
	"errors"

	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/jackc/pgx/v5"
)

// VerifyLegacyImportAlias checks only the immutable list/alias association.
// The normal WRITE Permit and repository SQL fence apply; callers receive no
// list contents and need no implicit READ permission. This check precedes the
// mutable registry guard, which is necessarily outside this SQL transaction.
func (s *Service) VerifyLegacyImportAlias(ctx context.Context, id domain.ListID, alias domain.HAAlias) error {
	listText, err := id.Value()
	if err != nil {
		return failure(Invalid)
	}
	aliasText, err := alias.Value()
	if err != nil {
		return failure(Invalid)
	}
	_, err = execute(s, ctx, authv1.ScopeOperation_SCOPE_OPERATION_WRITE, collection(), false,
		func(ctx context.Context, tx pgx.Tx, _ authorization.RightsEvidence, _ authorization.ListSelection) (struct{}, error) {
			var bound *string
			err := tx.QueryRow(ctx, `SELECT legacy_alias FROM public.homeward_lists WHERE list_id=$1 FOR SHARE`, listText).Scan(&bound)
			if errors.Is(err, pgx.ErrNoRows) {
				return struct{}{}, failure(NotFound)
			}
			if err != nil {
				return struct{}{}, err
			}
			if bound == nil || *bound != aliasText {
				return struct{}{}, failure(ImportConflict)
			}
			return struct{}{}, nil
		}, nil)
	return err
}

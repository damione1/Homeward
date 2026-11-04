package storage

import "context"

// AdmitInvitationBearer authenticates the restricted bearer before public field
// diagnostics or password derivation. It neither consumes the invitation nor
// grants a session; AcceptInvitation rechecks it atomically at the native commit.
func (s *Store) AdmitInvitationBearer(ctx context.Context, digest [32]byte) error {
	if digest == [32]byte{} || accountContextError(ctx) != nil || s == nil || s.pool == nil {
		return ErrInvitationUnavailable
	}
	var available bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.auth_invitations WHERE token_digest=$1 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at>now())`, digest[:]).Scan(&available); err != nil {
		return ErrAccountUnavailable
	}
	if !available {
		return ErrInvitationUnavailable
	}
	return nil
}

package server

import (
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
)

func TestNHA142RecoveryFinalizationRollsBackWithAuditAndUnknownCommit(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		deferred bool
	}{{"audit_insert_rollback", false}, {"deferred_commit_unknown", true}} {
		t.Run(scenario.name, func(t *testing.T) {
			rig := nha142NewServerRig(t)
			plain := rig.start(t).GetAuthenticated()
			if plain == nil {
				t.Fatal("initial password session missing")
			}
			activated := rig.enroll(t, nha142NewAuthenticator(t), plain.AccessToken, "")
			challenge := rig.start(t).GetChallenge()
			if challenge == nil {
				t.Fatal("factor challenge missing")
			}
			var beforeAudit int
			if err := rig.store.InTransaction(rig.ctx, func(tx pgx.Tx) error {
				if err := tx.QueryRow(rig.ctx, `SELECT count(*) FROM public.auth_audit WHERE action=17`).Scan(&beforeAudit); err != nil {
					return err
				}
				// The real PostgreSQL failure occurs after code consumption inside the
				// transaction, either at the mandatory audit or at deferred COMMIT.
				if !scenario.deferred {
					_, err := tx.Exec(rig.ctx, `ALTER TABLE public.auth_audit ADD CONSTRAINT nha142_deny_session_audit CHECK(action<>17) NOT VALID`)
					return err
				}
				_, err := tx.Exec(rig.ctx, `CREATE FUNCTION public.nha142_fail_session_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic MFA commit rejection' USING ERRCODE='23514'; END $$; CREATE CONSTRAINT TRIGGER nha142_reject_session_commit AFTER INSERT ON public.auth_sessions DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha142_fail_session_commit()`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			response, err := rig.handler.CompleteLogin(rig.ctx, nha142RecoveryRequest(challenge, activated.RecoveryCodes[0]))
			if response != nil || connect.CodeOf(err) != connect.CodeUnavailable {
				t.Fatal("failed finalization returned credentials or wrong error", connect.CodeOf(err))
			}
			sessions, err := rig.store.ListSessions(rig.ctx, rig.account, rig.account, rig.clock.Now())
			if err != nil || len(sessions) != 0 {
				t.Fatal("failed finalization persisted a session", err)
			}
			var consumed, audit int
			if err := rig.store.InTransaction(rig.ctx, func(tx pgx.Tx) error {
				if err := tx.QueryRow(rig.ctx, `SELECT count(*) FROM public.auth_mfa_recovery_codes WHERE consumed_at IS NOT NULL`).Scan(&consumed); err != nil {
					return err
				}
				return tx.QueryRow(rig.ctx, `SELECT count(*) FROM public.auth_audit WHERE action=17`).Scan(&audit)
			}); err != nil {
				t.Fatal(err)
			}
			if consumed != 0 || audit != beforeAudit {
				t.Fatal("rollback persisted a code consumption or session audit", consumed, audit-beforeAudit)
			}
			key, _ := rig.account.Value()
			rig.coordinator.mu.Lock()
			gate := rig.coordinator.accounts[key]
			quarantined := gate != nil && gate.quarantined
			rig.coordinator.mu.Unlock()
			if quarantined != scenario.deferred {
				t.Fatal("commit outcome did not select the expected quarantine state", quarantined)
			}
			if err := rig.store.InTransaction(rig.ctx, func(tx pgx.Tx) error {
				statement := `ALTER TABLE public.auth_audit DROP CONSTRAINT nha142_deny_session_audit`
				if scenario.deferred {
					statement = `DROP TRIGGER nha142_reject_session_commit ON public.auth_sessions; DROP FUNCTION public.nha142_fail_session_commit()`
				}
				_, err := tx.Exec(rig.ctx, statement)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			// This explicit new request is made only after inspecting primary state.
			// The handler itself never retries an indeterminate write.
			recovered, err := rig.handler.CompleteLogin(rig.ctx, nha142RecoveryRequest(challenge, activated.RecoveryCodes[0]))
			if err != nil || recovered == nil || recovered.Msg.Decision.GetAuthenticated() == nil {
				t.Fatal("rolled-back code/challenge did not survive primary reconciliation", err)
			}
			if _, err := rig.handler.CompleteLogin(rig.ctx, nha142RecoveryRequest(challenge, activated.RecoveryCodes[0])); connect.CodeOf(err) != connect.CodeUnauthenticated {
				t.Fatal("confirmed consumption replayed", connect.CodeOf(err))
			}
		})
	}
}

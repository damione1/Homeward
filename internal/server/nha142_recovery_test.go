package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/damione1/Homeward/internal/storage"
)

// This barrier delegates to the real verifier. It only pauses after a valid
// code has produced typed proof evidence, before either caller enters SQL.
type nha142RecoveryBarrierProvider struct {
	inner    factorProvider
	verified chan struct{}
	release  <-chan struct{}
}

func (p *nha142RecoveryBarrierProvider) Prepare(ctx context.Context, account contracts.AccountID, id contracts.ChallengeID) (factorPreparation, error) {
	return p.inner.Prepare(ctx, account, id)
}

func (p *nha142RecoveryBarrierProvider) Verify(ctx context.Context, snapshot storage.MFALoginSnapshot, proof *authv1.FactorProof) (authmfa.VerifiedLogin, error) {
	verified, err := p.inner.Verify(ctx, snapshot, proof)
	if err != nil {
		return authmfa.VerifiedLogin{}, err
	}
	select {
	case p.verified <- struct{}{}:
	case <-ctx.Done():
		return authmfa.VerifiedLogin{}, ctx.Err()
	}
	select {
	case <-p.release:
		return verified, nil
	case <-ctx.Done():
		return authmfa.VerifiedLogin{}, ctx.Err()
	}
}

func nha142RecoveryRequest(challenge *authv1.FactorChallenge, code string) *connect.Request[authv1.CompleteLoginRequest] {
	return connect.NewRequest(&authv1.CompleteLoginRequest{
		ChallengeId: &authv1.ChallengeId{Value: challenge.Id.Value},
		Proof:       &authv1.FactorProof{Proof: &authv1.FactorProof_RecoveryCode{RecoveryCode: code}},
	})
}

func TestNHA142RecoveryCodeConcurrentSingleUse(t *testing.T) {
	rig := nha142NewServerRig(t)
	plain := rig.start(t).GetAuthenticated()
	if plain == nil {
		t.Fatal("default false policy did not issue the initial password session")
	}
	activated := rig.enroll(t, nha142NewAuthenticator(t), plain.AccessToken, "")
	before, err := rig.store.ListSessions(rig.ctx, rig.account, rig.account, rig.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 0 {
		t.Fatal("factor activation retained an old password session")
	}
	challenge := rig.start(t).GetChallenge()
	if challenge == nil {
		t.Fatal("enrolled account did not receive a factor challenge")
	}
	offeredRecovery := false
	for _, option := range challenge.Options {
		if option.GetRecoveryCode() != nil {
			offeredRecovery = true
		}
	}
	if !offeredRecovery {
		t.Fatal("active recovery batch was not offered")
	}

	release := make(chan struct{})
	var releaseOnce sync.Once
	barrier := &nha142RecoveryBarrierProvider{
		inner:    rig.coordinator.mfa,
		verified: make(chan struct{}, 2),
		release:  release,
	}
	rig.handler.provider = barrier
	callCtx, cancel := context.WithCancel(rig.ctx)
	type outcome struct {
		response *connect.Response[authv1.CompleteLoginResponse]
		err      error
	}
	results := make(chan outcome, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	t.Cleanup(func() {
		cancel()
		releaseOnce.Do(func() { close(release) })
		workers.Wait()
	})
	for i := 0; i < 2; i++ {
		go func() {
			defer workers.Done()
			response, err := rig.handler.CompleteLogin(callCtx, nha142RecoveryRequest(challenge, activated.RecoveryCodes[0]))
			results <- outcome{response: response, err: err}
		}()
	}

	verifiedDeadline := time.NewTimer(20 * time.Second)
	defer verifiedDeadline.Stop()
	for i := 0; i < 2; i++ {
		select {
		case <-barrier.verified:
		case premature := <-results:
			t.Fatalf("login returned before both real code verifications: %v", connect.CodeOf(premature.err))
		case <-verifiedDeadline.C:
			t.Fatal("both real recovery verifications did not reach the barrier")
		}
	}
	releaseOnce.Do(func() { close(release) })

	successes, refused := 0, 0
	completedDeadline := time.NewTimer(20 * time.Second)
	defer completedDeadline.Stop()
	for i := 0; i < 2; i++ {
		var result outcome
		select {
		case result = <-results:
		case <-completedDeadline.C:
			t.Fatal("concurrent finalizations did not complete")
		}
		if result.err != nil {
			if connect.CodeOf(result.err) != connect.CodeUnauthenticated {
				t.Fatalf("losing finalization returned %v, want unauthenticated", connect.CodeOf(result.err))
			}
			refused++
			continue
		}
		if result.response == nil || result.response.Msg == nil || result.response.Msg.Decision.GetAuthenticated() == nil {
			t.Fatal("successful finalization did not return a session")
		}
		session := result.response.Msg.Decision.GetAuthenticated().Session
		if session == nil || session.AuthenticationSatisfaction != authv1.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_RECOVERY_CODE || session.CreatedAt == nil || session.AuthenticatedAt == nil || !session.CreatedAt.AsTime().Equal(session.AuthenticatedAt.AsTime()) {
			t.Fatal("recovery login lost original typed authentication evidence")
		}
		successes++
	}
	if successes != 1 || refused != 1 {
		t.Fatalf("one code yielded %d sessions and %d refusals, want 1/1", successes, refused)
	}
	after, err := rig.store.ListSessions(rig.ctx, rig.account, rig.account, rig.clock.Now())
	if err != nil || len(after) != len(before)+1 {
		t.Fatal("concurrent use did not create exactly one active session", err)
	}

	fresh := rig.start(t).GetChallenge()
	if fresh == nil {
		t.Fatal("new challenge missing after recovery login")
	}
	if _, err := rig.handler.CompleteLogin(rig.ctx, nha142RecoveryRequest(fresh, activated.RecoveryCodes[0])); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("consumed code was accepted on a new challenge", connect.CodeOf(err))
	}
	unused, err := rig.handler.CompleteLogin(rig.ctx, nha142RecoveryRequest(fresh, activated.RecoveryCodes[1]))
	if err != nil || unused == nil || unused.Msg.Decision.GetAuthenticated() == nil {
		t.Fatal("a different unused code failed on the same live challenge", err)
	}
	if unused.Msg.Decision.GetAuthenticated().Session.AuthenticationSatisfaction != authv1.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_RECOVERY_CODE {
		t.Fatal("unused recovery code issued a session without recovery provenance")
	}
	final, err := rig.store.ListSessions(rig.ctx, rig.account, rig.account, rig.clock.Now())
	if err != nil || len(final) != len(before)+2 {
		t.Fatal("unused code did not create exactly one further session", err)
	}
}

package server

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/authmfa"
)

func TestNHA142ProtectedEnrollmentRequiresOriginalRecentAuthentication(t *testing.T) {
	rig := nha142NewServerRig(t)
	plain := rig.start(t).GetAuthenticated()
	if plain == nil {
		t.Fatal("default false policy did not issue the initial password session")
	}
	existingKey := nha142NewAuthenticator(t)
	activated := rig.enroll(t, existingKey, plain.AccessToken, "")
	if activated.FactorCredential == nil || activated.FactorCredential.Name == "" ||
		len(activated.RecoveryCodes) != authmfa.RecoveryCodeCount {
		t.Fatal("initial real enrollment returned incomplete factor or recovery metadata")
	}

	fidoLogin := func() *api.SessionTokens {
		t.Helper()
		challenge := rig.start(t).GetChallenge()
		if challenge == nil || challenge.ChallengeId == "" {
			t.Fatal("existing FIDO2 factor did not yield a challenge")
		}
		completed, err := rig.handler.CompleteLogin(rig.ctx, connect.NewRequest(&api.CompleteLoginRequest{
			ChallengeId: challenge.ChallengeId,
			Proof:       existingKey.assertion(t, challenge, 0x05),
		}))
		if err != nil || completed == nil || completed.Msg == nil || completed.Msg.Decision == nil {
			t.Fatalf("existing FIDO2 assertion failed: %v", err)
		}
		tokens := completed.Msg.Decision.GetAuthenticated()
		if tokens == nil || tokens.Session == nil || tokens.Session.AuthenticationTime == nil ||
			tokens.Session.AuthenticationSatisfaction != api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_FIDO2 ||
			tokens.AccessToken == "" {
			t.Fatal("existing FIDO2 factor did not establish typed original authentication")
		}
		return tokens
	}
	fido := fidoLogin()
	original := fido.Session.AuthenticationTime.AsTime()

	// Begin while original authentication is recent. Complete while the
	// ceremony is still live but the original session evidence is stale.
	rig.clock.Advance(original.Add(4*time.Minute + 30*time.Second))
	newKey := nha142NewAuthenticator(t)
	beginRequest := connect.NewRequest(&api.BeginFactorEnrollmentRequest{Parent: rig.accountName.String(),
		Kind: api.FactorKind_FACTOR_KIND_FIDO2,
	})
	beginRequest.Header().Set("Authorization", "Bearer "+fido.AccessToken)
	begun, err := rig.handler.BeginFactorEnrollment(rig.tlsContext(), beginRequest)
	if err != nil || begun == nil || begun.Msg == nil || begun.Msg.Challenge == nil ||
		begun.Msg.Challenge.ChallengeId == "" || begun.Msg.Challenge.ExpireTime == nil ||
		begun.Msg.Challenge.Fido2 == nil {
		t.Fatalf("recent session could not begin a second real FIDO2 enrollment: %v", err)
	}
	staleAt := original.Add(authmfa.RecentAuthenticationLifetime + time.Second)
	if !staleAt.Before(begun.Msg.Challenge.ExpireTime.AsTime()) {
		t.Fatal("test setup let the enrollment ceremony expire before original authentication")
	}
	rig.clock.Advance(staleAt)
	completeRequest := connect.NewRequest(&api.CompleteFactorEnrollmentRequest{Parent: rig.accountName.String(),
		ChallengeId: begun.Msg.Challenge.ChallengeId,
		Attestation: newKey.attestation(t, begun.Msg.Challenge),
	})
	completeRequest.Header().Set("Authorization", "Bearer "+fido.AccessToken)
	if _, err := rig.handler.CompleteFactorEnrollment(rig.tlsContext(), completeRequest); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("stale original authentication completed second-factor enrollment: %v", connect.CodeOf(err))
	}

	// Begin itself is an authoritative admission, including when a caller
	// still holds a valid access token after the five-minute recency window.
	staleBegin := connect.NewRequest(&api.BeginFactorEnrollmentRequest{Parent: rig.accountName.String(),
		Kind: api.FactorKind_FACTOR_KIND_FIDO2,
	})
	staleBegin.Header().Set("Authorization", "Bearer "+fido.AccessToken)
	if _, err := rig.handler.BeginFactorEnrollment(rig.tlsContext(), staleBegin); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("stale original authentication began a new enrollment: %v", connect.CodeOf(err))
	}

	ctx, release := rig.session(t, fido)
	listed, err := rig.handler.ListFactorCredentials(ctx, connect.NewRequest(&api.ListFactorCredentialsRequest{
		Parent: rig.accountName.String(),
	}))
	release()
	if err != nil || listed == nil || listed.Msg == nil || len(listed.Msg.FactorCredentials) != 1 ||
		listed.Msg.FactorCredentials[0].Name == "" || listed.Msg.FactorCredentials[0].Name != activated.FactorCredential.Name {
		t.Fatalf("rejected enrollment changed the active factor set: %v", err)
	}

	// A failed second enrollment must not retire the original recovery batch.
	recoveryChallenge := rig.start(t).GetChallenge()
	if recoveryChallenge == nil {
		t.Fatal("protected account lost its recovery challenge")
	}
	recovered, err := rig.handler.CompleteLogin(rig.ctx, nha142RecoveryRequest(recoveryChallenge, activated.RecoveryCodes[0]))
	if err != nil || recovered == nil || recovered.Msg == nil || recovered.Msg.Decision == nil {
		t.Fatalf("old recovery batch stopped working after refused enrollment: %v", err)
	}
	recoveryTokens := recovered.Msg.Decision.GetAuthenticated()
	if recoveryTokens == nil || recoveryTokens.Session == nil ||
		recoveryTokens.Session.AuthenticationSatisfaction != api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_PASSWORD_RECOVERY_CODE {
		t.Fatal("old recovery batch did not produce a typed recovery session")
	}
	if fresh := fidoLogin(); !fresh.Session.AuthenticationTime.AsTime().After(original) {
		t.Fatal("existing FIDO2 key stopped authenticating after refused second enrollment")
	}
}

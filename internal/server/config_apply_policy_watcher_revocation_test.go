package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
)

// SQL24, the real default Run/TLS handlers, joined150 watcher and149/029 writer
// are exercised. The inherited ACK proxy stays passive and no pilot is loaded.
func TestNHA055ActualRunTLSPolicyWatcherCannotRestoreRevokedRightsWithoutANewPhysicalEdit(t *testing.T) {
	for _, observedBeforeRevocation := range []bool{false, true} {
		name := "before-first-poll"
		if observedBeforeRevocation {
			name = "already-observed-P1"
		}
		t.Run(name, func(t *testing.T) {
			f, _, _, _, clock := nha055UncertainSelfPolicyFixture(t, false)
			created, err := f.accounts.CreateAccount(f.ctx, nha056SessionRequest(&wire.CreateAccountRequest{Parent: nha203SessionAccount(t, f.tokens.Session).Home().String(), Account: &wire.Account{LoginName: "NHA055 watcher policy user", DisplayName: "NHA055 watcher policy user", Role: wire.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: "NHA055 watcher policy password"}}, f.token))
			if err != nil {
				t.Fatal(err)
			}
			accountName := created.Msg.Name
			account := nha203NativeAccount(t, f.ctx, f.run.observed.sessions.store, accountName)
			seed, err := f.policies.GetAuthorizationPolicy(f.ctx, nha056SessionRequest(&wire.GetAuthorizationPolicyRequest{Name: accountName + "/authorizationPolicy"}, f.token))
			if err != nil || seed.Msg == nil {
				t.Fatal("seed policy", err)
			}
			before := f.read(t)
			p1Raw := []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n - scope: {family: state_registry, operation: read}\n   objects: {entities: {all: {capabilities: {all: {}}}}}\n# explicitly materialized055 P1\n", nha203AccountText(t, account)))
			request := nha055Request(before, nha055PolicyCandidate(t, account, p1Raw), f.build(t, nha055PolicyCandidate(t, account, p1Raw)))
			request.Sources = []*wire.SourcePrecondition{nha055PolicyExpected(t, account, nha055PolicyVersion(t, seed.Msg), nil)}
			p1, err := f.apply.ApplyConfigurationPlan(f.ctx, nha056SessionRequest(request, f.token))
			if err != nil || !p1.Msg.Done {
				t.Fatal("real055 materialization", err)
			}
			p1Current := f.read(t)
			activeP1, err := f.policies.GetAuthorizationPolicy(f.ctx, nha056SessionRequest(&wire.GetAuthorizationPolicyRequest{Name: accountName + "/authorizationPolicy"}, f.token))
			if err != nil || activeP1.Msg == nil || !bytes.Equal(activeP1.Msg.YamlSource, p1Raw) {
				t.Fatal("P1 not active", err)
			}
			accountID, err := contracts.ParseAccountID(nha203AccountText(t, account))
			if err != nil {
				t.Fatal(err)
			}
			binding, err := storage.AuthorizationPolicyBinding(accountID)
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256([]byte(binding.ID().String()))
			physical := filepath.Join(filepath.Dir(f.path), "configuration.d", hex.EncodeToString(hash[:]), filepath.FromSlash(binding.Path()))
			if raw, err := os.ReadFile(physical); err != nil || !bytes.Equal(raw, p1Raw) {
				t.Fatal("055 did not materialize exact P1", err)
			}
			if observedBeforeRevocation {
				for i := 0; i < 4; i++ {
					clock.tick(t)
				}
			}
			p2Raw := []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants: []\n# independently revoked050 P2\n", nha203AccountText(t, account)))
			p2, err := f.policies.ReplaceAuthorizationPolicy(f.ctx, nha056SessionRequest(&wire.ReplaceAuthorizationPolicyRequest{Name: accountName + "/authorizationPolicy", YamlSource: p2Raw, Precondition: &wire.ReplaceAuthorizationPolicyRequest_ExpectedEtag{ExpectedEtag: activeP1.Msg.Etag}}, f.token))
			if err != nil {
				t.Fatal("050 revocation", err)
			}
			afterRevocation := f.read(t)
			if !proto.Equal(afterRevocation.ActiveGeneration, p1Current.ActiveGeneration) {
				t.Fatal("050 changed configuration generation")
			}
			// Four seconds cross both the500ms stability and2s retry windows. No
			// filesystem edit occurs: recapture must not rebase P1's old intention.
			for i := 0; i < 16; i++ {
				clock.tick(t)
			}
			still, err := f.policies.GetAuthorizationPolicy(f.ctx, nha056SessionRequest(&wire.GetAuthorizationPolicyRequest{Name: accountName + "/authorizationPolicy"}, f.token))
			if err != nil || still.Msg == nil || !proto.Equal(nha055PolicyVersion(t, still.Msg), nha055PolicyVersion(t, p2.Msg.Policy)) || !bytes.Equal(still.Msg.YamlSource, p2Raw) {
				t.Fatal("watcher restored an independently revoked policy", err)
			}
			if !proto.Equal(f.read(t).ActiveGeneration, afterRevocation.ActiveGeneration) {
				t.Fatal("unchanged old P1 created a generation")
			}
			if raw, err := os.ReadFile(physical); err != nil || !bytes.Equal(raw, p1Raw) {
				t.Fatal("conflicting old candidate was overwritten", err)
			}
			nha055PolicyRollbackNoCommands(t, f)
			// An explicit later bytes edit, observed while P2 remains the SQL base,
			// may use the same writer. It preserves revoked rights and exact comments.
			p3Raw := append(bytes.Clone(p2Raw), []byte("# later deliberate physical edit under captured P2\n")...)
			if err := os.WriteFile(physical, p3Raw, 0600); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 8; i++ {
				clock.tick(t)
			}
			p3, err := f.policies.GetAuthorizationPolicy(f.ctx, nha056SessionRequest(&wire.GetAuthorizationPolicyRequest{Name: accountName + "/authorizationPolicy"}, f.token))
			if err != nil || p3.Msg == nil || nha055PolicyVersion(t, p3.Msg).Revision != nha055PolicyVersion(t, p2.Msg.Policy).Revision+1 || !bytes.Equal(p3.Msg.YamlSource, p3Raw) {
				t.Fatal("new physical edit did not publish exactly once from current P2", err)
			}
			if f.read(t).ActiveGeneration.Revision != afterRevocation.ActiveGeneration.Revision+1 {
				t.Fatal("physical policy edit did not create exactly one generation")
			}
			if raw, err := os.ReadFile(physical); err != nil || !bytes.Equal(raw, p3Raw) {
				t.Fatal("published candidate bytes changed", err)
			}
			if raw, err := os.ReadFile(f.path); err != nil || !bytes.Equal(raw, f.raw) {
				t.Fatal("policy watcher changed operational bytes", err)
			}
			nha055PolicyRollbackNoCommands(t, f)
		})
	}
}

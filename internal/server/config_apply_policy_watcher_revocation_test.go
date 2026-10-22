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
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	configuration "github.com/damione1/Homeward/gen/home/configuration/v1"
	wire "github.com/damione1/Homeward/gen/home/configurationapply/v1"
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
			created, err := f.accounts.CreateAccount(f.ctx, nha056SessionRequest(&auth.CreateAccountRequest{LoginName: "NHA055 watcher policy user", DisplayName: "NHA055 watcher policy user", Role: auth.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: "NHA055 watcher policy password"}, f.token))
			if err != nil {
				t.Fatal(err)
			}
			account := created.Msg.Account.Id
			seed, err := f.policies.GetPolicy(f.ctx, nha056SessionRequest(&auth.GetPolicyRequest{AccountId: account}, f.token))
			if err != nil || seed.Msg.GetPresent() == nil {
				t.Fatal("seed policy", err)
			}
			before := f.read(t)
			p1Raw := []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n - scope: {family: state_registry, operation: read}\n   objects: {entities: {all: {capabilities: {all: {}}}}}\n# explicitly materialized055 P1\n", account.Value))
			request := nha055Request(before, nha055PolicyCandidate(t, account, p1Raw), nil)
			request.Sources = []*wire.SourcePrecondition{nha055PolicyExpected(t, account, seed.Msg.GetPresent().Version, nil)}
			p1, err := f.apply.Apply(f.ctx, nha056SessionRequest(request, f.token))
			if err != nil || p1.Msg.Activation.Outcome != configuration.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED {
				t.Fatal("real055 materialization", err)
			}
			activeP1, err := f.policies.GetPolicy(f.ctx, nha056SessionRequest(&auth.GetPolicyRequest{AccountId: account}, f.token))
			if err != nil || activeP1.Msg.GetPresent() == nil || !bytes.Equal(activeP1.Msg.GetPresent().YamlSource, p1Raw) {
				t.Fatal("P1 not active", err)
			}
			accountID, err := contracts.ParseAccountID(account.Value)
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
			p2Raw := []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants: []\n# independently revoked050 P2\n", account.Value))
			p2, err := f.policies.ReplacePolicy(f.ctx, nha056SessionRequest(&auth.ReplacePolicyRequest{AccountId: account, YamlSource: p2Raw, Precondition: &auth.ReplacePolicyRequest_Expected{Expected: activeP1.Msg.GetPresent().Version}}, f.token))
			if err != nil {
				t.Fatal("050 revocation", err)
			}
			afterRevocation := f.read(t)
			if !proto.Equal(afterRevocation.ActiveGeneration, p1.Msg.Current.ActiveGeneration) {
				t.Fatal("050 changed configuration generation")
			}
			// Four seconds cross both the500ms stability and2s retry windows. No
			// filesystem edit occurs: recapture must not rebase P1's old intention.
			for i := 0; i < 16; i++ {
				clock.tick(t)
			}
			still, err := f.policies.GetPolicy(f.ctx, nha056SessionRequest(&auth.GetPolicyRequest{AccountId: account}, f.token))
			if err != nil || still.Msg.GetPresent() == nil || !proto.Equal(still.Msg.GetPresent().Version, p2.Msg.Policy.Version) || !bytes.Equal(still.Msg.GetPresent().YamlSource, p2Raw) {
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
			p3, err := f.policies.GetPolicy(f.ctx, nha056SessionRequest(&auth.GetPolicyRequest{AccountId: account}, f.token))
			if err != nil || p3.Msg.GetPresent() == nil || p3.Msg.GetPresent().Version.Revision != p2.Msg.Policy.Version.Revision+1 || !bytes.Equal(p3.Msg.GetPresent().YamlSource, p3Raw) {
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

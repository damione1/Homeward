// Homeward NHA-136: deterministic synthetic SRP wire-width regression tests.
package pair

import (
	"bytes"
	"crypto/sha512"
	"math/big"
	"testing"

	"github.com/brutella/hc/util"
	"github.com/tadglines/go-pkgs/crypto/srp"
)

func TestNHA136SRPServerM2LeftPadsBigEndianPublicKey(t *testing.T) {
	for _, size := range []int{1, 382, 383, 384} {
		t.Run(string([]byte{'0' + byte(size/100), '0' + byte(size/10%10), '0' + byte(size%10)}), func(t *testing.T) {
			// Fixed positive B=2^(8*(size-1)), below the 3072-bit modulus.
			// In particular B=2^3056 has exactly383 minimal bytes, no key retries.
			raw := new(big.Int).Lsh(big.NewInt(1), uint(8*(size-1))).Bytes()
			if len(raw) != size {
				t.Fatal("deterministic minimal integer width")
			}
			original := append([]byte(nil), raw...)
			server := &SetupServerController{
				session: &SetupServerSession{PublicKey: raw, Salt: make([]byte, 16)},
				step:    PairStepWaiting,
			}
			request := util.NewTLV8Container()
			request.SetByte(TagSequence, PairStepStartRequest.Byte())
			response, err := server.Handle(request)
			if err != nil {
				t.Fatal(err)
			}
			// Exercise actual TLV8 fragmentation/reassembly as384 exceeds255.
			decoded, err := util.NewTLV8ContainerFromReader(response.BytesBuffer())
			if err != nil {
				t.Fatal(err)
			}
			wire := decoded.GetBytes(TagPublicKey)
			expected := make([]byte, 384)
			copy(expected[384-size:], original)
			if len(wire) != 384 || !bytes.Equal(wire, expected) {
				t.Fatal("M2 public key must be left-zero-padded to384 bytes")
			}
			if new(big.Int).SetBytes(wire).Cmp(new(big.Int).SetBytes(original)) != 0 || !bytes.Equal(raw, original) {
				t.Fatal("wire encoding changed the SRP integer or session input")
			}
			if decoded.GetByte(TagSequence) != PairStepStartResponse.Byte() || len(decoded.GetBytes(TagSalt)) != 16 {
				t.Fatal("M2 sequence or salt changed")
			}
			if size == 383 {
				// Same strict controller used by SetupClient.Pair. Only M2->M3
				// is asserted here; this is not a completed authentication proof.
				client := &SetupClientController{session: NewSetupClientSession("Pair-Setup", "001-02-003")}
				next, err := client.Handle(decoded)
				if err != nil {
					t.Fatal("strict client rejected canonical leading-zero M2:", err)
				}
				if next.GetByte(TagSequence) != PairStepVerifyRequest.Byte() || len(next.GetBytes(TagProof)) != 64 {
					t.Fatal("strict client did not produce SRP proof M3")
				}
			}
		})
	}
}

func TestNHA136SRPServerRejectsImpossiblePublicKeyLengths(t *testing.T) {
	for _, size := range []int{0, 385} {
		server := &SetupServerController{session: &SetupServerSession{PublicKey: make([]byte, size)}, step: PairStepWaiting}
		request := util.NewTLV8Container()
		request.SetByte(TagSequence, PairStepStartRequest.Byte())
		response, err := server.Handle(request)
		if err == nil || response != nil || server.step != PairStepWaiting {
			t.Fatal("invalid server public-key width must fail before M2")
		}
	}
}

func TestNHA136SRPClientStillRejectsInvalidPublicKeys(t *testing.T) {
	group, err := srp.NewSRP(SRPGroup, sha512.New, KeyDerivativeFuncRFC2945(sha512.New, []byte("Pair-Setup")))
	if err != nil {
		t.Fatal(err)
	}
	modulus := group.Group.Prime.FillBytes(make([]byte, 384))
	cases := []struct {
		name  string
		value []byte
	}{
		{"empty", nil},
		{"minimal383_without_padding", append([]byte{1}, make([]byte, 382)...)},
		{"oversize385", make([]byte, 385)},
		{"zero384", make([]byte, 384)},
		{"modulus384", modulus},
	}
	for _, entry := range cases {
		t.Run(entry.name, func(t *testing.T) {
			client := &SetupClientController{session: NewSetupClientSession("Pair-Setup", "001-02-003")}
			input := util.NewTLV8Container()
			input.SetByte(TagSequence, PairStepStartResponse.Byte())
			input.SetBytes(TagSalt, make([]byte, 16))
			input.SetBytes(TagPublicKey, entry.value)
			next, err := client.Handle(input)
			if err == nil || next != nil {
				t.Fatal("strict SRP client accepted invalid B")
			}
		})
	}
}

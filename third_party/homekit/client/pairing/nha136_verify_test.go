package pairing

import (
	"testing"

	"github.com/brutella/hc/hap/pair"
	"github.com/brutella/hc/util"
)

func TestNHA136PairVerifyShortAuthenticationTagRejected(t *testing.T) {
	for size := 0; size < 16; size++ {
		t.Run(string(rune('a'+size)), func(t *testing.T) {
			controller := NewVerifyClientController(nil, nil)
			if controller.InitialKeyVerifyRequest() == nil {
				t.Fatal("M1 unavailable")
			}
			peer := pair.NewVerifySession()
			input := util.NewTLV8Container()
			input.SetByte(pair.TagSequence, pair.VerifyStepStartResponse.Byte())
			input.SetBytes(pair.TagPublicKey, peer.PublicKey[:])
			input.SetBytes(pair.TagEncryptedData, make([]byte, size))
			if _, err := controller.Handle(input); err == nil {
				t.Fatalf("%d byte field admitted", size)
			}
		})
	}
	controller := NewVerifyClientController(nil, nil)
	if controller.InitialKeyVerifyRequest() == nil {
		t.Fatal("M1 unavailable")
	}
	peer := pair.NewVerifySession()
	input := util.NewTLV8Container()
	input.SetByte(pair.TagSequence, pair.VerifyStepStartResponse.Byte())
	input.SetBytes(pair.TagPublicKey, peer.PublicKey[:])
	input.SetBytes(pair.TagEncryptedData, make([]byte, 16))
	if _, err := controller.Handle(input); err == nil {
		t.Fatal("unauthenticated tag admitted")
	}
}

func TestNHA136PairVerifyStateCannotSkipOrRepeat(t *testing.T) {
	for _, test := range []struct {
		name     string
		sequence []byte
	}{{"M4-first", []byte{4}}, {"sequence-absent", nil}, {"sequence-duplicate", []byte{2, 2}}} {
		t.Run(test.name, func(t *testing.T) {
			controller := NewVerifyClientController(nil, nil)
			if crypto, err := controller.SessionCryptographer(); err == nil || crypto != nil {
				t.Fatal("crypto before M1")
			}
			if controller.InitialKeyVerifyRequest() == nil {
				t.Fatal("M1 unavailable")
			}
			input := util.NewTLV8Container()
			input.SetBytes(pair.TagSequence, test.sequence)
			if output, err := controller.Handle(input); err == nil || output != nil {
				t.Fatal("invalid sequence admitted")
			}
			if crypto, err := controller.SessionCryptographer(); err == nil || crypto != nil {
				t.Fatal("crypto after failed sequence")
			}
			if controller.InitialKeyVerifyRequest() != nil {
				t.Fatal("failed handshake reused ephemeral state")
			}
		})
	}
}

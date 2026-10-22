package ecobee

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"io"

	"github.com/damione1/Homeward/internal/storage"
	"github.com/mctofu/homekit/client"
)

// PairingInput is a trusted provisioning input. No runtime path performs setup.
// JSON bytes are accepted only inside the encrypted, session-scoped secret store.
type PairingInput struct {
	Version              int    `json:"version"`
	ControllerID         string `json:"controller_id"`
	ControllerPublicKey  []byte `json:"controller_public_key"`
	ControllerPrivateKey []byte `json:"controller_private_key"`
	AccessoryID          string `json:"accessory_id"`
	AccessoryPublicKey   []byte `json:"accessory_public_key"`
}

func (PairingInput) String() string     { return "ecobee pairing [REDACTED]" }
func (p PairingInput) GoString() string { return p.String() }

type pairingCredential struct {
	controller client.ControllerIdentity
	accessory  client.AccessoryConnectionConfig
}

func (pairingCredential) String() string     { return "ecobee credential [REDACTED]" }
func (p pairingCredential) GoString() string { return p.String() }

func decodeCredential(value storage.SecretValue, settings Settings) (pairingCredential, error) {
	raw, err := value.Bytes()
	if err != nil || len(raw) == 0 || len(raw) > 4096 || !settings.Valid() {
		return pairingCredential{}, ErrCredential
	}
	if !uniqueCredentialObject(raw) {
		return pairingCredential{}, ErrCredential
	}
	var in PairingInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&in) != nil {
		return pairingCredential{}, ErrCredential
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return pairingCredential{}, ErrCredential
	}
	if in.Version != 1 || in.ControllerID == "" || len(in.ControllerID) > 128 || in.ControllerID == in.AccessoryID || in.AccessoryID != settings.accessory || len(in.ControllerPublicKey) != ed25519.PublicKeySize || len(in.ControllerPrivateKey) != ed25519.PrivateKeySize || len(in.AccessoryPublicKey) != ed25519.PublicKeySize {
		return pairingCredential{}, ErrCredential
	}
	derived := ed25519.NewKeyFromSeed(in.ControllerPrivateKey[:ed25519.SeedSize])
	if !bytes.Equal(derived, in.ControllerPrivateKey) || bytes.Equal(in.AccessoryPublicKey, make([]byte, ed25519.PublicKeySize)) {
		return pairingCredential{}, ErrCredential
	}
	public := ed25519.PrivateKey(in.ControllerPrivateKey).Public().(ed25519.PublicKey)
	if !bytes.Equal(public, in.ControllerPublicKey) {
		return pairingCredential{}, ErrCredential
	}
	return pairingCredential{client.ControllerIdentity{DeviceID: in.ControllerID, PublicKey: append([]byte(nil), in.ControllerPublicKey...), PrivateKey: append([]byte(nil), in.ControllerPrivateKey...)}, client.AccessoryConnectionConfig{DeviceID: in.AccessoryID, PublicKey: append([]byte(nil), in.AccessoryPublicKey...), IPConnectionInfo: client.IPConnectionInfo{IPAddress: settings.address.Addr().String(), Port: int(settings.address.Port())}}}, nil
}

func uniqueCredentialObject(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	seen := make(map[string]bool, 6)
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return false
		}
		seen[name] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return false
		}
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') {
		return false
	}
	return decoder.Decode(new(any)) == io.EOF
}

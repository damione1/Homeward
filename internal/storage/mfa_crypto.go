package storage

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/authmfa"
)

// MFACipherBinding is admitted for exactly one purpose and version. Its private
// pointer also prevents fmt from walking sensitive nested payloads.
type MFACipherBinding struct{ *mfaCipherBindingValue }
type mfaCipherBindingValue struct {
	purpose protectedPurpose
	party   authmfa.RelyingParty
	account contracts.AccountID
	record  string
	version authmfa.RecordVersion
}

func NewMFACredentialBinding(party authmfa.RelyingParty, account contracts.AccountID, id contracts.FactorCredentialID, version authmfa.RecordVersion) (MFACipherBinding, error) {
	text, err := id.Value()
	if err != nil {
		return MFACipherBinding{}, mfaError(authmfa.ErrorInvalidInput)
	}
	return newMFACipherBinding(protectedMFACredential, party, account, text, version)
}
func NewMFACeremonyBinding(party authmfa.RelyingParty, binding authmfa.Binding) (MFACipherBinding, error) {
	account, ae := binding.Account()
	challenge, ce := binding.Challenge()
	instance, ie := binding.Instance()
	pi, _, _, pe := party.Values()
	if ae != nil || ce != nil || ie != nil || pe != nil || instance != pi {
		return MFACipherBinding{}, mfaError(authmfa.ErrorInvalidInput)
	}
	text, _ := challenge.Value()
	v, _ := authmfa.NewRecordVersion(1)
	return newMFACipherBinding(protectedMFACeremony, party, account, text, v)
}
func newMFACipherBinding(purpose protectedPurpose, party authmfa.RelyingParty, account contracts.AccountID, record string, version authmfa.RecordVersion) (MFACipherBinding, error) {
	_, _, _, pe := party.Values()
	_, ae := sqlAccountID(account)
	_, ve := version.Number()
	if pe != nil || ae != nil || ve != nil || record == "" || len(record) > 128 || (purpose != protectedMFACredential && purpose != protectedMFACeremony) {
		return MFACipherBinding{}, mfaError(authmfa.ErrorInvalidInput)
	}
	return MFACipherBinding{&mfaCipherBindingValue{purpose: purpose, party: party, account: account, record: record, version: version}}, nil
}
func (b MFACipherBinding) valid() bool {
	if b.mfaCipherBindingValue == nil {
		return false
	}
	_, err := newMFACipherBinding(b.purpose, b.party, b.account, b.record, b.version)
	return err == nil
}
func (b MFACipherBinding) same(other MFACipherBinding) bool {
	return b.valid() && other.valid() && *b.mfaCipherBindingValue == *other.mfaCipherBindingValue
}
func (b MFACipherBinding) aad(keyID, salt []byte) []byte {
	instance, rp, origin, _ := b.party.Values()
	instanceText, _ := instance.Value()
	account, _ := b.account.Value()
	version, _ := b.version.Number()
	domain, _ := b.purpose.domain()
	var result []byte
	for _, part := range [][]byte{[]byte("homeward-mfa-aad-v1"), []byte(domain), []byte(secretSuiteSQL), keyID, []byte(instanceText), []byte(rp), []byte(origin), []byte(account), []byte(b.record), salt} {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(part)))
		result = append(result, size[:]...)
		result = append(result, part...)
	}
	var revision [8]byte
	binary.BigEndian.PutUint64(revision[:], version)
	return append(result, revision[:]...)
}

type MFAEnvelope struct{ *mfaEnvelopeValue }
type mfaEnvelopeValue struct {
	binding    MFACipherBinding
	keyID      [32]byte
	salt       [32]byte
	ciphertext []byte
}

func (MFAEnvelope) String() string                 { return "[REDACTED MFA ENVELOPE]" }
func (e MFAEnvelope) GoString() string             { return e.String() }
func (e MFAEnvelope) MarshalJSON() ([]byte, error) { return json.Marshal(e.String()) }
func (MFACipherBinding) String() string            { return "[REDACTED MFA BINDING]" }
func (b MFACipherBinding) GoString() string        { return b.String() }
func (e MFAEnvelope) valid() bool {
	if e.mfaEnvelopeValue == nil || !e.binding.valid() || e.keyID == [32]byte{} || e.salt == [32]byte{} {
		return false
	}
	limit := authmfa.MaxCredentialRecordBytes
	if e.binding.purpose == protectedMFACeremony {
		limit = authmfa.MaxCeremonyRecordBytes
	}
	return len(e.ciphertext) > 28 && len(e.ciphertext) <= limit+28
}
func (r *MFARepository) prepareEnvelope(binding MFACipherBinding, plain []byte) (MFAEnvelope, error) {
	if !r.valid() || !binding.valid() {
		return MFAEnvelope{}, mfaError(authmfa.ErrorInvalidInput)
	}
	limit := authmfa.MaxCredentialRecordBytes
	if binding.purpose == protectedMFACeremony {
		limit = authmfa.MaxCeremonyRecordBytes
	}
	if len(plain) == 0 || len(plain) > limit {
		return MFAEnvelope{}, mfaError(authmfa.ErrorInvalidInput)
	}
	value := &mfaEnvelopeValue{binding: binding, keyID: r.protector.key.id}
	if _, err := rand.Read(value.salt[:]); err != nil || value.salt == [32]byte{} {
		return MFAEnvelope{}, mfaError(authmfa.ErrorUnavailable)
	}
	ciphertext, err := sealProtectedRecord(&r.protector.key, binding.purpose, binding.aad(value.keyID[:], value.salt[:]), value.salt[:], plain)
	if err != nil {
		return MFAEnvelope{}, mfaError(authmfa.ErrorUnavailable)
	}
	value.ciphertext = ciphertext
	return MFAEnvelope{value}, nil
}
func (r *MFARepository) PrepareCredentialEnvelope(binding MFACipherBinding, next authmfa.RecordVersion, record authmfa.CredentialRecord) (MFAEnvelope, error) {
	account, ae := record.Account()
	party, pe := record.RelyingParty()
	id, ie := record.ID()
	version, ve := record.Version()
	text, _ := id.Value()
	if !binding.valid() || binding.purpose != protectedMFACredential || ae != nil || pe != nil || ie != nil || ve != nil || account != binding.account || party != binding.party || text != binding.record || version != next || next != binding.version {
		return MFAEnvelope{}, mfaError(authmfa.ErrorIntegrity)
	}
	plain, err := record.Bytes()
	if err != nil {
		return MFAEnvelope{}, err
	}
	return r.prepareEnvelope(binding, plain)
}
func (r *MFARepository) PrepareCeremonyEnvelope(binding MFACipherBinding, ceremony authmfa.PreparedCeremony) (MFAEnvelope, error) {
	original, err := ceremony.Binding()
	party, pe := ceremony.RelyingParty()
	expected, be := NewMFACeremonyBinding(party, original)
	if err != nil || pe != nil || be != nil || !binding.same(expected) {
		return MFAEnvelope{}, mfaError(authmfa.ErrorIntegrity)
	}
	plain, err := ceremony.Bytes()
	if err != nil {
		return MFAEnvelope{}, err
	}
	return r.prepareEnvelope(binding, plain)
}
func (r *MFARepository) openEnvelope(binding MFACipherBinding, keyID, salt, ciphertext []byte) ([]byte, error) {
	if !r.valid() || !binding.valid() || !equalKeyID(keyID, r.protector.key.id[:]) || len(salt) != 32 {
		return nil, mfaError(authmfa.ErrorIntegrity)
	}
	limit := authmfa.MaxCredentialRecordBytes
	if binding.purpose == protectedMFACeremony {
		limit = authmfa.MaxCeremonyRecordBytes
	}
	if len(ciphertext) <= 28 || len(ciphertext) > limit+28 {
		return nil, mfaError(authmfa.ErrorIntegrity)
	}
	plain, err := openProtectedRecord(&r.protector.key, binding.purpose, binding.aad(keyID, salt), salt, ciphertext)
	if err != nil {
		return nil, mfaError(authmfa.ErrorIntegrity)
	}
	return plain, nil
}

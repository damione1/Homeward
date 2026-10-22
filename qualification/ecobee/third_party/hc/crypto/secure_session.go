package crypto

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"github.com/brutella/hc/crypto/chacha20poly1305"
	"github.com/brutella/hc/crypto/hkdf"
	"io"
	"net"
)

// secureSession provide a secure session by encrypting and decrypting data
type secureSession struct {
	encryptKey [32]byte
	decryptKey [32]byte

	encryptCount uint64
	decryptCount uint64

	readEncrypted bool
}

// NewSecureSessionFromSharedKey returns a session from a shared private key.
func NewSecureSessionFromSharedKey(sharedKey [32]byte) (Cryptographer, error) {
	salt := []byte("Control-Salt")
	out := []byte("Control-Read-Encryption-Key")
	in := []byte("Control-Write-Encryption-Key")

	var s = new(secureSession)
	var err error
	s.encryptKey, err = hkdf.Sha512(sharedKey[:], salt, out)
	s.encryptCount = 0
	if err != nil {
		return nil, err
	}

	s.decryptKey, err = hkdf.Sha512(sharedKey[:], salt, in)
	s.decryptCount = 0

	return s, err
}

// NewSecureClientSessionFromSharedKey returns a session from a shared secret key to simulate a HomeKit client.
// This is currently only used for testing.
func NewSecureClientSessionFromSharedKey(sharedKey [32]byte) (Cryptographer, error) {
	salt := []byte("Control-Salt")
	out := []byte("Control-Write-Encryption-Key")
	in := []byte("Control-Read-Encryption-Key")

	var s = new(secureSession)
	var err error
	s.encryptKey, err = hkdf.Sha512(sharedKey[:], salt, out)
	s.encryptCount = 0
	if err != nil {
		return nil, err
	}

	s.decryptKey, err = hkdf.Sha512(sharedKey[:], salt, in)
	s.decryptCount = 0

	return s, err
}

// Encrypt return the encrypted data by splitting it into packets
// [ length (2 bytes)] [ data ] [ auth (16 bytes)]
func (s *secureSession) Encrypt(r io.Reader) (io.Reader, error) {
	packets := packetsFromBytes(r)
	var buf bytes.Buffer
	for _, p := range packets {
		var nonce [8]byte
		binary.LittleEndian.PutUint64(nonce[:], s.encryptCount)
		s.encryptCount++

		bLength := make([]byte, 2)
		binary.LittleEndian.PutUint16(bLength, uint16(p.length))

		encrypted, mac, err := chacha20poly1305.EncryptAndSeal(s.encryptKey[:], nonce[:], p.value, bLength[:])
		if err != nil {
			return nil, err
		}

		buf.Write(bLength[:])
		buf.Write(encrypted)
		buf.Write(mac[:])
	}

	return &buf, nil
}

// Decrypt authenticates one bounded record immediately, then streams subsequent
// records only when the caller asks for more plaintext. A full1024-byte record
// is not a message terminator and never requires a following short record.
func (s *secureSession) Decrypt(r io.Reader) (io.Reader, error) {
	reader := &secureRecordReader{session: s, source: r}
	err := reader.nextRecord()
	return reader, err
}

// Ciphertext/partial read state and at most one authenticated plaintext record
// remain owned by this reader. Timeouts preserve partial frame progress.
type secureRecordReader struct {
	session    *secureSession
	source     io.Reader
	header     [2]byte
	headerRead int
	sealed     [PacketLengthMax + 16]byte
	sealedRead int
	plaintext  []byte
	offset     int
	terminal   error
}

func (r *secureRecordReader) failure(err error) error {
	if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		r.terminal = err
	}
	return err
}

func (r *secureRecordReader) nextRecord() error {
	if r.terminal != nil {
		return r.terminal
	}
	if r.source == nil || r.session == nil {
		return r.failure(fmt.Errorf("invalid encrypted record reader"))
	}
	if r.headerRead < len(r.header) {
		n, err := io.ReadFull(r.source, r.header[r.headerRead:])
		r.headerRead += n
		if err != nil {
			return r.failure(err)
		}
	}
	length := int(binary.LittleEndian.Uint16(r.header[:]))
	if length > PacketLengthMax {
		return r.failure(fmt.Errorf("encrypted record length exceeds1024"))
	}
	sealedLength := length + 16
	n, err := io.ReadFull(r.source, r.sealed[r.sealedRead:sealedLength])
	r.sealedRead += n
	if err != nil {
		return r.failure(err)
	}
	var nonce [8]byte
	binary.LittleEndian.PutUint64(nonce[:], r.session.decryptCount)
	var mac [16]byte
	copy(mac[:], r.sealed[length:sealedLength])
	plaintext, err := chacha20poly1305.DecryptAndVerify(r.session.decryptKey[:], nonce[:], r.sealed[:length], mac, r.header[:])
	if err != nil {
		return r.failure(fmt.Errorf("Data encryption failed %w", err))
	}
	// Commit the nonce only after the complete record has authenticated.
	r.session.decryptCount++
	r.plaintext, r.offset = plaintext, 0
	r.headerRead, r.sealedRead = 0, 0
	return nil
}

func (r *secureRecordReader) Read(destination []byte) (int, error) {
	if len(destination) == 0 {
		return 0, nil
	}
	for r.offset == len(r.plaintext) {
		if err := r.nextRecord(); err != nil {
			return 0, err
		}
	}
	n := copy(destination, r.plaintext[r.offset:])
	r.offset += n
	return n, nil
}

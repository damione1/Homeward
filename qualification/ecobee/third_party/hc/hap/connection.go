package hap

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brutella/hc/crypto"
)

// Connection is a connection based on HAP protocol which encrypts and decrypts the data.
//
// For every connection, a new session is created in the context. The session uses the Cryptographer
// to encrypt and decrypt data. The Cryptographer is created by one of the endpoint handlers after
// pairing has been verified. After that the communication is encrypted.
//
// When the connection is closed, the related session is removed from the context.
type Connection struct {
	connection net.Conn
	context    Context

	// Each direction owns its record state. Close must never wait for either
	// lock: closing the socket releases blocked network I/O.
	readMu    sync.Mutex
	writeMu   sync.Mutex
	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error

	// Used to buffer reads
	readBuffer      io.Reader
	encryptedReader *bufio.Reader
}

// NewConnection returns a hap connection.
func NewConnection(connection net.Conn, context Context) *Connection {
	conn := &Connection{
		connection: connection,
		context:    context,
	}

	// Setup new session for the connection
	session := NewSession(conn)
	context.SetSessionForConnection(session, conn)

	return conn
}

// EncryptedWrite encrypts and writes bytes to the connection.
// The method returns the number of written bytes and an error when writing failed.
func (con *Connection) EncryptedWrite(b []byte) (int, error) {
	con.writeMu.Lock()
	defer con.writeMu.Unlock()
	encrypter, err := con.getEncrypter()
	if err != nil {
		return 0, err
	}
	if encrypter == nil {
		return 0, fmt.Errorf("HAP encryption is not established")
	}
	return con.encryptedWrite(b, encrypter)
}

// encryptedWrite uses the crypto captured for this operation, never a second
// context lookup which could observe a concurrently removed session.
func (con *Connection) encryptedWrite(b []byte, encrypter crypto.Encrypter) (int, error) {
	encrypted, err := encrypter.Encrypt(bytes.NewReader(b))
	if err != nil {
		_ = con.Close()
		return 0, fmt.Errorf("encrypt HAP record: %w", err)
	}
	if encrypted == nil {
		_ = con.Close()
		return 0, fmt.Errorf("encrypter returned no ciphertext reader")
	}
	encryptedBytes, err := ioutil.ReadAll(encrypted)
	if err != nil {
		_ = con.Close()
		return 0, fmt.Errorf("read encrypted HAP record: %w", err)
	}
	n, err := con.connection.Write(encryptedBytes)
	if err == nil && n != len(encryptedBytes) {
		err = io.ErrShortWrite
	}
	if err != nil {
		// A partial encrypted record cannot be replayed with a new nonce.
		_ = con.Close()
		return 0, err
	}
	return len(b), nil
}

// DecryptedRead reads and decrypts bytes from the connection.
// The method returns the number of read bytes and an error when reading failed.
func (con *Connection) DecryptedRead(b []byte) (int, error) {
	con.readMu.Lock()
	defer con.readMu.Unlock()
	decrypter, err := con.getDecrypter()
	if err != nil {
		return 0, err
	}
	if decrypter == nil {
		return 0, fmt.Errorf("HAP decryption is not established")
	}
	return con.decryptedRead(b, decrypter)
}

func (con *Connection) decryptedRead(b []byte, decrypter crypto.Decrypter) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	if con.readBuffer == nil {
		if con.encryptedReader == nil {
			con.encryptedReader = bufio.NewReader(con.connection)
		}
		decrypted, err := decrypter.Decrypt(con.encryptedReader)
		// Preserve an incremental reader even after a timeout during a partial
		// first frame; the next Read resumes the same encrypted bytes/nonce.
		con.readBuffer = decrypted
		if err != nil {
			if neterr, ok := err.(net.Error); !ok || !neterr.Timeout() {
				_ = con.Close()
			}
			return 0, err
		}
		if decrypted == nil {
			_ = con.Close()
			return 0, fmt.Errorf("decrypter returned no plaintext reader")
		}
	}
	n, err := con.readBuffer.Read(b)
	if err != nil {
		if neterr, ok := err.(net.Error); !ok || !neterr.Timeout() {
			_ = con.Close()
		}
	}
	// A short Read is normal at an authenticated record boundary. Keep both
	// readers, including any encrypted read-ahead, until connection teardown.
	return n, err
}

// Write writes bytes to the connection.
// The written bytes are encrypted when possible.
func (con *Connection) Write(b []byte) (int, error) {
	con.writeMu.Lock()
	defer con.writeMu.Unlock()
	encrypter, err := con.getEncrypter()
	if err != nil {
		return 0, err
	}
	if encrypter != nil {
		return con.encryptedWrite(b, encrypter)
	}

	return con.connection.Write(b)
}

// Read reads bytes from the connection. The read bytes are decrypted when possible.
func (con *Connection) Read(b []byte) (int, error) {
	con.readMu.Lock()
	defer con.readMu.Unlock()
	decrypter, err := con.getDecrypter()
	if err != nil {
		return 0, err
	}
	if decrypter != nil {
		return con.decryptedRead(b, decrypter)
	}

	return con.connection.Read(b)
}

// Close closes the connection and deletes the related session from the context.
func (con *Connection) Close() error {
	con.closeOnce.Do(func() {
		con.closed.Store(true)
		// Mark closed before any callback; close the socket before removing its
		// session so no I/O can downgrade to plaintext during teardown.
		con.closeErr = con.connection.Close()
		con.context.DeleteSessionForConnection(con.connection)
	})
	return con.closeErr
}

// LocalAddr calls LocalAddr() of the underlying connection
func (con *Connection) LocalAddr() net.Addr {
	return con.connection.LocalAddr()
}

// RemoteAddr calls RemoteAddr() of the underlying connection
func (con *Connection) RemoteAddr() net.Addr {
	return con.connection.RemoteAddr()
}

// SetDeadline calls SetDeadline() of the underlying connection
func (con *Connection) SetDeadline(t time.Time) error {
	return con.connection.SetDeadline(t)
}

// SetReadDeadline calls SetReadDeadline() of the underlying connection
func (con *Connection) SetReadDeadline(t time.Time) error {
	return con.connection.SetReadDeadline(t)
}

// SetWriteDeadline calls SetWriteDeadline() of the underlying connection
func (con *Connection) SetWriteDeadline(t time.Time) error {
	return con.connection.SetWriteDeadline(t)
}

// ioSession refuses a missing session even before encryption: only a present,
// unencrypted session permits the initial plaintext pairing exchange.
func (con *Connection) ioSession() (Session, error) {
	if con.closed.Load() {
		return nil, net.ErrClosed
	}
	session := con.context.GetSessionForConnection(con.connection)
	if session == nil {
		_ = con.Close()
		return nil, net.ErrClosed
	}
	if con.closed.Load() {
		return nil, net.ErrClosed
	}
	return session, nil
}

func (con *Connection) getEncrypter() (crypto.Encrypter, error) {
	session, err := con.ioSession()
	if err != nil {
		return nil, err
	}
	encrypter := session.Encrypter()
	if con.closed.Load() {
		return nil, net.ErrClosed
	}
	return encrypter, nil
}

func (con *Connection) getDecrypter() (crypto.Decrypter, error) {
	session, err := con.ioSession()
	if err != nil {
		return nil, err
	}
	decrypter := session.Decrypter()
	if con.closed.Load() {
		return nil, net.ErrClosed
	}
	return decrypter, nil
}

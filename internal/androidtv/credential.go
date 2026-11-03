package androidtv

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"strings"
	"time"

	"github.com/damione1/Homeward/internal/storage"
)

// The imported client certificate/key must already be paired with this device.
// The server certificate digest is provisioned out of band, never learned or
// replaced by the reconnect loop. Values are only read from SecretRepository.
type credentialJSON struct {
	ClientCertificatePEM    string `json:"client_certificate_pem"`
	ClientKeyPEM            string `json:"client_key_pem"`
	ServerCertificateSHA256 string `json:"server_certificate_sha256"`
}

func (credentialJSON) String() string   { return "androidtv credential [REDACTED]" }
func (credentialJSON) GoString() string { return "androidtv credential [REDACTED]" }

func credentialTLS(value storage.SecretValue, expectedMAC string) (*tls.Config, error) {
	raw, e := value.Bytes()
	if e != nil || len(raw) > 64*1024 {
		return nil, ErrCredential
	}
	defer clear(raw)
	var c credentialJSON
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(&struct{}{}) != io.EOF {
		return nil, ErrCredential
	}
	pair, e := tls.X509KeyPair([]byte(c.ClientCertificatePEM), []byte(c.ClientKeyPEM))
	if e != nil {
		return nil, ErrCredential
	}
	pin, e := hex.DecodeString(c.ServerCertificateSHA256)
	if e != nil || len(pin) != sha256.Size {
		return nil, ErrCredential
	}
	// Android Remote uses a self-signed device certificate without DNS SANs.
	// Replace Web PKI verification with mandatory exact certificate pinning,
	// validity and the Remote v2 certificate MAC identity check. There is no
	// insecure fallback, including after a certificate rotation.
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}, InsecureSkipVerify: true, VerifyConnection: func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) != 1 {
			return ErrCredential
		}
		cert := cs.PeerCertificates[0]
		got := sha256.Sum256(cert.Raw)
		now := time.Now()
		if subtle.ConstantTimeCompare(got[:], pin) != 1 || now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
			return ErrCredential
		}
		roots := x509.NewCertPool()
		roots.AddCert(cert)
		if _, e := cert.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); e != nil {
			return ErrCredential
		}
		parts := strings.Split(cert.Subject.CommonName, "/")
		if len(parts) < 2 || parts[0] != "atvremote" {
			return ErrCredential
		}
		mac, e := net.ParseMAC(parts[len(parts)-1])
		if e != nil || len(mac) != 6 || mac.String() != expectedMAC {
			return ErrCredential
		}
		return nil
	}}, nil
}

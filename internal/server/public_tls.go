package server

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/storage"
)

const publicCertificateMaxBytes int64 = 1 << 20
const publicPrivateKeyMaxBytes int64 = 64 << 10

var ErrPublicH2CNonLoopback = errors.New("server: h2c requires a loopback listen address; configure public.transport: tls")
var ErrPublicTLSUnsafe = errors.New("server: public TLS certificate or private key is unsafe or invalid")

// publicServingTransport is prepared before PostgreSQL opens. Its TLS pair is
// fully parsed and kept in memory; ServeTLS must never reopen the file names.
type publicServingTransport struct {
	mode      config.PublicTransportMode
	tlsConfig *tls.Config
}

func preparePublicServingTransport(c config.Config, settings config.PublicSettings) (publicServingTransport, error) {
	endpoint, err := c.ListenEndpoint()
	if err != nil {
		return publicServingTransport{}, err
	}
	switch settings.Transport() {
	case config.PublicTransportH2C:
		if !endpoint.Addr().IsLoopback() {
			return publicServingTransport{}, ErrPublicH2CNonLoopback
		}
		return publicServingTransport{mode: config.PublicTransportH2C}, nil
	case config.PublicTransportTLS:
		files, ok := settings.TLSFiles()
		if !ok {
			return publicServingTransport{}, ErrPublicTLSUnsafe
		}
		dataDir, err := c.DataDir()
		if err != nil {
			return publicServingTransport{}, err
		}
		root, err := storage.OpenPrivateDataDir(dataDir)
		if err != nil {
			return publicServingTransport{}, ErrPublicTLSUnsafe
		}
		defer root.Close()
		certificatePEM, err := readPublicTLSFile(root, files.CertificateFile(), publicCertificateMaxBytes, false)
		if err != nil {
			return publicServingTransport{}, ErrPublicTLSUnsafe
		}
		privateKeyPEM, err := readPublicTLSFile(root, files.PrivateKeyFile(), publicPrivateKeyMaxBytes, true)
		if err != nil {
			return publicServingTransport{}, ErrPublicTLSUnsafe
		}
		defer clear(privateKeyPEM)
		pair, err := tls.X509KeyPair(certificatePEM, privateKeyPEM)
		if err != nil {
			return publicServingTransport{}, ErrPublicTLSUnsafe
		}
		if len(pair.Certificate) == 0 {
			return publicServingTransport{}, ErrPublicTLSUnsafe
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil || !validPublicServerCertificate(leaf, time.Now()) {
			return publicServingTransport{}, ErrPublicTLSUnsafe
		}
		pair.Leaf = leaf
		return publicServingTransport{mode: config.PublicTransportTLS, tlsConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{pair},
		}}, nil
	default:
		return publicServingTransport{}, config.ErrInvalidPublicSettings
	}
}

func validPublicServerCertificate(leaf *x509.Certificate, now time.Time) bool {
	if leaf == nil || len(leaf.UnhandledCriticalExtensions) != 0 ||
		now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) ||
		leaf.KeyUsage != 0 && leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return false
	}
	if len(leaf.ExtKeyUsage) == 0 {
		// An EKU extension with only unknown OIDs is restrictive to clients;
		// it does not mean unrestricted server authentication.
		return len(leaf.UnknownExtKeyUsage) == 0
	}
	for _, usage := range leaf.ExtKeyUsage {
		if usage == x509.ExtKeyUsageServerAuth || usage == x509.ExtKeyUsageAny {
			return true
		}
	}
	return false
}

func readPublicTLSFile(root *os.Root, name string, limit int64, privateKey bool) ([]byte, error) {
	if root == nil || limit < 1 {
		return nil, ErrPublicTLSUnsafe
	}
	before, err := root.Lstat(name)
	if err != nil || !validPublicTLSFile(before, limit, privateKey) {
		return nil, ErrPublicTLSUnsafe
	}
	// O_NOFOLLOW rejects a final symlink; O_NONBLOCK prevents a swapped FIFO
	// from hanging startup between Lstat and OpenFile.
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrPublicTLSUnsafe
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || !validPublicTLSFile(opened, limit, privateKey) {
		return nil, ErrPublicTLSUnsafe
	}
	contents, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || len(contents) == 0 || int64(len(contents)) > limit {
		return nil, ErrPublicTLSUnsafe
	}
	after, err := root.Lstat(name)
	if err != nil || !os.SameFile(opened, after) || !validPublicTLSFile(after, limit, privateKey) {
		clear(contents)
		return nil, ErrPublicTLSUnsafe
	}
	return contents, nil
}

func validPublicTLSFile(info os.FileInfo, limit int64, privateKey bool) bool {
	if info == nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > limit ||
		info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return false
	}
	mode := info.Mode().Perm()
	if privateKey {
		return mode == 0400 || mode == 0600
	}
	return mode&0400 != 0 && mode&0022 == 0 && mode&0111 == 0
}

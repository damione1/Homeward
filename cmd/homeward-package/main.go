// homeward-package owns deployment files, never the server's encryption key.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"
)

const packageRoot = "/var/lib/homeward"
const credentialRoot = "/run/homeward-credentials"

var errPackage = errors.New("package: private deployment files unavailable; existing files were preserved")

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		// Paths, upstream errors, arguments and credentials never enter diagnostics.
		fmt.Fprintln(os.Stderr, errPackage)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) > 0 && args[0] == "installation" {
		argv, env, err := installationInvocation(args[1:], credentialRoot, os.Environ())
		if err != nil {
			return errPackage
		}
		return syscall.Exec("/usr/local/bin/homed", argv, env)
	}
	if len(args) != 1 {
		return errPackage
	}
	switch args[0] {
	case "prepare":
		hostname := os.Getenv("HOMEWARD_HOSTNAME")
		if hostname == "" {
			hostname = "localhost"
		}
		return prepare(packageRoot, credentialRoot, "/existing-postgres", hostname)
	case "serve":
		root, err := openPrivate(credentialRoot)
		if err != nil {
			return errPackage
		}
		defer root.Close()
		password, err := readPrivate(root, "password")
		if err != nil || len(password) != 64 {
			return errPackage
		}
		env := databaseEnvironment(os.Environ(), string(password))
		clear(password)
		return syscall.Exec("/usr/local/bin/homed", []string{"homed", "-config", packageRoot + "/configuration.yaml"}, env)
	case "health":
		return health(packageRoot+"/data/admin.sock", out)
	case "tzcheck":
		location, err := time.LoadLocation("America/Toronto")
		if err != nil {
			return errPackage
		}
		_, winter := time.Date(2026, 1, 15, 12, 0, 0, 0, location).Zone()
		_, summer := time.Date(2026, 7, 15, 12, 0, 0, 0, location).Zone()
		if winter != -5*3600 || summer != -4*3600 {
			return errPackage
		}
		_, err = fmt.Fprintln(out, "America/Toronto: EST=-05:00 EDT=-04:00")
		return err
	default:
		return errPackage
	}
}

func databaseEnvironment(existing []string, password string) []string {
	result := make([]string, 0, len(existing)+1)
	for _, entry := range existing {
		if !strings.HasPrefix(entry, "HOMEWARD_POSTGRES_PASSWORD=") {
			result = append(result, entry)
		}
	}
	return append(result, "HOMEWARD_POSTGRES_PASSWORD="+password)
}

func openPrivate(path string) (*os.Root, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || !owned(info) {
		return nil, errPackage
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, errPackage
	}
	return root, nil
}

func owned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func readPrivate(root *os.Root, name string) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !owned(info) || info.Size() > 1<<20 {
		return nil, errPackage
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errPackage
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errPackage
	}
	return io.ReadAll(io.LimitReader(file, 1<<20))
}

func writeNew(root *os.Root, name string, content []byte) error {
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errPackage
	}
	_, writeErr := file.Write(content)
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}

func validHostname(value string) bool {
	if len(value) < 1 || len(value) > 253 || strings.HasPrefix(value, "-") || strings.HasSuffix(value, "-") {
		return false
	}
	if net.ParseIP(value) != nil {
		return true
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-') {
			return false
		}
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
	}
	return true
}

func prepare(dataPath, credentialPath, postgresPath, hostname string) error {
	if !validHostname(hostname) {
		return errPackage
	}
	data, err := openPrivate(dataPath)
	if err != nil {
		return errPackage
	}
	defer data.Close()
	credentials, err := openPrivate(credentialPath)
	if err != nil {
		return errPackage
	}
	defer credentials.Close()
	if marker, err := readPrivate(data, "package.json"); err == nil {
		var recorded struct {
			Hostname string `json:"hostname"`
		}
		if json.Unmarshal(marker, &recorded) != nil || recorded.Hostname != hostname {
			return errPackage
		}
		for _, name := range []string{"configuration.yaml", "data/server.crt", "data/server.key"} {
			if _, err := readPrivate(data, name); err != nil {
				return errPackage
			}
		}
		password, err := readPrivate(credentials, "password")
		clear(password)
		return err
	}
	// Missing credentials on an existing DB are an operator recovery, never
	// a reason to choose a new database password. Permission denial also refuses.
	entries, err := os.ReadDir(postgresPath)
	if err != nil || len(entries) != 0 {
		return errPackage
	}
	for _, name := range []string{"configuration.yaml", "package.json", "data"} {
		if _, err := data.Lstat(name); !errors.Is(err, os.ErrNotExist) {
			return errPackage
		}
	}
	if _, err := credentials.Lstat("password"); !errors.Is(err, os.ErrNotExist) {
		return errPackage
	}
	password := make([]byte, 32)
	if _, err := rand.Read(password); err != nil {
		return errPackage
	}
	defer clear(password)
	if err := writeNew(credentials, "password", []byte(hex.EncodeToString(password))); err != nil {
		return errPackage
	}
	if err := data.Mkdir("data", 0700); err != nil {
		return errPackage
	}
	certificate, key, err := newCertificate(hostname)
	if err != nil {
		return errPackage
	}
	defer clear(key)
	configuration := fmt.Sprintf("listen: '0.0.0.0:8787'\ndata_dir: '/var/lib/homeward/data'\npublic:\n  url: 'https://%s'\n  transport: tls\n  tls:\n    certificate_file: server.crt\n    private_key_file: server.key\npostgres:\n  host: postgres\n  port: 5432\n  database: homeward\n  user: homeward\n  password_env: HOMEWARD_POSTGRES_PASSWORD\n  sslmode: disable\n", net.JoinHostPort(hostname, "8787"))
	marker, _ := json.Marshal(struct {
		Hostname string `json:"hostname"`
	}{hostname})
	for _, entry := range []struct {
		name  string
		value []byte
	}{{"data/server.key", key}, {"data/server.crt", certificate}, {"configuration.yaml", []byte(configuration)}, {"package.json", marker}} {
		if err := writeNew(data, entry.name, entry.value); err != nil {
			return errPackage
		}
	}
	for _, root := range []*os.Root{credentials, data} {
		file, err := root.Open(".")
		if err != nil {
			return errPackage
		}
		err = file.Sync()
		file.Close()
		if err != nil {
			return errPackage
		}
	}
	return nil
}

func newCertificate(hostname string) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, errPackage
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, errPackage
	}
	now := time.Now()
	certificate := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: hostname}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(1, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if ip := net.ParseIP(hostname); ip != nil {
		certificate.IPAddresses = []net.IP{ip}
	} else {
		certificate.DNSNames = []string{hostname}
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		return nil, nil, errPackage
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, errPackage
	}
	defer clear(private)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), nil
}

func health(socket string, out io.Writer) error {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Clean(socket))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get("http://operator/health")
	if err != nil {
		return errPackage
	}
	defer response.Body.Close()
	var result struct {
		Live     bool `json:"live"`
		Ready    bool `json:"ready"`
		Degraded bool `json:"degraded"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 512)).Decode(&result) != nil || response.StatusCode != http.StatusOK {
		return errPackage
	}
	if err := json.NewEncoder(out).Encode(result); err != nil {
		return errPackage
	}
	if !result.Live || !result.Ready {
		return errPackage
	}
	return nil
}

// The local operator command reuses the package credential boundary. Secrets
// enter the child environment only; they never become argv or diagnostics.
func installationInvocation(args []string, credentialsPath string, existing []string) ([]string, []string, error) {
	if len(args) == 0 || (args[0] != "backup" && args[0] != "restore") {
		return nil, nil, errPackage
	}
	root, err := openPrivate(credentialsPath)
	if err != nil {
		return nil, nil, errPackage
	}
	defer root.Close()
	password, err := readPrivate(root, "password")
	if err != nil || len(password) != 64 {
		clear(password)
		return nil, nil, errPackage
	}
	defer clear(password)
	argv := []string{"homed", "installation", args[0], "--pg-bin-dir", "/opt/homeward/pg-client/bin"}
	if args[0] == "backup" {
		argv = append(argv, "--bootstrap", packageRoot+"/configuration.yaml.bootstrap.yaml")
	}
	argv = append(argv, args[1:]...)
	return argv, databaseEnvironment(existing, string(password)), nil
}

package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type endpointKind string

const (
	endpointHTTPS endpointKind = "https"
	endpointUnix  endpointKind = "unix"
)

type profile struct {
	Kind   endpointKind `json:"kind"`
	URL    string       `json:"url,omitempty"`
	Socket string       `json:"socket,omitempty"`
	CA     []byte       `json:"ca_pem,omitempty"`
}

type profileAdd struct {
	Name   string `arg:""`
	URL    string `xor:"endpoint" help:"Canonical HTTPS origin (no path, credentials, query or fragment)."`
	Socket string `xor:"endpoint" help:"Absolute path to the local protected admin.sock."`
	CA     string `help:"Optional PEM trust anchor file for HTTPS; normal hostname validation remains enabled."`
}
type profileList struct{}

func canonicalURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return "", fail("profile_requires_https_origin", ExitUsage)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.ContainsAny(host, " \t\r\n/%") || strings.HasSuffix(host, ".") {
		return "", fail("invalid_profile_origin", ExitUsage)
	}
	port := u.Port()
	if port != "" {
		parsed, err := strconv.ParseUint(port, 10, 16)
		if err != nil || parsed == 0 || strconv.FormatUint(parsed, 10) != port {
			return "", fail("invalid_profile_origin", ExitUsage)
		}
	}
	if port == "443" {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return "https://" + host, nil
}

func (p profile) validate() error {
	switch p.Kind {
	case endpointHTTPS:
		canonical, err := canonicalURL(p.URL)
		if err != nil || canonical != p.URL || p.Socket != "" {
			return fail("invalid_profile", ExitUsage)
		}
		if len(p.CA) != 0 {
			if _, err := certificatePEM(p.CA); err != nil {
				return err
			}
		}
	case endpointUnix:
		if p.URL != "" || len(p.CA) != 0 || !filepath.IsAbs(p.Socket) || filepath.Clean(p.Socket) != p.Socket || filepath.Base(p.Socket) != "admin.sock" {
			return fail("invalid_local_operator_profile", ExitUsage)
		}
	default:
		return fail("invalid_profile_kind", ExitUsage)
	}
	return nil
}

func (c *profileAdd) Run(v *invocation) error {
	name, err := parseProfileName(c.Name)
	if err != nil {
		return err
	}
	p := profile{}
	if c.URL != "" {
		p.Kind = endpointHTTPS
		p.URL, err = canonicalURL(c.URL)
		if err != nil {
			return err
		}
		if c.CA != "" {
			raw, err := readRegularInput(c.CA)
			if err != nil {
				return err
			}
			p.CA, err = certificatePEM(raw)
			clear(raw)
			if err != nil {
				return err
			}
		}
	} else if c.Socket != "" && c.CA == "" {
		p.Kind, p.Socket = endpointUnix, c.Socket
	} else {
		return fail("choose_one_profile_endpoint", ExitUsage)
	}
	if err := p.validate(); err != nil {
		return err
	}
	dir, err := v.directory()
	if err != nil {
		return err
	}
	if err := dir.write(name.value+".profile.json", p, false); err != nil {
		return err
	}
	return v.emitJSON(struct {
		Name    string        `json:"name"`
		Profile publicProfile `json:"profile"`
	}{name.value, p.public()})
}

func (*profileList) Run(v *invocation) error {
	dir, err := v.directory()
	if err != nil {
		return err
	}
	fd, err := unix.Openat(dir.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fail("profiles_unavailable", ExitLocal)
	}
	file := os.NewFile(uintptr(fd), dir.path)
	entries, err := file.ReadDir(-1)
	_ = file.Close()
	if err != nil {
		return fail("profiles_unavailable", ExitLocal)
	}
	type entry struct {
		Name    string        `json:"name"`
		Profile publicProfile `json:"profile"`
	}
	result := []entry{}
	for _, item := range entries {
		if !strings.HasSuffix(item.Name(), ".profile.json") {
			continue
		}
		name := strings.TrimSuffix(item.Name(), ".profile.json")
		if _, err := parseProfileName(name); err != nil {
			return fail("invalid_local_profile", ExitLocal)
		}
		var p profile
		if err := dir.read(item.Name(), &p); err != nil {
			return err
		}
		if err := p.validate(); err != nil {
			return err
		}
		result = append(result, entry{name, p.public()})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return v.emitJSON(result)
}

func (v *invocation) selected() (profileName, profile, error) {
	name, err := parseProfileName(v.args.Profile)
	if err != nil {
		return name, profile{}, fail("explicit_profile_required", ExitUsage)
	}
	dir, err := v.directory()
	if err != nil {
		return name, profile{}, err
	}
	var p profile
	if err := dir.read(name.value+".profile.json", &p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return name, p, fail("unknown_profile", ExitUsage)
		}
		return name, p, err
	}
	return name, p, p.validate()
}

func (p profile) client() (*http.Client, string, error) {
	if err := p.validate(); err != nil {
		return nil, "", err
	}
	transport := &http.Transport{Proxy: nil, ForceAttemptHTTP2: true}
	base := p.URL
	switch p.Kind {
	case endpointHTTPS:
		var roots *x509.CertPool
		if len(p.CA) != 0 {
			roots = x509.NewCertPool()
			if !roots.AppendCertsFromPEM(p.CA) {
				return nil, "", fail("invalid_profile_ca", ExitUsage)
			}
		}
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}
	case endpointUnix:
		base = "http://operator"
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) { return dialOperator(ctx, p.Socket) }
	default:
		return nil, "", fail("invalid_profile_kind", ExitUsage)
	}
	return &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return fail("redirect_refused", ExitPermission) }}, base, nil
}

func dialOperator(ctx context.Context, path string) (net.Conn, error) {
	dir, err := openPrivateDirectory(filepath.Dir(path), false)
	if err != nil {
		return nil, err
	}
	defer dir.close()
	var stat unix.Stat_t
	if err := unix.Fstatat(dir.fd, filepath.Base(path), &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFSOCK || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0777 != 0600 {
		return nil, fail("unsafe_operator_socket", ExitPermission)
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fail("operator_socket_unavailable", ExitUnavailable)
	}
	local, ok := conn.(*net.UnixConn)
	if !ok {
		_ = conn.Close()
		return nil, fail("unsafe_operator_socket", ExitPermission)
	}
	uid, err := peerUID(local)
	if err != nil || uid != uint32(os.Geteuid()) {
		_ = conn.Close()
		return nil, fail("operator_peer_uid_mismatch", ExitPermission)
	}
	return conn, nil
}

// No PEM bytes, including historic profile content, belong in terminal output.
type publicProfile struct {
	Kind     endpointKind `json:"kind"`
	URL      string       `json:"url,omitempty"`
	Socket   string       `json:"socket,omitempty"`
	CustomCA bool         `json:"custom_ca"`
}

func (p profile) public() publicProfile {
	return publicProfile{p.Kind, p.URL, p.Socket, len(p.CA) != 0}
}

func certificatePEM(raw []byte) ([]byte, error) {
	rest := bytes.TrimSpace(raw)
	result := []byte{}
	for len(rest) != 0 {
		if !bytes.HasPrefix(rest, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, fail("profile_ca_requires_certificates_only", ExitUsage)
		}
		block, tail := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, fail("profile_ca_requires_certificates_only", ExitUsage)
		}
		certificates, err := x509.ParseCertificates(block.Bytes)
		if err != nil || len(certificates) == 0 {
			return nil, fail("invalid_profile_ca", ExitUsage)
		}
		for _, certificate := range certificates {
			result = append(result, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})...)
		}
		rest = bytes.TrimSpace(tail)
	}
	if len(result) == 0 || len(result) > maximumLocalFile {
		return nil, fail("invalid_profile_ca", ExitUsage)
	}
	return result, nil
}

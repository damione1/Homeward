package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultMaxRequestBytes       int64 = 4 << 20
	maxMaxRequestBytes           int64 = 64 << 20
	defaultMaxConcurrentRequests       = 64
	maxMaxConcurrentRequests           = 4096
	defaultRequestTimeout              = 30 * time.Second
	maxRequestTimeout                  = 5 * time.Minute
	defaultStreamTimeout               = 5 * time.Minute
	maxStreamTimeout                   = 24 * time.Hour
	defaultShutdownTimeout             = 6 * time.Second
	maxShutdownTimeout                 = time.Minute
)

var ErrInvalidPublicSettings = errors.New("invalid public transport settings")

type publicInput struct {
	URL                   *string         `yaml:"url"`
	Transport             *string         `yaml:"transport"`
	TLS                   *publicTLSInput `yaml:"tls"`
	MaxRequestBytes       *int64          `yaml:"max_request_bytes"`
	MaxConcurrentRequests *int            `yaml:"max_concurrent_requests"`
	RequestTimeout        *string         `yaml:"request_timeout"`
	StreamTimeout         *string         `yaml:"stream_timeout"`
	ShutdownTimeout       *string         `yaml:"shutdown_timeout"`
}

type publicTLSInput struct {
	CertificateFile *string `yaml:"certificate_file"`
	PrivateKeyFile  *string `yaml:"private_key_file"`
}

// PublicTransportMode is a closed set admitted at the configuration boundary.
// Its zero value is never a serving mode.
type PublicTransportMode uint8

const (
	PublicTransportUnspecified PublicTransportMode = iota
	PublicTransportH2C
	PublicTransportTLS
)

// PublicTLSFiles names two files within the private data directory. Callers
// cannot replace either half of an admitted pair.
type PublicTLSFiles struct {
	certificateFile string
	privateKeyFile  string
}

func (f PublicTLSFiles) CertificateFile() string { return f.certificateFile }
func (f PublicTLSFiles) PrivateKeyFile() string  { return f.privateKeyFile }

// PublicSettings is parsed once at the YAML boundary. The canonical URL is
// metadata for clients and proxy configuration; it never proves peer origin.
type PublicSettings struct {
	canonicalURL          url.URL
	hasCanonicalURL       bool
	transport             PublicTransportMode
	tlsFiles              PublicTLSFiles
	maxRequestBytes       int64
	maxConcurrentRequests int
	requestTimeout        time.Duration
	streamTimeout         time.Duration
	shutdownTimeout       time.Duration
	valid                 bool
}

func (s PublicSettings) CanonicalURL() (url.URL, bool) {
	return s.canonicalURL, s.valid && s.hasCanonicalURL
}
func (s PublicSettings) Transport() PublicTransportMode { return s.transport }
func (s PublicSettings) TLSFiles() (PublicTLSFiles, bool) {
	return s.tlsFiles, s.valid && s.transport == PublicTransportTLS
}
func (s PublicSettings) MaxRequestBytes() int64         { return s.maxRequestBytes }
func (s PublicSettings) MaxConcurrentRequests() int     { return s.maxConcurrentRequests }
func (s PublicSettings) RequestTimeout() time.Duration  { return s.requestTimeout }
func (s PublicSettings) StreamTimeout() time.Duration   { return s.streamTimeout }
func (s PublicSettings) ShutdownTimeout() time.Duration { return s.shutdownTimeout }

func parsePublicSettings(input *publicInput) (PublicSettings, error) {
	result := PublicSettings{
		transport:       PublicTransportH2C,
		maxRequestBytes: defaultMaxRequestBytes, maxConcurrentRequests: defaultMaxConcurrentRequests,
		requestTimeout: defaultRequestTimeout, streamTimeout: defaultStreamTimeout,
		shutdownTimeout: defaultShutdownTimeout, valid: true,
	}
	if input == nil {
		return result, nil
	}
	if input.URL != nil {
		parsed, err := parseCanonicalURL(*input.URL)
		if err != nil {
			return PublicSettings{}, err
		}
		result.canonicalURL, result.hasCanonicalURL = parsed, true
	}
	if input.Transport != nil {
		switch *input.Transport {
		case "h2c":
			result.transport = PublicTransportH2C
		case "tls":
			result.transport = PublicTransportTLS
		default:
			return PublicSettings{}, fmt.Errorf("%w: transport", ErrInvalidPublicSettings)
		}
	}
	if result.transport == PublicTransportTLS && result.hasCanonicalURL && result.canonicalURL.Scheme != "https" {
		return PublicSettings{}, fmt.Errorf("%w: tls requires an https url", ErrInvalidPublicSettings)
	}
	if result.transport == PublicTransportTLS {
		if input.TLS == nil || input.TLS.CertificateFile == nil || input.TLS.PrivateKeyFile == nil ||
			!validPublicTLSFileName(*input.TLS.CertificateFile) || !validPublicTLSFileName(*input.TLS.PrivateKeyFile) ||
			*input.TLS.CertificateFile == *input.TLS.PrivateKeyFile {
			return PublicSettings{}, fmt.Errorf("%w: tls", ErrInvalidPublicSettings)
		}
		result.tlsFiles = PublicTLSFiles{certificateFile: *input.TLS.CertificateFile, privateKeyFile: *input.TLS.PrivateKeyFile}
	} else if input.TLS != nil {
		return PublicSettings{}, fmt.Errorf("%w: tls requires transport tls", ErrInvalidPublicSettings)
	}
	if input.MaxRequestBytes != nil {
		if *input.MaxRequestBytes < 1 || *input.MaxRequestBytes > maxMaxRequestBytes {
			return PublicSettings{}, fmt.Errorf("%w: max_request_bytes", ErrInvalidPublicSettings)
		}
		result.maxRequestBytes = *input.MaxRequestBytes
	}
	if input.MaxConcurrentRequests != nil {
		if *input.MaxConcurrentRequests < 1 || *input.MaxConcurrentRequests > maxMaxConcurrentRequests {
			return PublicSettings{}, fmt.Errorf("%w: max_concurrent_requests", ErrInvalidPublicSettings)
		}
		result.maxConcurrentRequests = *input.MaxConcurrentRequests
	}
	var err error
	if result.requestTimeout, err = parseBoundedDuration(input.RequestTimeout, defaultRequestTimeout, maxRequestTimeout); err != nil {
		return PublicSettings{}, fmt.Errorf("%w: request_timeout", ErrInvalidPublicSettings)
	}
	if result.streamTimeout, err = parseBoundedDuration(input.StreamTimeout, defaultStreamTimeout, maxStreamTimeout); err != nil {
		return PublicSettings{}, fmt.Errorf("%w: stream_timeout", ErrInvalidPublicSettings)
	}
	if result.shutdownTimeout, err = parseBoundedDuration(input.ShutdownTimeout, defaultShutdownTimeout, maxShutdownTimeout); err != nil {
		return PublicSettings{}, fmt.Errorf("%w: shutdown_timeout", ErrInvalidPublicSettings)
	}
	return result, nil
}

func validPublicTLSFileName(name string) bool {
	if len(name) < 1 || len(name) > 128 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		if i > 0 && (c == '_' || c == '-' || c == '.') {
			continue
		}
		return false
	}
	return true
}

func parseBoundedDuration(raw *string, fallback, ceiling time.Duration) (time.Duration, error) {
	if raw == nil {
		return fallback, nil
	}
	value, err := time.ParseDuration(*raw)
	if err != nil || value <= 0 || value > ceiling {
		return 0, ErrInvalidPublicSettings
	}
	return value, nil
}

func parseCanonicalURL(raw string) (url.URL, error) {
	value, err := url.Parse(raw)
	if err != nil || value == nil || value.Opaque != "" || value.User != nil ||
		value.Host == "" || value.Hostname() == "" || strings.HasSuffix(value.Host, ":") ||
		value.Path != "" && value.Path != "/" ||
		value.RawPath != "" || value.RawQuery != "" || value.ForceQuery ||
		value.Fragment != "" || value.RawFragment != "" || value.Scheme != "https" && value.Scheme != "http" {
		return url.URL{}, fmt.Errorf("%w: url", ErrInvalidPublicSettings)
	}
	if strings.ContainsAny(value.Hostname(), " \t\r\n") {
		return url.URL{}, fmt.Errorf("%w: url", ErrInvalidPublicSettings)
	}
	if port := value.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 || strconv.Itoa(number) != port {
			return url.URL{}, fmt.Errorf("%w: url", ErrInvalidPublicSettings)
		}
	}
	if value.Scheme == "http" {
		host := value.Hostname()
		if host != "localhost" {
			ip := net.ParseIP(host)
			if ip == nil || !ip.IsLoopback() {
				return url.URL{}, fmt.Errorf("%w: url", ErrInvalidPublicSettings)
			}
		}
	}
	value.Path = ""
	return *value, nil
}

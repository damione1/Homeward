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
	URL                   *string `yaml:"url"`
	MaxRequestBytes       *int64  `yaml:"max_request_bytes"`
	MaxConcurrentRequests *int    `yaml:"max_concurrent_requests"`
	RequestTimeout        *string `yaml:"request_timeout"`
	StreamTimeout         *string `yaml:"stream_timeout"`
	ShutdownTimeout       *string `yaml:"shutdown_timeout"`
}

// PublicSettings is parsed once at the YAML boundary. The canonical URL is
// metadata for clients and proxy configuration; it never proves peer origin.
type PublicSettings struct {
	canonicalURL          url.URL
	hasCanonicalURL       bool
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
func (s PublicSettings) MaxRequestBytes() int64         { return s.maxRequestBytes }
func (s PublicSettings) MaxConcurrentRequests() int     { return s.maxConcurrentRequests }
func (s PublicSettings) RequestTimeout() time.Duration  { return s.requestTimeout }
func (s PublicSettings) StreamTimeout() time.Duration   { return s.streamTimeout }
func (s PublicSettings) ShutdownTimeout() time.Duration { return s.shutdownTimeout }

func parsePublicSettings(input *publicInput) (PublicSettings, error) {
	result := PublicSettings{
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

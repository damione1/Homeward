package integrations

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

type conditionalDestinationKind uint8

const (
	conditionalPublicHTTPS conditionalDestinationKind = iota + 1
	conditionalExplicitNetwork
)

type ConditionalDestination struct {
	kind     conditionalDestinationKind
	origin   string
	prefixes []netip.Prefix
}

func PublicConditionalDestination() ConditionalDestination {
	return ConditionalDestination{kind: conditionalPublicHTTPS}
}
func ExplicitConditionalDestination(origin string, prefixes []netip.Prefix) (ConditionalDestination, error) {
	if len(prefixes) == 0 || len(prefixes) > 64 {
		return ConditionalDestination{}, conditionalFailure(ConditionalInvalid)
	}
	u, e := parseConditionalURL(origin)
	if e != nil || u.RawQuery != "" || u.ForceQuery || u.Path != "" && u.Path != "/" {
		return ConditionalDestination{}, conditionalFailure(ConditionalInvalid)
	}
	for i, p := range prefixes {
		if !p.IsValid() || p != p.Masked() || p.Addr().Is4In6() {
			return ConditionalDestination{}, conditionalFailure(ConditionalInvalid)
		}
		for _, prior := range prefixes[:i] {
			if prior == p {
				return ConditionalDestination{}, conditionalFailure(ConditionalInvalid)
			}
		}
	}
	return ConditionalDestination{kind: conditionalExplicitNetwork, origin: conditionalOrigin(u), prefixes: slices.Clone(prefixes)}, nil
}
func (d ConditionalDestination) valid() bool {
	switch d.kind {
	case conditionalPublicHTTPS:
		return d.origin == "" && len(d.prefixes) == 0
	case conditionalExplicitNetwork:
		return d.origin != "" && len(d.prefixes) > 0 && len(d.prefixes) <= 64
	default:
		return false
	}
}
func (d ConditionalDestination) String() string               { return "conditional destination [redacted]" }
func (d ConditionalDestination) GoString() string             { return d.String() }
func (d ConditionalDestination) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

func parseConditionalURL(raw string) (*url.URL, error) {
	if len(raw) == 0 || len(raw) > 8192 || strings.ContainsAny(raw, "\x00\r\n\t \\") {
		return nil, conditionalFailure(ConditionalInvalid)
	}
	u, e := url.Parse(raw)
	if e != nil || u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawFragment != "" || strings.Contains(raw, "#") || u.Opaque != "" || u.Hostname() == "" {
		return nil, conditionalFailure(ConditionalInvalid)
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, "%") {
		return nil, conditionalFailure(ConditionalInvalid)
	}
	if address, e := netip.ParseAddr(host); e == nil {
		if address.Zone() != "" {
			return nil, conditionalFailure(ConditionalInvalid)
		}
	} else {
		if len(host) > 253 || strings.HasSuffix(host, ".") {
			return nil, conditionalFailure(ConditionalInvalid)
		}
		for _, label := range strings.Split(host, ".") {
			if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return nil, conditionalFailure(ConditionalInvalid)
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					return nil, conditionalFailure(ConditionalInvalid)
				}
			}
		}
	}
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") {
		return nil, conditionalFailure(ConditionalInvalid)
	}
	if port != "" {
		n, e := strconv.ParseUint(port, 10, 16)
		if e != nil || n == 0 || strconv.FormatUint(n, 10) != port {
			return nil, conditionalFailure(ConditionalInvalid)
		}
	}
	if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	}
	// RawPath/RawQuery remain exact; no query sorting or decoding/reencoding.
	if _, e := http.NewRequest(http.MethodGet, u.String(), nil); e != nil {
		return nil, conditionalFailure(ConditionalInvalid)
	}
	return u, nil
}
func conditionalOrigin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return u.Scheme + "://" + net.JoinHostPort(u.Hostname(), port)
}
func (d ConditionalDestination) admitsAddress(a netip.Addr) bool {
	if !d.valid() || !a.IsValid() || a.Zone() != "" {
		return false
	}
	a = a.Unmap()
	if d.kind == conditionalPublicHTTPS {
		return conditionalPublicAddress(a)
	}
	for _, p := range d.prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
func (d ConditionalDestination) admitURL(raw string) (*url.URL, error) {
	if !d.valid() {
		return nil, conditionalFailure(ConditionalInvalid)
	}
	u, e := parseConditionalURL(raw)
	if e != nil {
		return nil, e
	}
	if d.kind == conditionalPublicHTTPS && u.Scheme != "https" || d.kind == conditionalExplicitNetwork && conditionalOrigin(u) != d.origin {
		return nil, conditionalFailure(ConditionalDestinationDenied)
	}
	if ip, e := netip.ParseAddr(u.Hostname()); e == nil && !d.admitsAddress(ip) {
		return nil, conditionalFailure(ConditionalDestinationDenied)
	}
	return u, nil
}

// Dependencies remain private. Only _test.go exports the loopback bridge;
// production construction always uses the standard resolver/dialer/trust.
type conditionalNetwork struct {
	lookup func(context.Context, string) ([]netip.Addr, error)
	dial   func(context.Context, string, string) (net.Conn, error)
}

func newConditionalTransport(destination ConditionalDestination, supplied *http.Transport, network conditionalNetwork) (*http.Transport, error) {
	if !destination.valid() {
		return nil, conditionalFailure(ConditionalInvalid)
	}
	lookup := network.lookup
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	dial := network.dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: -1}).DialContext
	}
	// Construct, never clone client hooks: TLS callbacks, ServerName, custom
	// roots, proxy, DialTLS and alternate protocol handlers cannot bypass us.
	limit := int64(cloudAuthorizationHeaderLimit)
	if supplied != nil && supplied.MaxResponseHeaderBytes > 0 && supplied.MaxResponseHeaderBytes < limit {
		limit = supplied.MaxResponseHeaderBytes
	}
	t := &http.Transport{Proxy: nil, DisableKeepAlives: true, MaxResponseHeaderBytes: limit, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, ForceAttemptHTTP2: false}
	t.DialContext = func(ctx context.Context, protocol, address string) (net.Conn, error) {
		if protocol != "tcp" && protocol != "tcp4" && protocol != "tcp6" {
			return nil, conditionalFailure(ConditionalDestinationDenied)
		}
		host, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, conditionalFailure(ConditionalDestinationDenied)
		}
		if destination.kind == conditionalExplicitNetwork {
			origin, e := url.Parse(destination.origin)
			if e != nil || !strings.EqualFold(origin.Hostname(), host) || origin.Port() != port {
				return nil, conditionalFailure(ConditionalDestinationDenied)
			}
		}
		var answers []netip.Addr
		if a, e := netip.ParseAddr(host); e == nil {
			answers = []netip.Addr{a}
		} else {
			answers, e = lookup(ctx, host)
			if e != nil {
				return nil, conditionalFailure(ConditionalDestinationDenied)
			}
		}
		if len(answers) == 0 || len(answers) > 64 {
			return nil, conditionalFailure(ConditionalDestinationDenied)
		}
		for _, a := range answers {
			if !destination.admitsAddress(a) {
				return nil, conditionalFailure(ConditionalDestinationDenied)
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return dial(ctx, "tcp", net.JoinHostPort(answers[0].Unmap().String(), port))
	}
	return t, nil
}

package integrations

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestNHA101PublicDestinationSpecialRegistry(t *testing.T) {
	for _, s := range []string{"8.8.8.8", "192.0.0.9", "192.0.0.10", "192.31.196.1", "2001:4860:4860::8888", "2001:1::3"} {
		if !conditionalPublicAddress(netip.MustParseAddr(s)) {
			t.Fatalf("public exception refused %s", s)
		}
	}
	for _, s := range []string{"0.0.0.0", "10.0.0.1", "100.64.0.1", "127.0.0.1", "169.254.169.254", "192.0.0.8", "192.88.99.2", "192.168.1.2", "198.18.0.1", "203.0.113.1", "224.0.0.1", "255.255.255.255", "::", "::1", "::ffff:127.0.0.1", "64:ff9b::a9fe:a9fe", "64:ff9b:1::1", "2001::1", "2002::1", "2001:db8::1", "3fff::1", "5f00::1", "fc00::1", "fe80::1"} {
		if conditionalPublicAddress(netip.MustParseAddr(s)) {
			t.Fatalf("nonpublic/indeterminate destination admitted %s", s)
		}
	}
	if !conditionalPublicAddress(netip.MustParseAddr("64:ff9b::808:808")) {
		t.Fatal("public translated IPv4 refused")
	}
}

func TestNHA101DestinationRejectsIndeterminateIPv6(t *testing.T) {
	for _, raw := range []string{"fec0::1", "::a9fe:00fe", "64:ff9b::7f00:1", "64:ff9b:1::a9fe:00fe", "2002:a9fe:00fe::1", "2001:0::a9fe:00fe", "4000::1"} {
		if conditionalPublicAddress(netip.MustParseAddr(raw)) {
			t.Fatalf("indeterminate/translating IPv6 admitted: %s", raw)
		}
	}
}

func TestNHA101DestinationDNSAllAnswersAndRebinding(t *testing.T) {
	ctx := context.Background()
	lookups, dials := 0, 0
	network := conditionalNetwork{
		lookup: func(context.Context, string) ([]netip.Addr, error) {
			lookups++
			if lookups == 1 {
				return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("::ffff:169.254.169.254")}, nil
			}
			if lookups == 2 {
				return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
			}
			return []netip.Addr{netip.MustParseAddr("10.0.0.2")}, nil
		},
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			dials++
			if address != "8.8.8.8:443" {
				t.Error("guard performed a second hostname lookup")
			}
			return nil, errors.New("synthetic dial stop")
		},
	}
	transport, err := newConditionalTransport(PublicConditionalDestination(), nil, network)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err = transport.DialContext(ctx, "tcp", "calendar.invalid:443"); err == nil {
			t.Fatal("synthetic dial unexpectedly succeeded")
		}
	}
	if lookups != 3 || dials != 1 {
		t.Fatal("mixed answer or rebinding bypassed pre-dial guard")
	}
	explicit, err := ExplicitConditionalDestination("http://calendar.invalid:8080", []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = explicit.admitURL("http://other.invalid:8080/"); err == nil {
		t.Fatal("explicit network widened origin")
	}
	if explicit.admitsAddress(netip.MustParseAddr("10.0.1.2")) || !explicit.admitsAddress(netip.MustParseAddr("10.0.0.2")) {
		t.Fatal("explicit prefix widened or lost")
	}
}

func TestNHA101DestinationTLSCannotInheritVerificationBypass(t *testing.T) {
	hello := make(chan string, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("untrusted TLS reached HTTP") }))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) { hello <- h.ServerName; return nil, nil }}
	server.StartTLS()
	defer server.Close()
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	destination, err := ExplicitConditionalDestination("https://calendar.invalid:"+port, []netip.Prefix{netip.MustParsePrefix("8.8.8.8/32")})
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	called := false
	supplied := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "example.com", InsecureSkipVerify: true, VerifyPeerCertificate: func([][]byte, [][]*x509.Certificate) error { called = true; return nil }, VerifyConnection: func(tls.ConnectionState) error { called = true; return nil }}}
	owned, err := newConditionalTransport(destination, supplied, conditionalNetwork{lookup: func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}, dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer owned.CloseIdleConnections()
	request, err := http.NewRequest(http.MethodGet, "https://calendar.invalid:"+port+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owned.RoundTrip(request); err == nil || called || owned.TLSClientConfig.RootCAs != nil {
		t.Fatal("custom trust/callback bypassed standard verification")
	}
	select {
	case name := <-hello:
		if name != "calendar.invalid" {
			t.Fatal("TLS SNI did not retain origin")
		}
	default:
		t.Fatal("TLS did not receive original SNI")
	}
}

func TestNHA101DestinationProxyEnvironmentAndCancellation(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
	entered := make(chan struct{})
	transport, err := newConditionalTransport(PublicConditionalDestination(), nil, conditionalNetwork{lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}, dial: func(context.Context, string, string) (net.Conn, error) {
		t.Error("canceled resolver reached dial")
		return nil, errors.New("synthetic dial stop")
	}})
	if err != nil || transport.Proxy != nil {
		t.Fatal("environment proxy inherited", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, e := transport.DialContext(ctx, "tcp", "calendar.invalid:443"); done <- e }()
	select {
	case <-entered:
	case e := <-done:
		t.Fatal("resolver guard returned before the cancellation barrier", e)
	case <-ctx.Done():
		t.Fatal("resolver did not reach its barrier")
	}
	cancel()
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("resolver cancellation ignored")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resolver operation did not join")
	}
}

func TestNHA101DestinationOwnedTransportAndExactResource(t *testing.T) {
	destination := PublicConditionalDestination()
	for _, s := range []string{"http://example.invalid/", "https://user:token@example.invalid/", "https://example.invalid/#fragment", "https://[fe80::1%25en0]/"} {
		if _, err := destination.admitURL(s); err == nil {
			t.Fatal("unsafe URL admitted")
		}
	}
	u, err := destination.admitURL("https://EXAMPLE.invalid/a?b=2&a=1&a=0")
	if err != nil || u.RawQuery != "b=2&a=1&a=0" {
		t.Fatal("resource query changed", err)
	}
	called := false
	supplied := &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{InsecureSkipVerify: true, VerifyConnection: func(tls.ConnectionState) error { called = true; return nil }}, DialTLSContext: func(context.Context, string, string) (net.Conn, error) { called = true; return nil, nil }}
	transport, err := newConditionalTransport(destination, supplied, conditionalNetwork{})
	if err != nil {
		t.Fatal(err)
	}
	if transport.Proxy != nil || transport.DialTLSContext != nil || transport.DialTLS != nil || transport.TLSClientConfig.InsecureSkipVerify || transport.TLSClientConfig.VerifyConnection != nil || transport.TLSClientConfig.VerifyPeerCertificate != nil || called {
		t.Fatal("client transport hooks bypass standard TLS/destination verification")
	}
}

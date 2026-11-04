package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"google.golang.org/protobuf/proto"
)

// A real unary Connect request over TLS/H2. No response-body read and no
// WINDOW_UPDATE can release the one-byte stream receive window.
func nha203UnreadHistoryH2(t *testing.T, ctx context.Context, base string, roots *x509.CertPool, procedure string, message proto.Message) *http2.ClientConn {
	t.Helper()
	transport := &http.Transport{HTTP2: &http.HTTP2Config{MaxReceiveBufferPerStream: 1}}
	h2, err := http2.ConfigureTransports(transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(transport.CloseIdleConnections)
	dialer := net.Dialer{Timeout: time.Second}
	connection, err := tls.DialWithDialer(&dialer, "tcp", strings.TrimPrefix(base, "https://"), &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if connection.ConnectionState().NegotiatedProtocol != "h2" {
		t.Fatal("no H2 negotiation")
	}
	wire := &nha147H2SettingsConn{Conn: connection}
	client, err := h2.NewClientConn(wire)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if actual, ok := wire.initialWindow(); !ok || actual != 1 {
		t.Fatal("receive window not actually advertised", actual, ok)
	}
	payload, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+procedure, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/proto")
	request.Header.Set("Connect-Protocol-Version", "1")
	request.Header.Set("Authorization", "Bearer nha164-synthetic")
	response, err := client.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if response.StatusCode != http.StatusOK || response.ProtoMajor != 2 || client.State().StreamsActive != 1 {
		t.Fatal("unary response did not remain in actual H2 send")
	}
	return client
}

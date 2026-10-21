package integrations

import (
	"context"
	"net"
	"net/http"
	"net/netip"

	"github.com/damione1/Homeward/internal/storage"
)

// Available only to this package's external tests. It changes DNS/dial, never
// URL admission, TLS verification, policy attempts, receipts or SQL selection.
func NHA101ConditionalFactoryForTest(settings ConditionalSettings, policy *CloudPolicy, lookup func(context.Context, string) ([]netip.Addr, error), dial func(context.Context, string, string) (net.Conn, error)) (ConditionalFactory, error) {
	f, e := NewConditionalFactory(settings, policy)
	if e != nil {
		return ConditionalFactory{}, e
	}
	f.network = conditionalNetwork{lookup: lookup, dial: dial}
	return f, nil
}

// Carrier-only probe for tests which need no migration23. It cannot produce a
// RemoteCalendarObservation/selection/receipt and is not a product cache path.
func NHA101ConditionalWireForTest(ctx context.Context, port ConditionalHTTP, attempt ConditionalAttempt, sent storage.RemoteCalendarValidators) (int, []byte, error) {
	c, ok := port.(*conditionalHTTP)
	if !ok || c == nil || !sent.Valid() || attempt.value == nil {
		return 0, nil, conditionalFailure(ConditionalInvalid)
	}
	c.mu.Lock()
	a := attempt.value
	if c.current != a || a.owner != c || a.used {
		c.mu.Unlock()
		return 0, nil, conditionalFailure(ConditionalInvalid)
	}
	a.used = true
	request := a.request
	request.header = request.header.Clone()
	if tag, ok := sent.ETag(); ok {
		request.header.Set("If-None-Match", tag)
	}
	if modified, ok := sent.LastModified(); ok {
		request.header.Set("If-Modified-Since", modified.Format(http.TimeFormat))
	}
	c.mu.Unlock()
	response, e := c.client.observe(ctx, request, cloudConditionalResponse)
	c.mu.Lock()
	a.request = CloudRequest{}
	if c.current == a {
		c.current = nil
	}
	c.mu.Unlock()
	return response.status, response.Bytes(), e
}

package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
)

const (
	cloudAuthorizationHeaderLimit   = 32 << 10
	cloudAuthorizationLocationLimit = 8 << 10
	cloudAuthorizationCookieLimit   = 16
	cloudAuthorizationCookieBytes   = 8 << 10
)

type cloudAuthorizationExchangeView struct{ attempt *CloudAuthorizationAttempt }

func (v cloudAuthorizationExchangeView) String() string {
	return "[REDACTED CLOUD AUTHORIZATION EXCHANGER]"
}
func (v cloudAuthorizationExchangeView) GoString() string { return v.String() }
func (v cloudAuthorizationExchangeView) MarshalJSON() ([]byte, error) {
	return json.Marshal(v.String())
}

func (v cloudAuthorizationExchangeView) Exchange(ctx context.Context, request CloudAuthorizationRequest) (CloudAuthorizationResponse, error) {
	if v.attempt == nil {
		return CloudAuthorizationResponse{}, &CloudError{kind: CloudInvalid}
	}
	return v.attempt.exchange(ctx, request)
}

type cloudAuthorizationCookieKey struct{ name, domain, path string }
type cloudAuthorizationJar struct {
	values map[cloudAuthorizationCookieKey]CloudAuthorizationCookie
}

func cloudAuthorizationDomainMatches(host, domain string) bool {
	return host == domain || strings.HasSuffix(host, "."+domain)
}
func cloudAuthorizationPathMatches(target, path string) bool {
	if target == path {
		return true
	}
	if !strings.HasPrefix(target, path) {
		return false
	}
	return strings.HasSuffix(path, "/") || (len(target) > len(path) && target[len(path)] == '/')
}
func cloudAuthorizationDefaultPath(path string) string {
	if path == "" || path[0] != '/' {
		return "/"
	}
	index := strings.LastIndex(path, "/")
	if index <= 0 {
		return "/"
	}
	return path[:index]
}

func cloudAuthorizationHTTPSURL(parsed *url.URL, raw string) bool {
	if parsed == nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil ||
		strings.Contains(raw, "#") || strings.ContainsAny(parsed.Hostname(), " ,;\\\t\r\n") {
		return false
	}
	if port := parsed.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	return true
}

func (j *cloudAuthorizationJar) header(target *url.URL) (string, error) {
	if j == nil || j.values == nil || target == nil {
		return "", &CloudError{kind: CloudInvalid}
	}
	host := strings.ToLower(target.Hostname())
	targetPath := target.EscapedPath()
	if targetPath == "" {
		targetPath = "/"
	}
	now := time.Now()
	matches := make([]CloudAuthorizationCookie, 0, len(j.values))
	for key, cookie := range j.values {
		if !cookie.expires.IsZero() && !cookie.expires.After(now) {
			delete(j.values, key)
			continue
		}
		if cookie.hostOnly && host != cookie.domain {
			continue
		}
		if !cookie.hostOnly && !cloudAuthorizationDomainMatches(host, cookie.domain) {
			continue
		}
		if !cloudAuthorizationPathMatches(targetPath, cookie.path) {
			continue
		}
		if cookie.secure && target.Scheme != "https" {
			continue
		}
		matches = append(matches, cookie)
	}
	sort.Slice(matches, func(i, k int) bool {
		if len(matches[i].path) != len(matches[k].path) {
			return len(matches[i].path) > len(matches[k].path)
		}
		if matches[i].name != matches[k].name {
			return matches[i].name < matches[k].name
		}
		return matches[i].domain < matches[k].domain
	})
	parts := make([]string, 0, len(matches))
	for _, cookie := range matches {
		parts = append(parts, cookie.name+"="+cookie.value)
	}
	joined := strings.Join(parts, "; ")
	if len(joined) > cloudAuthorizationCookieBytes {
		return "", &CloudError{kind: CloudResponseTooLarge}
	}
	return joined, nil
}

func cloudAuthorizationHeaderSize(h http.Header) (int, error) {
	size := 0
	for name, values := range h {
		if name == "" || strings.ContainsAny(name, "\r\n:") {
			return 0, &CloudError{kind: CloudInvalid}
		}
		for _, value := range values {
			if strings.ContainsAny(value, "\r\n") {
				return 0, &CloudError{kind: CloudInvalid}
			}
			size += len(name) + len(value) + 4
			if size > cloudAuthorizationHeaderLimit {
				return 0, &CloudError{kind: CloudResponseTooLarge}
			}
		}
	}
	return size, nil
}

func cloudAuthorizationLocation(status int, h http.Header) (string, error) {
	values := h.Values("Location")
	if len(values) > 1 {
		return "", &CloudError{kind: CloudInvalid}
	}
	if status == http.StatusFound && len(values) != 1 {
		return "", &CloudError{kind: CloudInvalid}
	}
	if len(values) == 0 {
		return "", nil
	}
	raw := values[0]
	if len(raw) == 0 || len(raw) > cloudAuthorizationLocationLimit {
		return "", &CloudError{kind: CloudResponseTooLarge}
	}
	if strings.ContainsAny(raw, "\r\n\x00") {
		return "", &CloudError{kind: CloudInvalid}
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return "", &CloudError{kind: CloudInvalid}
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil || !cloudAuthorizationHTTPSURL(parsed, raw) || !parsed.IsAbs() {
		return "", &CloudError{kind: CloudInvalid}
	}
	return raw, nil
}

func cloudAuthorizationCookies(h http.Header, source *url.URL) ([]CloudAuthorizationCookie, error) {
	raws := h.Values("Set-Cookie")
	if len(raws) > cloudAuthorizationCookieLimit {
		return nil, &CloudError{kind: CloudResponseTooLarge}
	}
	size := 0
	host := strings.ToLower(source.Hostname())
	now := time.Now()
	result := make([]CloudAuthorizationCookie, 0, len(raws))
	for _, raw := range raws {
		size += len(raw)
		if size > cloudAuthorizationCookieBytes {
			return nil, &CloudError{kind: CloudResponseTooLarge}
		}
		c, err := http.ParseSetCookie(raw)
		if err != nil || c == nil || len(c.Unparsed) != 0 || c.Name == "" || len(c.Name) > 256 || len(c.Value) > 4096 ||
			strings.ContainsAny(c.Name, "\r\n;= ") || strings.ContainsAny(c.Value, "\r\n;") {
			return nil, &CloudError{kind: CloudInvalid}
		}
		domain := host
		hostOnly := true
		if c.Domain != "" {
			domain = strings.ToLower(strings.TrimPrefix(c.Domain, "."))
			if domain == "" || net.ParseIP(host) != nil || !cloudAuthorizationDomainMatches(host, domain) {
				return nil, &CloudError{kind: CloudInvalid}
			}
			suffix, _ := publicsuffix.PublicSuffix(domain)
			if suffix == domain {
				return nil, &CloudError{kind: CloudInvalid}
			}
			hostOnly = false
		}
		path := c.Path
		if path == "" {
			path = cloudAuthorizationDefaultPath(source.EscapedPath())
		}
		if !strings.HasPrefix(path, "/") {
			return nil, &CloudError{kind: CloudInvalid}
		}
		expires := c.Expires
		if c.MaxAge > 0 {
			if int64(c.MaxAge) > int64(1<<63-1)/int64(time.Second) {
				return nil, &CloudError{kind: CloudInvalid}
			}
			expires = now.Add(time.Duration(c.MaxAge) * time.Second)
		}
		deleted := c.MaxAge < 0 || (!expires.IsZero() && !expires.After(now))
		result = append(result, CloudAuthorizationCookie{
			name: c.Name, value: c.Value, domain: domain, path: path, sourceHost: host,
			secure: c.Secure, hostOnly: hostOnly, deleted: deleted, expires: expires, maxAge: c.MaxAge,
		})
	}
	return result, nil
}
func (j *cloudAuthorizationJar) apply(cookies []CloudAuthorizationCookie) error {
	next := make(map[cloudAuthorizationCookieKey]CloudAuthorizationCookie, len(j.values)+len(cookies))
	for key, cookie := range j.values {
		next[key] = cookie
	}
	for _, cookie := range cookies {
		key := cloudAuthorizationCookieKey{cookie.name, cookie.domain, cookie.path}
		if cookie.deleted {
			delete(next, key)
		} else {
			next[key] = cookie
		}
	}
	if len(next) > cloudAuthorizationCookieLimit {
		return &CloudError{kind: CloudResponseTooLarge}
	}
	j.values = next
	return nil
}

func (a *CloudAuthorizationAttempt) exchange(ctx context.Context, request CloudAuthorizationRequest) (CloudAuthorizationResponse, error) {
	if a == nil || a.owner == nil || nilInterface(ctx) || request.id.value == "" || !request.expected.valid() {
		return CloudAuthorizationResponse{}, &CloudError{kind: CloudInvalid}
	}
	c := a.owner.client
	if !c.valid() || !c.requestAllowed(request.request) || request.request.url.Scheme != "https" ||
		!cloudAuthorizationHTTPSURL(&request.request.url, request.request.url.String()) ||
		(request.request.method != http.MethodGet && request.request.method != http.MethodPost) {
		return CloudAuthorizationResponse{}, &CloudError{kind: CloudInvalid}
	}
	if err := a.beginExchange(request.id); err != nil {
		return CloudAuthorizationResponse{}, err
	}
	defer a.finishExchange()
	if ctx.Err() != nil || a.ctx.Err() != nil {
		return CloudAuthorizationResponse{}, &CloudError{kind: CloudBudgetExhausted}
	}
	joined, cancel := context.WithCancel(ctx)
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-a.ctx.Done():
			cancel()
		case <-c.session.Done():
			cancel()
		case <-joined.Done():
		}
	}()
	defer func() { cancel(); <-watcherDone }()
	exchangeCtx, deadlineCancel := context.WithTimeout(joined, c.policy.limits.value.AttemptTimeout)
	defer deadlineCancel()
	release, err := c.policy.acquire(exchangeCtx, c.session.Done(), c.scope)
	if err != nil {
		return CloudAuthorizationResponse{}, err
	}
	defer release()
	if !c.live() || a.ctx.Err() != nil {
		return CloudAuthorizationResponse{}, &CloudError{kind: CloudSessionInactive}
	}
	var body io.Reader
	if len(request.request.body) != 0 {
		body = bytes.NewReader(request.request.body)
	}
	wire, err := http.NewRequestWithContext(exchangeCtx, request.request.method, request.request.url.String(), body)
	if err != nil {
		return CloudAuthorizationResponse{}, &CloudError{kind: CloudInvalid}
	}
	wire.Header = request.request.header.Clone()
	cookies, err := a.jar.header(&request.request.url)
	if err != nil {
		return CloudAuthorizationResponse{}, err
	}
	if cookies != "" {
		wire.Header.Set("Cookie", cookies)
	}
	if _, err := cloudAuthorizationHeaderSize(wire.Header); err != nil {
		return CloudAuthorizationResponse{}, err
	}
	if !c.live() || exchangeCtx.Err() != nil || a.ctx.Err() != nil {
		return CloudAuthorizationResponse{}, &CloudError{kind: CloudSessionInactive}
	}
	if c.admission != nil {
		retryAt, reserveErr := c.admission.ReserveAttempt(exchangeCtx)
		if reserveErr != nil {
			return CloudAuthorizationResponse{}, &CloudError{kind: CloudQuotaStoreUnavailable}
		}
		if !retryAt.IsZero() {
			return CloudAuthorizationResponse{}, &CloudError{kind: CloudLocalQuotaExhausted, retryAt: retryAt}
		}
		if !c.live() || exchangeCtx.Err() != nil || a.ctx.Err() != nil {
			return CloudAuthorizationResponse{}, &CloudError{kind: CloudSessionInactive}
		}
	}
	response, err := c.http.Do(wire)
	if err != nil {
		return CloudAuthorizationResponse{}, &CloudError{kind: CloudTransportFailed}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		now := c.policy.clock.Now()
		until, valid := parseCloudRetryAfter(response.Header.Get("Retry-After"), now)
		floor := now.Add(c.policy.limits.value.BackoffBase)
		if until.Before(floor) {
			until = floor
		}
		c.policy.deferScope(c.scope, until)
		return CloudAuthorizationResponse{}, &CloudError{kind: CloudQuotaDeferred, retryAt: until, retryable: valid}
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return CloudAuthorizationResponse{}, &CloudError{kind: CloudCredentialFailed}
	}
	if !request.expected.accepts(response.StatusCode) {
		return CloudAuthorizationResponse{}, &CloudError{kind: CloudHTTPFailed}
	}
	if _, err := cloudAuthorizationHeaderSize(response.Header); err != nil {
		return CloudAuthorizationResponse{}, err
	}
	location, err := cloudAuthorizationLocation(response.StatusCode, response.Header)
	if err != nil {
		return CloudAuthorizationResponse{}, err
	}
	snapshot, err := cloudAuthorizationCookies(response.Header, &request.request.url)
	if err != nil {
		return CloudAuthorizationResponse{}, err
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, c.policy.limits.value.ResponseBytes+1))
	if err != nil {
		return CloudAuthorizationResponse{}, &CloudError{kind: CloudTransportFailed}
	}
	if int64(len(content)) > c.policy.limits.value.ResponseBytes {
		return CloudAuthorizationResponse{}, &CloudError{kind: CloudResponseTooLarge}
	}
	if exchangeCtx.Err() != nil || a.ctx.Err() != nil {
		return CloudAuthorizationResponse{}, &CloudError{kind: CloudTransportFailed}
	}
	if !c.live() {
		return CloudAuthorizationResponse{}, &CloudError{kind: CloudSessionInactive}
	}
	if err := a.jar.apply(snapshot); err != nil {
		return CloudAuthorizationResponse{}, err
	}
	return CloudAuthorizationResponse{status: response.StatusCode, body: content, location: location, cookies: snapshot}, nil
}

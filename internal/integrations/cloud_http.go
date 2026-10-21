package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CloudRequest contains owned copies. Method is transport syntax only: it
// never classifies an action as safe to retry.
type CloudRequest struct {
	method string
	url    url.URL
	header http.Header
	body   []byte
}

func NewCloudRequest(method, rawURL string, header http.Header, body []byte) (CloudRequest, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || method == "" || len(body) > 4<<20 {
		return CloudRequest{}, &CloudError{kind: CloudInvalid}
	}
	probe, err := http.NewRequestWithContext(context.Background(), method, parsed.String(), nil)
	if err != nil || probe.Method != method {
		return CloudRequest{}, &CloudError{kind: CloudInvalid}
	}
	copyHeader := make(http.Header, len(header))
	for name, values := range header {
		if name == "" || strings.ContainsAny(name, "\r\n:") {
			return CloudRequest{}, &CloudError{kind: CloudInvalid}
		}
		copyHeader[name] = append([]string(nil), values...)
		for _, value := range values {
			if strings.ContainsAny(value, "\r\n") {
				return CloudRequest{}, &CloudError{kind: CloudInvalid}
			}
		}
	}
	return CloudRequest{method: method, url: *parsed, header: copyHeader, body: append([]byte(nil), body...)}, nil
}

func (r CloudRequest) String() string               { return "[REDACTED CLOUD REQUEST]" }
func (r CloudRequest) GoString() string             { return r.String() }
func (r CloudRequest) MarshalJSON() ([]byte, error) { return json.Marshal(r.String()) }

type CloudResponse struct {
	status      int
	header      http.Header // private bounded conditional response projection
	body        []byte
	cookies     CloudCookies
	owner       *CloudClient
	observation bool
	healthEpoch uint64
}

func (r CloudResponse) StatusCode() int       { return r.status }
func (r CloudResponse) Bytes() []byte         { return append([]byte(nil), r.body...) }
func (r CloudResponse) Cookies() CloudCookies { return r.cookies.clone() }
func (r CloudResponse) String() string        { return "[REDACTED CLOUD RESPONSE]" }
func (r CloudResponse) GoString() string      { return r.String() }
func (r CloudResponse) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.String())
}

// CloudCookies is an owned, bounded snapshot. Its diagnostic paths never
// expose session material; CookieHeader is an explicit secret-handling step.
type CloudCookies struct{ values []http.Cookie }

func (c CloudCookies) clone() CloudCookies {
	return CloudCookies{values: append([]http.Cookie(nil), c.values...)}
}
func (c CloudCookies) String() string               { return "[REDACTED CLOUD COOKIES]" }
func (c CloudCookies) GoString() string             { return c.String() }
func (c CloudCookies) MarshalJSON() ([]byte, error) { return json.Marshal(c.String()) }
func (c CloudCookies) CookieHeader() (string, error) {
	if len(c.values) == 0 {
		return "", &CloudError{kind: CloudInvalid}
	}
	return c.MergeHeader("")
}

// MergeHeader follows the upstream client cookie-jar rule for one account
// while keeping the owned snapshot bounded and diagnostics redacted.
func (c CloudCookies) MergeHeader(previous string) (string, error) {
	if len(c.values) == 0 {
		return previous, nil
	}
	values := make(map[string]string)
	for _, part := range strings.Split(previous, "; ") {
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		if !ok || name == "" {
			return "", &CloudError{kind: CloudInvalid}
		}
		values[name] = value
	}
	for _, cookie := range c.values {
		values[cookie.Name] = cookie.Value
	}
	if len(values) > 16 {
		return "", &CloudError{kind: CloudResponseTooLarge}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+values[name])
	}
	joined := strings.Join(parts, "; ")
	if len(joined) > 8192 {
		return "", &CloudError{kind: CloudResponseTooLarge}
	}
	return joined, nil
}

func snapshotCloudCookies(response *http.Response) (CloudCookies, error) {
	values := response.Cookies()
	if len(values) > 16 {
		return CloudCookies{}, &CloudError{kind: CloudResponseTooLarge}
	}
	owned := make([]http.Cookie, 0, len(values))
	for _, cookie := range values {
		if cookie.Name == "" || len(cookie.Name) > 256 || len(cookie.Value) > 4096 || strings.ContainsAny(cookie.Name, "\r\n;= ") || strings.ContainsAny(cookie.Value, "\r\n;") {
			return CloudCookies{}, &CloudError{kind: CloudInvalid}
		}
		owned = append(owned, http.Cookie{Name: cookie.Name, Value: cookie.Value})
	}
	return CloudCookies{values: owned}, nil
}

// CloudAttemptAdmission is trusted composition's durable fence. A nonzero
// retry time denies the attempt locally; an error denies it as unavailable.
// It runs before every HTTP attempt, including retries and login exchanges.
type CloudAttemptAdmission interface {
	ReserveAttempt(context.Context) (time.Time, error)
}

// CloudClient is a session-bound view of a shared policy. It neither owns
// pilot restarts nor a command ledger. A single policy must be shared by all
// pilots of a configured account to enforce their common quota.
type CloudClient struct {
	session        *Session
	secrets        *sessionSecretProvider
	policy         *CloudPolicy
	scope          *cloudScopeState
	http           *http.Client
	healthMu       sync.Mutex
	degraded       bool
	healthEpoch    uint64
	admission      CloudAttemptAdmission
	streamMu       sync.Mutex
	streamWG       sync.WaitGroup
	streamsClosed  chan struct{}
	streamsClosing bool
}

func (c *CloudClient) String() string   { return "[REDACTED CLOUD CLIENT]" }
func (c *CloudClient) GoString() string { return c.String() }
func (c *CloudClient) MarshalJSON() ([]byte, error) {
	return json.Marshal(c.String())
}

func NewCloudClient(host SessionHost, policy *CloudPolicy, transport *http.Transport, admission ...CloudAttemptAdmission) (*CloudClient, error) {
	if len(admission) > 1 || (len(admission) == 1 && nilInterface(admission[0])) {
		return nil, &CloudError{kind: CloudInvalid}
	}
	session, ok := host.(*Session)
	if !ok || session == nil || session.host == nil || policy == nil || policy.states == nil || (transport != nil && nilInterface(transport)) {
		return nil, &CloudError{kind: CloudInvalid}
	}
	provider, err := session.Secrets()
	if err != nil {
		return nil, &CloudError{kind: CloudSessionInactive}
	}
	secrets, ok := provider.(*sessionSecretProvider)
	if !ok {
		return nil, &CloudError{kind: CloudInvalid}
	}
	_, state, err := policy.scope(session.instance)
	if err != nil {
		return nil, err
	}
	if transport == nil {
		base, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, &CloudError{kind: CloudInvalid}
		}
		transport = base
	}
	ownedTransport := transport.Clone()
	// Authorization uses this same carrier. Bound raw response headers before
	// net/http allocates them; preserve a stricter injected transport limit.
	if ownedTransport.MaxResponseHeaderBytes <= 0 || ownedTransport.MaxResponseHeaderBytes > cloudAuthorizationHeaderLimit {
		ownedTransport.MaxResponseHeaderBytes = cloudAuthorizationHeaderLimit
	}
	// net/http can replay an idempotent-looking request on a reused connection.
	// Method and headers do not grant that authority to a cloud action, so use
	// fresh connections for every policy attempt, including GET actions.
	ownedTransport.DisableKeepAlives = true
	client := &http.Client{Transport: ownedTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	result := &CloudClient{session: session, secrets: secrets, policy: policy, scope: state, http: client, streamsClosed: make(chan struct{})}
	if len(admission) == 1 {
		result.admission = admission[0]
	}
	return result, nil
}

func (c *CloudClient) valid() bool {
	return c != nil && c.session != nil && c.secrets != nil && c.policy != nil && c.scope != nil && c.http != nil
}

// Close cancels and joins owned observation streams, then releases idle
// connections. Finite requests retain their existing context/session fence.
func (c *CloudClient) Close() {
	if c != nil && c.http != nil {
		c.closeObservationStreams()
		c.http.CloseIdleConnections()
	}
}

func (c *CloudClient) live() bool {
	if !c.valid() {
		return false
	}
	h := c.session.host
	h.mu.Lock()
	live := c.session.live() && h.admission == admissionOpen
	h.mu.Unlock()
	return live
}

func (c *CloudClient) requestAllowed(request CloudRequest) bool {
	return request.method != "" && request.url.Host != "" && (request.url.Scheme == "http" || request.url.Scheme == "https") && request.url.User == nil && request.url.Fragment == "" && len(request.body) <= int(c.policy.limits.value.RequestBytes)
}

// Observe may retry only read observations. A successful HTTP response does
// not mark the integration ready: the pilot must first admit the decoded
// observation through Host, then explicitly call ConfirmObservationAdmitted.
type cloudResponseMode uint8

const (
	cloudOrdinaryResponse cloudResponseMode = iota + 1
	cloudConditionalResponse
)

func (c *CloudClient) Observe(ctx context.Context, request CloudRequest) (CloudResponse, error) {
	return c.observe(ctx, request, cloudOrdinaryResponse)
}
func (c *CloudClient) observe(ctx context.Context, request CloudRequest, mode cloudResponseMode) (CloudResponse, error) {
	if mode != cloudOrdinaryResponse && mode != cloudConditionalResponse {
		return CloudResponse{}, &CloudError{kind: CloudInvalid}
	}
	if !c.valid() || ctx == nil || !c.requestAllowed(request) {
		return CloudResponse{}, &CloudError{kind: CloudInvalid}
	}
	if !c.live() {
		return CloudResponse{}, &CloudError{kind: CloudSessionInactive}
	}
	budget, cancel := context.WithTimeout(ctx, c.policy.limits.value.TotalTimeout)
	defer cancel()
	// The context deadline uses the process clock while Retry-After uses the
	// injected cloud clock. Compare them as a duration anchored once in the
	// same clock domain, including an earlier caller deadline.
	deadline, _ := budget.Deadline()
	remainingBudget := time.Until(deadline)
	if remainingBudget < 0 {
		remainingBudget = 0
	}
	policyDeadline := c.policy.clock.Now().Add(remainingBudget)
	for attempt := 0; attempt < c.policy.limits.value.ObservationAttempts; attempt++ {
		c.healthMu.Lock()
		healthEpoch := c.healthEpoch
		c.healthMu.Unlock()
		response, retryAfter, err := c.sendMode(budget, request, nil, nil, mode)
		if err == nil {
			response.owner = c
			response.observation = true
			response.healthEpoch = healthEpoch
			return response, nil
		}
		cloudErr, ok := err.(*CloudError)
		if !ok {
			return CloudResponse{}, &CloudError{kind: CloudTransportFailed}
		}
		c.degrade(cloudErr.kind)
		if !cloudErr.retryable {
			return CloudResponse{}, cloudErr
		}
		if attempt+1 >= c.policy.limits.value.ObservationAttempts {
			return CloudResponse{}, cloudErr
		}
		wait, waitErr := c.backoff(attempt)
		if waitErr != nil {
			c.degrade(CloudInvalid)
			return CloudResponse{}, waitErr
		}
		until := c.policy.clock.Now().Add(wait)
		if retryAfter.After(until) {
			until = retryAfter
		}
		if until.After(policyDeadline) {
			c.degrade(cloudErr.kind)
			return CloudResponse{}, &CloudError{kind: CloudQuotaDeferred, retryAt: until}
		}
		if waitErr := c.waitUntil(budget, until); waitErr != nil {
			c.degrade(CloudBudgetExhausted)
			return CloudResponse{}, waitErr
		}
	}
	return CloudResponse{}, &CloudError{kind: CloudBudgetExhausted}
}

// Act accepts only a real router-admitted command owned by this session. It
// performs one transport attempt irrespective of the HTTP method or response.
func (c *CloudClient) Act(ctx context.Context, command AdmittedCommand, request CloudRequest) (CloudResponse, error) {
	if !c.valid() || ctx == nil || !c.requestAllowed(request) || command.binding.instance != c.session.instance || command.binding.session != c.session.id {
		return CloudResponse{}, &CloudError{kind: CloudInvalid}
	}
	if _, _, err := command.RouteIdentity(); err != nil {
		return CloudResponse{}, &CloudError{kind: CloudInvalid}
	}
	budget, cancel := context.WithTimeout(ctx, c.policy.limits.value.TotalTimeout)
	defer cancel()
	response, _, err := c.send(budget, request, command.ReadyForTransport, nil)
	if err != nil {
		if failure, ok := err.(*CloudError); ok {
			c.degrade(failure.kind)
		}
		return CloudResponse{}, err
	}
	return response, nil
}

func (c *CloudClient) send(ctx context.Context, request CloudRequest, beforeTransport func() bool, entered *bool) (CloudResponse, time.Time, error) {
	return c.sendMode(ctx, request, beforeTransport, entered, cloudOrdinaryResponse)
}
func (c *CloudClient) sendMode(ctx context.Context, request CloudRequest, beforeTransport func() bool, entered *bool, mode cloudResponseMode) (CloudResponse, time.Time, error) {
	if mode != cloudOrdinaryResponse && mode != cloudConditionalResponse {
		return CloudResponse{}, time.Time{}, &CloudError{kind: CloudInvalid}
	}
	release, err := c.policy.acquire(ctx, c.session.Done(), c.scope)
	if err != nil {
		return CloudResponse{}, time.Time{}, err
	}
	defer release()
	if !c.live() || (beforeTransport != nil && !beforeTransport()) {
		return CloudResponse{}, time.Time{}, &CloudError{kind: CloudSessionInactive}
	}
	attempt, cancel := context.WithTimeout(ctx, c.policy.limits.value.AttemptTimeout)
	joined := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-c.session.Done():
			cancel()
		case <-joined:
		}
	}()
	defer func() { close(joined); cancel(); <-watcherDone }()
	var body io.Reader
	if len(request.body) != 0 {
		body = bytes.NewReader(request.body)
	}
	wire, err := http.NewRequestWithContext(attempt, request.method, request.url.String(), body)
	if err != nil {
		return CloudResponse{}, time.Time{}, &CloudError{kind: CloudInvalid}
	}
	wire.Header = request.header.Clone()
	// This is the last admission fence before an external transport may see an
	// action. A later timeout cannot prove that the server had no effect.
	if !c.live() || (beforeTransport != nil && !beforeTransport()) {
		return CloudResponse{}, time.Time{}, &CloudError{kind: CloudSessionInactive}
	}
	if c.admission != nil {
		retryAt, reserveErr := c.admission.ReserveAttempt(attempt)
		if reserveErr != nil {
			return CloudResponse{}, time.Time{}, &CloudError{kind: CloudQuotaStoreUnavailable}
		}
		if !retryAt.IsZero() {
			return CloudResponse{}, time.Time{}, &CloudError{kind: CloudLocalQuotaExhausted, retryAt: retryAt}
		}
		if attempt.Err() != nil || !c.live() || (beforeTransport != nil && !beforeTransport()) {
			return CloudResponse{}, time.Time{}, &CloudError{kind: CloudSessionInactive}
		}
	}
	if entered != nil {
		*entered = true
	}
	response, err := c.http.Do(wire)
	if err != nil {
		return CloudResponse{}, time.Time{}, &CloudError{kind: CloudTransportFailed, retryable: true}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		// A 429's headers govern the next attempt even when its body is slow,
		// malformed, oversized, or never delivered. Closing the body is enough.
		now := c.policy.clock.Now()
		until, valid := parseCloudRetryAfter(response.Header.Get("Retry-After"), now)
		localFloor := now.Add(c.policy.limits.value.BackoffBase)
		if until.Before(localFloor) {
			until = localFloor
		}
		c.policy.deferScope(c.scope, until)
		return CloudResponse{}, until, &CloudError{kind: CloudQuotaDeferred, retryAt: until, retryable: valid}
	}
	if mode == cloudConditionalResponse {
		if response.StatusCode == http.StatusNotModified {
			if attempt.Err() != nil || !c.live() {
				return CloudResponse{}, time.Time{}, &CloudError{kind: CloudSessionInactive}
			}
			return CloudResponse{status: response.StatusCode, header: conditionalResponseHeaders(response.Header)}, time.Time{}, nil
		}
		if response.StatusCode == http.StatusPartialContent {
			return CloudResponse{}, time.Time{}, &CloudError{kind: CloudHTTPFailed}
		}
	}
	responseLimit := c.policy.limits.value.ResponseBytes
	if mode == cloudConditionalResponse && responseLimit > 1<<20 {
		responseLimit = 1 << 20
	}
	limited := io.LimitReader(response.Body, responseLimit+1)
	content, err := io.ReadAll(limited)
	if err != nil {
		return CloudResponse{}, time.Time{}, &CloudError{kind: CloudTransportFailed, retryable: true}
	}
	if int64(len(content)) > responseLimit {
		return CloudResponse{}, time.Time{}, &CloudError{kind: CloudResponseTooLarge}
	}
	if err := attempt.Err(); err != nil {
		return CloudResponse{}, time.Time{}, &CloudError{kind: CloudTransportFailed, retryable: true}
	}
	if !c.live() {
		return CloudResponse{}, time.Time{}, &CloudError{kind: CloudSessionInactive}
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return CloudResponse{}, time.Time{}, &CloudError{kind: CloudCredentialFailed, responseBody: content}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return CloudResponse{}, time.Time{}, &CloudError{kind: CloudHTTPFailed, retryable: response.StatusCode >= 500 && response.StatusCode <= 599, responseBody: content}
	}
	if mode == cloudConditionalResponse {
		if response.StatusCode != http.StatusOK {
			return CloudResponse{}, time.Time{}, &CloudError{kind: CloudHTTPFailed}
		}
		return CloudResponse{status: response.StatusCode, body: content, header: conditionalResponseHeaders(response.Header)}, time.Time{}, nil
	}
	cookies, err := snapshotCloudCookies(response)
	if err != nil {
		return CloudResponse{}, time.Time{}, err
	}
	return CloudResponse{status: response.StatusCode, body: content, cookies: cookies}, time.Time{}, nil
}

func parseCloudRetryAfter(raw string, now time.Time) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	for _, char := range raw {
		if char < '0' || char > '9' {
			date, err := http.ParseTime(raw)
			if err != nil {
				return time.Time{}, false
			}
			if date.Before(now) {
				return now, true
			}
			return date, true
		}
	}
	seconds, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || seconds > uint64((1<<63-1)/int64(time.Second)) {
		return time.Time{}, false
	}
	return now.Add(time.Duration(seconds) * time.Second), true
}

func (c *CloudClient) backoff(attempt int) (time.Duration, error) {
	wait := c.policy.limits.value.BackoffBase
	for i := 0; i < attempt && wait < c.policy.limits.value.BackoffMaximum; i++ {
		if wait > c.policy.limits.value.BackoffMaximum/2 {
			wait = c.policy.limits.value.BackoffMaximum
		} else {
			wait *= 2
		}
	}
	limit := wait / 2
	offset := c.policy.jitter.Offset(limit)
	if offset < 0 || offset > limit {
		return 0, &CloudError{kind: CloudInvalid}
	}
	if wait > c.policy.limits.value.BackoffMaximum-offset {
		return c.policy.limits.value.BackoffMaximum, nil
	}
	return wait + offset, nil
}

func (c *CloudClient) waitUntil(ctx context.Context, until time.Time) error {
	for {
		if err := ctx.Err(); err != nil {
			return &CloudError{kind: CloudBudgetExhausted}
		}
		if !c.live() {
			return &CloudError{kind: CloudSessionInactive}
		}
		remaining := until.Sub(c.policy.clock.Now())
		if remaining <= 0 {
			return nil
		}
		timer := c.policy.timer.NewTimer(remaining)
		if nilInterface(timer) || timer.C() == nil {
			return &CloudError{kind: CloudInvalid}
		}
		select {
		case <-ctx.Done():
		case <-c.session.Done():
		case <-timer.C():
		}
		timer.Stop()
	}
}

func (c *CloudClient) degrade(kind CloudFailureKind) {
	if !c.valid() || kind == CloudSessionInactive || kind == CloudInvalid {
		return
	}
	c.healthMu.Lock()
	defer c.healthMu.Unlock()
	c.degradeLocked(kind)
}

// The shared epoch is advanced while healthMu excludes receipt admission.
// Callers must release any stream/network locks before reporting to Host.
func (c *CloudClient) degradeLocked(kind CloudFailureKind) {
	if kind == CloudSessionInactive || kind == CloudInvalid {
		return
	}
	c.healthEpoch++
	if c.degraded {
		return
	}
	report, _ := NewHealthReport(HealthDegraded, "cloud request or credential unavailable")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.session.ReportHealth(ctx, report); err == nil {
		c.degraded = true
	}
}

// ConfirmObservationAdmitted is an explicit consumer decision after a decoded
// observation was accepted by Host and the pilot confirmed no other fault is
// keeping its integration degraded. HTTP success alone never calls it.
func (c *CloudClient) ConfirmObservationAdmitted(ctx context.Context, response CloudResponse) error {
	if !c.valid() || ctx == nil || response.owner != c || !response.observation || response.status < 200 || response.status >= 300 {
		return &CloudError{kind: CloudInvalid}
	}
	return c.confirmObservationEpoch(ctx, response.healthEpoch)
}

// pendingObservationEpoch preserves the current receipt generation. This is
// called only by conditionalHTTP after checking its private observation.
func (c *CloudClient) pendingObservationEpoch(ctx context.Context, epoch uint64) error {
	c.healthMu.Lock()
	defer c.healthMu.Unlock()
	if epoch != c.healthEpoch {
		return &CloudError{kind: CloudInvalid}
	}
	if c.degraded {
		return nil
	}
	report, _ := NewHealthReport(HealthDegraded, "calendar publication pending")
	if err := c.session.ReportHealth(ctx, report); err != nil {
		return &CloudError{kind: CloudSessionInactive}
	}
	c.degraded = true
	return nil
}

func (c *CloudClient) confirmObservationEpoch(ctx context.Context, epoch uint64) error {
	c.healthMu.Lock()
	defer c.healthMu.Unlock()
	if epoch != c.healthEpoch {
		return &CloudError{kind: CloudInvalid}
	}
	if !c.degraded {
		return nil
	}
	report, _ := NewHealthReport(HealthReady, "")
	if err := c.session.ReportHealth(ctx, report); err != nil {
		return &CloudError{kind: CloudSessionInactive}
	}
	c.degraded = false
	return nil
}

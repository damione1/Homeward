package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
)

var (
	ErrInvalidPublicPlan   = errors.New("server: invalid public transport plan")
	ErrPublicPlanFrozen    = errors.New("server: public transport plan is frozen")
	ErrPublicRouteConflict = errors.New("server: public route conflict")
)

// A handler whose work deadline expires still needs a small bounded window to
// write a structured HTTP/Connect/gRPC error. This also bounds a blocked Write
// when the client has stopped consuming HTTP/2 flow-control credit.
const publicErrorWriteGrace = 100 * time.Millisecond

type RouteKind uint8

const (
	RouteKindUnspecified RouteKind = iota
	RouteKindConnect
	RouteKindMCP
	RouteKindMedia
)

type RouteMethod uint8

const (
	RouteMethodUnspecified RouteMethod = iota
	RouteMethodGet
	RouteMethodPost
)

func (m RouteMethod) httpMethod() string {
	switch m {
	case RouteMethodGet:
		return http.MethodGet
	case RouteMethodPost:
		return http.MethodPost
	default:
		return ""
	}
}

type RouteAccess uint8

const (
	RouteAccessUnspecified RouteAccess = iota
	RouteAccessAnonymous
	RouteAccessProtected
)

type RouteFlow uint8

const (
	RouteFlowUnspecified RouteFlow = iota
	RouteFlowUnary
	RouteFlowServerStream
)

// RouteTransport is a closed admission rule for the actual server connection.
// URL metadata and proxy headers never satisfy RouteTransportTLSRequired.
type RouteTransport uint8

const (
	RouteTransportUnspecified RouteTransport = iota
	RouteTransportOptionalTLS
	RouteTransportTLSRequired
)

// RouteKey is a transport identity, not a business permission. Authorization
// providers own the later mapping from an admitted route to actual rights.
type RouteKey struct {
	kind   RouteKind
	method RouteMethod
	path   string
}

func (k RouteKey) Kind() RouteKind     { return k.kind }
func (k RouteKey) Method() RouteMethod { return k.method }
func (k RouteKey) Path() string        { return k.path }

type AuditOutcome uint8

const (
	AuditOutcomeUnspecified AuditOutcome = iota
	AuditOutcomeAdmitted
	AuditOutcomeDenied
)

// AuditEvent deliberately has no request headers, body, token, URL query, or
// provider error. Actor is absent until an identity has been admitted.
type AuditEvent struct {
	Route    RouteKey
	Actor    domain.ActorID
	HasActor bool
	Outcome  AuditOutcome
}

type Authenticator interface {
	// Authenticate must return when ctx is canceled. It admits an identity,
	// never rights inferred from request headers or a proxy address.
	Authenticate(context.Context, *http.Request) (domain.ActorID, error)
}

type Authorizer interface {
	// Authorize must return when ctx is canceled.
	Authorize(context.Context, domain.ActorID, RouteKey) error
}

type Auditor interface {
	// Record must return when ctx is canceled and must not add request secrets.
	Record(context.Context, AuditEvent) error
}

type publicRoute struct {
	key       RouteKey
	access    RouteAccess
	flow      RouteFlow
	transport RouteTransport
	handler   http.Handler
}

// ConnectProcedure is an exact generated procedure, with explicit admission
// and flow policy. Unlisted methods on a generated service stay unreachable.
type ConnectProcedure struct {
	Path      string
	Access    RouteAccess
	Flow      RouteFlow
	Transport RouteTransport
}

type ConnectHandlerBuilder func(...connect.HandlerOption) (string, http.Handler)

type connectRegistration struct {
	build      ConnectHandlerBuilder
	procedures []ConnectProcedure
}

type routeLookup struct {
	method string
	path   string
}

// PublicPlan is a trusted composition input. Register closes before serving;
// the compiled route table and provider references cannot be replaced later.
type PublicPlan struct {
	mu            sync.Mutex
	valid         bool
	frozen        bool
	authenticator Authenticator
	authorizer    Authorizer
	auditor       Auditor
	routes        map[routeLookup]publicRoute
	connect       []connectRegistration
}

func NewPublicPlan(authenticator Authenticator, authorizer Authorizer, auditor Auditor) (*PublicPlan, error) {
	if typedNilPublic(authenticator) || typedNilPublic(authorizer) || typedNilPublic(auditor) {
		return nil, ErrInvalidPublicPlan
	}
	return &PublicPlan{
		valid: true, authenticator: authenticator, authorizer: authorizer,
		auditor: auditor, routes: make(map[routeLookup]publicRoute),
	}, nil
}

func typedNilPublic(value any) bool {
	if value == nil {
		return false // An absent provider is allowed, and closes protected routes.
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func (p *PublicPlan) Register(kind RouteKind, method RouteMethod, exactPath string, access RouteAccess, flow RouteFlow, transport RouteTransport, handler http.Handler) error {
	if p == nil {
		return ErrInvalidPublicPlan
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.valid || p.routes == nil {
		return ErrInvalidPublicPlan
	}
	if p.frozen {
		return ErrPublicPlanFrozen
	}
	if kind == RouteKindConnect || !validPublicRoute(kind, method, exactPath, access, flow, transport) || handler == nil || typedNilPublic(handler) {
		return ErrInvalidPublicPlan
	}
	key := routeLookup{method: method.httpMethod(), path: exactPath}
	if _, exists := p.routes[key]; exists {
		return ErrPublicRouteConflict
	}
	p.routes[key] = publicRoute{
		key:    RouteKey{kind: kind, method: method, path: exactPath},
		access: access, flow: flow, transport: transport, handler: handler,
	}
	return nil
}

// RegisterConnect delays generated handler construction until compile, where
// the server injects its mandatory decompressed-message size limit.
func (p *PublicPlan) RegisterConnect(build ConnectHandlerBuilder, procedures ...ConnectProcedure) error {
	if p == nil {
		return ErrInvalidPublicPlan
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.valid || p.routes == nil {
		return ErrInvalidPublicPlan
	}
	if p.frozen {
		return ErrPublicPlanFrozen
	}
	if build == nil || len(procedures) == 0 || len(procedures) > 128 {
		return ErrInvalidPublicPlan
	}
	seen := make(map[routeLookup]struct{}, len(procedures))
	for _, procedure := range procedures {
		if !validPublicRoute(RouteKindConnect, RouteMethodPost, procedure.Path, procedure.Access, procedure.Flow, procedure.Transport) {
			return ErrInvalidPublicPlan
		}
		key := routeLookup{method: http.MethodPost, path: procedure.Path}
		if _, exists := p.routes[key]; exists {
			return ErrPublicRouteConflict
		}
		if _, exists := seen[key]; exists {
			return ErrPublicRouteConflict
		}
		seen[key] = struct{}{}
	}
	copyOfProcedures := append([]ConnectProcedure(nil), procedures...)
	p.connect = append(p.connect, connectRegistration{build: build, procedures: copyOfProcedures})
	for _, procedure := range copyOfProcedures {
		key := routeLookup{method: http.MethodPost, path: procedure.Path}
		p.routes[key] = publicRoute{
			key:    RouteKey{kind: RouteKindConnect, method: RouteMethodPost, path: procedure.Path},
			access: procedure.Access, flow: procedure.Flow, transport: procedure.Transport,
		}
	}
	return nil
}

func validPublicRoute(kind RouteKind, method RouteMethod, exactPath string, access RouteAccess, flow RouteFlow, transport RouteTransport) bool {
	switch kind {
	case RouteKindConnect, RouteKindMCP, RouteKindMedia:
	default:
		return false
	}
	if method != RouteMethodGet && method != RouteMethodPost {
		return false
	}
	if kind == RouteKindConnect && method != RouteMethodPost {
		return false
	}
	if access != RouteAccessAnonymous && access != RouteAccessProtected {
		return false
	}
	if flow != RouteFlowUnary && flow != RouteFlowServerStream {
		return false
	}
	if transport != RouteTransportOptionalTLS && transport != RouteTransportTLSRequired {
		return false
	}
	if len(exactPath) < 2 || exactPath[0] != '/' || strings.HasSuffix(exactPath, "/") ||
		strings.ContainsAny(exactPath, "?#%\\\x00") {
		return false
	}
	for _, segment := range strings.Split(exactPath[1:], "/") {
		if segment == "" || segment == "." || segment == ".." ||
			strings.ContainsAny(segment, " \t\r\n") {
			return false
		}
	}
	operatorService := "/" + authv1connect.OperatorAccountServiceName
	if exactPath == operatorService || strings.HasPrefix(exactPath, operatorService+"/") {
		return false
	}
	if kind == RouteKindConnect && strings.Count(exactPath, "/") != 2 {
		return false
	}
	return true
}

type compiledPublic struct {
	authenticator Authenticator
	authorizer    Authorizer
	auditor       Auditor
	routes        map[routeLookup]publicRoute
	errorWriter   *connect.ErrorWriter
}

func compilePublicPlan(plan *PublicPlan, settings config.PublicSettings) (*compiledPublic, error) {
	result := &compiledPublic{routes: make(map[routeLookup]publicRoute), errorWriter: connect.NewErrorWriter()}
	if plan == nil {
		return result, nil
	}
	plan.mu.Lock()
	defer plan.mu.Unlock()
	if !plan.valid || plan.routes == nil || typedNilPublic(plan.authenticator) ||
		typedNilPublic(plan.authorizer) || typedNilPublic(plan.auditor) {
		return nil, ErrInvalidPublicPlan
	}
	plan.frozen = true
	result.authenticator, result.authorizer, result.auditor = plan.authenticator, plan.authorizer, plan.auditor
	for key, route := range plan.routes {
		if !validPublicRoute(route.key.kind, route.key.method, route.key.path, route.access, route.flow, route.transport) ||
			key.method != route.key.method.httpMethod() || key.path != route.key.path ||
			(route.key.kind != RouteKindConnect && (route.handler == nil || typedNilPublic(route.handler))) {
			return nil, ErrInvalidPublicPlan
		}
		result.routes[key] = route
	}
	for _, registration := range plan.connect {
		servicePrefix, handler := registration.build(connect.WithReadMaxBytes(int(settings.MaxRequestBytes())))
		if !strings.HasPrefix(servicePrefix, "/") || !strings.HasSuffix(servicePrefix, "/") ||
			strings.Count(servicePrefix, "/") != 2 ||
			handler == nil || typedNilPublic(handler) {
			return nil, ErrInvalidPublicPlan
		}
		for _, procedure := range registration.procedures {
			if !strings.HasPrefix(procedure.Path, servicePrefix) ||
				strings.Count(procedure.Path, "/") != 2 {
				return nil, ErrInvalidPublicPlan
			}
			key := routeLookup{method: http.MethodPost, path: procedure.Path}
			route, exists := result.routes[key]
			if !exists || route.key.kind != RouteKindConnect || route.handler != nil {
				return nil, ErrInvalidPublicPlan
			}
			route.handler = handler
			result.routes[key] = route
		}
	}
	for _, route := range result.routes {
		if route.handler == nil {
			return nil, ErrInvalidPublicPlan
		}
	}
	return result, nil
}

type PublicErrorCode uint8

const (
	PublicErrorUnspecified PublicErrorCode = iota
	PublicErrorUnauthenticated
	PublicErrorPermissionDenied
	PublicErrorResourceExhausted
	PublicErrorDeadlineExceeded
	PublicErrorUnavailable
)

func (c PublicErrorCode) wire() (string, int, connect.Code) {
	switch c {
	case PublicErrorUnauthenticated:
		return "unauthenticated", http.StatusUnauthorized, connect.CodeUnauthenticated
	case PublicErrorPermissionDenied:
		return "permission_denied", http.StatusForbidden, connect.CodePermissionDenied
	case PublicErrorResourceExhausted:
		return "resource_exhausted", http.StatusTooManyRequests, connect.CodeResourceExhausted
	case PublicErrorDeadlineExceeded:
		return "deadline_exceeded", http.StatusGatewayTimeout, connect.CodeDeadlineExceeded
	case PublicErrorUnavailable:
		return "unavailable", http.StatusServiceUnavailable, connect.CodeUnavailable
	default:
		return "unavailable", http.StatusServiceUnavailable, connect.CodeUnavailable
	}
}

type actorContextKey struct{}

// ActorFromContext returns only an identity admitted by the common transport
// gate. A raw client header never populates this context key.
func ActorFromContext(ctx context.Context) (domain.ActorID, bool) {
	actor, ok := ctx.Value(actorContextKey{}).(domain.ActorID)
	if !ok {
		return domain.ActorID{}, false
	}
	if _, err := actor.Value(); err != nil {
		return domain.ActorID{}, false
	}
	return actor, true
}

type publicAdmissions struct {
	mu      sync.Mutex
	closing bool
	active  int
	done    chan struct{}
}

func newPublicAdmissions() *publicAdmissions {
	return &publicAdmissions{done: make(chan struct{})}
}
func (a *publicAdmissions) begin() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closing {
		return false
	}
	a.active++
	return true
}
func (a *publicAdmissions) end() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.active--
	if a.closing && a.active == 0 {
		close(a.done)
	}
}
func (a *publicAdmissions) close() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.closing {
		a.closing = true
		if a.active == 0 {
			close(a.done)
		}
	}
	return a.done
}

func (p *compiledPublic) handler(settings config.PublicSettings, admissions *publicAdmissions) http.Handler {
	sem := make(chan struct{}, settings.MaxConcurrentRequests())
	return http.MaxBytesHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route, exists := p.routes[routeLookup{method: r.Method, path: r.URL.Path}]
		if !exists || r.URL.EscapedPath() != r.URL.Path {
			http.NotFound(w, r)
			return
		}
		if !admissions.begin() {
			p.sendError(w, r, route, PublicErrorUnavailable)
			return
		}
		defer admissions.end()
		timeout := settings.RequestTimeout()
		if route.flow == RouteFlowServerStream {
			timeout = settings.StreamTimeout()
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		r = r.WithContext(ctx)
		controller := http.NewResponseController(w)
		deadline, _ := ctx.Deadline()
		if controller.SetReadDeadline(deadline) != nil || controller.SetWriteDeadline(deadline.Add(publicErrorWriteGrace)) != nil {
			p.sendError(w, r, route, PublicErrorUnavailable)
			return
		}
		defer func() {
			_ = controller.SetReadDeadline(time.Time{})
			_ = controller.SetWriteDeadline(time.Time{})
		}()
		if route.transport == RouteTransportTLSRequired && r.TLS == nil {
			p.deny(w, r, route, nil, PublicErrorPermissionDenied)
			return
		}
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		default:
			p.deny(w, r, route, nil, PublicErrorResourceExhausted)
			return
		}
		var actor *domain.ActorID
		if route.access == RouteAccessProtected {
			if p.authenticator == nil || p.authorizer == nil {
				p.deny(w, r, route, nil, PublicErrorUnavailable)
				return
			}
			admitted, err := p.authenticator.Authenticate(ctx, r)
			if err != nil {
				p.deny(w, r, route, nil, contextErrorCode(ctx, PublicErrorUnauthenticated))
				return
			}
			if ctx.Err() != nil {
				p.deny(w, r, route, nil, contextErrorCode(ctx, PublicErrorUnavailable))
				return
			}
			if _, err := admitted.Value(); err != nil {
				p.deny(w, r, route, nil, PublicErrorUnauthenticated)
				return
			}
			actor = &admitted
			if err := p.authorizer.Authorize(ctx, admitted, route.key); err != nil {
				p.deny(w, r, route, actor, contextErrorCode(ctx, PublicErrorPermissionDenied))
				return
			}
			if ctx.Err() != nil {
				p.deny(w, r, route, actor, contextErrorCode(ctx, PublicErrorUnavailable))
				return
			}
			r = r.WithContext(context.WithValue(ctx, actorContextKey{}, admitted))
		}
		if err := p.audit(ctx, route.key, actor, AuditOutcomeAdmitted); err != nil {
			p.sendError(w, r, route, contextErrorCode(ctx, PublicErrorUnavailable))
			return
		}
		if ctx.Err() != nil {
			p.sendError(w, r, route, contextErrorCode(ctx, PublicErrorUnavailable))
			return
		}
		if route.key.kind != RouteKindConnect {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				code := PublicErrorUnavailable
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					code = PublicErrorResourceExhausted
				}
				p.sendError(w, r, route, contextErrorCode(ctx, code))
				return
			}
			r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		tracked := &trackedResponse{ResponseWriter: w}
		route.handler.ServeHTTP(tracked, r)
		if ctx.Err() != nil && !tracked.wrote {
			p.sendError(w, r, route, contextErrorCode(ctx, PublicErrorDeadlineExceeded))
		}
	}), settings.MaxRequestBytes())
}

func contextErrorCode(ctx context.Context, fallback PublicErrorCode) PublicErrorCode {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return PublicErrorDeadlineExceeded
	}
	if ctx.Err() != nil {
		return PublicErrorUnavailable
	}
	return fallback
}

func (p *compiledPublic) audit(ctx context.Context, key RouteKey, actor *domain.ActorID, outcome AuditOutcome) error {
	if p.auditor == nil {
		return ErrInvalidPublicPlan
	}
	event := AuditEvent{Route: key, Outcome: outcome}
	if actor != nil {
		event.Actor, event.HasActor = *actor, true
	}
	return p.auditor.Record(ctx, event)
}

func (p *compiledPublic) deny(w http.ResponseWriter, r *http.Request, route publicRoute, actor *domain.ActorID, code PublicErrorCode) {
	if err := p.audit(r.Context(), route.key, actor, AuditOutcomeDenied); err != nil {
		code = contextErrorCode(r.Context(), PublicErrorUnavailable)
	}
	p.sendError(w, r, route, code)
}

func (p *compiledPublic) sendError(w http.ResponseWriter, r *http.Request, route publicRoute, code PublicErrorCode) {
	name, status, connectCode := code.wire()
	if route.key.kind == RouteKindConnect {
		_ = p.errorWriter.Write(w, r, connect.NewError(connectCode, errors.New(name)))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Code string `json:"code"`
	}{Code: name})
}

type trackedResponse struct {
	http.ResponseWriter
	wrote bool
}

func (w *trackedResponse) WriteHeader(status int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(status)
}
func (w *trackedResponse) Write(data []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(data)
}
func (w *trackedResponse) Flush() {
	w.wrote = true
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
func (w *trackedResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }

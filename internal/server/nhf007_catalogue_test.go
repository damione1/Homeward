package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"connectrpc.com/connect"
	imported "github.com/damione1/Homeward/compat/ha/dashboard"
	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	configurationv1 "github.com/damione1/Homeward/gen/home/configuration/v1"
	dashboardv1 "github.com/damione1/Homeward/gen/home/dashboard/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type nhf007Version struct {
	hash     [32]byte
	revision int64
}
type nhf007Catalogue struct {
	file string
	// Guarded by the containing fixture's mutex; never stored in the client.
	versions map[string]nhf007Version
}
type nhf007Projection uint8

const (
	nhf007Normal nhf007Projection = iota
	nhf007FutureVersion
	nhf007FutureProfile
	nhf007WrongConfig
)

func (p *nhf007Projection) UnmarshalJSON(raw []byte) error {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	switch value {
	case "normal":
		*p = nhf007Normal
	case "future-version":
		*p = nhf007FutureVersion
	case "future-profile":
		*p = nhf007FutureProfile
	case "wrong-config":
		*p = nhf007WrongConfig
	default:
		return errors.New("unknown synthetic projection")
	}
	return nil
}

type nhf007Entry struct {
	ID         string           `json:"id"`
	URLPath    string           `json:"url_path"`
	File       string           `json:"file"`
	Projection nhf007Projection `json:"projection,omitempty"`
}
type nhf007Config struct {
	Version    uint32        `json:"version"`
	Dashboards []nhf007Entry `json:"dashboards"`
}

func (c *nhf007Catalogue) load() (nhf007Config, error) {
	raw, err := os.ReadFile(c.file)
	if err != nil || len(raw) > 1<<20 {
		return nhf007Config{}, errors.New("synthetic catalogue unavailable")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var result nhf007Config
	if err := decoder.Decode(&result); err != nil {
		return result, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF || result.Version != 1 || len(result.Dashboards) > 128 {
		return result, errors.New("invalid synthetic catalogue")
	}
	seen := map[string]bool{}
	for _, entry := range result.Dashboards {
		id, err := authorization.ParseDashboardID(entry.ID)
		if err != nil || seen[entry.ID] {
			return result, errors.New("duplicate or invalid synthetic identity")
		}
		seen[entry.ID] = true
		if _, err := imported.NewSource(id, entry.URLPath, entry.File); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (h *nhf005Dashboard) cataloguePermit(ctx context.Context, id authorization.DashboardID, owner contracts.AccountID) (*authorization.Permit, error) {
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return nil, coreRPCError(ctx, err)
	}
	object, err := authorization.DashboardObject(id, owner)
	if err != nil {
		return nil, coreRPCError(ctx, err)
	}
	permission, err := authorization.NewPermission(authv1.ScopeFamily_SCOPE_FAMILY_DASHBOARDS, authv1.ScopeOperation_SCOPE_OPERATION_READ)
	if err != nil {
		return nil, coreRPCError(ctx, err)
	}
	permit, err := h.authority.Permit(ctx, principal, permission, object)
	if err != nil {
		return nil, coreRPCError(ctx, err)
	}
	return permit, nil
}

type nhf007Capture struct {
	documents []*dashboardv1.Dashboard
	token     string
	permits   []*authorization.Permit
}

func (c *nhf007Capture) release() {
	for _, permit := range c.permits {
		permit.Release()
	}
	c.permits = nil
}
func (c *nhf007Capture) ready() bool {
	for _, permit := range c.permits {
		if !permit.Ready() {
			return false
		}
	}
	return true
}
func (h *nhf005Dashboard) captureCatalogue(ctx context.Context, support *dashboardv1.ClientLayoutSupport) (nhf007Capture, error) {
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return nhf007Capture{}, coreRPCError(ctx, err)
	}
	grant, err := h.authority.Current(principal)
	if err != nil {
		return nhf007Capture{}, coreRPCError(ctx, err)
	}
	rights, rightsHash, err := grant.Binding()
	if err != nil {
		return nhf007Capture{}, coreRPCError(ctx, err)
	}
	stamp, err := h.activation.CurrentConfig(ctx)
	if err != nil {
		return nhf007Capture{}, coreRPCError(ctx, err)
	}
	config, err := h.catalogue.load()
	if err != nil {
		return nhf007Capture{}, connect.NewError(connect.CodeFailedPrecondition, errors.New("invalid synthetic dashboard catalogue"))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	ownerText, err := h.owner.Value()
	if err != nil {
		return nhf007Capture{}, coreRPCError(ctx, err)
	}
	var result nhf007Capture
	success := false
	defer func() {
		if !success {
			result.release()
		}
	}()
	token := sha256.New()
	token.Write(h.epoch[:])
	token.Write(rightsHash[:])
	for _, entry := range config.Dashboards {
		id, _ := authorization.ParseDashboardID(entry.ID)
		permit, err := h.cataloguePermit(ctx, id, h.owner)
		if err != nil {
			if connect.CodeOf(err) == connect.CodePermissionDenied {
				continue
			}
			return nhf007Capture{}, err
		}
		result.permits = append(result.permits, permit)
		source, _ := imported.NewSource(id, entry.URLPath, entry.File)
		compiled, err := imported.Import(filepath.Dir(h.catalogue.file), source)
		definition, present := compiled.Definition()
		if err != nil || !present || compiled.Status() == imported.StatusFatal {
			return nhf007Capture{}, connect.NewError(connect.CodeFailedPrecondition, errors.New("invalid synthetic Lovelace source"))
		}
		resolve := func(alias domain.HAAlias) (domain.EntityID, bool) {
			name, err := alias.Value()
			if err != nil {
				return domain.EntityID{}, false
			}
			entity, present := h.bindings[name]
			return entity, present
		}
		overview, err := projectDashboardOverview(definition, stamp, rights, dashboardv1.ServingMode_SERVING_MODE_SYNTHETIC, resolve)
		if err != nil {
			return nhf007Capture{}, coreRPCError(ctx, err)
		}
		layout, err := projectDashboardLayout(compiled, support, resolve)
		if err != nil {
			return nhf007Capture{}, coreRPCError(ctx, err)
		}
		digest := definition.SHA256()
		version := h.catalogue.versions[entry.ID]
		if version.hash != digest || version.revision == 0 {
			version.hash = digest
			version.revision++
			h.catalogue.versions[entry.ID] = version
		}
		metadata := &dashboardv1.Metadata{Id: &dashboardv1.DashboardId{Value: entry.ID}, Owner: &authv1.AccountId{Value: ownerText}, Profile: definition.Profile(), Title: overview.Title,
			Version: &configurationv1.DocumentVersion{Revision: version.revision, SourceSha256: digest[:]}, UpdatedAt: timestamppb.New(time.Unix(version.revision, 0))}
		switch entry.Projection {
		case nhf007Normal:
		case nhf007FutureVersion:
			layout.Version = 99
		case nhf007FutureProfile:
			metadata.Profile = "homeward-dashboard-99"
		case nhf007WrongConfig:
			overview.ConfigRevision++
		default:
			return nhf007Capture{}, errors.New("unparsed synthetic projection")
		}
		token.Write([]byte{byte(entry.Projection)})
		doc := &dashboardv1.Dashboard{Metadata: metadata, NativeOverview: overview, NativeLayout: layout,
			Definition: &dashboardv1.Definition{Id: metadata.Id, Owner: metadata.Owner, UrlPath: entry.URLPath, Entry: entry.File}}
		for _, file := range compiled.Sources() {
			doc.Definition.Files = append(doc.Definition.Files, &dashboardv1.SourceFile{Path: file.Path(), ExactSource: file.Bytes()})
		}
		for _, diagnostic := range compiled.Diagnostics() {
			at := diagnostic.Location().Position()
			doc.Diagnostics = append(doc.Diagnostics, &dashboardv1.Diagnostic{Code: nhf007DiagnosticCode(diagnostic.Code()), File: at.File, Line: uint32(max(0, at.Line)), Column: uint32(max(0, at.Column)), Path: diagnostic.Location().Path(), Reason: diagnostic.Reason()})
		}
		if !permit.Ready() {
			return nhf007Capture{}, coreRPCError(ctx, authorization.ErrDenied)
		}
		result.documents = append(result.documents, doc)
		// Length-delimited serialization avoids ambiguous identity concatenation.
		encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(metadata)
		if err != nil {
			return nhf007Capture{}, err
		}
		token.Write([]byte(strconv.Itoa(len(encoded)) + ":"))
		token.Write(encoded)
	}
	after, err := h.activation.CurrentConfig(ctx)
	if err != nil || after != stamp {
		return nhf007Capture{}, connect.NewError(connect.CodeUnavailable, errors.New("configuration changed during dashboard read"))
	}
	afterGrant, err := h.authority.Current(principal)
	if err != nil {
		return nhf007Capture{}, coreRPCError(ctx, err)
	}
	afterRights, afterHash, err := afterGrant.Binding()
	if err != nil || rights != afterRights || rightsHash != afterHash {
		return nhf007Capture{}, coreRPCError(ctx, authorization.ErrDenied)
	}
	revision, hash, err := stamp.Components()
	if err != nil {
		return nhf007Capture{}, err
	}
	token.Write([]byte(strconv.FormatUint(revision, 10)))
	token.Write(hash[:])
	result.token = hex.EncodeToString(token.Sum(nil))
	if !result.ready() {
		return nhf007Capture{}, coreRPCError(ctx, authorization.ErrDenied)
	}
	success = true
	return result, nil
}
func nhf007DiagnosticCode(code imported.Code) dashboardv1.DiagnosticCode {
	switch code {
	case imported.CodeSyntax:
		return dashboardv1.DiagnosticCode_DIAGNOSTIC_CODE_SYNTAX
	case imported.CodeInvalidValue:
		return dashboardv1.DiagnosticCode_DIAGNOSTIC_CODE_INVALID_VALUE
	case imported.CodeMissingField:
		return dashboardv1.DiagnosticCode_DIAGNOSTIC_CODE_MISSING_FIELD
	case imported.CodeUnknownField:
		return dashboardv1.DiagnosticCode_DIAGNOSTIC_CODE_UNKNOWN_FIELD
	case imported.CodeUnsupportedVariant:
		return dashboardv1.DiagnosticCode_DIAGNOSTIC_CODE_UNSUPPORTED_VARIANT
	case imported.CodeUnsupportedProperty:
		return dashboardv1.DiagnosticCode_DIAGNOSTIC_CODE_UNSUPPORTED_PROPERTY
	case imported.CodeUnboundHAUser:
		return dashboardv1.DiagnosticCode_DIAGNOSTIC_CODE_UNBOUND_HA_USER
	case imported.CodeBlockedByAncestor:
		return dashboardv1.DiagnosticCode_DIAGNOSTIC_CODE_BLOCKED_BY_ANCESTOR
	case imported.CodeInvalid:
		return dashboardv1.DiagnosticCode_DIAGNOSTIC_CODE_UNSPECIFIED
	}
	return dashboardv1.DiagnosticCode_DIAGNOSTIC_CODE_UNSPECIFIED
}
func (h *nhf005Dashboard) listCatalogue(ctx context.Context) (*connect.Response[dashboardv1.ListDashboardsResponse], error) {
	capture, err := h.captureCatalogue(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer capture.release()
	if !capture.ready() {
		return nil, coreRPCError(ctx, authorization.ErrDenied)
	}
	response := &dashboardv1.ListDashboardsResponse{ResumeToken: capture.token}
	for _, doc := range capture.documents {
		response.Dashboards = append(response.Dashboards, doc.Metadata)
	}
	return connect.NewResponse(response), nil
}
func (h *nhf005Dashboard) getCatalogue(ctx context.Context, request *dashboardv1.GetDashboardRequest) (*connect.Response[dashboardv1.GetDashboardResponse], error) {
	if err := h.catalogue.pause(ctx, nhf007GateGet); err != nil {
		return nil, err
	}
	capture, err := h.captureCatalogue(ctx, request.Support)
	if err != nil {
		return nil, err
	}
	defer capture.release()
	if !capture.ready() {
		return nil, coreRPCError(ctx, authorization.ErrDenied)
	}
	for _, doc := range capture.documents {
		if doc.Metadata.Id.Value == request.GetId().GetValue() {
			return connect.NewResponse(&dashboardv1.GetDashboardResponse{Dashboard: doc, ResumeToken: capture.token}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, errors.New("dashboard not found"))
}
func (h *nhf005Dashboard) watchCatalogue(ctx context.Context, request *dashboardv1.WatchDashboardsRequest, stream *connect.ServerStream[dashboardv1.WatchDashboardsResponse]) error {
	if err := h.catalogue.pause(ctx, nhf007GateWatch); err != nil {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		capture, err := h.captureCatalogue(ctx, nil)
		if err != nil {
			return err
		}
		if !capture.ready() {
			capture.release()
			return coreRPCError(ctx, authorization.ErrDenied)
		}
		if capture.token != request.ResumeToken {
			capture.release()
			return stream.Send(&dashboardv1.WatchDashboardsResponse{Result: &dashboardv1.WatchDashboardsResponse_ResyncRequired{ResyncRequired: &configurationv1.ResyncRequired{}}})
		}
		capture.release()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// One-shot local barriers, only in the explicitly enabled test fixture. They
// mutate no device and have no public route; cancellation always joins them.
type nhf007Gate uint8

const (
	nhf007GateGet nhf007Gate = iota
	nhf007GateWatch
)

func (c *nhf007Catalogue) pause(ctx context.Context, gate nhf007Gate) error {
	var name string
	switch gate {
	case nhf007GateGet:
		name = "get"
	case nhf007GateWatch:
		name = "watch"
	default:
		return errors.New("invalid synthetic barrier")
	}
	root := filepath.Dir(c.file)
	if err := os.Rename(filepath.Join(root, "pause-"+name), filepath.Join(root, name+"-paused")); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	deadline := time.NewTimer(25 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(filepath.Join(root, "release-"+name)); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("synthetic barrier was not released")
		case <-ticker.C:
		}
	}
}

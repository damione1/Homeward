package configplans

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"time"
)

// receiptPayload contains no source bytes or authorization capability. The
// process-epoch key prevents changing audit/base provenance; current permission
// checks and an eventual writer CAS remain independent and mandatory.
type receiptPayload struct {
	Version                      uint8    `json:"v"`
	Author                       string   `json:"a"`
	Family                       Family   `json:"f"`
	Identity                     string   `json:"i"`
	Entry                        [32]byte `json:"e"`
	Source, Semantic             [32]byte
	BaseKind                     BaseKind
	BaseRevision                 int64
	BaseSource                   [32]byte
	Generation                   int64
	Manifest, Workspace, Catalog [32]byte
	State                        State
	Expires                      int64
}

func (p receiptPayload) matches(plan Plan) bool {
	return p.Source == plan.source && p.Semantic == plan.semantic && p.BaseKind == plan.base.kind && p.BaseRevision == plan.base.revision && p.BaseSource == plan.base.source && p.Generation == plan.generation && p.Manifest == plan.manifest && p.Workspace == plan.workspace && p.Catalog == plan.catalog && p.State == plan.state
}
func (s *Service) sign(plan Plan, now time.Time) (string, error) {
	if !s.valid() {
		return "", ErrInvalid
	}
	return signReceipt(s.key, plan, now)
}
func signReceipt(key [32]byte, plan Plan, now time.Time) (string, error) {
	if key == ([32]byte{}) || now.IsZero() || !label(plan.author, 255) || !plan.family.Valid() || plan.source == ([32]byte{}) {
		return "", ErrInvalid
	}
	payload := receiptPayload{Version: 1, Author: plan.author, Family: plan.family, Identity: plan.identity, Entry: sha256.Sum256([]byte(plan.entry)), Source: plan.source, Semantic: plan.semantic, BaseKind: plan.base.kind, BaseRevision: plan.base.revision, BaseSource: plan.base.source, Generation: plan.generation, Manifest: plan.manifest, Workspace: plan.workspace, Catalog: plan.catalog, State: plan.state, Expires: now.Add(30 * time.Minute).Unix()}
	raw, e := json.Marshal(payload)
	if e != nil || len(raw) > 3072 {
		return "", ErrUnavailable
	}
	mac := hmac.New(sha256.New, key[:])
	part(mac, []byte("homeward.config-plan.receipt/1"))
	part(mac, raw)
	return base64.RawURLEncoding.EncodeToString(append(raw, mac.Sum(nil)...)), nil
}
func (s *Service) verify(token string, now time.Time) (receiptPayload, error) {
	if !s.valid() {
		return receiptPayload{}, ErrInvalid
	}
	return verifyReceipt(s.key, token, now)
}
func verifyReceipt(key [32]byte, token string, now time.Time) (receiptPayload, error) {
	if key == ([32]byte{}) || len(token) < 44 || len(token) > 4200 || now.IsZero() {
		return receiptPayload{}, ErrInvalid
	}
	raw, e := base64.RawURLEncoding.DecodeString(token)
	if e != nil || len(raw) < 33 || len(raw) > 3104 || base64.RawURLEncoding.EncodeToString(raw) != token {
		return receiptPayload{}, ErrInvalid
	}
	body, signature := raw[:len(raw)-32], raw[len(raw)-32:]
	mac := hmac.New(sha256.New, key[:])
	part(mac, []byte("homeward.config-plan.receipt/1"))
	part(mac, body)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return receiptPayload{}, ErrInvalid
	}
	var payload receiptPayload
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&payload) != nil {
		return receiptPayload{}, ErrInvalid
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || payload.Version != 1 || !label(payload.Author, 255) || !payload.Family.Valid() || !label(payload.Identity, 255) || payload.Entry == ([32]byte{}) || payload.Source == ([32]byte{}) || payload.Generation < 1 || payload.Manifest == ([32]byte{}) || payload.Workspace == ([32]byte{}) || payload.Catalog == ([32]byte{}) {
		return receiptPayload{}, ErrInvalid
	}
	switch payload.State {
	case Admitted:
		if payload.Semantic == ([32]byte{}) {
			return receiptPayload{}, ErrInvalid
		}
	case Rejected:
		if payload.Semantic != ([32]byte{}) {
			return receiptPayload{}, ErrInvalid
		}
	case StateInvalid, Stale:
		return receiptPayload{}, ErrInvalid
	default:
		return receiptPayload{}, ErrInvalid
	}
	switch payload.BaseKind {
	case ActiveSQL:
		if payload.BaseRevision < 1 || payload.BaseSource == ([32]byte{}) {
			return receiptPayload{}, ErrInvalid
		}
	case ImmutableReference:
		if payload.BaseRevision != 0 || payload.BaseSource == ([32]byte{}) {
			return receiptPayload{}, ErrInvalid
		}
	case NoActiveDocument:
		if payload.BaseRevision != 0 || payload.BaseSource != ([32]byte{}) {
			return receiptPayload{}, ErrInvalid
		}
	case BaseInvalid:
		return receiptPayload{}, ErrInvalid
	default:
		return receiptPayload{}, ErrInvalid
	}
	if payload.Expires <= now.Unix() || payload.Expires > now.Add(30*time.Minute+time.Second).Unix() {
		return receiptPayload{}, ErrStale
	}
	return payload, nil
}

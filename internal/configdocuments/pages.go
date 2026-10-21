package configdocuments

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"

	"github.com/damione1/Homeward/internal/storage"
)

type PageKind uint8

const (
	PageUnknown PageKind = iota
	PageDocuments
	PageVersions
	PageGenerations
	PageSchemas
)

type PageCursor struct {
	kind         PageKind
	document     storage.ConfigDocumentID
	after        string
	before       int64
	cut, binding [32]byte
}

func NewPageCursor(kind PageKind, document storage.ConfigDocumentID, after string, before int64, cut, binding [32]byte) (PageCursor, error) {
	if cut == ([32]byte{}) || binding == ([32]byte{}) || before < 0 {
		return PageCursor{}, &Error{kind: FailureInvalid}
	}
	switch kind {
	case PageDocuments:
		if document.Valid() || before != 0 {
			return PageCursor{}, &Error{kind: FailureInvalid}
		}
		if after != "" {
			if _, err := storage.NewConfigDocumentID(after); err != nil {
				return PageCursor{}, &Error{kind: FailureInvalid}
			}
		}
	case PageVersions:
		if !document.Valid() || after != "" {
			return PageCursor{}, &Error{kind: FailureInvalid}
		}
	case PageGenerations, PageSchemas:
		if document.Valid() || after != "" {
			return PageCursor{}, &Error{kind: FailureInvalid}
		}
	case PageUnknown:
		return PageCursor{}, &Error{kind: FailureInvalid}
	default:
		return PageCursor{}, &Error{kind: FailureInvalid}
	}
	return PageCursor{kind: kind, document: document, after: after, before: before, cut: cut, binding: binding}, nil
}
func (c PageCursor) After() string { return c.after }
func (c PageCursor) Before() int64 { return c.before }
func (c PageCursor) Cut() [32]byte { return c.cut }

type pagePayload struct {
	Kind         PageKind `json:"k"`
	Document     string   `json:"d,omitempty"`
	After        string   `json:"a,omitempty"`
	Before       int64    `json:"n,omitempty"`
	Cut, Binding [32]byte
}

func (s *Service) EncodePage(c PageCursor) (string, error) {
	if s == nil || !s.admitted {
		return "", &Error{kind: FailureInvalid}
	}
	if _, err := NewPageCursor(c.kind, c.document, c.after, c.before, c.cut, c.binding); err != nil {
		return "", err
	}
	raw, err := json.Marshal(pagePayload{Kind: c.kind, Document: c.document.String(), After: c.after, Before: c.before, Cut: c.cut, Binding: c.binding})
	if err != nil || len(raw) > 800 {
		return "", &Error{kind: Unavailable}
	}
	mac := hmac.New(sha256.New, s.key[:])
	_, _ = mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(append(raw, mac.Sum(nil)...)), nil
}
func (s *Service) DecodePage(token string, kind PageKind, document storage.ConfigDocumentID, binding [32]byte) (PageCursor, error) {
	if s == nil || !s.admitted || len(token) < 44 || len(token) > 1200 {
		return PageCursor{}, &Error{kind: Conflict}
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) < 33 || len(raw) > 832 || base64.RawURLEncoding.EncodeToString(raw) != token {
		return PageCursor{}, &Error{kind: Conflict}
	}
	body, signature := raw[:len(raw)-32], raw[len(raw)-32:]
	mac := hmac.New(sha256.New, s.key[:])
	_, _ = mac.Write(body)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return PageCursor{}, &Error{kind: Conflict}
	}
	var p pagePayload
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil {
		return PageCursor{}, &Error{kind: Conflict}
	}
	var extra any
	if d.Decode(&extra) != io.EOF || p.Kind != kind || p.Document != document.String() || p.Binding != binding {
		return PageCursor{}, &Error{kind: Conflict}
	}
	result, err := NewPageCursor(kind, document, p.After, p.Before, p.Cut, p.Binding)
	if err != nil {
		return PageCursor{}, &Error{kind: Conflict}
	}
	return result, nil
}

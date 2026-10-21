// Package s3test is an isolated, in-memory S3 HTTP fixture. It has no AWS client,
// configuration discovery, domestic endpoint or filesystem storage.
package s3test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"github.com/aws/aws-sdk-go-v2/aws"
	signerv4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const Bundle = `{"access_key_id":"NHA156ACCESS","secret_access_key":"NHA156SECRET","session_token":"NHA156TOKEN"}`

type Object struct {
	Data   []byte
	Header http.Header
	ETag   string
}
type Request struct {
	Method, Path, Query           string
	Signed, Conditional, Checksum bool
}
type Server struct {
	HTTP      *httptest.Server
	mu        sync.Mutex
	objects   map[string]Object
	requests  []Request
	mode      string
	redirects string
	pageSize  int
}

func New() *Server {
	s := &Server{objects: map[string]Object{}, pageSize: 1}
	s.HTTP = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}
func (s *Server) Close()           { s.HTTP.Close() }
func (s *Server) URL() string      { return s.HTTP.URL }
func (s *Server) Mode(mode string) { s.mu.Lock(); s.mode = mode; s.mu.Unlock() }
func (s *Server) Redirect(url string) {
	s.mu.Lock()
	s.mode = "redirect"
	s.redirects = url
	s.mu.Unlock()
}
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}
func (s *Server) Objects() map[string]Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := map[string]Object{}
	for k, v := range s.objects {
		v.Data = append([]byte(nil), v.Data...)
		v.Header = v.Header.Clone()
		m[k] = v
	}
	return m
}
func (s *Server) Inject(key string, o Object) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o.Data = append([]byte(nil), o.Data...)
	o.Header = o.Header.Clone()
	s.objects[key] = o
}
func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	mode, redirect := s.mode, s.redirects
	signed := validSignature(r)
	s.requests = append(s.requests, Request{r.Method, r.URL.Path, r.URL.RawQuery, signed, r.Header.Get("If-None-Match") == "*" || r.Header.Get("If-Match") != "", r.Header.Get("X-Amz-Checksum-Sha256") != ""})
	s.mu.Unlock()
	if !signed {
		failure(w, 403, "AccessDenied")
		return
	}
	switch mode {
	case "deny":
		failure(w, 403, "AccessDenied")
		return
	case "fail":
		failure(w, 503, "SlowDown")
		return
	case "redirect":
		w.Header().Set("Location", redirect)
		w.WriteHeader(307)
		return
	case "large-error":
		w.WriteHeader(500)
		_, _ = io.WriteString(w, strings.Repeat("NHA156SECRET", 100000))
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/nha156-bucket/")
	if r.Method == "GET" && r.URL.Query().Get("list-type") == "2" {
		s.list(w, r, mode)
		return
	}
	if key == r.URL.Path || key == "" {
		failure(w, 404, "NoSuchKey")
		return
	}
	if r.Method == "PUT" {
		if mode == "drop-upload" {
			drop(w)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		if err != nil || int64(len(data)) != r.ContentLength {
			failure(w, 400, "InvalidRequest")
			return
		}
		digest := sha256.Sum256(data)
		if r.Header.Get("If-None-Match") != "*" || r.Header.Get("X-Amz-Checksum-Sha256") != base64.StdEncoding.EncodeToString(digest[:]) {
			failure(w, 400, "BadDigest")
			return
		}
		s.mu.Lock()
		if _, exists := s.objects[key]; exists {
			s.mu.Unlock()
			failure(w, 412, "PreconditionFailed")
			return
		}
		headers := http.Header{}
		for k, v := range r.Header {
			if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") {
				headers[k] = append([]string(nil), v...)
			}
		}
		etag := `"` + hex.EncodeToString(digest[:16]) + `"`
		s.objects[key] = Object{append([]byte(nil), data...), headers, etag}
		s.mu.Unlock()
		if mode == "drop-response" {
			drop(w)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("X-Amz-Checksum-Sha256", base64.StdEncoding.EncodeToString(digest[:]))
		w.WriteHeader(200)
		return
	}
	s.mu.Lock()
	object, found := s.objects[key]
	if mode == "etag-swap" && r.Method == "DELETE" {
		object.ETag = `"changed"`
		s.objects[key] = object
	}
	s.mu.Unlock()
	if !found {
		failure(w, 404, "NoSuchKey")
		return
	}
	if r.Method == "DELETE" {
		if r.Header.Get("If-Match") != object.ETag {
			failure(w, 412, "PreconditionFailed")
			return
		}
		s.mu.Lock()
		delete(s.objects, key)
		s.mu.Unlock()
		w.WriteHeader(204)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		failure(w, 405, "MethodNotAllowed")
		return
	}
	for k, v := range object.Header {
		w.Header()[k] = append([]string(nil), v...)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(object.Data)))
	w.Header().Set("ETag", object.ETag)
	if r.Method == "HEAD" {
		w.WriteHeader(200)
		return
	}
	if mode == "slow-get" {
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
		return
	}
	data := append([]byte(nil), object.Data...)
	if mode == "corrupt-get" && len(data) > 0 {
		data[len(data)-1] ^= 1
	}
	if mode == "truncate-get" && len(data) > 0 {
		data = data[:len(data)-1]
	}
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

type listing struct {
	XMLName   xml.Name `xml:"ListBucketResult"`
	Truncated bool     `xml:"IsTruncated"`
	Next      string   `xml:"NextContinuationToken,omitempty"`
	Contents  []entry  `xml:"Contents"`
}
type entry struct {
	Key  string `xml:"Key"`
	Size int    `xml:"Size"`
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := r.URL.Query().Get("prefix")
	after := r.URL.Query().Get("start-after")
	if token := r.URL.Query().Get("continuation-token"); token != "" {
		after = token
	}
	var keys []string
	for k := range s.objects {
		if strings.HasPrefix(k, prefix) && k > after {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	count, _ := strconv.Atoi(r.URL.Query().Get("max-keys"))
	if count < 1 {
		count = 1
	}
	count = min(count, s.pageSize)
	response := listing{}
	for i, k := range keys {
		if i >= count {
			response.Truncated = true
			break
		}
		response.Contents = append(response.Contents, entry{k, len(s.objects[k].Data)})
	}
	if response.Truncated {
		response.Next = response.Contents[len(response.Contents)-1].Key
	}
	if mode == "foreign-list" {
		response.Contents = []entry{{"foreign/outside.age", 10}}
		response.Truncated = false
	}
	if mode == "loop-list" {
		response.Truncated = true
		response.Next = "repeated"
		response.Contents = []entry{{prefix + "zzzz", 10}}
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(response)
}
func failure(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}{Code: code, Message: "NHA156SECRET NHA156TOKEN"})
}
func drop(w http.ResponseWriter) {
	if h, ok := w.(http.Hijacker); ok {
		if c, _, err := h.Hijack(); err == nil {
			_ = c.Close()
		}
	}
}

// Verify with the maintained AWS signer, not a home-grown SigV4 algorithm.
// Only the transmitted signed headers are reconstructed; Go may add unsigned
// transport headers after the SDK has signed its original request.
func validSignature(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") || !strings.Contains(auth, "Credential=NHA156ACCESS/") || r.Header.Get("X-Amz-Security-Token") != "NHA156TOKEN" {
		return false
	}
	start := strings.Index(auth, "SignedHeaders=")
	if start < 0 {
		return false
	}
	names := strings.SplitN(auth[start+len("SignedHeaders="):], ",", 2)[0]
	clone := r.Clone(r.Context())
	clone.Header = make(http.Header)
	for _, name := range strings.Split(names, ";") {
		if name != "host" {
			clone.Header[http.CanonicalHeaderKey(name)] = append([]string(nil), r.Header.Values(name)...)
		}
	}
	clone.URL.Scheme = "http"
	clone.URL.Host = r.Host
	clone.Host = r.Host
	at, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil {
		return false
	}
	payload := r.Header.Get("X-Amz-Content-Sha256")
	if payload == "" {
		return false
	}
	err = signerv4.NewSigner().SignHTTP(r.Context(), aws.Credentials{AccessKeyID: "NHA156ACCESS", SecretAccessKey: "NHA156SECRET", SessionToken: "NHA156TOKEN"}, clone, payload, "s3", "us-east-1", at, func(o *signerv4.SignerOptions) { o.DisableURIPathEscaping = true })
	return err == nil && clone.Header.Get("Authorization") == auth
}

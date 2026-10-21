// Package s3 adapts the opaque backup port to an explicitly selected S3 bucket.
package s3

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/logging"
	"github.com/damione1/Homeward/internal/backups"
	"github.com/damione1/Homeward/internal/storage"
)

type SecretReader interface {
	Read(context.Context, storage.SecretHandle) (storage.SecretValue, error)
}

type Factory struct {
	secrets   SecretReader
	directory string
}

func NewFactory(secrets SecretReader, directory string) (Factory, error) {
	if backups.IsNil(secrets) || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || directory == "/" {
		return Factory{}, backups.NewError(backups.InvalidInput)
	}
	return Factory{secrets: secrets, directory: directory}, nil
}
func (Factory) Kind() backups.ProviderKind { return backups.S3 }
func (f Factory) Open(ctx context.Context, settings backups.InstanceSettings) (backups.ManagedProvider, error) {
	return open(ctx, settings, f.secrets, f.directory, nil)
}
func (Factory) String() string                 { return "backup.S3Factory(<private>)" }
func (f Factory) GoString() string             { return f.String() }
func (f Factory) Format(s fmt.State, _ rune)   { _, _ = s.Write([]byte(f.String())) }
func (f Factory) MarshalJSON() ([]byte, error) { return json.Marshal(f.String()) }

type credentials struct {
	reader SecretReader
	ref    storage.SecretHandle
}

func (credentials) String() string                 { return "backup.S3Credentials(<private>)" }
func (c credentials) GoString() string             { return c.String() }
func (c credentials) Format(s fmt.State, _ rune)   { _, _ = s.Write([]byte(c.String())) }
func (c credentials) MarshalJSON() ([]byte, error) { return json.Marshal(c.String()) }
func (c credentials) Retrieve(ctx context.Context) (aws.Credentials, error) {
	if backups.IsNil(c.reader) {
		return aws.Credentials{}, backups.NewError(backups.InvalidInput)
	}
	v, err := c.reader.Read(ctx, c.ref)
	if err != nil {
		return aws.Credentials{}, backups.NewError(backups.PermissionDenied)
	}
	raw, err := v.Bytes()
	if err != nil {
		return aws.Credentials{}, backups.NewError(backups.PermissionDenied)
	}
	defer clear(raw)
	// One repository read is one atomic credential revision. Token is optional,
	// but an explicitly present empty token is invalid, not absence.
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return aws.Credentials{}, backups.NewError(backups.PermissionDenied)
	}
	fields := map[string]string{}
	for d.More() {
		k, err := d.Token()
		if err != nil {
			return aws.Credentials{}, backups.NewError(backups.PermissionDenied)
		}
		name, ok := k.(string)
		if !ok || name != "access_key_id" && name != "secret_access_key" && name != "session_token" {
			return aws.Credentials{}, backups.NewError(backups.PermissionDenied)
		}
		if _, exists := fields[name]; exists {
			return aws.Credentials{}, backups.NewError(backups.PermissionDenied)
		}
		var value string
		if err := d.Decode(&value); err != nil || !credentialText(value) {
			return aws.Credentials{}, backups.NewError(backups.PermissionDenied)
		}
		fields[name] = value
	}
	if t, err := d.Token(); err != nil || t != json.Delim('}') {
		return aws.Credentials{}, backups.NewError(backups.PermissionDenied)
	}
	var extra any
	if d.Decode(&extra) != io.EOF || fields["access_key_id"] == "" || fields["secret_access_key"] == "" {
		return aws.Credentials{}, backups.NewError(backups.PermissionDenied)
	}
	return aws.Credentials{AccessKeyID: fields["access_key_id"], SecretAccessKey: fields["secret_access_key"], SessionToken: fields["session_token"], Source: "Homeward secret reference"}, nil
}
func credentialText(s string) bool {
	if len(s) == 0 || len(s) > 8192 {
		return false
	}
	for _, c := range s {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

type pinnedClient struct {
	client   *http.Client
	endpoint *url.URL
}

func (c pinnedClient) Do(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != c.endpoint.Scheme || r.URL.Host != c.endpoint.Host || r.URL.User != nil {
		return nil, backups.NewError(backups.PermissionDenied)
	}
	response, err := c.client.Do(r)
	if err != nil {
		return nil, err
	}
	if r.Method != "GET" || r.URL.Query().Get("list-type") == "2" || response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body = &boundedBody{ReadCloser: response.Body, remaining: 512 << 10}
	}
	return response, nil
}

type boundedBody struct {
	io.ReadCloser
	remaining int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		var one [1]byte
		n, err := b.ReadCloser.Read(one[:])
		if n > 0 {
			return 0, backups.NewError(backups.Capacity)
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	return n, err
}
func newClient(settings backups.S3Settings, secrets SecretReader, transport *http.Transport) (*awss3.Client, *http.Transport, error) {
	u, err := url.Parse(settings.Endpoint())
	if err != nil {
		return nil, nil, backups.NewError(backups.InvalidInput)
	}
	if transport == nil {
		transport = &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 15 * time.Second, MaxResponseHeaderBytes: 32 << 10, MaxConnsPerHost: backups.MaxConcurrentOperations, MaxIdleConnsPerHost: backups.MaxConcurrentOperations, IdleConnTimeout: 30 * time.Second}
	}
	httpClient := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	client := awss3.New(awss3.Options{BaseEndpoint: aws.String(settings.Endpoint()), Region: settings.Region(), UsePathStyle: true, DisableMultiRegionAccessPoints: true, DisableS3ExpressSessionAuth: aws.Bool(true), Credentials: credentials{reader: secrets, ref: settings.CredentialRef()}, RetryMaxAttempts: 1, HTTPClient: pinnedClient{client: httpClient, endpoint: u}, Logger: logging.Nop{}, ClientLogMode: 0, DisableLogOutputChecksumValidationSkipped: true, RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired, ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired})
	return client, transport, nil
}
func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return backups.NewError(backups.Cancelled)
	}
	var admitted *backups.Error
	if errors.As(err, &admitted) {
		return backups.NewError(admitted.Kind())
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchBucket":
			return backups.NewError(backups.NotFound)
		case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch", "ExpiredToken", "InvalidToken":
			return backups.NewError(backups.PermissionDenied)
		case "PreconditionFailed", "ConditionalRequestConflict":
			return backups.NewError(backups.Conflict)
		case "BadDigest", "InvalidDigest":
			return backups.NewError(backups.Corrupt)
		}
	}
	return backups.NewError(backups.Unavailable)
}
func errorIs(err error, kind backups.ErrorKind) bool {
	return errors.Is(classify(err), backups.NewError(kind))
}
func fixedMetadataKeys(fields map[string]string) bool {
	for k := range fields {
		if !strings.HasPrefix(k, "homeward-") {
			continue
		}
		switch k {
		case "homeward-version", "homeward-instance", "homeward-artifact", "homeward-sha256", "homeward-published":
		default:
			return false
		}
	}
	return true
}

package backups

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/damione1/Homeward/internal/storage"
)

// S3 uses one conditional PutObject. The lower backend limit is advertised to
// consumers; the common port's 8 GiB ceiling and local limits are unchanged.
const MaxS3ArtifactBytes int64 = 5 << 30

type S3Settings struct {
	endpoint, bucket, prefix, region string
	credential                       storage.SecretHandle
	limits                           Limits
}

func NewS3Settings(id InstanceID, enabled bool, endpoint, bucket, prefix, region string, credential storage.SecretHandle, limits Limits) (InstanceSettings, error) {
	if !id.Valid() || !limits.Valid() || limits.ArtifactBytes() > MaxS3ArtifactBytes {
		return InstanceSettings{}, NewError(InvalidInput)
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || u.Opaque != "" {
		return InstanceSettings{}, NewError(InvalidInput)
	}
	if u.Scheme != "https" {
		ip, err := netip.ParseAddr(u.Hostname())
		if u.Scheme != "http" || err != nil || !ip.IsLoopback() {
			return InstanceSettings{}, NewError(InvalidInput)
		}
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return InstanceSettings{}, NewError(InvalidInput)
		}
	}
	if strings.HasSuffix(u.Host, ":") {
		return InstanceSettings{}, NewError(InvalidInput)
	}
	if u.Hostname() == "" || strings.ContainsAny(endpoint, "\r\n\t ") || !s3Name(bucket, 3, 63, "-.") || strings.Contains(bucket, "..") || strings.HasPrefix(bucket, "xn--") || strings.HasSuffix(bucket, "--x-s3") || strings.HasSuffix(bucket, "-s3alias") {
		return InstanceSettings{}, NewError(InvalidInput)
	}
	if _, err := netip.ParseAddr(bucket); err == nil {
		return InstanceSettings{}, NewError(InvalidInput)
	}
	if !s3Name(region, 3, 64, "-") || len(prefix) == 0 || len(prefix) > 256 || strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/") {
		return InstanceSettings{}, NewError(InvalidInput)
	}
	for _, part := range strings.Split(prefix, "/") {
		if !s3Name(part, 1, 64, "-_") {
			return InstanceSettings{}, NewError(InvalidInput)
		}
	}
	if _, _, err := credential.Components(); err != nil {
		return InstanceSettings{}, NewError(InvalidInput)
	}
	u.Path = ""
	return InstanceSettings{id: id, kind: S3, enabled: enabled, s3: S3Settings{endpoint: u.String(), bucket: bucket, prefix: prefix, region: region, credential: credential, limits: limits}}, nil
}

func s3Name(s string, min, max int, extra string) bool {
	if len(s) < min || len(s) > max || (s[0] < 'a' || s[0] > 'z') && (s[0] < '0' || s[0] > '9') {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.ContainsRune(extra, c)) {
			return false
		}
	}
	last := s[len(s)-1]
	return last >= 'a' && last <= 'z' || last >= '0' && last <= '9'
}
func DefaultS3Limits() Limits {
	l, _ := NewLimits(MaxS3ArtifactBytes, MaxPageSize, MaxScanEntries, MaxConcurrentOperations)
	return l
}
func (s S3Settings) Valid() bool {
	return s.endpoint != "" && s.limits.Valid() && s.limits.ArtifactBytes() <= MaxS3ArtifactBytes
}
func (s S3Settings) Endpoint() string                    { return s.endpoint }
func (s S3Settings) Bucket() string                      { return s.bucket }
func (s S3Settings) Prefix() string                      { return s.prefix }
func (s S3Settings) Region() string                      { return s.region }
func (s S3Settings) CredentialRef() storage.SecretHandle { return s.credential }
func (s S3Settings) Limits() Limits                      { return s.limits }
func (s InstanceSettings) S3() (S3Settings, error) {
	if !s.Valid() || s.kind != S3 {
		return S3Settings{}, NewError(InvalidInput)
	}
	return s.s3, nil
}
func (S3Settings) String() string                 { return "backup.S3Settings(<private>)" }
func (s S3Settings) GoString() string             { return s.String() }
func (s S3Settings) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(s.String())) }
func (s S3Settings) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

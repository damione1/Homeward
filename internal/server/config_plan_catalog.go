package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/proto"
)

const configurationPlanCapacity = 256
const configurationPlanCredentialCapacity = 64
const configurationPlanLifetime = 10 * time.Minute
const configurationPlanMetadataBytes = 64 << 10

type configurationPlanRecord struct {
	name                      apitypes.ConfigurationPlanName
	binding                   configDocumentsBinding
	metadata                  *api.ConfigurationPlan
	created, expires, updated time.Time
	version                   uint64
}

func (b configDocumentsBinding) valid() bool {
	_, err := b.account.Value()
	return err == nil && b.digest != ([32]byte{})
}
func (v configurationPlanRecord) clone() configurationPlanRecord {
	v.metadata = proto.Clone(v.metadata).(*api.ConfigurationPlan)
	return v
}

// This catalogue preserves only redacted diagnostic metadata during the native
// receipt epoch. It does not retain candidate bytes or create an activation
// owner. A restart drops captures; native source/CAS/receipt admission remains
// mandatory for any subsequent explicit mutation.
type configurationPlanCatalog struct {
	valid    bool
	mu       sync.Mutex
	records  map[string]configurationPlanRecord
	pending  map[string]configurationPlanRecord
	key      [32]byte
	revision uint64
	now      func() time.Time
}

func newConfigurationPlanCatalog(now func() time.Time) (*configurationPlanCatalog, error) {
	if now == nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	value := &configurationPlanCatalog{records: make(map[string]configurationPlanRecord), pending: make(map[string]configurationPlanRecord), revision: 1, now: now}
	if _, err := rand.Read(value.key[:]); err != nil {
		return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	value.valid = true
	return value, nil
}
func (c *configurationPlanCatalog) purgeLocked() {
	now := c.now()
	for key, record := range c.records {
		if !now.Before(record.expires) {
			delete(c.records, key)
			c.revision++
		}
	}
	for key, record := range c.pending {
		if !now.Before(record.expires) {
			delete(c.pending, key)
		}
	}
}
func (c *configurationPlanCatalog) publishLocked(record configurationPlanRecord) configurationPlanRecord {
	record.version = c.revision + 1
	c.records[record.name.String()] = record.clone()
	delete(c.pending, record.name.String())
	c.revision++
	return record.clone()
}

func (c *configurationPlanCatalog) add(home apitypes.HomeName, binding configDocumentsBinding, metadata *api.ConfigurationPlan, nativeExpiry time.Time) (configurationPlanRecord, error) {
	if c == nil || !c.valid {
		return configurationPlanRecord{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	record, err := c.prepareAddLocked(home, binding, metadata, nativeExpiry)
	if err != nil {
		return configurationPlanRecord{}, err
	}
	return c.publishLocked(record), nil
}
func (c *configurationPlanCatalog) prepareAddLocked(home apitypes.HomeName, binding configDocumentsBinding, metadata *api.ConfigurationPlan, nativeExpiry time.Time) (configurationPlanRecord, error) {
	if c == nil || !c.valid || !binding.valid() || !home.Valid() || metadata == nil || proto.Size(metadata) > configurationPlanMetadataBytes || contracts.AdmitConfigurationPlanMetadata(metadata) != nil {
		return configurationPlanRecord{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	c.purgeLocked()
	credentialCount := 0
	for _, record := range c.pending {
		if record.binding == binding {
			credentialCount++
		}
	}
	for _, record := range c.records {
		if record.binding == binding {
			credentialCount++
		}
	}
	if credentialCount >= configurationPlanCredentialCapacity {
		return configurationPlanRecord{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
	}
	if len(c.records)+len(c.pending) >= configurationPlanCapacity {
		return configurationPlanRecord{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return configurationPlanRecord{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	name, err := apitypes.ParseConfigurationPlanName(home.String() + "/configurationPlans/p-" + hex.EncodeToString(random[:]))
	if err != nil {
		return configurationPlanRecord{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
	}
	_, pending := c.pending[name.String()]
	if _, exists := c.records[name.String()]; exists || pending {
		return configurationPlanRecord{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	now := c.now()
	if !nativeExpiry.After(now) {
		return configurationPlanRecord{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_CONFIG_CHANGED)
	}
	expiry := now.Add(configurationPlanLifetime)
	if nativeExpiry.Before(expiry) {
		expiry = nativeExpiry
	}
	record := configurationPlanRecord{name: name, binding: binding, metadata: proto.Clone(metadata).(*api.ConfigurationPlan), created: now, expires: expiry, updated: now, version: c.revision + 1}
	return record.clone(), nil
}
func (c *configurationPlanCatalog) get(name apitypes.ConfigurationPlanName, binding configDocumentsBinding) (configurationPlanRecord, error) {
	if c == nil || !c.valid || !binding.valid() || !name.Valid() {
		return configurationPlanRecord{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purgeLocked()
	record, exists := c.records[name.String()]
	if !exists || record.binding != binding {
		return configurationPlanRecord{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND)
	}
	return record.clone(), nil
}
func (c *configurationPlanCatalog) replace(name apitypes.ConfigurationPlanName, binding configDocumentsBinding, expectedVersion uint64, metadata *api.ConfigurationPlan, nativeExpiry time.Time) (configurationPlanRecord, error) {
	if c == nil || !c.valid {
		return configurationPlanRecord{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	record, err := c.prepareReplaceLocked(name, binding, expectedVersion, metadata, nativeExpiry)
	if err != nil {
		return configurationPlanRecord{}, err
	}
	return c.publishLocked(record), nil
}
func (c *configurationPlanCatalog) prepareReplaceLocked(name apitypes.ConfigurationPlanName, binding configDocumentsBinding, expectedVersion uint64, metadata *api.ConfigurationPlan, nativeExpiry time.Time) (configurationPlanRecord, error) {
	if c == nil || !c.valid || !binding.valid() || !name.Valid() || metadata == nil || proto.Size(metadata) > configurationPlanMetadataBytes || contracts.AdmitConfigurationPlanMetadata(metadata) != nil {
		return configurationPlanRecord{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	c.purgeLocked()
	if _, pending := c.pending[name.String()]; pending {
		return configurationPlanRecord{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE)
	}
	count := 0
	for _, record := range c.records {
		if record.binding == binding {
			count++
		}
	}
	for _, record := range c.pending {
		if record.binding == binding {
			count++
		}
	}
	if count >= configurationPlanCredentialCapacity || len(c.records)+len(c.pending) >= configurationPlanCapacity {
		return configurationPlanRecord{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
	}
	record, exists := c.records[name.String()]
	if !exists || record.binding != binding {
		return configurationPlanRecord{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND)
	}
	if record.version != expectedVersion {
		return configurationPlanRecord{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_ETAG_MISMATCH)
	}

	if !nativeExpiry.IsZero() && nativeExpiry.Before(record.expires) {
		record.expires = nativeExpiry
	}
	record.updated = c.now()
	record.version = c.revision + 1
	record.metadata = proto.Clone(metadata).(*api.ConfigurationPlan)
	return record.clone(), nil
}

type configurationPlanPageCursor struct {
	Capture [32]byte `json:"capture"`
	Parent  string   `json:"parent"`
	Binding [32]byte `json:"binding"`
	After   string   `json:"after"`
}

func (c *configurationPlanCatalog) encode(cursor configurationPlanPageCursor) (string, error) {
	data, err := json.Marshal(cursor)
	if err != nil {
		return "", apierrors.New(api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE)
	}
	mac := hmac.New(sha256.New, c.key[:])
	_, _ = mac.Write(data)
	data = append(data, mac.Sum(nil)...)
	return base64.RawURLEncoding.EncodeToString(data), nil
}
func (c *configurationPlanCatalog) decode(token string) (configurationPlanPageCursor, error) {
	fail := func() (configurationPlanPageCursor, error) {
		return configurationPlanPageCursor{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED)
	}
	if len(token) > 1200 {
		return fail()
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(raw) < sha256.Size {
		return fail()
	}
	data, signature := raw[:len(raw)-sha256.Size], raw[len(raw)-sha256.Size:]
	mac := hmac.New(sha256.New, c.key[:])
	_, _ = mac.Write(data)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return fail()
	}
	var cursor configurationPlanPageCursor
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cursor) != nil {
		return fail()
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return fail()
	}
	return cursor, nil
}
func (c *configurationPlanCatalog) list(home apitypes.HomeName, binding configDocumentsBinding, size int32, token string) ([]configurationPlanRecord, string, error) {
	if c == nil || !c.valid || !binding.valid() || !home.Valid() || size < 0 {
		return nil, "", apierrors.New(api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED)
	}
	if size == 0 || size > 100 {
		size = 100
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.purgeLocked()
	keys := make([]string, 0, len(c.records))
	for key, record := range c.records {
		if record.binding == binding && record.name.Home() == home {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	hash := sha256.New()
	for _, key := range keys {
		record := c.records[key]
		_, _ = hash.Write([]byte(key))
		var version [8]byte
		binary.BigEndian.PutUint64(version[:], record.version)
		_, _ = hash.Write(version[:])
	}
	var capture [32]byte
	copy(capture[:], hash.Sum(nil))
	after := ""
	if token != "" {
		cursor, err := c.decode(token)
		if err != nil {
			return nil, "", err
		}
		if cursor.Capture != capture || cursor.Parent != home.String() || cursor.Binding != binding.digest {
			return nil, "", apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED)
		}
		after = cursor.After
	}
	filtered := keys[:0]
	for _, key := range keys {
		if key > after {
			filtered = append(filtered, key)
		}
	}
	keys = filtered
	count := len(keys)
	more := count > int(size)
	if more {
		count = int(size)
	}
	result := make([]configurationPlanRecord, 0, count)
	for _, key := range keys[:count] {
		result = append(result, c.records[key].clone())
	}
	next := ""
	if more {
		var err error
		next, err = c.encode(configurationPlanPageCursor{Capture: capture, Parent: home.String(), Binding: binding.digest, After: keys[count-1]})
		if err != nil {
			return nil, "", err
		}
	}
	return result, next, nil
}

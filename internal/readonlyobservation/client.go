package readonlyobservation

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Config struct {
	URL            string  `json:"url"`
	CAFile         string  `json:"ca_file"`
	CredentialFile string  `json:"credential_file"`
	Instance       string  `json:"integration_instance"`
	Expected       Binding `json:"expected"`
}
type credentials struct {
	Login    string `json:"login_name"`
	Password string `json:"password"`
}
type NativeSource struct {
	config          Config
	auth            apiv2connect.AuthServiceClient
	observation     apiv2connect.ObservabilityServiceClient
	homes           apiv2connect.HomeServiceClient
	views           apiv2connect.EntityViewServiceClient
	access, refresh string
	expires         time.Time
	renewalKey      []byte
	epoch           string
}
type ReadError struct{ Code string }

func (e *ReadError) Error() string { return "observation read rejected: " + e.Code }
func readError(err error) error    { return &ReadError{Code: connect.CodeOf(err).String()} }

func LoadConfig(path string) (Config, error) {
	var c Config
	if err := readProtectedJSON(path, &c); err != nil {
		return c, err
	}
	if err := validateConfig(c); err != nil {
		return Config{}, err
	}
	return c, nil
}
func validateConfig(c Config) error {
	u, err := url.Parse(c.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || c.CredentialFile == "" || c.Instance == "" || len(c.Instance) > 256 || c.Expected.Profile != "mqtt-passive-readonly-v1" {
		return ErrSample
	}
	if _, err := apitypes.ParseIntegrationInstanceName(c.Instance); err != nil {
		return ErrSample
	}
	for _, s := range []string{c.Expected.Executable, c.Expected.Manifest, c.Expected.Source} {
		b, e := hex.DecodeString(s)
		if e != nil || len(b) != 32 {
			return ErrSample
		}
	}
	return nil
}
func readProtectedJSON(path string, target any) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 64<<10 {
		return ErrSample
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ErrSample
	}
	defer clear(raw)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		return ErrSample
	}
	if decoder.Decode(new(any)) != io.EOF {
		return ErrSample
	}
	return nil
}
func NewNativeSource(c Config) (*NativeSource, error) {
	if err := validateConfig(c); err != nil {
		return nil, err
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if c.CAFile != "" {
		raw, err := os.ReadFile(c.CAFile)
		if err != nil || len(raw) > 1<<20 || !roots.AppendCertsFromPEM(raw) {
			return nil, ErrSample
		}
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &NativeSource{config: c, auth: apiv2connect.NewAuthServiceClient(client, c.URL, connect.WithReadMaxBytes(64<<10)), observation: apiv2connect.NewObservabilityServiceClient(client, c.URL, connect.WithReadMaxBytes(2<<20)), homes: apiv2connect.NewHomeServiceClient(client, c.URL, connect.WithReadMaxBytes(64<<10)), views: apiv2connect.NewEntityViewServiceClient(client, c.URL, connect.WithReadMaxBytes(2<<20))}, nil
}
func (n *NativeSource) session(ctx context.Context) error {
	if len(n.renewalKey) == 0 && n.access != "" && time.Until(n.expires) > time.Minute {
		return nil
	}
	if n.refresh != "" {
		if len(n.renewalKey) == 0 {
			var key [32]byte
			if _, err := rand.Read(key[:]); err != nil {
				return ErrSample
			}
			// This source never persists bearer tokens. A retry within its lifetime
			// keeps this exact key; restarting creates a fresh password session.
			n.renewalKey = append([]byte(nil), key[:]...)
		}
		if len(n.renewalKey) != 32 {
			return ErrSample
		}
		response, err := n.auth.RefreshSession(ctx, connect.NewRequest(&api.RefreshSessionRequest{RefreshToken: n.refresh, RenewalKey: n.renewalKey}))
		if err != nil {
			return readError(err)
		}
		// No hidden password login after a failed or ambiguous renewal. Keep both
		// the old family token and pending key until an admitted successor arrives.
		if response == nil || response.Msg == nil || response.Msg.Tokens == nil {
			return ErrSample
		}
		return n.tokens(response.Msg.Tokens)
	}
	var credential credentials
	if err := readProtectedJSON(n.config.CredentialFile, &credential); err != nil {
		return err
	}
	defer func() { credential.Password = "" }()
	response, err := n.auth.StartLogin(ctx, connect.NewRequest(&api.StartLoginRequest{LoginName: credential.Login, Password: credential.Password}))
	if err != nil {
		return readError(err)
	}
	tokens := response.Msg.GetDecision().GetAuthenticated()
	if tokens == nil {
		return &ReadError{Code: "unauthenticated"}
	}
	return n.tokens(tokens)
}
func (n *NativeSource) tokens(tokens *api.SessionTokens) error {
	if contracts.AdmitAPISessionTokens(tokens, time.Now()) != nil {
		return ErrSample
	}
	n.access, n.refresh, n.expires = tokens.AccessToken, tokens.RefreshToken, tokens.AccessExpireTime.AsTime()
	clear(n.renewalKey)
	n.renewalKey = nil
	return nil
}

func bearer[T any](message *T, token string) *connect.Request[T] {
	r := connect.NewRequest(message)
	r.Header().Set("Authorization", "Bearer "+token)
	return r
}
func safeJSON(message proto.Message) (json.RawMessage, error) {
	if contracts.AdmitObservationResponse(message) != nil {
		return nil, ErrSample
	}
	raw, err := protojson.Marshal(message)
	if err != nil {
		return nil, ErrSample
	}
	return raw, nil
}
func (n *NativeSource) Read(ctx context.Context, lastLog uint64) (Sample, error) {
	var sample Sample
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	if err := n.session(ctx); err != nil {
		return sample, err
	}
	instance, err := apitypes.ParseIntegrationInstanceName(n.config.Instance)
	if err != nil {
		return sample, ErrSample
	}
	parent := instance.Home().String()
	health, err := n.observation.GetHealth(ctx, bearer(&api.GetHealthRequest{Name: parent + "/health"}, n.access))
	if err != nil {
		return sample, n.reject(err)
	}
	sample.Binding = Binding{health.Header().Get("Homeward-Observation-Profile"), health.Header().Get("Homeward-Executable-SHA256"), health.Header().Get("Homeward-Manifest-SHA256"), health.Header().Get("Homeward-Source-SHA256")}
	if sample.Binding != n.config.Expected {
		return sample, &ReadError{Code: "binding_mismatch"}
	}
	sample.DriverEntries, err = strconv.ParseUint(health.Header().Get("Homeward-Command-Driver-Entries"), 10, 64)
	if err != nil {
		return sample, ErrSample
	}
	sample.Health, err = safeJSON(health.Msg)
	if err != nil {
		return sample, err
	}
	if health.Msg.Name != parent+"/health" {
		return sample, ErrSample
	}
	if health.Msg.GetServer().GetReadiness() != api.ObservationServerReadiness_OBSERVATION_SERVER_READINESS_READY {
		return sample, &ReadError{Code: "server_unready"}
	}
	revision := health.Msg.GetServer().GetGeneration().GetPresent().GetValue()
	if revision <= 0 {
		return sample, ErrSample
	}
	sample.ConfigRevision = uint64(revision)
	sample.Epoch = hex.EncodeToString(health.Msg.GetProcessEpoch().GetValue())
	if sample.Epoch == "" || sample.ConfigRevision == 0 {
		return sample, ErrSample
	}
	sample.ServerRSSBytes = headerUint(health.Header(), "Homeward-Process-RSS-Bytes")
	sample.ServerCPUMicroseconds = headerUint(health.Header(), "Homeward-Process-CPU-Microseconds")
	sample.ResourceSource = "server_process:/proc/self/statm,getrusage; missing values are unmeasured"
	sample.HAComparison = "unavailable"
	metrics, err := n.observation.GetMetrics(ctx, bearer(&api.GetMetricsRequest{Name: parent + "/metrics"}, n.access))
	if err != nil {
		return sample, n.reject(err)
	}
	sample.Metrics, err = safeJSON(metrics.Msg)
	if err != nil {
		return sample, err
	}
	if metrics.Msg.Name != parent+"/metrics" || !bytes.Equal(metrics.Msg.GetProcessEpoch().GetValue(), health.Msg.GetProcessEpoch().GetValue()) {
		return sample, ErrSample
	}
	integration, err := n.observation.GetIntegrationHealth(ctx, bearer(&api.GetIntegrationHealthRequest{Name: n.config.Instance + "/integrationHealth"}, n.access))
	if err != nil {
		return sample, n.reject(err)
	}
	if integration.Msg.Name != n.config.Instance+"/integrationHealth" {
		return sample, ErrSample
	}
	attempts := integration.Msg.GetSupervision().GetSupervised().GetAttempts()
	if attempts < 0 {
		return sample, ErrSample
	}
	sample.PilotAttempts = uint64(attempts)
	sample.Integration, err = safeJSON(integration.Msg)
	if err != nil {
		return sample, err
	}
	if err = n.readStates(ctx, &sample); err != nil {
		return sample, err
	}
	if n.epoch != sample.Epoch {
		lastLog = 0
		n.epoch = sample.Epoch
	}
	if err = n.readLogs(ctx, lastLog, &sample); err != nil {
		return sample, err
	}

	closing, err := n.observation.GetHealth(ctx, bearer(&api.GetHealthRequest{Name: parent + "/health"}, n.access))
	if err != nil {
		return sample, n.reject(err)
	}
	if contracts.AdmitObservationResponse(closing.Msg) != nil || closing.Msg.GetServer().GetReadiness() != api.ObservationServerReadiness_OBSERVATION_SERVER_READINESS_READY || !bytes.Equal(closing.Msg.GetProcessEpoch().GetValue(), health.Msg.GetProcessEpoch().GetValue()) || closing.Msg.GetServer().GetGeneration().GetPresent().GetValue() != revision {
		return sample, ErrSample
	}
	if closing.Header().Get("Homeward-Observation-Profile") != sample.Binding.Profile || closing.Header().Get("Homeward-Executable-SHA256") != sample.Binding.Executable || closing.Header().Get("Homeward-Manifest-SHA256") != sample.Binding.Manifest || closing.Header().Get("Homeward-Source-SHA256") != sample.Binding.Source {
		return sample, &ReadError{Code: "binding_mismatch"}
	}
	sample.DriverEntries, err = strconv.ParseUint(closing.Header().Get("Homeward-Command-Driver-Entries"), 10, 64)
	if err != nil {
		return sample, ErrSample
	}
	return sample, nil
}
func (n *NativeSource) reject(err error) error {
	if connect.CodeOf(err) == connect.CodeUnauthenticated {
		n.access = ""
		n.expires = time.Time{}
	}
	return readError(err)
}
func headerUint(header http.Header, key string) *uint64 {
	v, err := strconv.ParseUint(header.Get(key), 10, 64)
	if err != nil {
		return nil
	}
	return &v
}
func (n *NativeSource) readStates(ctx context.Context, sample *Sample) error {
	homes, err := n.homes.ListHomes(ctx, bearer(&api.ListHomesRequest{PageSize: 10}, n.access))
	if err != nil {
		return n.reject(err)
	}
	if len(homes.Msg.GetHomes()) != 1 || homes.Msg.GetHomes()[0].GetName() == "" {
		return ErrSample
	}
	created, err := n.views.CreateEntityView(ctx, bearer(&api.CreateEntityViewRequest{Parent: homes.Msg.GetHomes()[0].GetName(), EntityView: &api.EntityView{}}, n.access))
	if err != nil {
		return n.reject(err)
	}
	viewName := created.Msg.GetName()
	if viewName == "" {
		return ErrSample
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_, _ = n.views.DeleteEntityView(releaseCtx, bearer(&api.DeleteEntityViewRequest{Name: viewName}, n.access))
	}()
	var pageToken string
	for pages := 0; ; pages++ {
		if pages >= 1024 {
			return ErrSample
		}
		response, err := n.views.QueryEntityView(ctx, bearer(&api.QueryEntityViewRequest{Name: viewName, PageSize: 100, PageToken: pageToken}, n.access))
		if err != nil {
			return n.reject(err)
		}
		page := response.Msg
		if page == nil || len(page.Entries) > 100 {
			return ErrSample
		}
		for _, entry := range page.Entries {
			summarizeEntry(&sample.States, entry)
		}
		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}
	return nil
}
func capabilityObservation(capability *api.Capability) (*api.ObservationQuality, *timestamppb.Timestamp) {
	if capability == nil {
		return nil, nil
	}
	message := capability.ProtoReflect()
	field := message.WhichOneof(message.Descriptor().Oneofs().ByName("trait"))
	if field == nil {
		return nil, nil
	}
	trait := message.Get(field).Message()
	if !trait.IsValid() {
		return nil, nil
	}
	obsField := trait.Descriptor().Fields().ByName("observation")
	if obsField == nil || obsField.Kind() != protoreflect.MessageKind || !trait.Has(obsField) {
		return nil, nil
	}
	reading := trait.Get(obsField).Message()
	var quality *api.ObservationQuality
	if qField := reading.Descriptor().Fields().ByName("quality"); qField != nil && qField.Kind() == protoreflect.MessageKind && reading.Has(qField) {
		quality, _ = reading.Get(qField).Message().Interface().(*api.ObservationQuality)
	}
	var observed *timestamppb.Timestamp
	if tField := reading.Descriptor().Fields().ByName("observed_time"); tField != nil && tField.Kind() == protoreflect.MessageKind && reading.Has(tField) {
		observed, _ = reading.Get(tField).Message().Interface().(*timestamppb.Timestamp)
	}
	return quality, observed
}
func summarizeEntry(summary *StateSummary, entry *api.EntityViewEntry) {
	summary.Total++
	var quality *api.ObservationQuality
	var observed *timestamppb.Timestamp
	for _, capability := range entry.GetCapabilities() {
		q, t := capabilityObservation(capability)
		if q != nil || t != nil {
			quality, observed = q, t
			break
		}
	}
	if quality == nil && observed == nil {
		summary.Unknown++
		summary.MissingTimestamp++
		return
	}
	switch quality.GetAvailability() {
	case api.Availability_AVAILABILITY_AVAILABLE:
		summary.Available++
	case api.Availability_AVAILABILITY_UNAVAILABLE:
		summary.Unavailable++
	default:
		summary.Unknown++
	}
	switch quality.GetFreshness() {
	case api.Freshness_FRESHNESS_CURRENT:
		summary.Fresh++
	case api.Freshness_FRESHNESS_STALE:
		summary.Stale++
	}
	if observed == nil || observed.CheckValid() != nil {
		summary.MissingTimestamp++
		return
	}
	age := time.Since(observed.AsTime()).Seconds()
	if age > summary.OldestReceivedAgeSeconds {
		summary.OldestReceivedAgeSeconds = age
	}
}
func (n *NativeSource) readLogs(ctx context.Context, last uint64, sample *Sample) error {
	var token string
	var upper uint64
	for pages := 0; ; pages++ {
		if pages > 11 {
			return ErrSample
		}
		instance, err := apitypes.ParseIntegrationInstanceName(n.config.Instance)
		if err != nil {
			return ErrSample
		}
		size := int32(100)
		response, err := n.observation.QueryRuntimeLogs(ctx, bearer(&api.QueryRuntimeLogsRequest{Parent: instance.Home().String(), PageSize: size, PageToken: token}, n.access))
		if err != nil {
			return n.reject(err)
		}
		if contracts.AdmitObservationResponse(response.Msg) != nil {
			return ErrSample
		}
		page := response.Msg
		if page == nil {
			sample.JournalGap = true
			return nil
		}
		if hex.EncodeToString(page.ProcessEpoch.GetValue()) != sample.Epoch {
			return ErrSample
		}
		pageUpper, parseErr := apitypes.ParseDecimalUint64(page.UpperSequence)
		if parseErr != nil {
			return ErrSample
		}
		if pages == 0 {
			upper = pageUpper.Value()
		} else if upper != pageUpper.Value() {
			return ErrSample
		}
		for _, row := range page.Rows {
			sequence, parseErr := apitypes.ParseDecimalUint64(row.Sequence)
			if parseErr != nil {
				return ErrSample
			}
			if sample.LogFirst == 0 {
				sample.LogFirst = sequence.Value()
			}
			if sequence.Value() > last {
				raw, err := sanitizedRuntimeLog(row)
				if err != nil {
					return ErrSample
				}
				sample.Logs = append(sample.Logs, raw)
			}
		}
		next := page.GetNextPageToken()
		if next == "" {
			break
		}
		token = next
	}
	sample.LogUpper = upper
	if last > 0 && (upper < last || (sample.LogFirst > last && sample.LogFirst-last > 1)) {
		sample.JournalGap = true
	}
	return nil
}

// Runtime authors may contain a real human account ID. Durable qualification
// evidence needs only finite code/level/component/time and its epoch cursor.
func sanitizedRuntimeLog(row *api.ObservationRuntimeLogRow) (json.RawMessage, error) {
	if row == nil {
		return nil, ErrSample
	}
	sequence, err := apitypes.ParseDecimalUint64(row.Sequence)
	if err != nil {
		return nil, ErrSample
	}
	return json.Marshal(struct {
		Sequence   uint64 `json:"sequence"`
		Code       string `json:"code"`
		Level      string `json:"level"`
		Component  string `json:"component"`
		OccurredAt string `json:"occurred_at"`
	}{sequence.Value(), row.GetEvent().GetCode().String(), row.GetEvent().GetLevel().String(), row.GetEvent().GetComponent().String(), row.GetEvent().GetOccurredAt().AsTime().UTC().Format(time.RFC3339Nano)})
}

package readonlyobservation

import (
	"bytes"
	"context"
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
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/gen/home/observability/v1/observabilityv1connect"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/gen/home/state/v1/statev1connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
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
	auth            authv1connect.AuthServiceClient
	observation     observabilityv1connect.ObservabilityServiceClient
	states          statev1connect.StateServiceClient
	access, refresh string
	expires         time.Time
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
	return &NativeSource{config: c, auth: authv1connect.NewAuthServiceClient(client, c.URL, connect.WithReadMaxBytes(64<<10)), observation: observabilityv1connect.NewObservabilityServiceClient(client, c.URL, connect.WithReadMaxBytes(2<<20)), states: statev1connect.NewStateServiceClient(client, c.URL, connect.WithReadMaxBytes(2<<20))}, nil
}
func (n *NativeSource) session(ctx context.Context) error {
	if n.access != "" && time.Until(n.expires) > time.Minute {
		return nil
	}
	if n.refresh != "" {
		response, err := n.auth.RefreshSession(ctx, connect.NewRequest(&auth.RefreshSessionRequest{RefreshToken: n.refresh}))
		if err == nil && response.Msg.Tokens != nil {
			return n.tokens(response.Msg.Tokens)
		}
		// Do not replay a refresh token whose consumed outcome is unknown. A fresh
		// password login is safe for this isolated account and is bounded to once.
		n.access, n.refresh = "", ""
	}
	var credential credentials
	if err := readProtectedJSON(n.config.CredentialFile, &credential); err != nil {
		return err
	}
	defer func() { credential.Password = "" }()
	response, err := n.auth.StartLogin(ctx, connect.NewRequest(&auth.StartLoginRequest{LoginName: credential.Login, Password: credential.Password}))
	if err != nil {
		return readError(err)
	}
	tokens := response.Msg.GetDecision().GetAuthenticated()
	if tokens == nil {
		return &ReadError{Code: "unauthenticated"}
	}
	return n.tokens(tokens)
}
func (n *NativeSource) tokens(tokens *auth.SessionTokens) error {
	if tokens == nil || tokens.AccessToken == "" || tokens.RefreshToken == "" || tokens.AccessExpiresAt == nil || tokens.AccessExpiresAt.CheckValid() != nil {
		return ErrSample
	}
	n.access, n.refresh, n.expires = tokens.AccessToken, tokens.RefreshToken, tokens.AccessExpiresAt.AsTime()
	return nil
}
func bearer[T any](message *T, token string) *connect.Request[T] {
	r := connect.NewRequest(message)
	r.Header().Set("Authorization", "Bearer "+token)
	return r
}
func safeJSON(message proto.Message) (json.RawMessage, error) {
	if contracts.AdmitObservabilityResponse(message) != nil {
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
	health, err := n.observation.GetHealth(ctx, bearer(&obs.GetHealthRequest{}, n.access))
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
	if health.Msg.GetHealth().GetServer().GetReadiness() != obs.ServerReadiness_SERVER_READINESS_READY {
		return sample, &ReadError{Code: "server_unready"}
	}
	sample.ConfigRevision = health.Msg.GetHealth().GetServer().GetGeneration().GetPresent().GetValue()
	sample.Epoch = hex.EncodeToString(health.Msg.GetHealth().GetProcessEpoch().GetValue())
	if sample.Epoch == "" || sample.ConfigRevision == 0 {
		return sample, ErrSample
	}
	sample.ServerRSSBytes = headerUint(health.Header(), "Homeward-Process-RSS-Bytes")
	sample.ServerCPUMicroseconds = headerUint(health.Header(), "Homeward-Process-CPU-Microseconds")
	sample.ResourceSource = "server_process:/proc/self/statm,getrusage; missing values are unmeasured"
	sample.HAComparison = "unavailable"
	metrics, err := n.observation.GetMetrics(ctx, bearer(&obs.GetMetricsRequest{}, n.access))
	if err != nil {
		return sample, n.reject(err)
	}
	sample.Metrics, err = safeJSON(metrics.Msg)
	if err != nil {
		return sample, err
	}
	if !bytes.Equal(metrics.Msg.GetMetrics().GetProcessEpoch().GetValue(), health.Msg.GetHealth().GetProcessEpoch().GetValue()) {
		return sample, ErrSample
	}
	integration, err := n.observation.GetIntegrationHealth(ctx, bearer(&obs.GetIntegrationHealthRequest{IntegrationInstanceId: n.config.Instance}, n.access))
	if err != nil {
		return sample, n.reject(err)
	}
	sample.PilotAttempts = integration.Msg.GetSupervision().GetSupervised().GetAttempts()
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

	closing, err := n.observation.GetHealth(ctx, bearer(&obs.GetHealthRequest{}, n.access))
	if err != nil {
		return sample, n.reject(err)
	}
	if contracts.AdmitObservabilityResponse(closing.Msg) != nil || closing.Msg.GetHealth().GetServer().GetReadiness() != obs.ServerReadiness_SERVER_READINESS_READY || !bytes.Equal(closing.Msg.GetHealth().GetProcessEpoch().GetValue(), health.Msg.GetHealth().GetProcessEpoch().GetValue()) || closing.Msg.GetHealth().GetServer().GetGeneration().GetPresent().GetValue() != sample.ConfigRevision {
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
	first, err := n.states.OpenView(ctx, bearer(&state.OpenViewRequest{Filter: &state.ViewFilter{}, PageSize: 100}, n.access))
	if err != nil {
		return n.reject(err)
	}
	page := first.Msg
	for pages := 0; ; pages++ {
		if pages >= 1024 || page == nil || page.Cursor == nil || page.Cursor.ConfigRevision != sample.ConfigRevision || len(page.States) > 100 || len(page.Entities) > 100 {
			return ErrSample
		}
		// The server's entity capture may carry state either inside entities or in
		// the legacy states field. Count each once, never persist values/aliases.
		if len(page.Entities) > 0 {
			for _, entity := range page.Entities {
				summarizeState(&sample.States, entity.GetState())
			}
		} else {
			for _, value := range page.States {
				summarizeState(&sample.States, value)
			}
		}
		if page.NextPageToken == "" {
			break
		}
		response, err := n.states.ReadPage(ctx, bearer(&state.ReadPageRequest{SnapshotToken: page.SnapshotToken, PageToken: page.NextPageToken}, n.access))
		if err != nil {
			return n.reject(err)
		}
		page = response.Msg.Page
	}
	return nil
}
func summarizeState(summary *StateSummary, value *state.ObservedState) {
	summary.Total++
	if value == nil {
		summary.Unknown++
		summary.MissingTimestamp++
		return
	}
	switch value.GetQuality().GetAvailability() {
	case state.Availability_AVAILABILITY_AVAILABLE:
		summary.Available++
	case state.Availability_AVAILABILITY_UNAVAILABLE:
		summary.Unavailable++
	default:
		summary.Unknown++
	}
	switch value.GetQuality().GetFreshness() {
	case state.Freshness_FRESHNESS_FRESH:
		summary.Fresh++
	case state.Freshness_FRESHNESS_STALE:
		summary.Stale++
	}
	if value.ReceivedAt == nil || value.ReceivedAt.CheckValid() != nil {
		summary.MissingTimestamp++
		return
	}
	age := time.Since(value.ReceivedAt.AsTime()).Seconds()
	if age > summary.OldestReceivedAgeSeconds {
		summary.OldestReceivedAgeSeconds = age
	}
}
func (n *NativeSource) readLogs(ctx context.Context, last uint64, sample *Sample) error {
	var token *string
	var upper uint64
	for pages := 0; ; pages++ {
		if pages > 11 {
			return ErrSample
		}
		size := uint32(100)
		response, err := n.observation.QueryRuntimeLogs(ctx, bearer(&obs.QueryRuntimeLogsRequest{PageSize: &size, PageToken: token}, n.access))
		if err != nil {
			return n.reject(err)
		}
		if contracts.AdmitObservabilityResponse(response.Msg) != nil {
			return ErrSample
		}
		page := response.Msg.GetPage()
		if page == nil {
			sample.JournalGap = true
			return nil
		}
		if hex.EncodeToString(page.ProcessEpoch.GetValue()) != sample.Epoch {
			return ErrSample
		}
		if pages == 0 {
			upper = page.UpperSequence
		} else if upper != page.UpperSequence {
			return ErrSample
		}
		for _, row := range page.Rows {
			if sample.LogFirst == 0 {
				sample.LogFirst = row.Sequence
			}
			if row.Sequence > last {
				raw, err := sanitizedRuntimeLog(row)
				if err != nil {
					return ErrSample
				}
				sample.Logs = append(sample.Logs, raw)
			}
		}
		next := page.GetContinuation().GetNextPageToken()
		if next == "" {
			break
		}
		token = &next
	}
	sample.LogUpper = upper
	if last > 0 && (upper < last || (sample.LogFirst > last+1)) {
		sample.JournalGap = true
	}
	return nil
}

// Runtime authors may contain a real human account ID. Durable qualification
// evidence needs only finite code/level/component/time and its epoch cursor.
func sanitizedRuntimeLog(row *obs.RuntimeLogRow) (json.RawMessage, error) {
	return json.Marshal(struct {
		Sequence   uint64 `json:"sequence"`
		Code       string `json:"code"`
		Level      string `json:"level"`
		Component  string `json:"component"`
		OccurredAt string `json:"occurred_at"`
	}{row.Sequence, row.GetEvent().GetCode().String(), row.GetEvent().GetLevel().String(), row.GetEvent().GetComponent().String(), row.GetEvent().GetOccurredAt().AsTime().UTC().Format(time.RFC3339Nano)})
}

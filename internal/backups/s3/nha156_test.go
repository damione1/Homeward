package s3

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/damione1/Homeward/internal/backups"
	archive "github.com/damione1/Homeward/internal/backups/archive"
	"github.com/damione1/Homeward/internal/backups/s3/s3test"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
)

type fixtureSecrets struct {
	mu    sync.Mutex
	raw   string
	reads int
	fail  bool
	ref   storage.SecretHandle
}

func (f *fixtureSecrets) Read(_ context.Context, h storage.SecretHandle) (storage.SecretValue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.fail || h != f.ref {
		return storage.SecretValue{}, errors.New("NHA156SECRET")
	}
	return storage.NewSecretValue([]byte(f.raw))
}
func ref156() storage.SecretHandle {
	i, _ := domain.ParseIntegrationInstanceID("nha156-credentials")
	n, _ := storage.ParseSecretName("credential-bundle")
	h, _ := storage.NewSecretHandle(i, n)
	return h
}
func settings156(t *testing.T, endpoint string, enabled bool) backups.InstanceSettings {
	t.Helper()
	id, _ := backups.ParseInstanceID("offsite")
	s, err := backups.NewS3Settings(id, enabled, endpoint, "nha156-bucket", "homeward/test", "us-east-1", ref156(), backups.DefaultS3Limits())
	if err != nil {
		t.Fatal("admit fixture S3 settings")
	}
	return s
}
func setup156(t *testing.T) (*provider, *s3test.Server, *fixtureSecrets) {
	t.Helper()
	s := s3test.New()
	t.Cleanup(s.Close)
	r := &fixtureSecrets{raw: s3test.Bundle, ref: ref156()}
	p, err := open(context.Background(), settings156(t, s.URL(), true), r, filepath.Join(t.TempDir(), "private"), nil)
	if err != nil {
		t.Fatal("open S3 fixture")
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, s, r
}
func id156(t *testing.T, n int) backups.ArtifactID {
	t.Helper()
	id, err := backups.ParseArtifactID(fmt.Sprintf("156%029d", n))
	if err != nil {
		t.Fatal("admit ID")
	}
	return id
}
func write156(p backups.Provider, id backups.ArtifactID, raw []byte) (backups.WriteResult, error) {
	intent, _ := backups.NewWriteIntent(id, int64(len(raw)))
	return p.Write(context.Background(), intent, bytes.NewReader(raw))
}
func success156(t *testing.T, p backups.Provider, id backups.ArtifactID, raw []byte) {
	t.Helper()
	r, err := write156(p, id, raw)
	if err != nil || r.State() != backups.Published {
		t.Fatal("expected verified publication")
	}
}
func private156(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "private")
	if os.Mkdir(p, 0700) != nil {
		t.Fatal("private fixture")
	}
	return p
}

func TestNHA156DisabledNoNetworkOrSecrets(t *testing.T) {
	s := s3test.New()
	defer s.Close()
	r := &fixtureSecrets{raw: s3test.Bundle, ref: ref156()}
	f, err := NewFactory(r, filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatal(err)
	}
	plan, _ := backups.NewConfiguration([]backups.InstanceSettings{settings156(t, s.URL(), false)})
	registry, err := backups.NewRegistry(context.Background(), plan, []backups.Factory{f})
	if err != nil {
		t.Fatal("disabled registry")
	}
	defer registry.Close()
	if len(s.Requests()) != 0 || r.reads != 0 {
		t.Fatal("disabled S3 performed I/O")
	}
	if _, err := os.Stat(f.directory); !os.IsNotExist(err) {
		t.Fatal("disabled factory created staging")
	}
	if _, err := registry.Instance(settings156(t, s.URL(), false).ID()); !errorIs(err, backups.NotFound) {
		t.Fatal("disabled instance borrowed")
	}
	var nilReader *fixtureSecrets
	if _, err := NewFactory(nilReader, "/tmp/private"); err == nil {
		t.Fatal("typed nil reader admitted")
	}
}
func TestNHA156SealedRoundTripCommonPort(t *testing.T) {
	p, s, _ := setup156(t)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal("identity fixture")
	}
	public, err := archive.ParseRecoveryPublic(identity.Recipient().String())
	if err != nil {
		t.Fatal(err)
	}
	private, err := archive.ParseRecoveryPrivate([]byte(identity.String()))
	if err != nil {
		t.Fatal(err)
	}
	keyText := "HOMEWARD_SECRETS_KEY_V1=" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x56}, 32)) + "\n"
	key, err := storage.ParseBackupServerKey([]byte(keyText))
	if err != nil {
		t.Fatal(err)
	}
	const yaml = "nha156_yaml: NHA156_PLAINTEXT_YAML"
	source, _ := archive.NewYAMLSource("configuration.yaml", int64(len(yaml)), strings.NewReader(yaml))
	const dump = "PGDMP_NHA156_PLAINTEXT_DATABASE"
	input, _ := archive.NewInput(strings.NewReader(dump), int64(len(dump)), []archive.YAMLSource{source}, key)
	image, err := archive.Seal(context.Background(), filepath.Join(private156(t), "cipher"), id156(t, 1), public, input, archive.DefaultLimits())
	if err != nil {
		t.Fatal("seal155")
	}
	defer image.Close()
	published, err := image.Publish(context.Background(), p)
	if err != nil || published.State() != backups.Published {
		t.Fatal("sealed155 through S3 port")
	}
	for name, o := range s.Objects() {
		if !strings.HasPrefix(name, "homeward/test/offsite/") {
			t.Fatal("namespace")
		}
		for _, secret := range []string{"NHA156_PLAINTEXT_YAML", "NHA156_PLAINTEXT_DATABASE", keyText, identity.String(), "NHA156SECRET"} {
			if bytes.Contains(o.Data, []byte(secret)) {
				t.Fatal("plaintext reached S3")
			}
		}
		if !bytes.HasPrefix(o.Data, []byte("age-encryption.org/v1\n")) {
			t.Fatal("not raw age ciphertext")
		}
	}
	parent := filepath.Join(private156(t), "restore")
	recovered, err := archive.RecoverProvider(context.Background(), p, image.ID(), private, parent, "complete", archive.DefaultLimits())
	if err != nil || recovered.State() != archive.Restored {
		t.Fatal("independent recovery through common Open")
	}
	raw, err := os.ReadFile(filepath.Join(parent, "complete", "yaml", "configuration.yaml"))
	if err != nil || string(raw) != yaml {
		t.Fatal("recovered YAML differs")
	}
	for n := 2; n <= 4; n++ {
		success156(t, p, id156(t, n), []byte("opaque test ciphertext"))
	}
	request, _ := backups.NewListRequest(2, backups.Cursor{})
	page, err := p.List(context.Background(), request)
	if err != nil || len(page.Items()) != 2 || !page.Next().Present() {
		t.Fatal("first paginated page")
	}
	request, _ = backups.NewListRequest(2, page.Next())
	last, err := p.List(context.Background(), request)
	if err != nil || len(last.Items()) != 2 || last.Next().Present() {
		t.Fatal("second paginated page")
	}
	for _, request := range s.Requests() {
		if !request.Signed {
			t.Fatal("SDK did not sign request")
		}
		if request.Method == "PUT" && (!request.Conditional || !request.Checksum) {
			t.Fatal("missing conditional/checksum")
		}
	}
	if r, err := p.Delete(context.Background(), image.ID()); err != nil || r.State() != backups.Deleted {
		t.Fatal("delete")
	}
	if r, err := p.Delete(context.Background(), image.ID()); err != nil || r.State() != backups.AlreadyAbsent {
		t.Fatal("delete absent")
	}
}

type badSource struct{}

func (badSource) Read([]byte) (int, error) { return 0, errors.New("NHA156SECRET") }
func TestNHA156InterruptedAndAmbiguousPublication(t *testing.T) {
	for _, mode := range []string{"drop-upload", "drop-response", "corrupt-get", "truncate-get", "deny", "fail", "large-error"} {
		t.Run(mode, func(t *testing.T) {
			p, s, _ := setup156(t)
			s.Mode(mode)
			r, err := write156(p, id156(t, 1), []byte("opaque ciphertext"))
			if err == nil || r.State() != backups.Indeterminate {
				t.Fatal("failure announced definite publication")
			}
			assertRedacted156(t, err)
			puts := 0
			for _, request := range s.Requests() {
				if request.Method == "PUT" {
					puts++
				}
			}
			if puts != 1 {
				t.Fatal("hidden mutation retry")
			}
			s.Mode("")
			v, verifyErr := p.Verify(context.Background(), id156(t, 1))
			if mode == "drop-response" || mode == "corrupt-get" || mode == "truncate-get" {
				if verifyErr != nil || !v.Valid() {
					t.Fatal("committed ambiguous object not recoverable")
				}
			} else if verifyErr == nil {
				t.Fatal("uncommitted object falsely visible")
			}
		})
	}
	p, s, _ := setup156(t)
	intent, _ := backups.NewWriteIntent(id156(t, 2), 10)
	r, err := p.Write(context.Background(), intent, badSource{})
	if err == nil || r.State() != backups.NotPublished || len(s.Requests()) != 0 {
		t.Fatal("producer failure reached network")
	}
	intent, _ = backups.NewWriteIntent(id156(t, 3), 1)
	r, err = p.Write(context.Background(), intent, strings.NewReader("too large"))
	if err == nil || r.State() != backups.NotPublished || len(s.Requests()) != 0 {
		t.Fatal("capacity failure reached network")
	}
}
func TestNHA156ConditionalConcurrentWrites(t *testing.T) {
	p, s, _ := setup156(t)
	id := id156(t, 1)
	var wg sync.WaitGroup
	results := make(chan backups.WriteResult, 2)
	failures := make(chan error, 2)
	for n := 0; n < 2; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := write156(p, id, []byte("same ciphertext"))
			results <- r
			failures <- e
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal("concurrent same bytes failed")
		}
	}
	published, present := 0, 0
	for r := range results {
		switch r.State() {
		case backups.Published:
			published++
		case backups.AlreadyPresent:
			present++
		default:
			t.Fatal("invalid state")
		}
	}
	if published != 1 || present != 1 {
		t.Fatal("conditional publication lost exclusivity")
	}
	r, err := write156(p, id, []byte("different"))
	if !errorIs(err, backups.Conflict) || r.State() != backups.NotPublished {
		t.Fatal("different content overwrote winner")
	}
	objects := s.Objects()
	if string(objects[p.namespace+id.String()+suffix].Data) != "same ciphertext" {
		t.Fatal("winner changed")
	}
	s.Mode("etag-swap")
	d, err := p.Delete(context.Background(), id)
	if err == nil || d.State() != backups.DeleteIndeterminate || len(s.Objects()) != 1 {
		t.Fatal("conditional delete removed changed object")
	}
}
func TestNHA156ForeignKeysAndPaginationBoundaries(t *testing.T) {
	p, s, _ := setup156(t)
	id := id156(t, 1)
	success156(t, p, id, []byte("cipher"))
	objects := s.Objects()
	own := objects[p.key(id)]
	s.Inject("outside/"+id.String()+suffix, own)
	foreign := own
	foreign.Header = own.Header.Clone()
	foreign.Header.Set("X-Amz-Meta-Homeward-Instance", "neighbor")
	s.Inject(p.key(id156(t, 2)), foreign)
	if _, err := p.Delete(context.Background(), id156(t, 2)); !errorIs(err, backups.Corrupt) {
		t.Fatal("foreign marker delete admitted")
	}
	if len(s.Objects()) != 3 {
		t.Fatal("foreign object removed")
	}
	for _, mode := range []string{"foreign-list", "loop-list"} {
		s.Mode(mode)
		req, _ := backups.NewListRequest(2, backups.Cursor{})
		page, err := p.List(context.Background(), req)
		if mode == "foreign-list" && (err != nil || len(page.Items()) != 0) {
			t.Fatal("foreign key followed")
		}
		if mode == "loop-list" && err == nil {
			t.Fatal("pagination loop accepted")
		}
	}
	other, _ := backups.ParseInstanceID("neighbor")
	cursor, _ := backups.NewCursor(other, id)
	req, _ := backups.NewListRequest(2, cursor)
	before := len(s.Requests())
	if _, err := p.List(context.Background(), req); err == nil || len(s.Requests()) != before {
		t.Fatal("foreign cursor reached wire")
	}
	for _, r := range s.Requests() {
		if r.Method != "GET" || strings.Contains(r.Query, "list-type=2") {
			continue
		}
		if !strings.HasPrefix(r.Path, "/nha156-bucket/"+p.namespace) {
			t.Fatal("server key retargeted GET")
		}
	}
}
func TestNHA156RedirectAndCredentialRedaction(t *testing.T) {
	p, s, secrets := setup156(t)
	var mu sync.Mutex
	leaked := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mu.Lock(); leaked++; mu.Unlock(); w.WriteHeader(200) }))
	defer target.Close()
	s.Redirect(target.URL + "/NHA156SECRET")
	r, err := write156(p, id156(t, 1), []byte("cipher"))
	if err == nil || r.State() != backups.Indeterminate {
		t.Fatal("redirect accepted")
	}
	mu.Lock()
	count := leaked
	mu.Unlock()
	if count != 0 {
		t.Fatal("credentials followed redirect")
	}
	assertRedacted156(t, err)
	for _, value := range []any{p, p.settings, Factory{secrets: secrets, directory: "NHA156SECRET"}, credentials{reader: secrets, ref: ref156()}} {
		assertRedacted156(t, value)
		raw, err := json.Marshal(value)
		if err != nil || strings.Contains(string(raw), "NHA156SECRET") {
			t.Fatal("JSON leak")
		}
	}
	for _, raw := range []string{`null`, `{}`, `{"access_key_id":"x","secret_access_key":"y","session_token":""}`, `{"access_key_id":"x","access_key_id":"z","secret_access_key":"y"}`, `{"access_key_id":"x","secret_access_key":"y","extra":"z"}`, `{"access_key_id":"x","secret_access_key":"y\n"}`} {
		secrets.mu.Lock()
		secrets.raw = raw
		secrets.mu.Unlock()
		_, err := (credentials{reader: secrets, ref: ref156()}).Retrieve(context.Background())
		if err == nil {
			t.Fatal("invalid credential bundle admitted")
		}
		assertRedacted156(t, err)
	}
}
func assertRedacted156(t *testing.T, v any) {
	t.Helper()
	text := fmt.Sprintf("%v %+v %#v", v, v, v)
	for _, secret := range []string{"NHA156SECRET", "NHA156TOKEN", "NHA156ACCESS"} {
		if strings.Contains(text, secret) {
			t.Fatal("credential leaked through diagnostics")
		}
	}
}
func TestNHA156ReadersCancellationAndClose(t *testing.T) {
	p, s, _ := setup156(t)
	id := id156(t, 1)
	success156(t, p, id, []byte("ciphertext"))
	s.Mode("slow-get")
	ctx, cancel := context.WithCancel(context.Background())
	reader, err := p.Open(ctx, id)
	if err != nil {
		t.Fatal("open slow fixture")
	}
	done := make(chan error, 1)
	go func() { _, e := io.ReadAll(reader); done <- e }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled read succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("read not cancelled")
	}
	_ = reader.Close()
	reader, err = p.Open(context.Background(), id)
	if err != nil {
		t.Fatal("open close fixture")
	}
	done = make(chan error, 1)
	go func() { _, e := io.ReadAll(reader); done <- e }()
	if err := reader.Close(); err != nil {
		t.Fatal("reader close failed")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not unblock Read")
	}
	var borrowed []backups.ArtifactReader
	for n := 0; n < backups.MaxConcurrentOperations; n++ {
		r, err := p.Open(context.Background(), id)
		if err != nil {
			t.Fatal("borrow slot")
		}
		borrowed = append(borrowed, r)
	}
	if _, err := p.Open(context.Background(), id); !errorIs(err, backups.Capacity) {
		t.Fatal("unbounded reader concurrency")
	}
	if err := p.Close(); err != nil {
		t.Fatal("provider Close")
	}
	for _, r := range borrowed {
		if _, err := io.ReadAll(r); err == nil {
			t.Fatal("closed provider reader succeeded")
		}
		_ = r.Close()
	}
	if _, err := p.Verify(context.Background(), id); !errorIs(err, backups.Closed) {
		t.Fatal("closed provider reused")
	}
}

type closeFaultClient struct{ base awss3.HTTPClient }

func (c closeFaultClient) Do(r *http.Request) (*http.Response, error) {
	response, err := c.base.Do(r)
	if err == nil && r.Method == "GET" && r.URL.Query().Get("list-type") == "" {
		response.Body = closeFaultBody{response.Body}
	}
	return response, err
}

type closeFaultBody struct{ io.ReadCloser }

func (b closeFaultBody) Close() error { _ = b.ReadCloser.Close(); return errors.New("NHA156SECRET") }
func TestNHA156CompleteEOFAndCloseRequired(t *testing.T) {
	p, s, _ := setup156(t)
	options := p.client.Options()
	options.HTTPClient = closeFaultClient{options.HTTPClient}
	p.client = awss3.New(options)
	r, err := write156(p, id156(t, 1), []byte("valid ciphertext"))
	if err == nil || r.State() != backups.Indeterminate {
		t.Fatal("failed response Close announced complete")
	}
	assertRedacted156(t, err)
	if len(s.Objects()) != 1 {
		t.Fatal("ambiguous object incorrectly deleted")
	}
	reader, err := p.Open(context.Background(), id156(t, 1))
	if err != nil {
		t.Fatal("open valid body")
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal("valid EOF")
	}
	if err := reader.Close(); err == nil {
		t.Fatal("Close error suppressed")
	}
}

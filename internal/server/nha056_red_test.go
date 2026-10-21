package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"github.com/damione1/Homeward/internal/testpg"
)

func TestNHA056ReadOutsideMCPOverCommonTLS(t *testing.T) {
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	cfg := nha181Config(t, cluster.Port, data, "127.0.0.1:0", nha181TLSYAML())
	run := nha162StartDefaultRun(t, cfg)
	client := nha162RunClient(t, roots, run.address)
	base := "https://" + run.address.String()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const login, password = "NHA056 admin", "NHA056 synthetic login canary"
	nha162RunBootstrap(t, ctx, data, login, password)
	auth := authv1connect.NewAuthServiceClient(client, base)
	token := nha162RunLogin(t, ctx, auth, login, password).AccessToken
	for _, tc := range []struct{ method, body string }{
		{"GetHealth", `{}`},
		{"GetMetrics", `{}`},
		{"QueryRuntimeLogs", `{"pageSize":1}`},
		{"QueryAudits", `{"source":"AUDIT_SOURCE_AUTHENTICATION","pageSize":1}`},
	} {
		t.Run(tc.method, func(t *testing.T) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost,
				base+"/home.observability.v1.ObservabilityService/"+tc.method,
				strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Connect-Protocol-Version", "1")
			req.Header.Set("Authorization", "Bearer "+token)
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if _, err := io.Copy(io.Discard, io.LimitReader(res.Body, 256<<10)); err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != http.StatusOK || res.ProtoMajor != 2 {
				t.Fatalf("method=%s status=%d HTTP=%d", tc.method, res.StatusCode, res.ProtoMajor)
			}
		})
	}
}

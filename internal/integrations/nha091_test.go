package integrations_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/integrations"
)

type nha091Admission struct {
	count atomic.Int32
	limit int32
}

func (a *nha091Admission) ReserveAttempt(context.Context) (time.Time, error) {
	if a.count.Add(1) > a.limit {
		return time.Now().Add(time.Hour), nil
	}
	return time.Time{}, nil
}

func TestNHA091CloudAttemptFenceCountsRetryAndDeniesTransport(t *testing.T) {
	rig := nha096CloudRig(t, nil)
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"temporary"}}`))
	}))
	defer server.Close()
	admission := &nha091Admission{limit: 1}
	client, err := integrations.NewCloudClient(rig.session, rig.policy, nil, admission)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	request, err := integrations.NewCloudRequest(http.MethodGet, server.URL, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Observe(context.Background(), request)
	var cloudErr *integrations.CloudError
	if !errors.As(err, &cloudErr) || cloudErr.Kind() != integrations.CloudLocalQuotaExhausted {
		t.Fatalf("second attempt should stop locally: %v", err)
	}
	if received.Load() != 1 || admission.count.Load() != 2 {
		t.Fatalf("reserved/transport=%d/%d", admission.count.Load(), received.Load())
	}
}

func TestNHA091CloudCookiesOwnedBoundedAndRedacted(t *testing.T) {
	rig := nha096CloudRig(t, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "private-cookie"})
		_, _ = w.Write([]byte(`{"session":"private-session"}`))
	}))
	defer server.Close()
	request, _ := integrations.NewCloudRequest(http.MethodGet, server.URL, nil, nil)
	response, err := rig.client.Observe(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	cookies := response.Cookies()
	header, err := cookies.CookieHeader()
	if err != nil || header != "sid=private-cookie" {
		t.Fatal("cookie snapshot lost")
	}
	merged, err := cookies.MergeHeader("other=old")
	if err != nil || merged != "other=old; sid=private-cookie" {
		t.Fatalf("cookie merge: %s %v", merged, err)
	}
	for _, diagnostic := range []string{fmt.Sprint(response), fmt.Sprintf("%#v", cookies)} {
		if strings.Contains(diagnostic, "private-cookie") || strings.Contains(diagnostic, "private-session") {
			t.Fatal("cloud diagnostic leaked session")
		}
	}
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i < 17; i++ {
			http.SetCookie(w, &http.Cookie{Name: fmt.Sprintf("c%d", i), Value: "v"})
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer limited.Close()
	request, _ = integrations.NewCloudRequest(http.MethodGet, limited.URL, nil, nil)
	_, err = rig.client.Observe(context.Background(), request)
	var cloudErr *integrations.CloudError
	if !errors.As(err, &cloudErr) || cloudErr.Kind() != integrations.CloudResponseTooLarge {
		t.Fatalf("oversized cookie jar admitted: %v", err)
	}
}

func TestNHA091CloudHTTPErrorBodyExplicitButDiagnosticsRedacted(t *testing.T) {
	rig := nha096CloudRig(t, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"USRBADLOGIN"},"token":"private-token"}`))
	}))
	defer server.Close()
	request, _ := integrations.NewCloudRequest(http.MethodPost, server.URL, nil, []byte(`{}`))
	_, err := rig.client.Observe(context.Background(), request)
	var failure *integrations.CloudError
	if !errors.As(err, &failure) || failure.Kind() != integrations.CloudCredentialFailed {
		t.Fatalf("HTTP error kind: %v", err)
	}
	if !strings.Contains(string(failure.ResponseBytes()), "USRBADLOGIN") {
		t.Fatal("provider code inaccessible to adapter")
	}
	if strings.Contains(fmt.Sprintf("%#v", failure), "private-token") || strings.Contains(err.Error(), "private-token") {
		t.Fatal("error diagnostic leaked body")
	}
}

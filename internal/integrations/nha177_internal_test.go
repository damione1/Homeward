package integrations

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/damione1/Homeward/internal/storage"
)

func TestNHA177TransportHeaderAndCookieBounds(t *testing.T) {
	header := http.Header{"X-A": {strings.Repeat("x", cloudAuthorizationHeaderLimit-len("X-A")-4)}}
	if _, err := cloudAuthorizationHeaderSize(header); err != nil {
		t.Fatal(err)
	}
	header["X-A"][0] += "x"
	if _, err := cloudAuthorizationHeaderSize(header); err == nil {
		t.Fatal("header bound +1 admitted")
	}

	prefix := "https://example.test/"
	location := prefix + strings.Repeat("a", cloudAuthorizationLocationLimit-len(prefix))
	accepted := http.Header{"Location": {location}}
	if got, err := cloudAuthorizationLocation(http.StatusFound, accepted); err != nil || got != location {
		t.Fatal("Location exact bound refused")
	}
	accepted.Set("Location", location+"a")
	if _, err := cloudAuthorizationLocation(http.StatusFound, accepted); err == nil {
		t.Fatal("Location bound +1 admitted")
	}
	accepted = http.Header{"Location": {"https://example.test/one", "https://example.test/two"}}
	if _, err := cloudAuthorizationLocation(http.StatusFound, accepted); err == nil {
		t.Fatal("multiple Locations admitted")
	}
	accepted = http.Header{"Location": {"https://example.test/callback#fragment"}}
	if _, err := cloudAuthorizationLocation(http.StatusFound, accepted); err == nil {
		t.Fatal("fragment admitted")
	}

	source, _ := url.Parse("https://example.test/api/start")
	rawOne := "a=" + strings.Repeat("x", 4094)
	rawTwo := "b=" + strings.Repeat("x", 4094)
	cookieHeader := http.Header{"Set-Cookie": {rawOne, rawTwo}}
	if cookies, err := cloudAuthorizationCookies(cookieHeader, source); err != nil || len(cookies) != 2 {
		t.Fatal("cookie aggregate exact bound refused")
	}
	cookieHeader["Set-Cookie"][1] += "x"
	if _, err := cloudAuthorizationCookies(cookieHeader, source); err == nil {
		t.Fatal("cookie aggregate bound +1 admitted")
	}
	cardinality := make([]string, cloudAuthorizationCookieLimit)
	for index := range cardinality {
		cardinality[index] = "sid" + strconv.Itoa(index) + "=x"
	}
	if got, err := cloudAuthorizationCookies(http.Header{"Set-Cookie": cardinality}, source); err != nil || len(got) != cloudAuthorizationCookieLimit {
		t.Fatal("exact cookie count refused")
	}
	cardinality = append(cardinality, "extra=x")
	if _, err := cloudAuthorizationCookies(http.Header{"Set-Cookie": cardinality}, source); err == nil {
		t.Fatal("cookie count +1 admitted")
	}
}

func TestNHA177CookieScopeExpiryAndDeletion(t *testing.T) {
	source, _ := url.Parse("https://login.example.test/api/start")
	header := http.Header{"Set-Cookie": {"sid=private; Domain=example.test; Path=/api; Secure; Max-Age=30"}}
	cookies, err := cloudAuthorizationCookies(header, source)
	if err != nil || len(cookies) != 1 || cookies[0].Deleted() {
		t.Fatal("scoped cookie refused")
	}
	jar := cloudAuthorizationJar{values: make(map[cloudAuthorizationCookieKey]CloudAuthorizationCookie)}
	if err := jar.apply(cookies); err != nil {
		t.Fatal(err)
	}
	inside, _ := url.Parse("https://other.example.test/api/next")
	outside, _ := url.Parse("https://other.example.test/outside")
	foreign, _ := url.Parse("https://outside.test/api/next")
	if value, err := jar.header(inside); err != nil || value != "sid=private" {
		t.Fatal("domain/path cookie not delivered")
	}
	if value, err := jar.header(outside); err != nil || value != "" {
		t.Fatal("cookie escaped path")
	}
	if value, err := jar.header(foreign); err != nil || value != "" {
		t.Fatal("cookie escaped domain")
	}
	header = http.Header{"Set-Cookie": {"sid=; Domain=example.test; Path=/api; Max-Age=0"}}
	deletion, err := cloudAuthorizationCookies(header, source)
	if err != nil || len(deletion) != 1 || !deletion[0].Deleted() {
		t.Fatal("cookie deletion not admitted")
	}
	if err := jar.apply(deletion); err != nil {
		t.Fatal(err)
	}
	if value, err := jar.header(inside); err != nil || value != "" {
		t.Fatal("deleted cookie was sent")
	}
	header = http.Header{"Set-Cookie": {"sid=private; Domain=outside.test; Path=/api"}}
	if _, err := cloudAuthorizationCookies(header, source); err == nil {
		t.Fatal("foreign cookie domain admitted")
	}
}

func TestNHA177ZeroAndTypedNilInputs(t *testing.T) {
	if _, err := ParseCloudAuthorizationProfileID(""); err == nil {
		t.Fatal("zero profile admitted")
	}
	if _, err := NewCloudAuthorizationInput(CloudAuthorizationProfileID{}, storage.SecretValue{}); err == nil {
		t.Fatal("zero input admitted")
	}
	if _, err := NewCloudAuthorizedCredential(CloudAuthorizationProfileID{}, storage.SecretValue{}); err == nil {
		t.Fatal("zero credential admitted")
	}
	if _, err := AdmitCloudAuthorizationLimits(CloudAuthorizationLimitsInput{MaxExchanges: 6, MaxActive: 16, MaxPerHandle: 1}); err == nil {
		t.Fatal("zero timeout admitted")
	}
}

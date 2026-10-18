package accounts

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestNHA141SessionSecretsAreTypedAndRedacted(t *testing.T) {
	access, err := NewAccessSecret()
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := NewRefreshSecret()
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := NewEnrollmentSecret()
	if err != nil {
		t.Fatal(err)
	}
	accessText, _ := access.Text()
	refreshText, _ := refresh.Text()
	enrollmentText, _ := enrollment.Text()
	for _, value := range []string{accessText, refreshText, enrollmentText} {
		if len(value) != 43 || strings.ContainsAny(value, "=+/ ") {
			t.Fatalf("noncanonical token encoding: length=%d", len(value))
		}
	}
	parsed, err := ParseAccessSecret(accessText)
	if err != nil {
		t.Fatal(err)
	}
	parsedText, _ := parsed.Text()
	if parsedText != accessText {
		t.Fatal("access round trip changed token")
	}
	if _, err := ParseAccessSecret(accessText + "="); err == nil {
		t.Fatal("padded bearer admitted")
	}
	if _, err := ParseRefreshSecret(accessText[:42]); err == nil {
		t.Fatal("short bearer admitted")
	}
	accessDigest, err := access.Digest()
	if err != nil {
		t.Fatal(err)
	}
	refreshShapedAccess, err := ParseRefreshSecret(accessText)
	if err != nil {
		t.Fatal(err)
	}
	accessAsRefreshDigest, err := refreshShapedAccess.Digest()
	if err != nil {
		t.Fatal(err)
	}
	accessBytes, _ := accessDigest.Bytes()
	accessAsRefreshBytes, _ := accessAsRefreshDigest.Bytes()
	if accessBytes == accessAsRefreshBytes {
		t.Fatal("same opaque syntax crossed access and refresh digest domains")
	}
	refreshDigest, err := refresh.Digest()
	if err != nil {
		t.Fatal(err)
	}
	enrollmentDigest, err := enrollment.Digest()
	if err != nil {
		t.Fatal(err)
	}
	first, err := accessDigest.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	first[0] ^= 0xff
	second, err := accessDigest.Bytes()
	if err != nil || first == second {
		t.Fatal("digest bytes leaked mutable state")
	}
	if _, err := (AccessSecret{}).Text(); err == nil {
		t.Fatal("zero access admitted")
	}
	if _, err := (RefreshDigest{}).Bytes(); err == nil {
		t.Fatal("zero refresh digest admitted")
	}
	if _, err := (EnrollmentDigest{}).Bytes(); err == nil {
		t.Fatal("zero enrollment digest admitted")
	}
	for _, value := range []any{access, refresh, enrollment, accessDigest, refreshDigest, enrollmentDigest} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, canary := range []string{accessText, refreshText, enrollmentText} {
			if strings.Contains(string(encoded), canary) || strings.Contains(fmt.Sprintf("%v %#v", value, value), canary) {
				t.Fatal("secret escaped a formatting boundary")
			}
		}
	}
}

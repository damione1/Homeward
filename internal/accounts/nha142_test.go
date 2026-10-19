package accounts

import (
	"strings"
	"testing"
)

func TestNHA142EnrollmentParsingKeepsCanonicalFormAndPurpose(t *testing.T) {
	issued, err := NewEnrollmentSecret()
	if err != nil {
		t.Fatal(err)
	}
	text, err := issued.Text()
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := ParseEnrollmentSecret(text)
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := issued.Digest()
	actual, _ := admitted.Digest()
	if expected != actual {
		t.Fatal("parsed grant changed its digest")
	}
	access, err := ParseAccessSecret(text)
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := ParseRefreshSecret(text)
	if err != nil {
		t.Fatal(err)
	}
	accessDigest, _ := access.Digest()
	refreshDigest, _ := refresh.Digest()
	a, _ := accessDigest.Bytes()
	r, _ := refreshDigest.Bytes()
	g, _ := actual.Bytes()
	if a == g || r == g || a == r {
		t.Fatal("credential purposes shared a digest")
	}
	for _, invalid := range []string{"", text + "=", strings.Repeat("A", 43), strings.Repeat("!", 43)} {
		secret, err := ParseEnrollmentSecret(invalid)
		if err == nil {
			t.Fatal("invalid/noncanonical/zero grant admitted")
		}
		if _, err := secret.Digest(); err == nil {
			t.Fatal("rejected grant retained a usable digest")
		}
	}
}

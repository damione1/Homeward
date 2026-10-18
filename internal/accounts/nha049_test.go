package accounts

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type nha049NilContext struct{ context.Context }

func TestNHA049PasswordNormalizationBoundsAndVerifier(t *testing.T) {
	const decomposed = "NHA049 a very long pa\u0308ssword 🔒"
	const composed = "NHA049 a very long pässword 🔒"
	verifier, err := HashNewPassword(context.Background(), decomposed)
	if err != nil {
		t.Fatal(err)
	}
	match, err := VerifyPassword(context.Background(), composed, verifier)
	if err != nil || !match {
		t.Fatalf("NFC equivalent password rejected: %v", err)
	}
	match, err = VerifyPassword(context.Background(), composed+" ", verifier)
	if err != nil || match {
		t.Fatalf("password whitespace was trimmed: %v", err)
	}
	if _, err := HashNewPassword(context.Background(), "123456789012345"); !errors.Is(err, ErrBlockedPassword) {
		t.Fatalf("local blocklist: %v", err)
	}
	if _, err := NormalizePasswordForVerification("123456789012345"); err != nil {
		t.Fatalf("blocklist wrongly applied to verification: %v", err)
	}
	for _, invalid := range []string{strings.Repeat("a", 14), strings.Repeat("a", 1025), strings.Repeat("🔒", 1024) + "a"} {
		if _, err := NormalizePasswordForVerification(invalid); !errors.Is(err, ErrInvalidPassword) {
			t.Fatalf("invalid password length admitted: %v", err)
		}
	}
	profile, salt, digest, err := verifier.StorageValues()
	if err != nil || profile != passwordProfileArgon2idV1 || len(salt) != 16 || len(digest) != 32 {
		t.Fatalf("verifier profile: %v", err)
	}
	if _, err := ParseVerifier(profile, salt[:15], digest[:]); !errors.Is(err, ErrInvalidVerifier) {
		t.Fatalf("short SQL salt admitted: %v", err)
	}
	if _, err := ParseVerifier(profile+1, salt[:], digest[:]); !errors.Is(err, ErrInvalidVerifier) {
		t.Fatalf("unknown SQL profile admitted: %v", err)
	}
}

func TestNHA049KDFCanceledAndTypedNilContext(t *testing.T) {
	var typedNil *nha049NilContext
	if _, err := HashNewPassword(typedNil, "NHA049 typed nil password"); !errors.Is(err, context.Canceled) {
		t.Fatalf("typed nil context did not fail closed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := HashNewPassword(ctx, "NHA049 canceled password"); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled derivation: %v", err)
	}
	if len(kdfPermits) != 0 || len(kdfAdmissions) != 0 {
		t.Fatal("canceled KDF retained capacity")
	}
}

func TestNHA049LoginCasefoldAndAdmission(t *testing.T) {
	first, err := ParseLoginName("Straße Admin")
	if err != nil {
		t.Fatal(err)
	}
	second, err := ParseLoginName("STRASSE ADMIN")
	if err != nil {
		t.Fatal(err)
	}
	firstDisplay, firstKey, _ := first.Values()
	secondDisplay, secondKey, _ := second.Values()
	if firstDisplay == secondDisplay || firstKey != secondKey {
		t.Fatal("display spelling and casefold SQL key were not separated")
	}
	if _, err := ParseLoginName(" Admin "); !errors.Is(err, ErrInvalidLogin) {
		t.Fatalf("border-space login admitted: %v", err)
	}
}

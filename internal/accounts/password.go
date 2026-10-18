package accounts

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"reflect"

	"golang.org/x/crypto/argon2"
)

const passwordProfileArgon2idV1 int16 = 1
const argonMemoryKiB uint32 = 64 * 1024
const argonIterations uint32 = 3
const argonParallelism uint8 = 4
const saltSize = 16
const digestSize = 32

var ErrInvalidVerifier = errors.New("accounts: invalid password verifier")
var ErrKDFBusy = errors.New("accounts: password derivation capacity exhausted")
var kdfAdmissions = make(chan struct{}, 4) // two running, at most two waiting
var kdfPermits = make(chan struct{}, 2)

// Verifier never renders credentials through ordinary formatting. Its profile
// fixes the Argon2 allocation cost; untrusted SQL may not choose parameters.
type Verifier struct {
	profile int16
	salt    [saltSize]byte
	digest  [digestSize]byte
}

func (v Verifier) String() string   { return "[REDACTED PASSWORD VERIFIER]" }
func (v Verifier) GoString() string { return v.String() }

func ParseVerifier(profile int16, salt, digest []byte) (Verifier, error) {
	var zeroSalt [saltSize]byte
	var zeroDigest [digestSize]byte
	if profile != passwordProfileArgon2idV1 || len(salt) != saltSize || len(digest) != digestSize ||
		bytes.Equal(salt, zeroSalt[:]) || bytes.Equal(digest, zeroDigest[:]) {
		return Verifier{}, ErrInvalidVerifier
	}
	v := Verifier{profile: profile}
	copy(v.salt[:], salt)
	copy(v.digest[:], digest)
	return v, nil
}

func (v Verifier) StorageValues() (int16, [saltSize]byte, [digestSize]byte, error) {
	if v.profile != passwordProfileArgon2idV1 || v.salt == [saltSize]byte{} || v.digest == [digestSize]byte{} {
		return 0, [saltSize]byte{}, [digestSize]byte{}, ErrInvalidVerifier
	}
	return v.profile, v.salt, v.digest, nil
}

func HashNewPassword(ctx context.Context, raw string) (Verifier, error) {
	value, err := AdmitNewPassword(raw)
	if err != nil {
		return Verifier{}, err
	}
	var salt [saltSize]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return Verifier{}, ErrInvalidVerifier
	}
	digest, err := derive(ctx, value, salt)
	if err != nil {
		return Verifier{}, err
	}
	return Verifier{profile: passwordProfileArgon2idV1, salt: salt, digest: digest}, nil
}

func VerifyPassword(ctx context.Context, raw string, verifier Verifier) (bool, error) {
	value, err := NormalizePasswordForVerification(raw)
	if err != nil {
		return false, err
	}
	if verifier.profile != passwordProfileArgon2idV1 {
		return false, ErrInvalidVerifier
	}
	digest, err := derive(ctx, value, verifier.salt)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(digest[:], verifier.digest[:]) == 1, nil
}

func derive(ctx context.Context, value string, salt [saltSize]byte) ([digestSize]byte, error) {
	if ctx == nil || typedNilContext(ctx) {
		return [digestSize]byte{}, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return [digestSize]byte{}, err
	}
	select {
	case kdfAdmissions <- struct{}{}:
	default:
		return [digestSize]byte{}, ErrKDFBusy
	}
	defer func() { <-kdfAdmissions }()
	select {
	case kdfPermits <- struct{}{}:
	case <-ctx.Done():
		return [digestSize]byte{}, ctx.Err()
	}
	defer func() { <-kdfPermits }()
	if err := ctx.Err(); err != nil {
		return [digestSize]byte{}, err
	}
	done := make(chan [digestSize]byte, 1)
	go func() {
		derived := argon2.IDKey([]byte(value), salt[:], argonIterations, argonMemoryKiB, argonParallelism, digestSize)
		var result [digestSize]byte
		copy(result[:], derived)
		clear(derived)
		done <- result
	}()
	select {
	case digest := <-done:
		if err := ctx.Err(); err != nil {
			return [digestSize]byte{}, err
		}
		return digest, nil
	case <-ctx.Done():
		// IDKey cannot be stopped. Join before releasing its global permit.
		<-done
		return [digestSize]byte{}, ctx.Err()
	}
}

func typedNilContext(ctx context.Context) bool {
	value := reflect.ValueOf(ctx)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

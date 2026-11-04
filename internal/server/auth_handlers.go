package server

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
)

const loginBucketCapacity = 20

type factorPreparationKind uint8

const (
	factorPreparationUnspecified factorPreparationKind = iota
	factorPreparationAuthenticated
	factorPreparationChallenge
	factorPreparationEnrollment
)

type factorPreparation struct {
	revision storage.RevocationRevision
	policy   authmfa.ExpectedPolicyVersion
	ceremony authmfa.PreparedCeremony
	started  time.Time
	kind     factorPreparationKind
	options  []*authv1.FactorOption
}

func factorAuthenticated() factorPreparation {
	return factorPreparation{kind: factorPreparationAuthenticated}
}
func factorChallenge(options []*authv1.FactorOption) factorPreparation {
	copied := make([]*authv1.FactorOption, 0, len(options))
	for _, option := range options {
		if option == nil {
			return factorPreparation{}
		}
		copied = append(copied, proto.Clone(option).(*authv1.FactorOption))
	}
	return factorPreparation{kind: factorPreparationChallenge, options: copied}
}

// factorProvider is a private NHA-142 seam. Prepare returns exactly one of
// authenticated, challenge or enrollment required. Verify must perform the
// cryptographic proof check; wire admission alone cannot.
type factorProvider interface {
	Prepare(context.Context, contracts.AccountID, contracts.ChallengeID) (factorPreparation, error)
	Verify(context.Context, storage.MFALoginSnapshot, *authv1.FactorProof) (authmfa.VerifiedLogin, error)
}

type loginBucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func (b *loginBucket) take(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.last.IsZero() {
		b.last, b.tokens = now, loginBucketCapacity
	}
	if now.After(b.last) {
		b.tokens += now.Sub(b.last).Seconds() // 60 starts/minute
		if b.tokens > loginBucketCapacity {
			b.tokens = loginBucketCapacity
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

type authHandler struct {
	store       *storage.Store
	coordinator *sessionCoordinator
	provider    factorProvider
	bucket      loginBucket
}

func newAuthHandler(store *storage.Store, coordinator *sessionCoordinator, provider factorProvider) (*authHandler, error) {
	if store == nil || coordinator == nil || typedNilPublic(provider) {
		return nil, storage.ErrSessionInvalid
	}
	return &authHandler{store: store, coordinator: coordinator, provider: provider}, nil
}

type issuedCredentials struct {
	access        accounts.AccessSecret
	refresh       accounts.RefreshSecret
	accessDigest  accounts.AccessDigest
	refreshDigest accounts.RefreshDigest
}

func (issuedCredentials) String() string                 { return "[REDACTED SESSION CREDENTIALS]" }
func (v issuedCredentials) GoString() string             { return v.String() }
func (v issuedCredentials) MarshalJSON() ([]byte, error) { return json.Marshal(v.String()) }

func newIssuedCredentials() (issuedCredentials, error) {
	access, err := accounts.NewAccessSecret()
	if err != nil {
		return issuedCredentials{}, err
	}
	refresh, err := accounts.NewRefreshSecret()
	if err != nil {
		return issuedCredentials{}, err
	}
	accessDigest, err := access.Digest()
	if err != nil {
		return issuedCredentials{}, err
	}
	refreshDigest, err := refresh.Digest()
	if err != nil {
		return issuedCredentials{}, err
	}
	return issuedCredentials{access: access, refresh: refresh, accessDigest: accessDigest, refreshDigest: refreshDigest}, nil
}

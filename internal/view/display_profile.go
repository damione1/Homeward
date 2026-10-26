package view

import (
	"context"
	"crypto/sha256"
	"time"
)

// DisplayProfileSource is an optional trusted server-side profile authority.
// An empty name means no configured profile. Failures must not serve cached
// metadata; subscribers are notified on updates or closure. The client never
// supplies an actor or profile through this port.
type DisplayProfileSource interface {
	DisplayTimeZone(context.Context, ActorID) (string, error)
	SubscribeDisplayProfileChanges(context.Context, ActorID) (<-chan struct{}, func(), error)
}

func validDisplayTimeZone(name string) bool {
	// Go's special Local name would silently use the server machine's zone.
	if name == "" || name == "Local" || len(name) > 128 {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

// WithDisplayTimeZone attaches a trusted, actor-scoped display profile to an
// already authorized grant. It does not authorize any additional entity or slot.
// The profile becomes part of the signed view generation: changes invalidate
// pagination, replay and Watch through the existing authority checks. Providers
// must notify grant subscribers on profile changes just as for rights changes.
// Calling this again replaces the profile without accumulating digest changes.
// A provider without a profile returns its undecorated grant (no local fallback).
func (g Grant) WithDisplayTimeZone(name string) (Grant, error) {
	if !g.validFor(g.actor) || !validDisplayTimeZone(name) {
		return Grant{}, ErrInvalidInput
	}
	base := g.profileBaseDigest
	if base == ([32]byte{}) {
		base = g.digest
	}
	digest := sha256.New()
	digest.Write([]byte("homeward-view-display-profile-v1\x00"))
	digest.Write(base[:])
	digest.Write([]byte(name))
	copy(g.digest[:], digest.Sum(nil))
	g.profileBaseDigest = base
	g.displayTimeZone = name
	return g, nil
}

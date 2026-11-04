package server

import (
	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"testing"
	"time"
)

func TestNHA203SessionPaginationSurvivesItsOwnSlidingAuthentication(t *testing.T) {
	rig := nha142NewServerRig(t)
	caller := rig.start(t).GetAuthenticated()
	if caller == nil {
		t.Fatal("caller session missing")
	}
	for range 2 {
		if rig.start(t).GetAuthenticated() == nil {
			t.Fatal("sibling session missing")
		}
	}
	list := func(token string) (*api.ListSessionsResponse, error) {
		t.Helper()
		ctx, release := rig.session(t, caller)
		defer release()
		response, err := rig.handler.ListSessions(ctx, connect.NewRequest(&api.ListSessionsRequest{Parent: rig.accountName.String(), PageSize: 1, PageToken: token}))
		if err != nil {
			return nil, err
		}
		return response.Msg, nil
	}
	first, err := list("")
	if err != nil || len(first.Sessions) != 1 || first.NextPageToken == "" {
		t.Fatal("first page", err)
	}
	seen := map[string]bool{first.Sessions[0].Name: true}
	token := first.NextPageToken
	for range 2 {
		// This authentication is real: SQL advances last_used_at and sliding expiry.
		rig.clock.Advance(rig.clock.Now().Add(time.Second))
		page, err := list(token)
		if err != nil || len(page.Sessions) != 1 {
			t.Fatal("activity invalidated collection membership", err)
		}
		if seen[page.Sessions[0].Name] {
			t.Fatal("duplicate session across pages")
		}
		seen[page.Sessions[0].Name] = true
		token = page.NextPageToken
	}
	if token != "" || len(seen) != 3 {
		t.Fatal("pagination lost native sessions")
	}
	// An actual member addition does invalidate the captured collection.
	if rig.start(t).GetAuthenticated() == nil {
		t.Fatal("new member missing")
	}
	if _, err := list(first.NextPageToken); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatal("changed collection accepted old cursor", err)
	}
}

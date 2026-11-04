package server

import (
	"connectrpc.com/connect"
	"context"
	"crypto/sha256"
	"fmt"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/mediacontent"
	"github.com/damione1/Homeward/internal/mediasessions"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
	"time"
)

func TestNHF016ResourceTLSExactSourcesAndCredentialBinding(t *testing.T) {
	r := nhf016NewResourceRig(t, 60*time.Second)
	_, err := r.api.ListMediaSources(r.ctx, connect.NewRequest(&api.ListMediaSourcesRequest{Parent: r.home.String()}))
	nha162Code(t, err, connect.CodeUnauthenticated)
	sources := r.catalog(t)
	if len(sources) != 2 || sources[0].DisplayName != sources[1].DisplayName || sources[0].Name == sources[1].Name {
		t.Fatal("exact distinct sources required")
	}
	a, b := r.open(t, sources[0].Name), r.open(t, sources[1].Name)
	left, err := r.api.ReadMediaSession(r.ctx, nha162Bearer(nha202SnapshotRequest(a.Name), r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	right, err := r.api.ReadMediaSession(r.ctx, nha162Bearer(nha202SnapshotRequest(b.Name), r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(left.Msg.Image.Png) == sha256.Sum256(right.Msg.Image.Png) {
		t.Fatal("images exchanged")
	}
	owner := r.owners[a.MediaSource]
	owner.mu.Lock()
	owner.wrongImage = true
	owner.mu.Unlock()
	_, err = r.api.ReadMediaSession(r.ctx, nha162Bearer(nha202SnapshotRequest(a.Name), r.adminToken))
	if err == nil {
		t.Fatal("foreign native image admitted")
	}
	owner.mu.Lock()
	owner.wrongImage = false
	owner.mu.Unlock()
	other := r.login(t, "NHA162 Admin", "NHA162 synthetic admin password canary")
	readsBefore := owner.imageRequests.Load()
	_, err = r.api.ReadMediaSession(r.ctx, nha162Bearer(nha202SnapshotRequest(a.Name), other))
	nha162Code(t, err, connect.CodeNotFound)
	err = r.closeSession(r.ctx, a.Name, other)
	nha162Code(t, err, connect.CodeNotFound)
	if owner.imageRequests.Load() != readsBefore {
		t.Fatal("foreign credential reached native image source")
	}
	if _, err = r.api.ReadMediaSession(r.ctx, nha162Bearer(nha202SnapshotRequest(a.Name), r.adminToken)); err != nil {
		t.Fatal("foreign credential invalidated owner", err)
	}
	for _, session := range []*api.MediaSession{a, b} {
		if err = r.closeSession(r.ctx, session.Name, r.adminToken); err != nil {
			t.Fatal(err)
		}
	}
	_, err = r.api.ReadMediaSession(r.ctx, nha162Bearer(nha202SnapshotRequest(a.Name), r.adminToken))
	if err == nil {
		t.Fatal("deleted session exposes pixels")
	}
}
func TestNHF016ResourceTLSAccountsCannotShareSession(t *testing.T) {
	r := nhf016NewResourceRig(t, 60*time.Second)
	const login, password = "NHF016 Other Account", "NHF016 synthetic account password"
	created, err := r.accounts.CreateAccount(r.ctx, nha162Bearer(&api.CreateAccountRequest{Parent: r.taskHome(t), Account: &api.Account{LoginName: login, DisplayName: login, InitialPassword: password, Role: api.AccountRole_ACCOUNT_ROLE_USER}}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	account := nha203NativeAccount(t, r.ctx, r.store, created.Msg.Name)
	nha162SetPolicy(t, r.nha162RPCRig, account, nhf016AllCameras)
	token := r.login(t, login, password)
	session := r.open(t, r.catalog(t)[0].Name)
	owner := r.owners[session.MediaSource]
	readsBefore := owner.imageRequests.Load()
	_, err = r.api.ReadMediaSession(r.ctx, nha162Bearer(nha202SnapshotRequest(session.Name), token))
	nha162Code(t, err, connect.CodeNotFound)
	err = r.closeSession(r.ctx, session.Name, token)
	nha162Code(t, err, connect.CodeNotFound)
	if owner.imageRequests.Load() != readsBefore {
		t.Fatal("foreign account reached native image source")
	}
	if _, err = r.api.ReadMediaSession(r.ctx, nha162Bearer(nha202SnapshotRequest(session.Name), r.adminToken)); err != nil {
		t.Fatal(err)
	}
	own, err := r.api.CreateMediaSession(r.ctx, nha162Bearer(&api.CreateMediaSessionRequest{Parent: r.home.String(), MediaSession: &api.MediaSession{MediaSource: session.MediaSource, ContentKind: api.MediaContentKind_MEDIA_CONTENT_KIND_CAMERA}}, token))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.api.ReadMediaSession(r.ctx, nha162Bearer(nha202SnapshotRequest(own.Msg.Name), token)); err != nil {
		t.Fatal(err)
	}
}
func TestNHF016ResourceTLSPagesCapacityAndLongEvents(t *testing.T) {
	r := nhf016NewResourceRig(t, 90*time.Second)
	source := r.catalog(t)[0].Name
	session := r.open(t, source)
	first, err := r.api.QueryMediaSessionEvents(r.ctx, nha162Bearer(&api.QueryMediaSessionEventsRequest{Name: session.Name, PageSize: 1}, r.adminToken))
	if err != nil || len(first.Msg.MediaEvents) != 1 || first.Msg.NextPageToken == "" {
		t.Fatal("first page", err)
	}
	next, err := r.api.QueryMediaSessionEvents(r.ctx, nha162Bearer(&api.QueryMediaSessionEventsRequest{Name: session.Name, PageSize: 1, PageToken: first.Msg.NextPageToken}, r.adminToken))
	if err != nil || next.Msg.MediaEvents[0].EventId == first.Msg.MediaEvents[0].EventId {
		t.Fatal("duplicate page", err)
	}
	_, err = r.api.QueryMediaSessionEvents(r.ctx, nha162Bearer(&api.QueryMediaSessionEventsRequest{Name: session.Name, PageToken: "tampered" + first.Msg.NextPageToken}, r.adminToken))
	if err == nil {
		t.Fatal("tampered token admitted")
	}
	owner := r.owners[source]
	owner.invalidate()
	_, err = r.api.QueryMediaSessionEvents(r.ctx, nha162Bearer(&api.QueryMediaSessionEventsRequest{Name: session.Name, PageToken: first.Msg.NextPageToken}, r.adminToken))
	nha162Code(t, err, connect.CodeFailedPrecondition)
	owner.mu.Lock()
	owner.events = nil
	for index := 0; index < 10000; index++ {
		uid, e := mediasessions.ParseEventID(fmt.Sprintf("wide-event-%05d", index))
		if e != nil {
			t.Fatal(e)
		}
		// The native collection keeps its 8MiB bound. Long initial rows
		// exercise 64KiB response clipping; remaining rows cover 10000 events.
		width := 512
		if index < 100 {
			width = 1024
		}
		event, e := mediacontent.NewEvent(uid, "Synthetic event", strings.Repeat("s", width), time.Now().Add(-time.Duration(index+1)*time.Second), true)
		if e != nil {
			t.Fatal(e)
		}
		owner.events = append(owner.events, event)
	}
	owner.mu.Unlock()
	owner.invalidate()
	count, previous, cursor := 0, "", ""
	for {
		page, e := r.api.QueryMediaSessionEvents(r.ctx, nha162Bearer(&api.QueryMediaSessionEventsRequest{Name: session.Name, PageSize: 64, PageToken: cursor}, r.adminToken))
		if e != nil {
			t.Fatal(e)
		}
		if proto.Size(page.Msg) > 64*1024 || len(page.Msg.MediaEvents) > 64 || len(page.Msg.MediaEvents) == 0 {
			t.Fatal("page exceeds bytes/rows")
		}
		for _, event := range page.Msg.MediaEvents {
			if event.EventId <= previous {
				t.Fatal("event order/duplicate")
			}
			previous = event.EventId
			count++
		}
		cursor = page.Msg.NextPageToken
		if cursor == "" {
			break
		}
	}
	if count != 10000 {
		t.Fatal("incomplete events", count)
	}
	for range mediasessions.MaxActivePerActor - 1 {
		r.open(t, source)
	}
	_, err = r.api.CreateMediaSession(r.ctx, nha162Bearer(&api.CreateMediaSessionRequest{Parent: r.home.String(), MediaSession: &api.MediaSession{MediaSource: source, ContentKind: api.MediaContentKind_MEDIA_CONTENT_KIND_CAMERA}}, r.adminToken))
	nha162Code(t, err, connect.CodeResourceExhausted)
}
func TestNHF016ResourceTLSExpiryDownloadRevocationAndWatch(t *testing.T) {
	r := nhf016NewResourceRig(t, 60*time.Second)
	source := r.catalog(t)[0].Name
	session := r.open(t, source)
	owner := r.owners[source]
	owner.mu.Lock()
	owner.imageGate = make(chan struct{})
	owner.mu.Unlock()
	if err := r.closeSession(r.ctx, session.Name, r.adminToken); err != nil {
		t.Fatal(err)
	}
	session = r.openLifetime(t, source, 200*time.Millisecond)
	_, err := r.api.ReadMediaSession(r.ctx, nha162Bearer(nha202SnapshotRequest(session.Name), r.adminToken))
	if err == nil {
		t.Fatal("expired paused read exposed pixels")
	}
	if owner.activeImages.Load() != 0 {
		t.Fatal("expired read not joined")
	}
	session = r.open(t, source)
	done := make(chan error, 1)
	go func() {
		_, e := r.api.ReadMediaSession(r.ctx, nha162Bearer(nha202SnapshotRequest(session.Name), r.adminToken))
		done <- e
	}()
	deadline := time.Now().Add(5 * time.Second)
	for owner.activeImages.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("read did not start")
		}
		time.Sleep(time.Millisecond)
	}
	nha162SetPolicy(t, r.nha162RPCRig, r.admin, "  []\n")
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("revoked image exposed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read not joined")
	}
	if owner.activeImages.Load() != 0 {
		t.Fatal("private read remains")
	}
	nha162SetPolicy(t, r.nha162RPCRig, r.admin, nhf016AllCameras)
	owner.mu.Lock()
	owner.imageGate = nil
	owner.mu.Unlock()
	for _, reason := range []string{"delete", "revoke", "readable-epoch"} {
		session = r.open(t, source)
		ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
		stream, e := r.api.WatchMediaSession(ctx, nha162Bearer(&api.WatchMediaSessionRequest{Name: session.Name}, r.adminToken))
		if e != nil {
			cancel()
			t.Fatal(e)
		}
		if !stream.Receive() || stream.Msg().GetReady() == nil {
			cancel()
			t.Fatal("watch ready", stream.Err())
		}
		switch reason {
		case "delete":
			if e = r.closeSession(r.ctx, session.Name, r.adminToken); e != nil {
				t.Fatal(e)
			}
		case "revoke":
			nha162SetPolicy(t, r.nha162RPCRig, r.admin, "  []\n")
		case "readable-epoch":
			nha162SetPolicy(t, r.nha162RPCRig, r.admin, nhf016AllCameras+"  - scope: {family: history, operation: read}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n")
		}
		if stream.Receive() || stream.Err() == nil {
			t.Fatal("retired watch remained alive", reason)
		}
		stream.Close()
		cancel()
		nha162SetPolicy(t, r.nha162RPCRig, r.admin, nhf016AllCameras)
	}
	fresh := r.open(t, source)
	if _, err = r.api.ReadMediaSession(r.ctx, nha162Bearer(nha202SnapshotRequest(fresh.Name), r.adminToken)); err != nil {
		t.Fatal("fresh read after rights change", err)
	}
}

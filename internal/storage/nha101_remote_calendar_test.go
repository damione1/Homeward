package storage

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/jackc/pgx/v5"
)

func nha101RemoteKind(t *testing.T, err error, kind RemoteCalendarFailure) {
	t.Helper()
	var failure *RemoteCalendarError
	if !errors.As(err, &failure) || failure.Kind() != kind {
		t.Fatalf("remote kind got=%v want=%d", err, kind)
	}
}

func TestNHA101RemoteHTTPMetadataBoundsAndCorruption(t *testing.T) {
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	profile := sha256.Sum256([]byte("representation profile"))
	fields := make([]RemoteHTTPField, 0, 5)
	for i := 1; i <= 5; i++ {
		f, e := NewRemoteHTTPField(RemoteHTTPFieldKind(i), RemoteHTTPAbsent, nil)
		if e != nil {
			t.Fatal(e)
		}
		fields = append(fields, f)
	}
	if (RemoteCalendarHTTPMetadata{}).Valid() || (RemoteCalendarSelection{}).Valid() || (RemoteCalendarConditionalDisposition(0)).Valid() {
		t.Fatal("zero HTTP storage admission valid")
	}
	// 54-byte metadata prefix, five 3-byte fields, one 2-byte value length.
	boundary := strings.Repeat("x", RemoteCalendarHTTPMetadataLimit-71)
	f, e := NewRemoteHTTPField(RemoteHTTPCacheControl, RemoteHTTPPresent, []string{boundary})
	if e != nil {
		t.Fatal(e)
	}
	fields[0] = f
	m, e := NewRemoteCalendarHTTPMetadata(profile, fields, now, 0)
	if e != nil {
		t.Fatal(e)
	}
	encoded, e := m.Encode()
	if e != nil || len(encoded) != RemoteCalendarHTTPMetadataLimit {
		t.Fatal("metadata exact boundary", e)
	}
	restored, e := DecodeRemoteCalendarHTTPMetadata(encoded)
	if e != nil || !restored.Present() {
		t.Fatal("metadata roundtrip", e)
	}
	got, _ := restored.Field(RemoteHTTPCacheControl).Values()
	got[0] = "mutated"
	again, _ := restored.Field(RemoteHTTPCacheControl).Values()
	if again[0] != boundary {
		t.Fatal("HTTP metadata leaked mutable values")
	}
	fields[0], e = NewRemoteHTTPField(RemoteHTTPCacheControl, RemoteHTTPPresent, []string{boundary + "x"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = NewRemoteCalendarHTTPMetadata(profile, fields, now, 0); e == nil {
		t.Fatal("metadata bound+1 admitted")
	}
	for _, bad := range [][]byte{encoded[:len(encoded)-1], append(append([]byte(nil), encoded...), 0), {2, 2}, make([]byte, RemoteCalendarHTTPMetadataLimit+1)} {
		if _, e = DecodeRemoteCalendarHTTPMetadata(bad); e == nil {
			t.Fatal("corrupt/oversized HTTP metadata admitted")
		}
	}
	if _, e = NewRemoteHTTPField(RemoteHTTPCacheControl, RemoteHTTPAbsent, []string{""}); e == nil {
		t.Fatal("absent HTTP field retained a value")
	}
}

func TestNHA101RemoteHTTPObservationBindsBodyOrSelectedDigest(t *testing.T) {
	scope := nha101RemoteScope(t, "HTTP admission")
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	fields := make([]RemoteHTTPField, 0, 5)
	for i := 1; i <= 5; i++ {
		f, e := NewRemoteHTTPField(RemoteHTTPFieldKind(i), RemoteHTTPAbsent, nil)
		if e != nil {
			t.Fatal(e)
		}
		fields = append(fields, f)
	}
	metadata, e := NewRemoteCalendarHTTPMetadata(sha256.Sum256([]byte("profile")), fields, now, 0)
	if e != nil {
		t.Fatal(e)
	}
	validators, e := NewRemoteCalendarValidators(RemoteValidatorsNone, "", time.Time{})
	if e != nil {
		t.Fatal(e)
	}
	body := sha256.Sum256([]byte("exact HTTP body"))
	representation, e := RemoteCalendarHTTPRepresentation(body)
	if e != nil {
		t.Fatal(e)
	}
	observation, e := NewRemoteCalendarHTTPObservation(scope, sha256.Sum256([]byte("200")), now, now.Add(time.Hour), validators, metadata, representation)
	if e != nil || !observation.AdmitsRepresentation(body) || observation.AdmitsRepresentation(sha256.Sum256([]byte("other body"))) {
		t.Fatal("200 admission did not bind exact bytes", e)
	}
	selection, e := RemoteCalendarHTTPRevalidation(body)
	if e != nil {
		t.Fatal(e)
	}
	revalidation, e := NewRemoteCalendarHTTPObservation(scope, sha256.Sum256([]byte("304")), now, now.Add(time.Hour), validators, metadata, selection)
	if e != nil || revalidation.AdmitsRepresentation(body) {
		t.Fatal("304 admission accepted a200 replacement", e)
	}
	if _, e = NewRemoteCalendarHTTPObservation(scope, sha256.Sum256([]byte("zero")), now, now.Add(time.Hour), validators, metadata, RemoteCalendarHTTPAdmission{}); e == nil {
		t.Fatal("zero HTTP admission accepted")
	}
}

// T1 oracle: reopening the same configured source must not invalidate the
// persisted validators. This uses the authoritative migration runner only.
func TestNHA101RemoteSameScopeResumeAndRenamePreserveSelection(t *testing.T) {
	ctx := context.Background()
	store, settings := nha051Store(t)
	b := nha101RemoteStoreBinding(t, store)
	scope := nha101RemoteScope(t, "resume-resource")
	h, err := store.ConfigureRemoteCalendar(ctx, b, scope, "Synthetic remote")
	if err != nil {
		t.Fatal(err)
	}
	expected, err := ExpectRemoteCalendar(h, scope)
	if err != nil {
		t.Fatal(err)
	}
	validators, err := NewRemoteCalendarValidators(RemoteValidatorsBoth, `"resume-tag"`, time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	_, committed, err := store.ReplaceRemoteCalendar(ctx, expected, nha101RemoteObservation(t, scope, "resume200", validators), nha101EmptyPayload(t))
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	reopened, err := Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	resumed, err := reopened.ConfigureRemoteCalendar(ctx, b, scope, "Synthetic remote")
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Revision() != committed.Revision() || !resumed.UpdatedAt().Equal(committed.UpdatedAt()) || !resumed.Validators().Equal(validators) || resumed.Digest() != committed.Digest() {
		t.Fatal("same-scope reopen changed durable revision, dates, representation or validators")
	}
	renamed, err := reopened.ConfigureRemoteCalendar(ctx, b, scope, "Renamed remote")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Revision() != resumed.Revision()+1 || renamed.Name() != "Renamed remote" || !renamed.Validators().Equal(validators) || renamed.Digest() != committed.Digest() {
		t.Fatal("rename invalidated representation selection")
	}
}
func nha101RemoteStoreBinding(t *testing.T, store *Store) RemoteCalendarBinding {
	t.Helper()
	ctx := context.Background()
	entity, err := domain.ParseEntityID("nha101-store-calendar")
	if err != nil {
		t.Fatal(err)
	}
	var binding RemoteCalendarBinding
	err = store.InTransaction(ctx, func(tx pgx.Tx) error {
		for _, sql := range []string{
			`INSERT INTO public.registry_devices(id) VALUES('nha101-store-device')`,
			`INSERT INTO public.registry_device_sources(instance_id,source_device,device_id) VALUES('nha101-store-instance','calendar-source','nha101-store-device')`,
			`INSERT INTO public.registry_entities(id,device_id,manifest_alias) VALUES('nha101-store-calendar','nha101-store-device','calendar.famille')`,
			`INSERT INTO public.registry_source_bindings(instance_id,source_device,source_entity,entity_id) VALUES('nha101-store-instance','calendar-source','calendar','nha101-store-calendar')`,
		} {
			if _, e := tx.Exec(ctx, sql); e != nil {
				return e
			}
		}
		var e error
		binding, e = ReadRemoteCalendarBinding(ctx, tx, entity, true)
		if e != nil {
			return e
		}
		_, e = EnsureRemoteCalendar(ctx, tx, binding, "Synthetic remote")
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	return binding
}
func nha101RemoteHeader(t *testing.T, store *Store, b RemoteCalendarBinding) RemoteCalendarHeader {
	t.Helper()
	var h RemoteCalendarHeader
	if err := store.InTransaction(context.Background(), func(tx pgx.Tx) error {
		var e error
		h, e = ReadRemoteCalendarHeader(context.Background(), tx, b, false)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	return h
}
func nha101EmptyPayload(t *testing.T) RemoteCalendarPayload {
	t.Helper()
	raw, e := os.ReadFile("../calendars/testdata/nha101/remote-v1-empty.hex")
	if e != nil {
		t.Fatal(e)
	}
	data, e := hex.DecodeString(strings.TrimSpace(string(raw)))
	if e != nil {
		t.Fatal(e)
	}
	p, e := BorrowRemoteCalendarPayload(data, sha256.Sum256(data), RemoteCalendarCounts{}, remoteMinimumCharge(int64(len(data)), RemoteCalendarCounts{}))
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func nha101RemoteScope(t *testing.T, seed string, credentials ...SecretRevision) RemoteCalendarScope {
	t.Helper()
	s, e := NewRemoteCalendarScope(sha256.Sum256([]byte(seed)), credentials)
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func nha101RemoteObservation(t *testing.T, s RemoteCalendarScope, seed string, v RemoteCalendarValidators) RemoteCalendarObservation {
	t.Helper()
	at := time.Now().UTC().Truncate(time.Second)
	o, e := NewRemoteCalendarObservation(s, sha256.Sum256([]byte(seed)), at, at.Add(time.Hour), v)
	if e != nil {
		t.Fatal(e)
	}
	return o
}
func TestNHA101RemoteOfficialMigrationAndNeverValidated(t *testing.T) {
	index := -1
	for i, m := range migrations {
		if m.version.Number() == 23 {
			index = i
			break
		}
	}
	if index < 1 || migrations[index-1].version.Number() != 22 || migrations[index].sql != remoteCalendarSchemaSQL {
		t.Fatal("real owner migration23 must be registered after real22; no test-created future schema")
	}
	store, settings := nha051Store(t)
	b := nha101RemoteStoreBinding(t, store)
	first := nha101RemoteHeader(t, store, b)
	if !first.Valid() || first.State() != RemoteCalendarNeverValidated || first.Revision() != 1 || first.PayloadLength() != 0 {
		t.Fatal("bound never-validated state is not durable and explicit")
	}
	var second RemoteCalendarHeader
	if e := store.InTransaction(context.Background(), func(tx pgx.Tx) error {
		var e error
		second, e = EnsureRemoteCalendar(context.Background(), tx, b, "ignored replacement name")
		return e
	}); e != nil {
		t.Fatal(e)
	}
	if second.Revision() != 1 || second.Name() != first.Name() {
		t.Fatal("Bind initialization is not idempotent")
	}
	reopened, e := Open(context.Background(), settings)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	if got := nha101RemoteHeader(t, reopened, b); got.Revision() != first.Revision() || got.State() != RemoteCalendarNeverValidated {
		t.Fatal("reopen fabricated remote state")
	}
}
func TestNHA101RemoteStoreReplaceRevalidateDegradeAndSecretCAS(t *testing.T) {
	ctx := context.Background()
	store, _ := nha051Store(t)
	b := nha101RemoteStoreBinding(t, store)
	secrets, e := store.BootstrapSecrets(ctx, filepath.Join(t.TempDir(), "private"))
	if e != nil {
		t.Fatal(e)
	}
	handle := nha051Handle(t, "nha101-store-instance", "ics-token")
	if e = secrets.RotateCredential(ctx, handle, nha051Value(t, "NHA101_PRIVATE_CANARY_FIRST")); e != nil {
		t.Fatal(e)
	}
	_, version, e := secrets.ReadCredentialVersioned(ctx, handle)
	if e != nil {
		t.Fatal(e)
	}
	scope := nha101RemoteScope(t, "same configured resource", version)
	h, e := store.ConfigureRemoteCalendar(ctx, b, scope, "Synthetic remote")
	if e != nil {
		t.Fatal(e)
	}
	expected, e := ExpectRemoteCalendar(h, scope)
	if e != nil {
		t.Fatal(e)
	}
	validators, e := NewRemoteCalendarValidators(RemoteValidatorsETag, `W/"nha101-etag-canary"`, time.Time{})
	if e != nil {
		t.Fatal(e)
	}
	obs := nha101RemoteObservation(t, scope, "first200", validators)
	payload := nha101EmptyPayload(t)
	receipt, h, e := store.ReplaceRemoteCalendar(ctx, expected, obs, payload)
	if e != nil || !receipt.Valid() {
		t.Fatalf("real replace: %v", e)
	}
	key, _ := scope.Key()
	observationID, _ := obs.ID()
	if !receipt.Matches(b.entity, key, observationID) || receipt.Matches(b.entity, key, sha256.Sum256([]byte("other observation"))) {
		t.Fatal("receipt is not bound to exact observation")
	}
	old := expected
	expected, e = ExpectRemoteCalendar(h, scope)
	if e != nil {
		t.Fatal(e)
	}
	nextObs := nha101RemoteObservation(t, scope, "304", validators)
	notCommitted, _, e := store.ReplaceRemoteCalendar(ctx, old, nextObs, payload)
	nha101RemoteKind(t, e, RemoteCalendarConflict)
	if notCommitted.Valid() {
		t.Fatal("CAS loser produced receipt")
	}
	priorDigest := h.Digest()
	receipt, h, e = store.RevalidateRemoteCalendar(ctx, expected, nextObs, validators, priorDigest)
	if e != nil || !receipt.Valid() || h.Digest() != priorDigest {
		t.Fatalf("revalidation changed snapshot or lost receipt: %v", e)
	}
	expected, e = ExpectRemoteCalendar(h, scope)
	if e != nil {
		t.Fatal(e)
	}
	if e = secrets.RotateCredential(ctx, handle, nha051Value(t, "NHA101_PRIVATE_CANARY_ROTATED")); e != nil {
		t.Fatal(e)
	}
	receipt, _, e = store.ReplaceRemoteCalendar(ctx, expected, obs, payload)
	nha101RemoteKind(t, e, RemoteCalendarConflict)
	if receipt.Valid() {
		t.Fatal("stale authenticated version produced receipt")
	}
	_, newVersion, e := secrets.ReadCredentialVersioned(ctx, handle)
	if e != nil {
		t.Fatal(e)
	}
	nextScope := nha101RemoteScope(t, "same configured resource", newVersion)
	nextKey, _ := nextScope.Key()
	if key == nextKey {
		t.Fatal("same-handle rotation did not change cache scope")
	}
	h, e = store.ConfigureRemoteCalendar(ctx, b, nextScope, "Synthetic remote")
	if e != nil {
		t.Fatal(e)
	}
	if h.State() != RemoteCalendarValidatedStale || h.Digest() != priorDigest || h.Validators().Kind() != RemoteValidatorsNone {
		t.Fatal("reconfiguration discarded known business image or reused old validators")
	}
	expected, e = ExpectRemoteCalendar(h, nextScope)
	if e != nil {
		t.Fatal(e)
	}
	h, e = store.DegradeRemoteCalendar(ctx, expected, nextScope, time.Now(), RemoteCalendarReasonTransport)
	if e != nil || h.State() != RemoteCalendarValidatedStale || h.Digest() != priorDigest {
		t.Fatalf("degrade lost cache: %v", e)
	}
	for _, thing := range []any{scope, obs, payload, receipt, h, validators, e} {
		encoded, err := json.Marshal(thing)
		if err != nil {
			t.Fatal(err)
		}
		for _, rendering := range []string{fmt.Sprintf("%v", thing), fmt.Sprintf("%+v", thing), fmt.Sprintf("%#v", thing), string(encoded)} {
			if strings.Contains(rendering, "CANARY") || strings.Contains(rendering, "etag-canary") {
				t.Fatal("remote diagnostics disclosed source metadata or credential")
			}
		}
	}
}
func TestNHA101RemoteAdmissionZerosAndBounds(t *testing.T) {
	if (RemoteCalendarCommit{}).Valid() || (RemoteCalendarHeader{}).Valid() || (RemoteCalendarScope{}).Valid() || (RemoteCalendarObservation{}).Valid() {
		t.Fatal("zero value admitted")
	}
	if _, e := NewRemoteCalendarScope([32]byte{}, nil); e == nil {
		t.Fatal("zero scope key admitted")
	}
	if _, e := NewRemoteCalendarValidators(RemoteValidatorsETag, strings.Repeat("x", 1025), time.Time{}); e == nil {
		t.Fatal("unbounded validator admitted")
	}
	if _, e := NewRemoteCalendarValidators(RemoteValidatorsNone, "unaccounted", time.Time{}); e == nil {
		t.Fatal("variant with unused payload admitted")
	}
	opaque := string([]byte{'"', 0x80, 0xff, '"'})
	validators, e := NewRemoteCalendarValidators(RemoteValidatorsETag, opaque, time.Time{})
	if e != nil {
		t.Fatal("opaque RFC entity tag was narrowed to UTF-8", e)
	}
	if got, ok := validators.ETag(); !ok || got != opaque {
		t.Fatal("opaque entity tag changed")
	}
	if _, e := BorrowRemoteCalendarPayload(make([]byte, 20), sha256.Sum256(make([]byte, 20)), RemoteCalendarCounts{Events: 1025}, 33554432); e == nil {
		t.Fatal("count+1 admitted")
	}
	var typedNil *nha101NilStorageContext
	if e := remoteContext(typedNil); e == nil {
		t.Fatal("typed nil context admitted")
	}
}

func TestNHA101RemoteSQLPayloadAndPresenceConstraints(t *testing.T) {
	ctx := context.Background()
	store, _ := nha051Store(t)
	b := nha101RemoteStoreBinding(t, store)
	scope := nha101RemoteScope(t, "constraint source")
	h, e := store.ConfigureRemoteCalendar(ctx, b, scope, "Synthetic")
	if e != nil {
		t.Fatal(e)
	}
	expected, e := ExpectRemoteCalendar(h, scope)
	if e != nil {
		t.Fatal(e)
	}
	validators, _ := NewRemoteCalendarValidators(RemoteValidatorsNone, "", time.Time{})
	receipt, h, e := store.ReplaceRemoteCalendar(ctx, expected, nha101RemoteObservation(t, scope, "constraints200", validators), nha101EmptyPayload(t))
	if e != nil || !receipt.Valid() {
		t.Fatal(e)
	}
	for _, sql := range []string{
		`UPDATE public.remote_calendar_cache SET event_count=NULL WHERE entity_id=$1`,
		`UPDATE public.remote_calendar_cache SET event_count=1025 WHERE entity_id=$1`,
		`UPDATE public.remote_calendar_cache SET source_scope=NULL WHERE entity_id=$1`,
		`UPDATE public.remote_calendar_cache SET payload=repeat('a',33554433)::bytea,event_count=0,image_charge=2097152 WHERE entity_id=$1`,
	} {
		entity, _ := b.entity.Value()
		e = store.InTransaction(ctx, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, sql, entity); return err })
		if e == nil {
			t.Fatal("real schema admitted an incoherent presence/count or independent bytea bound")
		}
	}
	if got := nha101RemoteHeader(t, store, b); got.Digest() != h.Digest() || got.Revision() != h.Revision() {
		t.Fatal("failed constraint transaction changed durable cache")
	}
}

type nha101NilStorageContext struct{ context.Context }

// Proxy reads actual PostgreSQL backend framing and drops the COMMIT response
// only after the server emitted CommandComplete(COMMIT). This establishes the
// durable/unknown distinction independently of the client return value.
func nha101CommitProxy(t *testing.T, target string) (int, *atomic.Bool) {
	t.Helper()
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	drop := &atomic.Bool{}
	var wg sync.WaitGroup
	var connections sync.Map
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			client, e := listener.Accept()
			if e != nil {
				return
			}
			server, e := net.Dial("tcp", target)
			if e != nil {
				client.Close()
				continue
			}
			connections.Store(client, server)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer connections.Delete(client)
				defer client.Close()
				defer server.Close()
				copied := make(chan struct{})
				go func() { defer close(copied); io.Copy(server, client); server.Close() }()
				defer func() { client.Close(); server.Close(); <-copied }()
				var header [5]byte
				for {
					if _, e := io.ReadFull(server, header[:]); e != nil {
						return
					}
					size := int64(binary.BigEndian.Uint32(header[1:])) - 4
					if size < 0 || size > 64<<20 {
						return
					}
					payload := make([]byte, int(size))
					if _, e := io.ReadFull(server, payload); e != nil {
						return
					}
					if header[0] == 'C' && string(payload) == "COMMIT\x00" && drop.CompareAndSwap(true, false) {
						return
					}
					if _, e := client.Write(header[:]); e != nil {
						return
					}
					if _, e := client.Write(payload); e != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		connections.Range(func(k, v any) bool { k.(net.Conn).Close(); v.(net.Conn).Close(); return true })
		wg.Wait()
	})
	return listener.Addr().(*net.TCPAddr).Port, drop
}
func TestNHA101RemoteDurableCommitLostACKHasNoReceipt(t *testing.T) {
	ctx := context.Background()
	direct, settings := nha051Store(t)
	b := nha101RemoteStoreBinding(t, direct)
	scope := nha101RemoteScope(t, "synthetic source")
	h, e := direct.ConfigureRemoteCalendar(ctx, b, scope, "Synthetic remote")
	if e != nil {
		t.Fatal(e)
	}
	expected, e := ExpectRemoteCalendar(h, scope)
	if e != nil {
		t.Fatal(e)
	}
	port, drop := nha101CommitProxy(t, net.JoinHostPort(settings.host, fmt.Sprint(settings.port)))
	proxiedSettings := settings
	proxiedSettings.port = uint16(port)
	proxied, e := Open(ctx, proxiedSettings)
	if e != nil {
		t.Fatal(e)
	}
	defer proxied.Close()
	validators, _ := NewRemoteCalendarValidators(RemoteValidatorsNone, "", time.Time{})
	obs := nha101RemoteObservation(t, scope, "lostACK", validators)
	drop.Store(true)
	receipt, _, e := proxied.ReplaceRemoteCalendar(ctx, expected, obs, nha101EmptyPayload(t))
	nha101RemoteKind(t, e, RemoteCalendarCommitUnknown)
	if receipt.Valid() || drop.Load() {
		t.Fatal("lost ACK returned a receipt or proxy did not observe COMMIT")
	}
	durable := nha101RemoteHeader(t, direct, b)
	if durable.Revision() != h.Revision()+1 || durable.State() != RemoteCalendarValidatedFresh {
		t.Fatal("independent PostgreSQL connection did not observe the durable commit")
	}
}

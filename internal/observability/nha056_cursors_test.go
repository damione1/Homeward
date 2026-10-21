package observability

import (
	"strings"
	"testing"
	"time"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
)

func TestNHA056CursorsBindPurposeSourceRightsProcessAndDeadline(t *testing.T) {
	_, _, codec := nha056Fixture(t)
	now := time.Duration(0)
	codec.elapsed = func() time.Duration { return now }
	binding := nha056Binding(t, 1)
	cursor, err := codec.BeginLogs(binding, 9)
	if err != nil {
		t.Fatal(err)
	}
	cursor.first = false
	cursor.next = 3
	token, err := codec.EncodeLogs(cursor)
	if err != nil || len(token) > 512 {
		t.Fatal("token bounds", err)
	}
	decoded, err := codec.DecodeLogs(token, binding)
	if err != nil || decoded.upper != 9 || decoded.next != 3 {
		t.Fatal("cursor changed", err)
	}
	if _, err := codec.DecodeLogs(token, nha056Binding(t, 2)); err == nil {
		t.Fatal("cross-binding cursor")
	}
	revisedBinding := binding
	revisedBinding.revision++
	if _, err := codec.DecodeLogs(token, revisedBinding); err == nil {
		t.Fatal("cross-rights-revision cursor")
	}
	if _, err := codec.DecodeAudits(token, binding, obs.AuditSource_AUDIT_SOURCE_AUTHENTICATION); err == nil {
		t.Fatal("cross-purpose cursor")
	}
	if _, err := codec.DecodeLogs(token[:len(token)-1]+"!", binding); err == nil {
		t.Fatal("tampered cursor")
	}
	tampered := []byte(token)
	if tampered[len(tampered)/2] == 'A' {
		tampered[len(tampered)/2] = 'B'
	} else {
		tampered[len(tampered)/2] = 'A'
	}
	if _, err := codec.DecodeLogs(string(tampered), binding); err == nil {
		t.Fatal("authenticated ciphertext mutation admitted")
	}
	audit, err := codec.BeginAudits(binding, obs.AuditSource_AUDIT_SOURCE_AUTHENTICATION, 30)
	if err != nil {
		t.Fatal(err)
	}
	audit, err = audit.After(20)
	if err != nil {
		t.Fatal(err)
	}
	auditToken, err := codec.EncodeAudits(audit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.DecodeAudits(auditToken, binding, obs.AuditSource_AUDIT_SOURCE_AUTHENTICATION); err != nil {
		t.Fatal("valid audit token", err)
	}
	if _, err := codec.DecodeAudits(auditToken, binding, obs.AuditSource_AUDIT_SOURCE_SECRET_ACCESS); err == nil {
		t.Fatal("cross-audit-source cursor")
	}
	if _, err := codec.DecodeLogs(strings.Repeat("a", 513), binding); err == nil {
		t.Fatal("oversized cursor")
	}
	_, _, restarted := nha056Fixture(t)
	if _, err := restarted.DecodeLogs(token, binding); err == nil {
		t.Fatal("prior process cursor")
	}
	now = 60 * time.Second
	if _, err := codec.DecodeLogs(token, binding); err == nil {
		t.Fatal("expired cursor")
	}
	for _, size := range []uint32{0, 101} {
		if _, err := NewPageSize(size); err == nil {
			t.Fatal("invalid page size")
		}
	}
}

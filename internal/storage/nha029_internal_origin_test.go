package storage

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
)

func TestNHA029InternalFingerprintCannotBePairedWithAnotherOriginKey(t *testing.T) {
	store, ctx := nha151Store(t)
	keyA, err := NewInternalAutomationOriginKey("automation-a")
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := NewInternalAutomationOriginKey("automation-b")
	if err != nil {
		t.Fatal(err)
	}
	fingerprintA, err := NewInternalAutomationFingerprint(keyA, []byte("same canonical command"))
	if err != nil {
		t.Fatal(err)
	}
	fingerprintB, err := NewInternalAutomationFingerprint(keyB, []byte("same canonical command"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(fingerprintA.Bytes(), fingerprintB.Bytes()) {
		t.Fatal("distinct occurrence keys produced an identical fingerprint")
	}
	copyOfA := fingerprintA.Bytes()
	copyOfA[0] ^= 0xff
	if bytes.Equal(copyOfA, fingerprintA.Bytes()) {
		t.Fatal("fingerprint bytes exposed mutable internal digest")
	}
	id, err := domain.ParseCommandID("nha029-incoherent-pair")
	if err != nil {
		t.Fatal(err)
	}
	entity, err := domain.ParseEntityID("nha029-incoherent-pair-entity")
	if err != nil {
		t.Fatal(err)
	}
	slot, err := authorization.NewCapabilitySlot("core.switch.v1", "main")
	if err != nil {
		t.Fatal(err)
	}
	target, err := authorization.NewCommandTarget(entity, slot)
	if err != nil {
		t.Fatal(err)
	}
	_, created, outcome, err := store.AcceptInternalAutomationIntent(ctx, id, keyB, "epoch", fingerprintA, target)
	var storageErr *Error
	if created || outcome != TransactionNotCommitted || !errors.As(err, &storageErr) || storageErr.Kind() != InvalidSettings {
		t.Fatalf("incoherent key/fingerprint crossed pre-SQL validation: %t %v %v", created, outcome, err)
	}
	if _, present, err := store.CommandIntent(ctx, id); err != nil || present {
		t.Fatalf("incoherent pair left a durable command row: present=%t err=%v", present, err)
	}
	record, created, outcome, err := store.AcceptInternalAutomationIntent(ctx, id, keyA, "epoch", fingerprintA, target)
	if err != nil || !created || outcome != TransactionCommitted || record.OriginKind() != IntentOriginInternalAutomation || !bytes.Equal(record.Fingerprint(), fingerprintA.Bytes()) {
		t.Fatalf("coherent pair rejected: created=%t outcome=%v err=%v", created, outcome, err)
	}
	if admitted, present := record.InternalOriginKey(); !present || admitted != keyA {
		t.Fatal("coherent pair lost its occurrence key")
	}
	otherID, err := domain.ParseCommandID("nha029-zero-target")
	if err != nil {
		t.Fatal(err)
	}
	_, created, outcome, err = store.AcceptInternalAutomationIntent(ctx, otherID, keyA, "epoch", fingerprintA, authorization.CommandTarget{})
	if created || outcome != TransactionNotCommitted || !errors.As(err, &storageErr) || storageErr.Kind() != InvalidSettings {
		t.Fatalf("zero target was admitted: created=%t outcome=%v err=%v", created, outcome, err)
	}
	var absent *Store
	_, created, outcome, err = absent.AcceptInternalAutomationIntent(context.Background(), otherID, keyB, "epoch", fingerprintA, target)
	if created || outcome != TransactionNotCommitted || !errors.As(err, &storageErr) || storageErr.Kind() != InvalidSettings {
		t.Fatalf("nil store obscured pre-SQL pair validation: created=%t outcome=%v err=%v", created, outcome, err)
	}
}

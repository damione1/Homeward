package accounts

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestNHA198RenewalSuccessorRequiresBothSecrets(t *testing.T) {
	old, err := NewRefreshSecret()
	if err != nil {
		t.Fatal(err)
	}
	key, err := ParseRenewalKey(bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	first, err := old.Successor(key)
	if err != nil {
		t.Fatal(err)
	}
	second, err := old.Successor(key)
	if err != nil || first != second || first == old {
		t.Fatal("same saved operation did not yield the same distinct successor")
	}
	otherKey, _ := ParseRenewalKey(bytes.Repeat([]byte{0x32}, 32))
	other, err := old.Successor(otherKey)
	if err != nil || first == other {
		t.Fatal("operation secret did not bind the successor")
	}
	for _, invalid := range [][]byte{nil, make([]byte, 32), make([]byte, 31), make([]byte, 33)} {
		if _, err := ParseRenewalKey(invalid); err == nil {
			t.Fatal("malformed renewal key admitted")
		}
	}
	if _, err := (RefreshSecret{}).Successor(key); err == nil {
		t.Fatal("zero old credential admitted")
	}
	if _, err := old.Successor(RenewalKey{}); err == nil {
		t.Fatal("zero operation key admitted")
	}
	if strings.Contains(fmt.Sprintf("%#v", key), "313131") {
		t.Fatal("renewal secret was not redacted")
	}
}

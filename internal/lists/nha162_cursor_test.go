package lists

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/damione1/Homeward/internal/domain"
)

func TestNHA162CursorTokensSealViewAndExpireAtRestart(t *testing.T) {
	codec, err := NewCursorCodec()
	if err != nil {
		t.Fatal(err)
	}
	id, err := domain.ParseListID("list-synthetic")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := ParseItemUID("item-synthetic")
	if err != nil {
		t.Fatal(err)
	}
	list := ListCursor{valid: true, kind: cursorViewExplicit, last: id, rights: 7}
	list.view = sha256.Sum256([]byte("private scoped list view"))
	list.semantic = sha256.Sum256([]byte("private policy semantics"))
	token, err := codec.EncodeList(list)
	if err != nil || len(token) > MaxCursorTokenBytes {
		t.Fatalf("encode list cursor: %v, length %d", err, len(token))
	}
	sealed, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || bytes.Contains(sealed, list.view[:]) || bytes.Contains(sealed, list.semantic[:]) || bytes.Contains(sealed, []byte("list-synthetic")) {
		t.Fatalf("list cursor exposed its private view, semantics, or position: %v", err)
	}
	again, err := codec.EncodeList(list)
	if err != nil || again == token {
		t.Fatalf("list cursor did not use a fresh nonce: %v", err)
	}
	decoded, err := codec.DecodeList(token)
	if err != nil || decoded != list {
		t.Fatalf("list cursor lost bound view: %v", err)
	}
	if _, err := codec.DecodeItem(token); !IsCode(err, StaleCursor) {
		t.Fatalf("list token admitted as item: %v", err)
	}
	unknownVersion := append([]byte(nil), sealed...)
	unknownVersion[0]++
	if _, err := codec.DecodeList(base64.RawURLEncoding.EncodeToString(unknownVersion)); !IsCode(err, StaleCursor) {
		t.Fatalf("unknown cursor version admitted: %v", err)
	}
	unknownKind := append([]byte(nil), sealed...)
	unknownKind[1] = 'X'
	if _, err := codec.DecodeList(base64.RawURLEncoding.EncodeToString(unknownKind)); !IsCode(err, StaleCursor) {
		t.Fatalf("unknown cursor kind admitted: %v", err)
	}
	item := ItemCursor{valid: true, list: id, revision: 3, lastPosition: 17, last: uid}
	itemToken, err := codec.EncodeItem(item)
	if err != nil {
		t.Fatal(err)
	}
	itemDecoded, err := codec.DecodeItem(itemToken)
	if err != nil || itemDecoded != item {
		t.Fatalf("item cursor lost position or revision: %v", err)
	}
	tampered := []byte(token)
	if tampered[10] == 'A' {
		tampered[10] = 'B'
	} else {
		tampered[10] = 'A'
	}
	if _, err := codec.DecodeList(string(tampered)); !IsCode(err, StaleCursor) {
		t.Fatalf("tampered token admitted: %v", err)
	}
	if _, err := codec.DecodeList(strings.Repeat("A", MaxCursorTokenBytes*2)); !IsCode(err, StaleCursor) {
		t.Fatalf("oversized token admitted: %v", err)
	}
	if _, err := codec.DecodeList("@@not-base64@@"); !IsCode(err, StaleCursor) {
		t.Fatalf("malformed encoding admitted: %v", err)
	}
	restarted, err := NewCursorCodec()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.DecodeList(token); !IsCode(err, StaleCursor) {
		t.Fatalf("prior-process token admitted: %v", err)
	}
	first, err := codec.DecodeList("")
	if err != nil || first.valid {
		t.Fatalf("empty token is not first page: %v", err)
	}
}

func TestNHA162CursorCodecNeverFormatsKey(t *testing.T) {
	codec, err := NewCursorCodec()
	if err != nil {
		t.Fatal(err)
	}
	copy := *codec
	for _, formatted := range []string{
		fmt.Sprintf("%v", codec),
		fmt.Sprintf("%+v", codec),
		fmt.Sprintf("%#v", codec),
		fmt.Sprintf("%v", copy),
		fmt.Sprintf("%#v", copy),
		fmt.Sprintf("%s", codec),
		fmt.Sprintf("%x", codec),
		codec.String(), codec.GoString(),
	} {
		if formatted != redactedCursorCodec {
			t.Fatalf("cursor codec diagnostic exposed state: %q", formatted)
		}
	}
	for _, value := range []any{codec, copy} {
		encoded, err := json.Marshal(value)
		if err != nil || string(encoded) != `"lists: cursor codec"` {
			t.Fatalf("cursor codec JSON exposed state: %q, %v", encoded, err)
		}
	}
}

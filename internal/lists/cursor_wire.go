package lists

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"

	"github.com/damione1/Homeward/internal/domain"
)

const MaxCursorTokenBytes = 1024

const (
	cursorWireVersion  byte = 1
	listCursorWireKind byte = 'L'
	itemCursorWireKind byte = 'I'
)

// CursorCodec seals pagination state; it never grants access. Each page call
// still obtains a fresh Permit and checks the current authorized SQL view.
type CursorCodec struct {
	key   [sha256.Size]byte
	valid bool
}

const redactedCursorCodec = "lists: cursor codec"

// Keep even Go's diagnostic formats and JSON from printing the process key.
func (c CursorCodec) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redactedCursorCodec)) }
func (c CursorCodec) String() string             { return redactedCursorCodec }
func (c CursorCodec) GoString() string           { return redactedCursorCodec }
func (c CursorCodec) MarshalJSON() ([]byte, error) {
	return []byte(`"lists: cursor codec"`), nil
}

func NewCursorCodec() (*CursorCodec, error) {
	codec := &CursorCodec{valid: true}
	if _, err := rand.Read(codec.key[:]); err != nil {
		return nil, failure(Unavailable)
	}
	return codec, nil
}

func (c *CursorCodec) aead() (cipher.AEAD, error) {
	if c == nil || !c.valid {
		return nil, failure(Invalid)
	}
	block, err := aes.NewCipher(c.key[:])
	if err != nil {
		return nil, failure(Unavailable)
	}
	return cipher.NewGCM(block)
}

func cursorAAD(header []byte) []byte {
	return append([]byte("homeward-lists-cursor-v1"), header...)
}

func (c *CursorCodec) seal(payload []byte) (string, error) {
	aead, err := c.aead()
	if err != nil || len(payload) < 2 || payload[0] != cursorWireVersion ||
		(payload[1] != listCursorWireKind && payload[1] != itemCursorWireKind) {
		return "", failure(Invalid)
	}
	header := payload[:2]
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", failure(Unavailable)
	}
	sealed := append(append([]byte(nil), header...), nonce[:]...)
	sealed = aead.Seal(sealed, nonce[:], payload, cursorAAD(header))
	if base64.RawURLEncoding.EncodedLen(len(sealed)) > MaxCursorTokenBytes {
		return "", failure(Invalid)
	}
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (c *CursorCodec) open(token string, kind byte) ([]byte, error) {
	if token == "" || len(token) > MaxCursorTokenBytes {
		return nil, failure(StaleCursor)
	}
	aead, err := c.aead()
	if err != nil {
		return nil, failure(StaleCursor)
	}
	sealed, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(sealed) < 2+aead.NonceSize()+aead.Overhead()+2 ||
		sealed[0] != cursorWireVersion || sealed[1] != kind {
		return nil, failure(StaleCursor)
	}
	header := sealed[:2]
	nonce := sealed[2 : 2+aead.NonceSize()]
	payload, err := aead.Open(nil, nonce, sealed[2+aead.NonceSize():], cursorAAD(header))
	if err != nil || len(payload) < 2 || payload[0] != cursorWireVersion || payload[1] != kind {
		return nil, failure(StaleCursor)
	}
	return payload, nil
}

func appendU64(dst []byte, n uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], n)
	return append(dst, encoded[:]...)
}
func appendText(dst []byte, value string) []byte {
	var size [2]byte
	binary.BigEndian.PutUint16(size[:], uint16(len(value)))
	return append(append(dst, size[:]...), value...)
}
func readU64(src []byte, offset *int) (uint64, bool) {
	if len(src)-*offset < 8 {
		return 0, false
	}
	value := binary.BigEndian.Uint64(src[*offset : *offset+8])
	*offset += 8
	return value, true
}
func readText(src []byte, offset *int) (string, bool) {
	if len(src)-*offset < 2 {
		return "", false
	}
	size := int(binary.BigEndian.Uint16(src[*offset : *offset+2]))
	*offset += 2
	if size < 1 || size > MaxUIDBytes || len(src)-*offset < size {
		return "", false
	}
	value := string(src[*offset : *offset+size])
	*offset += size
	return value, true
}

func (c *CursorCodec) EncodeList(cursor ListCursor) (string, error) {
	if !cursor.valid || (cursor.kind != cursorViewAll && cursor.kind != cursorViewExplicit) || cursor.revision < 0 {
		return "", failure(Invalid)
	}
	last, err := cursor.last.Value()
	if err != nil {
		return "", failure(Invalid)
	}
	payload := []byte{cursorWireVersion, listCursorWireKind, byte(cursor.kind)}
	payload = appendU64(payload, uint64(cursor.revision))
	payload = appendU64(payload, cursor.rights)
	payload = append(payload, cursor.view[:]...)
	payload = append(payload, cursor.semantic[:]...)
	payload = appendText(payload, last)
	return c.seal(payload)
}
func (c *CursorCodec) DecodeList(token string) (ListCursor, error) {
	if token == "" {
		return ListCursor{}, nil
	}
	payload, err := c.open(token, listCursorWireKind)
	if err != nil || len(payload) < 3+8+8+sha256.Size*2+2 {
		return ListCursor{}, failure(StaleCursor)
	}
	kind := cursorViewKind(payload[2])
	if kind != cursorViewAll && kind != cursorViewExplicit {
		return ListCursor{}, failure(StaleCursor)
	}
	offset := 3
	revision, ok := readU64(payload, &offset)
	if !ok || revision > math.MaxInt64 {
		return ListCursor{}, failure(StaleCursor)
	}
	rights, ok := readU64(payload, &offset)
	if !ok {
		return ListCursor{}, failure(StaleCursor)
	}
	var view, semantic [sha256.Size]byte
	copy(view[:], payload[offset:offset+sha256.Size])
	offset += sha256.Size
	copy(semantic[:], payload[offset:offset+sha256.Size])
	offset += sha256.Size
	lastText, ok := readText(payload, &offset)
	if !ok || offset != len(payload) {
		return ListCursor{}, failure(StaleCursor)
	}
	last, err := domain.ParseListID(lastText)
	if err != nil || kind == cursorViewExplicit && revision != 0 {
		return ListCursor{}, failure(StaleCursor)
	}
	return ListCursor{valid: true, kind: kind, revision: int64(revision), view: view, last: last, rights: rights, semantic: semantic}, nil
}

func (c *CursorCodec) EncodeItem(cursor ItemCursor) (string, error) {
	if !cursor.valid || cursor.revision < 1 || cursor.lastPosition < 0 {
		return "", failure(Invalid)
	}
	list, err := cursor.list.Value()
	if err != nil {
		return "", failure(Invalid)
	}
	last, err := cursor.last.Value()
	if err != nil {
		return "", failure(Invalid)
	}
	payload := appendText([]byte{cursorWireVersion, itemCursorWireKind}, list)
	payload = appendU64(payload, uint64(cursor.revision))
	payload = appendU64(payload, uint64(cursor.lastPosition))
	payload = appendText(payload, last)
	return c.seal(payload)
}
func (c *CursorCodec) DecodeItem(token string) (ItemCursor, error) {
	if token == "" {
		return ItemCursor{}, nil
	}
	payload, err := c.open(token, itemCursorWireKind)
	if err != nil {
		return ItemCursor{}, failure(StaleCursor)
	}
	offset := 2
	listText, ok := readText(payload, &offset)
	if !ok {
		return ItemCursor{}, failure(StaleCursor)
	}
	list, err := domain.ParseListID(listText)
	if err != nil {
		return ItemCursor{}, failure(StaleCursor)
	}
	revision, ok := readU64(payload, &offset)
	if !ok || revision < 1 || revision > math.MaxInt64 {
		return ItemCursor{}, failure(StaleCursor)
	}
	position, ok := readU64(payload, &offset)
	if !ok || position > math.MaxInt64 {
		return ItemCursor{}, failure(StaleCursor)
	}
	lastText, ok := readText(payload, &offset)
	if !ok || offset != len(payload) {
		return ItemCursor{}, failure(StaleCursor)
	}
	last, err := ParseItemUID(lastText)
	if err != nil {
		return ItemCursor{}, failure(StaleCursor)
	}
	return ItemCursor{valid: true, list: list, revision: int64(revision), lastPosition: int64(position), last: last}, nil
}

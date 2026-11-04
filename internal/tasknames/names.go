// Package tasknames owns lossless public names and identity-bound task etags.
// Native/imported IDs are never normalized, hashed into an identity or renamed.
package tasknames

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"

	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/lists"
	"github.com/damione1/Homeward/internal/resourceid"
)

var ErrInvalid = errors.New("invalid task resource name or etag")

// Native identities permit up to 255 UTF-8 bytes. Hex preserves every byte and
// uses only lower-case DNS characters, at most 513 bytes per opaque segment.
// This server-assigned migration encoding intentionally exceeds the usual
// 63-character recommendation for historical IDs; it is canonical and bounded.
func segment(raw string) string             { encoded, _ := resourceid.Encode(raw); return encoded }
func native(segment string) (string, error) { return resourceid.Decode(segment) }

type ListName struct {
	home apitypes.HomeName
	id   domain.ListID
}
type TaskName struct {
	list ListName
	uid  lists.ItemUID
}

func List(home apitypes.HomeName, id domain.ListID) (ListName, error) {
	if _, err := id.Value(); err != nil || !home.Valid() {
		return ListName{}, ErrInvalid
	}
	return ListName{home, id}, nil
}
func Task(parent ListName, uid lists.ItemUID) (TaskName, error) {
	if _, err := uid.Value(); err != nil || !parent.home.Valid() {
		return TaskName{}, ErrInvalid
	}
	return TaskName{parent, uid}, nil
}
func (n ListName) String() string {
	if !n.home.Valid() {
		return ""
	}
	text, _ := n.id.Value()
	return n.home.String() + "/taskLists/" + segment(text)
}
func (n ListName) Home() apitypes.HomeName { return n.home }
func (n ListName) ID() domain.ListID       { return n.id }
func (n TaskName) String() string {
	if n.list.String() == "" {
		return ""
	}
	text, _ := n.uid.Value()
	return n.list.String() + "/tasks/" + segment(text)
}
func (n TaskName) Parent() ListName   { return n.list }
func (n TaskName) UID() lists.ItemUID { return n.uid }
func ParseList(raw string) (ListName, error) {
	if len(raw) > 1200 {
		return ListName{}, ErrInvalid
	}
	parts := strings.Split(raw, "/")
	if len(parts) != 4 || parts[2] != "taskLists" {
		return ListName{}, ErrInvalid
	}
	home, err := apitypes.ParseHomeName(strings.Join(parts[:2], "/"))
	if err != nil {
		return ListName{}, ErrInvalid
	}
	text, err := native(parts[3])
	if err != nil {
		return ListName{}, err
	}
	id, err := domain.ParseListID(text)
	if err != nil {
		return ListName{}, ErrInvalid
	}
	return List(home, id)
}
func ParseTask(raw string) (TaskName, error) {
	if len(raw) > 1800 {
		return TaskName{}, ErrInvalid
	}
	parts := strings.Split(raw, "/")
	if len(parts) != 6 || parts[4] != "tasks" {
		return TaskName{}, ErrInvalid
	}
	parent, err := ParseList(strings.Join(parts[:4], "/"))
	if err != nil {
		return TaskName{}, err
	}
	text, err := native(parts[5])
	if err != nil {
		return TaskName{}, err
	}
	uid, err := lists.ParseItemUID(text)
	if err != nil {
		return TaskName{}, ErrInvalid
	}
	return Task(parent, uid)
}

// Etags are opaque representation preconditions, never credentials. Binding to
// the exact canonical name prevents using another resource's same-number CAS.
func Etag(name string, listNumber, itemNumber int64) (string, error) {
	if name == "" || listNumber < 1 || itemNumber < 0 {
		return "", ErrInvalid
	}
	var raw [49]byte
	raw[0] = 2
	digest := sha256.Sum256([]byte(name))
	copy(raw[1:33], digest[:])
	binary.BigEndian.PutUint64(raw[33:41], uint64(listNumber))
	binary.BigEndian.PutUint64(raw[41:], uint64(itemNumber))
	return "\"" + base64.RawURLEncoding.EncodeToString(raw[:]) + "\"", nil
}

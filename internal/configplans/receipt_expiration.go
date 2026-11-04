package configplans

import (
	"github.com/damione1/Homeward/internal/storage"
	"time"
)

// ReceiptExpiration exposes only already-verified metadata to bounded public
// captures. It does not grant publication permission, extend validity, compile
// a candidate or return any signing key/source bytes.
func (s *Service) ReceiptExpiration(receipt string, author storage.ConfigAuthor, now time.Time) (time.Time, error) {
	name, err := usableAuthor(author)
	if err != nil {
		return time.Time{}, err
	}
	payload, err := s.verify(receipt, now)
	if err != nil {
		return time.Time{}, err
	}
	if payload.Author != name {
		return time.Time{}, ErrAuthorMismatch
	}
	return time.Unix(payload.Expires, 0).UTC(), nil
}

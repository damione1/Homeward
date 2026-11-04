package server

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"time"

	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/historyquery"
	"github.com/damione1/Homeward/internal/storage"
)

func (h *historyV2Handler) epochBinding(ctx context.Context, home apitypes.HomeName) ([]byte, error) {
	if !home.Valid() {
		return nil, historyquery.ErrInvalid
	}
	if err := h.route(ctx, historyquery.ExecutionTraces); err != nil {
		return nil, err
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil {
		return nil, authorization.ErrDenied
	}
	current, err := h.native.runtime.authority.Current(principal)
	if err != nil {
		return nil, err
	}
	return homePageBinding(current, home.String(), "", "HistoryExecutionEpoch")
}

// Epoch references are opaque selectors, not native IDs or authority. A new
// process or changed credential/rights invalidates them without a token store.
func (h *historyV2Handler) epochToken(ctx context.Context, home apitypes.HomeName, epoch storage.HistoryEpoch) (string, error) {
	if !epoch.Valid() {
		return "", historyquery.ErrIntegrity
	}
	binding, err := h.epochBinding(ctx, home)
	if err != nil {
		return "", err
	}
	raw, err := hex.DecodeString(epoch.String())
	if err != nil || len(raw) != 16 {
		return "", historyquery.ErrIntegrity
	}
	payload := make([]byte, 24)
	binary.BigEndian.PutUint64(payload[:8], uint64(time.Now().UTC().Add(historyquery.CursorTTL).Unix()))
	copy(payload[8:], raw)
	return sealAPIPage(h.epochs, payload, binding)
}
func (h *historyV2Handler) openEpoch(ctx context.Context, home apitypes.HomeName, token string) (string, error) {
	if token == "" {
		return "", nil
	}
	binding, err := h.epochBinding(ctx, home)
	if err != nil {
		return "", err
	}
	payload, err := openAPIPage(h.epochs, token, binding)
	if err != nil || len(payload) != 24 {
		return "", historyquery.ErrStale
	}
	expiry := int64(binary.BigEndian.Uint64(payload[:8]))
	now := time.Now().UTC()
	if expiry <= now.Unix() || expiry > now.Add(historyquery.CursorTTL).Unix() {
		return "", historyquery.ErrStale
	}
	epoch, err := storage.ParseHistoryEpoch(hex.EncodeToString(payload[8:]))
	if err != nil {
		return "", historyquery.ErrStale
	}
	return epoch.String(), nil
}

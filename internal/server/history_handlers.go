package server

import (
	"context"
	"github.com/damione1/Homeward/internal/historyquery"
	"github.com/damione1/Homeward/internal/storage"
)

// historyHandler owns the native admission and query lifecycle used by the v2
// projection. It exposes no legacy RPC methods.
type historyHandler struct{ runtime *historyRuntime }

func (h *historyHandler) recorderPage(ctx context.Context, q historyquery.Query, token, procedure string) (context.Context, historyAdmission, storage.RecorderQueryPage, func(), error) {
	leased, admitted, release, err := h.begin(ctx, q)
	if err != nil {
		return nil, admitted, storage.RecorderQueryPage{}, nil, err
	}
	var cursor *storage.RecorderQueryCursor
	if token != "" {
		raw, e := h.runtime.codec.Open(procedure, q, admitted.binding, token)
		if e == nil {
			cursor, e = storage.ParseRecorderQueryCursor(raw)
		}
		if e != nil {
			release()
			return nil, admitted, storage.RecorderQueryPage{}, nil, historyquery.ErrStale
		}
	}
	page, err := h.runtime.reader.ReadHistoricalRecorderPageAt(leased, q, cursor, admitted.liveHigh, historyRecordedAuthority{handler: h, query: q, admitted: admitted})
	if cause := historyContextError(leased); cause != nil {
		page.Release()
		release()
		return nil, admitted, storage.RecorderQueryPage{}, nil, cause
	}

	if err != nil {
		page.Release()
		release()
		return nil, admitted, storage.RecorderQueryPage{}, nil, err
	}
	if _, err = h.capture(leased, q, &admitted.binding); err != nil {
		page.Release()
		release()
		return nil, admitted, storage.RecorderQueryPage{}, nil, err
	}
	return leased, admitted, page, func() { page.Release(); release() }, nil
}

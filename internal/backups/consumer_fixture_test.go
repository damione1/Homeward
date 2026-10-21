package backups_test

import (
	"bytes"
	"context"
	"io"

	"github.com/damione1/Homeward/internal/backups"
)

// This producer/consumer fixture depends only on the port and opaque bytes.
// It contains no local/filesystem/storage imports or archive-format knowledge.
func consumeOpaqueArtifact(ctx context.Context, provider backups.Provider, id backups.ArtifactID, payload []byte) error {
	intent, err := backups.NewWriteIntent(id, int64(len(payload)))
	if err != nil {
		return err
	}
	result, err := provider.Write(ctx, intent, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	if result.State() != backups.Published && result.State() != backups.AlreadyPresent {
		return backups.NewError(backups.Unavailable)
	}
	verified, err := provider.Verify(ctx, id)
	if err != nil || !verified.Valid() {
		return backups.NewError(backups.Corrupt)
	}
	reader, err := provider.Open(ctx, id)
	if err != nil {
		return err
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if !bytes.Equal(got, payload) {
		return backups.NewError(backups.Corrupt)
	}
	return nil
}

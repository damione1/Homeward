package archive

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/damione1/Homeward/internal/storage"
)

func contextError(ctx context.Context) error {
	if ctx == nil {
		return fail(Invalid)
	}
	if ctx.Err() != nil {
		return fail(Cancelled)
	}
	return nil
}
func ioError(err error) error {
	var admitted *Error
	if errors.As(err, &admitted) {
		return admitted
	}
	return fail(Unavailable)
}

type contextReader struct {
	ctx    context.Context
	source io.Reader
	empty  int
}

func contextBound(ctx context.Context, source io.Reader) *contextReader {
	return &contextReader{ctx: ctx, source: source}
}
func (r *contextReader) Read(p []byte) (int, error) {
	if err := contextError(r.ctx); err != nil {
		return 0, err
	}
	n, err := r.source.Read(p)
	if n < 0 || n > len(p) {
		return 0, fail(Invalid)
	}
	if n == 0 && err == nil && len(p) > 0 {
		r.empty++
		if r.empty >= 100 {
			return 0, fail(Unavailable)
		}
	} else {
		r.empty = 0
	}
	if cancelled := contextError(r.ctx); cancelled != nil {
		return n, cancelled
	}
	return n, err
}

type budgetWriter struct {
	target    io.Writer
	remaining int64
}

func (w *budgetWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, fail(Capacity)
	}
	n, err := w.target.Write(p)
	w.remaining -= int64(n)
	return n, err
}
func token(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fail(Unavailable)
	}
	return prefix + hex.EncodeToString(b[:]), nil
}
func privateRoot(directory string) (*os.Root, error) {
	r, err := storage.OpenPrivateDataDir(directory)
	if err != nil {
		return nil, fail(Unavailable)
	}
	return r, nil
}
func syncRoot(root *os.Root, name string) error {
	f, err := root.Open(name)
	if err != nil {
		return fail(Unavailable)
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return fail(Unavailable)
	}
	return nil
}
func newPrivateFile(root *os.Root, prefix string) (*os.File, string, error) {
	name, err := token(prefix)
	if err != nil {
		return nil, "", err
	}
	f, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, "", fail(Unavailable)
	}
	return f, name, nil
}
func regularOwned(info os.FileInfo, private bool) bool {
	if info == nil || !info.Mode().IsRegular() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return false
	}
	if private {
		return info.Mode().Perm() == 0600
	}
	return info.Mode().Perm()&0022 == 0
}

// Root confinement is not a no-symlink policy: validate the leaf on both sides
// of Open and compare the resulting descriptor, including relative symlinks.
func readKeyFile(filePath string, private bool) ([]byte, error) {
	if !filepath.IsAbs(filePath) || filepath.Clean(filePath) != filePath || strings.ContainsRune(filePath, '\x00') {
		return nil, fail(Invalid)
	}
	r, err := privateRoot(filepath.Dir(filePath))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	name := filepath.Base(filePath)
	before, err := r.Lstat(name)
	if err != nil || !regularOwned(before, private) || before.Size() < 1 || before.Size() > 256 {
		return nil, fail(Invalid)
	}
	f, err := r.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fail(Unavailable)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !regularOwned(opened, private) || !os.SameFile(before, opened) {
		return nil, fail(Invalid)
	}
	after, err := r.Lstat(name)
	if err != nil || !regularOwned(after, private) || !os.SameFile(opened, after) {
		return nil, fail(Invalid)
	}
	raw, err := io.ReadAll(io.LimitReader(f, 257))
	if err != nil || len(raw) > 256 {
		clear(raw)
		return nil, fail(Invalid)
	}
	return raw, nil
}
func exclusiveFile(filePath string, data []byte) error {
	if !filepath.IsAbs(filePath) || filepath.Clean(filePath) != filePath || strings.ContainsRune(filePath, '\x00') {
		return fail(Invalid)
	}
	r, err := privateRoot(filepath.Dir(filePath))
	if err != nil {
		return err
	}
	defer r.Close()
	f, pending, err := newPrivateFile(r, ".recovery-export-")
	if err != nil {
		return err
	}
	defer r.Remove(pending)
	n, writeErr := f.Write(data)
	if n != len(data) && writeErr == nil {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		return fail(Unavailable)
	}
	if err := r.Link(pending, filepath.Base(filePath)); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fail(Conflict)
		}
		return fail(Unavailable)
	}
	// An error after Link never removes the published export.
	return syncRoot(r, ".")
}

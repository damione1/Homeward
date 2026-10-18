package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"github.com/damione1/Homeward/internal/storage"
)

const operatorSocketName = "admin.sock"
const operatorRequestBytes int64 = 1 << 20

var ErrOperatorSocketUnsafe = errors.New("server: unsafe operator socket")
var ErrOperatorSocketInUse = errors.New("server: operator socket already active")
var ErrOperatorSocketPathTooLong = errors.New("server: operator socket path exceeds AF_UNIX limit")
var ErrOperatorShutdownTimeout = errors.New("server: operator socket shutdown timed out")

type operatorListener struct {
	root     *os.Root
	listener *net.UnixListener
	server   *http.Server
	served   <-chan error
	active   *publicAdmissions
	file     os.FileInfo
}

// startOperatorListener is the only path to the generated operator service.
// It binds within the already validated 0700 data directory before public
// readiness, then admits only local peers whose kernel UID is our effective UID.
func startOperatorListener(ctx context.Context, dataDir string, store *storage.Store, coordinator *sessionCoordinator) (*operatorListener, error) {
	if ctx == nil || store == nil || coordinator == nil {
		return nil, ErrInvalidContext
	}
	root, err := storage.OpenPrivateDataDir(dataDir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, operatorSocketName)
	if len(path) > operatorSocketPathLimit {
		root.Close()
		return nil, ErrOperatorSocketPathTooLong
	}
	if err := removeStaleOperatorSocket(root, path); err != nil {
		root.Close()
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		root.Close()
		return nil, errors.Join(ErrOperatorSocketUnsafe, err)
	}
	listener.SetUnlinkOnClose(false)
	info, err := root.Lstat(operatorSocketName)
	if err != nil || !operatorFileOwned(info) {
		listener.Close()
		root.Close()
		return nil, errors.Join(ErrOperatorSocketUnsafe, errors.New("initial socket identity"), err)
	}
	createdInfo := info
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		_ = removeFreshOperatorSocket(root, createdInfo)
		root.Close()
		return nil, errors.Join(ErrOperatorSocketUnsafe, errors.New("socket permissions"), err)
	}
	info, err = root.Lstat(operatorSocketName)
	if err != nil || !operatorFileValid(info) || info.Mode().Perm() != 0600 || !os.SameFile(createdInfo, info) {
		listener.Close()
		_ = removeFreshOperatorSocket(root, createdInfo)
		root.Close()
		return nil, errors.Join(ErrOperatorSocketUnsafe, errors.New("final socket identity"), err)
	}
	_, handler := authv1connect.NewOperatorAccountServiceHandler(&accountHandler{store: store, coordinator: coordinator})
	active := newPublicAdmissions()
	server := &http.Server{
		Handler: http.MaxBytesHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !active.begin() {
				http.Error(w, "operator shutting down", http.StatusServiceUnavailable)
				return
			}
			defer active.end()
			handler.ServeHTTP(w, r)
		}), operatorRequestBytes),
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(&sameUIDListener{UnixListener: listener}) }()
	return &operatorListener{root: root, listener: listener, server: server, served: served, active: active, file: info}, nil
}

func operatorFileValid(info os.FileInfo) bool {
	return operatorFileOwned(info) && info.Mode().Perm()&0077 == 0
}

func operatorFileOwned(info os.FileInfo) bool {
	if info == nil || info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func removeStaleOperatorSocket(root *os.Root, path string) error {
	info, err := root.Lstat(operatorSocketName)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !operatorFileValid(info) {
		return ErrOperatorSocketUnsafe
	}
	conn, dialErr := net.DialTimeout("unix", path, 200*time.Millisecond)
	if dialErr == nil {
		conn.Close()
		return ErrOperatorSocketInUse
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		return ErrOperatorSocketUnsafe
	}
	return removeOwnedOperatorSocket(root, info)
}

func removeOwnedOperatorSocket(root *os.Root, original os.FileInfo) error {
	current, err := root.Lstat(operatorSocketName)
	if err != nil || !operatorFileValid(current) || !os.SameFile(original, current) {
		return ErrOperatorSocketUnsafe
	}
	if err := root.Remove(operatorSocketName); err != nil {
		return ErrOperatorSocketUnsafe
	}
	return nil
}

func removeFreshOperatorSocket(root *os.Root, original os.FileInfo) error {
	current, err := root.Lstat(operatorSocketName)
	if err != nil || !operatorFileOwned(current) || !os.SameFile(original, current) {
		return ErrOperatorSocketUnsafe
	}
	if err := root.Remove(operatorSocketName); err != nil {
		return ErrOperatorSocketUnsafe
	}
	return nil
}

// stop closes admissions first, then returns a channel that closes only after
// all in-flight operator handlers have returned. Store must outlive that drain.
func (o *operatorListener) stop(ctx context.Context) (<-chan struct{}, error) {
	if o == nil {
		return closedPublicDone(), nil
	}
	done := o.active.close()
	err := o.server.Shutdown(ctx)
	if err != nil {
		err = errors.Join(ErrOperatorShutdownTimeout, err, o.server.Close())
	}
	serveErr := <-o.served
	if !errors.Is(serveErr, http.ErrServerClosed) && !errors.Is(serveErr, net.ErrClosed) {
		err = errors.Join(err, serveErr)
	}
	if unlinkErr := removeOwnedOperatorSocket(o.root, o.file); unlinkErr != nil {
		err = errors.Join(err, unlinkErr)
	}
	err = errors.Join(err, o.root.Close())
	return done, err
}

type sameUIDListener struct{ *net.UnixListener }

func (l *sameUIDListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.UnixListener.AcceptUnix()
		if err != nil {
			return nil, err
		}
		uid, err := operatorPeerUID(conn)
		if admitOperatorUID(uid, uint32(os.Geteuid()), err) {
			return conn, nil
		}
		conn.Close()
	}
}

func admitOperatorUID(peer, operator uint32, credentialErr error) bool {
	return credentialErr == nil && peer == operator
}

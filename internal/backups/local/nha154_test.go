package local

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/backups"
)

func nha154ID(n int) backups.ArtifactID {
	id, _ := backups.ParseArtifactID(fmt.Sprintf("%032x", n))
	return id
}
func nha154Instance(t *testing.T, raw string) backups.InstanceID {
	t.Helper()
	id, err := backups.ParseInstanceID(raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func nha154Open(t *testing.T, root, instance string, limits backups.Limits) *provider {
	t.Helper()
	settings, err := backups.NewLocalSettings(nha154Instance(t, instance), true, root, limits)
	if err != nil {
		t.Fatal(err)
	}
	p, err := open(context.Background(), settings, productionIO())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	})
	return p
}
func nha154Provider(t *testing.T) (*provider, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "store")
	return nha154Open(t, root, "local", backups.DefaultLimits()), root
}
func nha154Write(t *testing.T, p backups.Provider, id backups.ArtifactID, payload []byte) backups.Metadata {
	t.Helper()
	intent, err := backups.NewWriteIntent(id, int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Write(context.Background(), intent, bytes.NewReader(payload))
	metadata, ok := result.Metadata()
	if err != nil || result.State() != backups.Published || !ok || metadata.Size() != int64(len(payload)) || metadata.SHA256() != sha256.Sum256(payload) {
		t.Fatalf("publish result=%v metadata=%v err=%v", result.State(), ok, err)
	}
	return metadata
}
func nha154List(t *testing.T, p backups.Provider) []backups.Metadata {
	t.Helper()
	request, _ := backups.NewListRequest(backups.MaxPageSize, backups.Cursor{})
	page, err := p.List(context.Background(), request)
	if err != nil || !page.Valid() || page.Next().Present() {
		t.Fatalf("list: %v", err)
	}
	return page.Items()
}
func nha154Kind(t *testing.T, err error, kind backups.ErrorKind) {
	t.Helper()
	var admitted *backups.Error
	if !errors.As(err, &admitted) || admitted.Kind() != kind {
		t.Fatalf("want %s, got %v", kind, err)
	}
}

func TestNHA154LocalRoundTrip(t *testing.T) {
	p, root := nha154Provider(t)
	capabilities := p.Capabilities()
	if !capabilities.Valid() || !capabilities.Write() || !capabilities.List() || !capabilities.Open() || !capabilities.Delete() || !capabilities.Verify() || !capabilities.AtomicPublication() || capabilities.Limits() != backups.DefaultLimits() {
		t.Fatal("incomplete capabilities")
	}
	payload := bytes.Repeat([]byte("opaque\x00archive\xff"), 9000)
	metadata := nha154Write(t, p, nha154ID(1), payload)
	reader, err := p.Open(context.Background(), metadata.ID())
	if err != nil || reader.Metadata() != metadata {
		t.Fatalf("open: %v", err)
	}
	got, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("read: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	verified, err := p.Verify(context.Background(), metadata.ID())
	if err != nil || !verified.Valid() || verified.Metadata() != metadata {
		t.Fatalf("verify: %v", err)
	}
	items := nha154List(t, p)
	if len(items) != 1 || items[0] != metadata {
		t.Fatal("metadata not retained")
	}
	fileInfo, err := os.Stat(filepath.Join(root, "local", filename(metadata.ID())))
	if err != nil || fileInfo.Mode().Perm() != 0600 || fileInfo.Size() != headerBytes+int64(len(payload)) {
		t.Fatalf("envelope/mode: %v", err)
	}
	if strings.Contains(fmt.Sprintf("%+v %#v", p, p), root) {
		t.Fatal("private path escaped formatting")
	}
	intent, _ := backups.NewWriteIntent(metadata.ID(), int64(len(payload)))
	result, err := p.Write(context.Background(), intent, bytes.NewReader(payload))
	old, ok := result.Metadata()
	if err != nil || result.State() != backups.AlreadyPresent || !ok || old != metadata {
		t.Fatalf("retry: %v", err)
	}
	result, err = p.Write(context.Background(), intent, bytes.NewReader(bytes.Repeat([]byte{'x'}, len(payload))))
	nha154Kind(t, err, backups.Conflict)
	if result.State() != backups.NotPublished || result.ID() != metadata.ID() {
		t.Fatal("conflict replaced identity/state")
	}
	if verified, err := p.Verify(context.Background(), metadata.ID()); err != nil || verified.Metadata() != metadata {
		t.Fatal("conflict changed old artifact")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := nha154Open(t, root, "local", backups.DefaultLimits())
	if got := nha154List(t, reopened); len(got) != 1 || got[0] != metadata {
		t.Fatal("restart lost publication")
	}
}

type interruptedReader struct{ sent bool }

func (r *interruptedReader) Read(buffer []byte) (int, error) {
	if r.sent {
		return 0, errors.New("NHA154 private reader diagnostic")
	}
	r.sent = true
	return copy(buffer, "partial bytes"), nil
}
func TestNHA154InterruptedPublication(t *testing.T) {
	p, root := nha154Provider(t)
	old := nha154Write(t, p, nha154ID(1), []byte("preserved"))
	intent, _ := backups.NewWriteIntent(nha154ID(2), 1024)
	result, err := p.Write(context.Background(), intent, &interruptedReader{})
	nha154Kind(t, err, backups.Unavailable)
	if result.State() != backups.NotPublished || strings.Contains(err.Error(), "private reader") {
		t.Fatal("interruption leaked/claimed publication")
	}
	if err := os.WriteFile(filepath.Join(root, "local", ".pending-dead-writer"), []byte("incomplete"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "local", filename(nha154ID(3))), []byte("incomplete envelope"), 0600); err != nil {
		t.Fatal(err)
	}
	if items := nha154List(t, p); len(items) != 1 || items[0] != old {
		t.Fatal("partial artifact advertised")
	}
	_, err = p.Verify(context.Background(), nha154ID(2))
	nha154Kind(t, err, backups.NotFound)
	_, err = p.Verify(context.Background(), nha154ID(3))
	nha154Kind(t, err, backups.Corrupt)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err = p.Write(ctx, intent, bytes.NewReader(nil))
	nha154Kind(t, err, backups.Cancelled)
	if result.State() != backups.NotPublished {
		t.Fatal("cancelled publication")
	}
}

type crashReader struct{ sent bool }

func (r *crashReader) Read(buffer []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(buffer, bytes.Repeat([]byte{'p'}, 1000)), nil
	}
	_, _ = fmt.Fprintln(os.Stdout, "pending-written")
	select {}
}
func TestNHA154CrashHelper(t *testing.T) {
	if os.Getenv("HOMEWARD_NHA154_HELPER") != "1" {
		return
	}
	root := os.Getenv("HOMEWARD_NHA154_ROOT")
	p := nha154Open(t, root, "local", backups.DefaultLimits())
	intent, _ := backups.NewWriteIntent(nha154ID(2), 10000)
	_, _ = p.Write(context.Background(), intent, &crashReader{})
	t.Fatal("crash helper unexpectedly returned")
}
func TestNHA154KilledWriterKeepsOldArtifact(t *testing.T) {
	p, root := nha154Provider(t)
	old := nha154Write(t, p, nha154ID(1), []byte("preexisting complete"))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestNHA154CrashHelper$")
	command.Env = append(os.Environ(), "HOMEWARD_NHA154_HELPER=1", "HOMEWARD_NHA154_ROOT="+root)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != "pending-written\n" {
			t.Fatalf("helper not at interrupted payload: %q %s", line, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("helper failed to reach pending payload")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("writer was not killed")
	}
	entries, err := os.ReadDir(filepath.Join(root, "local"))
	if err != nil {
		t.Fatal(err)
	}
	foundPending := false
	for _, entry := range entries {
		foundPending = foundPending || strings.HasPrefix(entry.Name(), ".pending-")
	}
	if !foundPending {
		t.Fatal("kill did not leave the actual uncommitted payload")
	}
	if items := nha154List(t, p); len(items) != 1 || items[0] != old {
		t.Fatal("killed writer published partial/lost old artifact")
	}
	_, err = p.Verify(context.Background(), nha154ID(2))
	nha154Kind(t, err, backups.NotFound)
}

func TestNHA154ConfinementAndIOFailures(t *testing.T) {
	t.Run("admission-and-symlinks", func(t *testing.T) {
		p, root := nha154Provider(t)
		old := nha154Write(t, p, nha154ID(1), []byte("old"))
		outside := filepath.Join(t.TempDir(), "private-canary")
		if err := os.WriteFile(outside, []byte("NHA154_PRIVATE_CANARY"), 0600); err != nil {
			t.Fatal(err)
		}
		for _, raw := range []string{"../x", "/tmp/canary", "x/y", strings.Repeat("A", 32), strings.Repeat("0", 33), ""} {
			if _, err := backups.ParseArtifactID(raw); err == nil {
				t.Fatalf("accepted traversal %q", raw)
			}
		}
		for _, raw := range []string{"../x", "x/y", "/local", "", ".", "..", "x\\y"} {
			if _, err := backups.ParseInstanceID(raw); err == nil {
				t.Fatalf("accepted instance %q", raw)
			}
		}
		linkPath := filepath.Join(root, "local", filename(nha154ID(2)))
		if err := os.Symlink(outside, linkPath); err != nil {
			t.Fatal(err)
		}
		_, err := p.Open(context.Background(), nha154ID(2))
		nha154Kind(t, err, backups.PermissionDenied)
		_, err = p.Verify(context.Background(), nha154ID(2))
		nha154Kind(t, err, backups.PermissionDenied)
		_, err = p.Delete(context.Background(), nha154ID(2))
		nha154Kind(t, err, backups.PermissionDenied)
		intent, _ := backups.NewWriteIntent(nha154ID(2), 3)
		_, err = p.Write(context.Background(), intent, strings.NewReader("new"))
		nha154Kind(t, err, backups.PermissionDenied)
		if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), outside) || strings.Contains(err.Error(), "CANARY") {
			t.Fatal("filesystem error leaked")
		}
		got, err := os.ReadFile(outside)
		if err != nil || string(got) != "NHA154_PRIVATE_CANARY" {
			t.Fatal("outside root modified")
		}
		if err := syscall.Mkfifo(filepath.Join(root, "local", filename(nha154ID(3))), 0600); err != nil {
			t.Fatal(err)
		}
		_, err = p.Open(context.Background(), nha154ID(3))
		nha154Kind(t, err, backups.PermissionDenied)
		if items := nha154List(t, p); len(items) != 1 || items[0] != old {
			t.Fatal("nonregular entries became artifacts")
		}
		if err := os.Chmod(filepath.Join(root, "local"), 0500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "local"), 0700) })
		_, err = p.Write(context.Background(), intent, strings.NewReader("new"))
		nha154Kind(t, err, backups.PermissionDenied)
		if err := os.Chmod(filepath.Join(root, "local"), 0700); err != nil {
			t.Fatal(err)
		}
		if verified, err := p.Verify(context.Background(), old.ID()); err != nil || verified.Metadata() != old {
			t.Fatal("permission failure changed old artifact")
		}
		alias := filepath.Join(t.TempDir(), "linked-store")
		if err := os.Symlink(root, alias); err != nil {
			t.Fatal(err)
		}
		settings, _ := backups.NewLocalSettings(nha154Instance(t, "local"), true, alias, backups.DefaultLimits())
		if opened, err := open(context.Background(), settings, productionIO()); err == nil {
			opened.Close()
			t.Fatal("symlink root admitted")
		}
		if err := os.Symlink(filepath.Join(root, "local"), filepath.Join(root, "neighbor")); err != nil {
			t.Fatal(err)
		}
		settings, _ = backups.NewLocalSettings(nha154Instance(t, "neighbor"), true, root, backups.DefaultLimits())
		if opened, err := open(context.Background(), settings, productionIO()); err == nil {
			opened.Close()
			t.Fatal("symlink instance admitted")
		}
	})
	t.Run("internal-relative-leaf", func(t *testing.T) {
		p, root := nha154Provider(t)
		old := nha154Write(t, p, nha154ID(1), []byte("preexisting artifact"))
		payload := []byte("valid envelope for the symlink's own identity")
		metadata := nha154Write(t, p, nha154ID(4), payload)
		canonical := filepath.Join(root, "local", filename(metadata.ID()))
		const targetName = "preserved-internal-envelope"
		target := filepath.Join(root, "local", targetName)
		if err := os.Rename(canonical, target); err != nil {
			t.Fatal(err)
		}
		original, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(targetName, canonical); err != nil {
			t.Fatal(err)
		}
		_, err = p.Open(context.Background(), metadata.ID())
		nha154Kind(t, err, backups.PermissionDenied)
		_, err = p.Verify(context.Background(), metadata.ID())
		nha154Kind(t, err, backups.PermissionDenied)
		intent, _ := backups.NewWriteIntent(metadata.ID(), int64(len(payload)))
		write, err := p.Write(context.Background(), intent, bytes.NewReader(payload))
		nha154Kind(t, err, backups.PermissionDenied)
		if write.State() != backups.NotPublished || write.ID() != metadata.ID() {
			t.Fatal("symlink retry claimed an existing published artifact")
		}
		deleted, err := p.Delete(context.Background(), metadata.ID())
		nha154Kind(t, err, backups.PermissionDenied)
		if deleted.State() != backups.DeleteNotApplied || deleted.ID() != metadata.ID() {
			t.Fatal("symlink deletion claimed an effect")
		}
		if info, err := os.Lstat(canonical); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatal("refused operation removed or replaced the symlink")
		}
		unchanged, err := os.ReadFile(target)
		if err != nil || !bytes.Equal(original, unchanged) {
			t.Fatal("refused operation changed the internal target envelope")
		}
		if verified, err := p.Verify(context.Background(), old.ID()); err != nil || verified.Metadata() != old {
			t.Fatal("refused operation changed a previous artifact")
		}
		if items := nha154List(t, p); len(items) != 1 || items[0] != old {
			t.Fatal("symlink and target did not remain excluded from listing")
		}
		if err := os.Remove(canonical); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(target, canonical); err != nil {
			t.Fatal(err)
		}
		if verified, err := p.Verify(context.Background(), metadata.ID()); err != nil || verified.Metadata() != metadata {
			t.Fatal("internal target was not the intact valid envelope for the same identity")
		}
	})
	tests := []struct {
		name   string
		mutate func(ioOperations) ioOperations
		kind   backups.ErrorKind
		state  backups.PublicationState
	}{
		{"enospc", func(ops ioOperations) ioOperations {
			ops.write = func(f *os.File, b []byte) (int, error) {
				if len(b) == headerBytes {
					return f.Write(b)
				}
				return 0, &os.PathError{Op: "write", Path: "NHA154_PRIVATE_PATH", Err: syscall.ENOSPC}
			}
			return ops
		}, backups.Capacity, backups.NotPublished},
		{"short-write", func(ops ioOperations) ioOperations {
			ops.write = func(f *os.File, b []byte) (int, error) { return f.Write(b[:len(b)-1]) }
			return ops
		}, backups.Unavailable, backups.NotPublished},
		{"header-write", func(ops ioOperations) ioOperations {
			ops.writeAt = func(*os.File, []byte, int64) (int, error) { return 0, syscall.ENOSPC }
			return ops
		}, backups.Capacity, backups.NotPublished},
		{"file-sync", func(ops ioOperations) ioOperations {
			ops.fileSync = func(*os.File) error { return syscall.EIO }
			return ops
		}, backups.Unavailable, backups.NotPublished},
		{"file-close", func(ops ioOperations) ioOperations {
			ops.fileClose = func(f *os.File) error { _ = f.Close(); return syscall.EIO }
			return ops
		}, backups.Unavailable, backups.NotPublished},
		{"link-permission", func(ops ioOperations) ioOperations {
			ops.link = func(*os.Root, string, string) error { return syscall.EACCES }
			return ops
		}, backups.PermissionDenied, backups.NotPublished},
		{"link-after-effect", func(ops ioOperations) ioOperations {
			ops.link = func(r *os.Root, a, b string) error {
				if err := r.Link(a, b); err != nil {
					return err
				}
				return syscall.EIO
			}
			return ops
		}, backups.Unavailable, backups.Indeterminate},
		{"pending-remove", func(ops ioOperations) ioOperations {
			ops.remove = func(*os.Root, string) error { return syscall.EIO }
			return ops
		}, backups.Unavailable, backups.Indeterminate},
		{"directory-sync", func(ops ioOperations) ioOperations {
			ops.dirSync = func(*os.File) error { return syscall.EIO }
			return ops
		}, backups.Unavailable, backups.Indeterminate},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p, _ := nha154Provider(t)
			old := nha154Write(t, p, nha154ID(1), []byte("untouched"))
			p.io = test.mutate(productionIO())
			intent, _ := backups.NewWriteIntent(nha154ID(2), 1024)
			result, err := p.Write(context.Background(), intent, strings.NewReader("new opaque bytes"))
			nha154Kind(t, err, test.kind)
			if result.State() != test.state || result.ID() != intent.ID() {
				t.Fatalf("state=%v, expected=%v", result.State(), test.state)
			}
			if _, ok := result.Metadata(); ok || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("failed operation exposed metadata/diagnostics")
			}
			p.io = productionIO()
			verified, err := p.Verify(context.Background(), old.ID())
			if err != nil || verified.Metadata() != old {
				t.Fatalf("old artifact damaged: %v", err)
			}
			if test.state == backups.Indeterminate {
				if _, err := p.Verify(context.Background(), intent.ID()); err != nil {
					t.Fatalf("ambiguous final removed: %v", err)
				}
				result, err := p.Write(context.Background(), intent, strings.NewReader("new opaque bytes"))
				if err != nil || result.State() != backups.AlreadyPresent {
					t.Fatalf("stable-ID recovery: %v", err)
				}
			} else {
				if items := nha154List(t, p); len(items) != 1 {
					t.Fatal("failed write became visible")
				}
			}
		})
	}
}

func TestNHA154BoundedListing(t *testing.T) {
	p, root := nha154Provider(t)
	for _, n := range []int{3, 1, 2} {
		nha154Write(t, p, nha154ID(n), []byte{byte(n)})
	}
	for _, name := range []string{"foreign.txt", ".pending-0123456789", filename(nha154ID(99))} {
		if err := os.WriteFile(filepath.Join(root, "local", name), []byte("not a complete envelope"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	request, _ := backups.NewListRequest(2, backups.Cursor{})
	page, err := p.List(context.Background(), request)
	items := page.Items()
	if err != nil || !page.Valid() || len(items) != 2 || items[0].ID() != nha154ID(1) || items[1].ID() != nha154ID(2) || !page.Next().Present() {
		t.Fatalf("first page: %v", err)
	}
	items[0] = backups.Metadata{}
	if !page.Items()[0].Valid() {
		t.Fatal("mutable page backing store")
	}
	request, _ = backups.NewListRequest(2, page.Next())
	last, err := p.List(context.Background(), request)
	if err != nil || len(last.Items()) != 1 || last.Items()[0].ID() != nha154ID(3) || last.Next().Present() {
		t.Fatalf("last page: %v", err)
	}
	foreign, _ := backups.NewCursor(nha154Instance(t, "other"), nha154ID(1))
	request, _ = backups.NewListRequest(2, foreign)
	_, err = p.List(context.Background(), request)
	nha154Kind(t, err, backups.InvalidInput)
	if _, err := backups.NewListRequest(backups.MaxPageSize+1, backups.Cursor{}); err == nil {
		t.Fatal("unbounded page admitted")
	}
	limits, _ := backups.NewLimits(1024, 2, 4, 2)
	limited := nha154Open(t, filepath.Join(t.TempDir(), "limited"), "local", limits)
	localSettingsRoot := limited.root.Name()
	for i := 0; i < 5; i++ {
		if err := os.WriteFile(filepath.Join(localSettingsRoot, fmt.Sprintf("foreign-%d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	request, _ = backups.NewListRequest(2, backups.Cursor{})
	page, err = limited.List(context.Background(), request)
	nha154Kind(t, err, backups.Capacity)
	if page.Valid() || len(page.Items()) != 0 {
		t.Fatal("scan limit returned a partial success")
	}
}

func TestNHA154ReadDeleteVerifyIsolation(t *testing.T) {
	p, root := nha154Provider(t)
	neighbor := nha154Open(t, root, "neighbor", backups.DefaultLimits())
	for _, provider := range []backups.Provider{p, neighbor} {
		nha154Write(t, provider, nha154ID(1), []byte("same identity, isolated instance"))
	}
	nha154Write(t, p, nha154ID(2), nil)
	verified, err := p.Verify(context.Background(), nha154ID(2))
	if err != nil || verified.Metadata().Size() != 0 || verified.Metadata().SHA256() != sha256.Sum256(nil) {
		t.Fatalf("empty payload: %v", err)
	}
	reader, err := p.Open(context.Background(), nha154ID(2))
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(reader)
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if readErr != nil || len(got) != 0 {
		t.Fatalf("empty read: %v", readErr)
	}
	path := filepath.Join(root, "local", filename(nha154ID(1)))
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{'X'}, headerBytes); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = p.Verify(context.Background(), nha154ID(1))
	nha154Kind(t, err, backups.Corrupt)
	reader, err = p.Open(context.Background(), nha154ID(1))
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(reader)
	nha154Kind(t, err, backups.Corrupt)
	_ = reader.Close()
	if err := os.Truncate(path, headerBytes+1); err != nil {
		t.Fatal(err)
	}
	_, err = p.Open(context.Background(), nha154ID(1))
	nha154Kind(t, err, backups.Corrupt)
	result, err := p.Delete(context.Background(), nha154ID(1))
	if err != nil || result.State() != backups.Deleted {
		t.Fatalf("targeted delete: %v", err)
	}
	_, err = p.Verify(context.Background(), nha154ID(1))
	nha154Kind(t, err, backups.NotFound)
	if _, err := neighbor.Verify(context.Background(), nha154ID(1)); err != nil {
		t.Fatal("deleted neighbor")
	}
	if _, err := p.Verify(context.Background(), nha154ID(2)); err != nil {
		t.Fatal("deleted other artifact")
	}
	result, err = p.Delete(context.Background(), nha154ID(1))
	if err != nil || result.State() != backups.AlreadyAbsent {
		t.Fatalf("idempotent delete: %v", err)
	}
	p.io.dirSync = func(*os.File) error { return syscall.EIO }
	result, err = p.Delete(context.Background(), nha154ID(2))
	nha154Kind(t, err, backups.Unavailable)
	if result.State() != backups.DeleteIndeterminate || result.ID() != nha154ID(2) {
		t.Fatal("delete ambiguity hidden")
	}
	p.io = productionIO()
	_, err = p.Verify(context.Background(), nha154ID(2))
	nha154Kind(t, err, backups.NotFound)
}

func TestNHA154ConcurrencyLimitsAndOwnership(t *testing.T) {
	p, _ := nha154Provider(t)
	intent, _ := backups.NewWriteIntent(nha154ID(1), 4096)
	start := make(chan struct{})
	results := make(chan backups.PublicationState, 4)
	failures := make(chan error, 4)
	var workers sync.WaitGroup
	for i := 0; i < 4; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			result, err := p.Write(context.Background(), intent, strings.NewReader("same bytes"))
			results <- result.State()
			failures <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	published := 0
	for state := range results {
		if state == backups.Published {
			published++
		} else if state != backups.AlreadyPresent {
			t.Fatalf("concurrent state %v", state)
		}
	}
	if published != 1 {
		t.Fatalf("published %d times", published)
	}
	readers := make([]backups.ArtifactReader, 0, 4)
	for i := 0; i < 4; i++ {
		reader, err := p.Open(context.Background(), nha154ID(1))
		if err != nil {
			t.Fatal(err)
		}
		readers = append(readers, reader)
	}
	_, err := p.Verify(context.Background(), nha154ID(1))
	nha154Kind(t, err, backups.Capacity)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	for _, reader := range readers {
		_, err := reader.Read(make([]byte, 1))
		if err == nil {
			t.Fatal("owner close left borrowed reader usable")
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
	}
	_, err = p.Open(context.Background(), nha154ID(1))
	nha154Kind(t, err, backups.Closed)
}

type nilReader struct{}

func (*nilReader) Read([]byte) (int, error) { panic("typed nil reader invoked") }

type nilLocalContext struct{ context.Context }

func TestNHA154ZeroAndBoundedWriteAdmission(t *testing.T) {
	p, _ := nha154Provider(t)
	var reader *nilReader
	var ctx *nilLocalContext
	intent, _ := backups.NewWriteIntent(nha154ID(1), 4)
	_, err := p.Write(context.Background(), backups.WriteIntent{}, strings.NewReader("x"))
	nha154Kind(t, err, backups.InvalidInput)
	_, err = p.Write(context.Background(), intent, reader)
	nha154Kind(t, err, backups.InvalidInput)
	_, err = p.Write(ctx, intent, strings.NewReader("x"))
	nha154Kind(t, err, backups.InvalidInput)
	_, err = p.Open(context.Background(), backups.ArtifactID{})
	nha154Kind(t, err, backups.InvalidInput)
	_, err = p.Verify(context.Background(), backups.ArtifactID{})
	nha154Kind(t, err, backups.InvalidInput)
	_, err = p.Delete(context.Background(), backups.ArtifactID{})
	nha154Kind(t, err, backups.InvalidInput)
	_, err = p.List(context.Background(), backups.ListRequest{})
	nha154Kind(t, err, backups.InvalidInput)
	result, err := p.Write(context.Background(), intent, strings.NewReader("too many bytes"))
	nha154Kind(t, err, backups.Capacity)
	if result.State() != backups.NotPublished || len(nha154List(t, p)) != 0 {
		t.Fatal("oversize payload published")
	}
	intent, _ = backups.NewWriteIntent(nha154ID(1), 0)
	result, err = p.Write(context.Background(), intent, bytes.NewReader(nil))
	if err != nil || result.State() != backups.Published {
		t.Fatalf("explicit empty intent: %v", err)
	}
}

type blockedReader struct {
	sent    bool
	blocked chan struct{}
	release chan struct{}
}

func (r *blockedReader) Read(buffer []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(buffer, "partial bytes"), nil
	}
	close(r.blocked)
	<-r.release
	return 0, io.EOF
}
func TestNHA154CloseDuringWriteCannotPublish(t *testing.T) {
	p, root := nha154Provider(t)
	old := nha154Write(t, p, nha154ID(1), []byte("preexisting"))
	source := &blockedReader{blocked: make(chan struct{}), release: make(chan struct{})}
	intent, _ := backups.NewWriteIntent(nha154ID(2), 100)
	type outcome struct {
		result backups.WriteResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() { result, err := p.Write(context.Background(), intent, source); done <- outcome{result, err} }()
	select {
	case <-source.blocked:
	case <-time.After(5 * time.Second):
		close(source.release)
		t.Fatal("writer did not reach blocked producer")
	}
	if err := p.Close(); err != nil {
		close(source.release)
		t.Fatal(err)
	}
	close(source.release)
	select {
	case result := <-done:
		if result.err == nil || result.result.State() != backups.NotPublished {
			t.Fatalf("closed writer published: %v", result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("released writer did not finish")
	}
	reopened := nha154Open(t, root, "local", backups.DefaultLimits())
	items := nha154List(t, reopened)
	if len(items) != 1 || items[0] != old {
		t.Fatal("close exposed partial or deleted old artifact")
	}
}

package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"
)

const maximumLocalFile = 2 << 20

var safeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

type profileName struct{ value string }

func parseProfileName(raw string) (profileName, error) {
	if !safeName.MatchString(raw) {
		return profileName{}, fail("invalid_profile_name", ExitUsage)
	}
	return profileName{value: raw}, nil
}

type privateDirectory struct {
	fd   int
	path string
}

// Walk every component with O_NOFOLLOW. The final directory is private and
// owned by the operator; ancestors may be shared but cannot be symlinks.
func openPrivateDirectory(path string, create bool) (*privateDirectory, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, fail("private_directory_requires_absolute_clean_path", ExitUsage)
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fail("private_directory_unavailable", ExitLocal)
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for index, part := range parts {
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(openErr, unix.ENOENT) && create {
			if mkdirErr := unix.Mkdirat(fd, part, 0700); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = unix.Close(fd)
				return nil, fail("private_directory_unavailable", ExitLocal)
			}
			next, openErr = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		_ = unix.Close(fd)
		if openErr != nil {
			return nil, fail("unsafe_private_directory", ExitLocal)
		}
		fd = next
		if index == len(parts)-1 {
			var stat unix.Stat_t
			if unix.Fstat(fd, &stat) != nil || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0777 != 0700 {
				_ = unix.Close(fd)
				return nil, fail("private_directory_requires_owner_mode_0700", ExitLocal)
			}
		}
	}
	return &privateDirectory{fd: fd, path: path}, nil
}
func (d *privateDirectory) close() error { return unix.Close(d.fd) }

func (d *privateDirectory) read(name string, value any) error {
	fd, err := unix.Openat(d.fd, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return os.ErrNotExist
	}
	if err != nil {
		return fail("unsafe_local_file", ExitLocal)
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0777 != 0600 || stat.Nlink != 1 || stat.Size > maximumLocalFile {
		return fail("local_file_requires_owner_mode_0600", ExitLocal)
	}
	data, err := io.ReadAll(io.LimitReader(file, maximumLocalFile+1))
	if err != nil || len(data) > maximumLocalFile {
		return fail("invalid_local_file", ExitLocal)
	}
	defer clear(data)
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil {
		return fail("invalid_local_file", ExitLocal)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return fail("invalid_local_file", ExitLocal)
	}
	return nil
}

func (d *privateDirectory) write(name string, value any, replace bool) error {
	data, err := json.Marshal(value)
	if err != nil || len(data) > maximumLocalFile {
		return fail("invalid_local_file", ExitLocal)
	}
	defer clear(data)
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fail("local_file_write_failed", ExitLocal)
	}
	temporary := ".pending-" + hex.EncodeToString(random[:])
	fd, err := unix.Openat(d.fd, temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return fail("local_file_write_failed", ExitLocal)
	}
	defer unix.Unlinkat(d.fd, temporary, 0)
	file := os.NewFile(uintptr(fd), temporary)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return fail("local_file_write_failed", ExitLocal)
	}
	if replace {
		// Existing targets must satisfy the same regular-file/owner checks.
		var existing any
		if err := d.read(name, &existing); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := unix.Renameat(d.fd, temporary, d.fd, name); err != nil {
			return fail("local_file_write_failed", ExitLocal)
		}
	} else {
		// linkat is an atomic no-replace publication, including against symlinks.
		if err := unix.Linkat(d.fd, temporary, d.fd, name, 0); err != nil {
			return fail("local_file_already_exists_or_unwritable", ExitConflict)
		}
		if err := unix.Unlinkat(d.fd, temporary, 0); err != nil {
			return fail("local_file_write_failed", ExitLocal)
		}
	}
	if err := unix.Fsync(d.fd); err != nil {
		return fail("local_file_write_failed", ExitLocal)
	}
	return nil
}

func (d *privateDirectory) remove(name string) error {
	var existing any
	if err := d.read(name, &existing); err != nil {
		return err
	}
	if err := unix.Unlinkat(d.fd, name, 0); err != nil {
		return fail("local_file_remove_failed", ExitLocal)
	}
	if err := unix.Fsync(d.fd); err != nil {
		return fail("local_file_remove_failed", ExitLocal)
	}
	return nil
}

// Open untrusted operator input without ever blocking on FIFO/device streams.
// Symlinks to regular input files are allowed; the opened descriptor is admitted.
func readRegularInput(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fail("input_file_unavailable", ExitUsage)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Size < 0 || stat.Size > maximumLocalFile {
		return nil, fail("input_requires_bounded_regular_file", ExitUsage)
	}
	data, err := io.ReadAll(io.LimitReader(file, maximumLocalFile+1))
	if err != nil || len(data) > maximumLocalFile {
		clear(data)
		return nil, fail("input_file_unavailable", ExitUsage)
	}
	return data, nil
}

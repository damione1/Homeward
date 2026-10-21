package config

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/storage"
	"go.yaml.in/yaml/v3"
)

const (
	maxStartupSourceBytes = 1 << 20
	bootstrapLogicalPath  = "bootstrap.launch.yaml"
)

var ErrStartupSource = errors.New("configuration: startup source unavailable or invalid")
var ErrStartupSourceChanged = errors.New("configuration: workspace changed before activation")

type EnvLookup func(string) (string, bool)

type bootstrapChoiceKind uint8

const (
	bootstrapChoiceUnknown bootstrapChoiceKind = iota
	bootstrapChoiceDefault
	bootstrapChoiceExplicit
)

// BootstrapChoice separates the stable default from an already provisioned
// explicit sidecar. An explicit path is read-only to Homeward.
type BootstrapChoice struct {
	kind bootstrapChoiceKind
	path string
}

func DefaultBootstrapChoice() BootstrapChoice { return BootstrapChoice{kind: bootstrapChoiceDefault} }

func ExistingBootstrap(path string) (BootstrapChoice, error) {
	if path == "" || strings.ContainsRune(path, '\x00') {
		return BootstrapChoice{}, ErrStartupSource
	}
	return BootstrapChoice{kind: bootstrapChoiceExplicit, path: path}, nil
}

type BootstrapOrigin uint8

const (
	BootstrapOriginUnknown BootstrapOrigin = iota
	BootstrapOriginLegacyCombined
	BootstrapOriginDefaultSidecar
	BootstrapOriginExplicitSidecar
)

// StartupInputs has no constructor from a parsed Config. A production startup
// must carry a physical locator or a deliberately detached test source.
type StartupInputs struct {
	bootstrapRaw  []byte
	bootstrapPath string
	origin        BootstrapOrigin
	workspacePath string
	logicalPath   string
	detachedRaw   []byte
	launch        Config
	lookup        EnvLookup
	valid         bool
}

func (s StartupInputs) String() string               { return "[REDACTED STARTUP INPUTS]" }
func (s StartupInputs) GoString() string             { return s.String() }
func (s StartupInputs) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

func (s StartupInputs) Valid() bool {
	if !s.valid || s.lookup == nil || len(s.bootstrapRaw) == 0 || len(s.bootstrapRaw) > maxStartupSourceBytes ||
		s.origin == BootstrapOriginUnknown || s.logicalPath == "" {
		return false
	}
	if s.workspacePath == "" && len(s.detachedRaw) == 0 {
		return false
	}
	return s.launch.listen.IsValid() && s.launch.postgres.Valid() && s.launch.dataDir != "" &&
		s.launch.public.valid && s.launch.recorder.Valid() && s.launch.backupProviders.Valid()
}

func (s StartupInputs) LaunchConfig() (Config, error) {
	if !s.Valid() {
		return Config{}, ErrUninitialized
	}
	return s.launch, nil
}

func (s StartupInputs) Origin() BootstrapOrigin { return s.origin }

func (s StartupInputs) BootstrapPath() (string, bool) {
	return s.bootstrapPath, s.Valid() && s.bootstrapPath != ""
}

type BootstrapSourceSnapshot struct {
	raw  []byte
	path string
	hash [sha256.Size]byte
}

func (b BootstrapSourceSnapshot) Valid() bool {
	return len(b.raw) > 0 && len(b.raw) <= maxStartupSourceBytes && sha256.Sum256(b.raw) == b.hash
}
func (b BootstrapSourceSnapshot) Bytes() []byte             { return bytes.Clone(b.raw) }
func (b BootstrapSourceSnapshot) Path() string              { return b.path }
func (b BootstrapSourceSnapshot) SHA256() [sha256.Size]byte { return b.hash }
func (b BootstrapSourceSnapshot) String() string            { return "[REDACTED BOOTSTRAP SOURCE]" }
func (b BootstrapSourceSnapshot) GoString() string          { return b.String() }
func (b BootstrapSourceSnapshot) MarshalJSON() ([]byte, error) {
	return json.Marshal(b.String())
}

func (s StartupInputs) BootstrapSource() (BootstrapSourceSnapshot, error) {
	if !s.Valid() {
		return BootstrapSourceSnapshot{}, ErrUninitialized
	}
	raw := bytes.Clone(s.bootstrapRaw)
	return BootstrapSourceSnapshot{raw: raw, path: bootstrapLogicalPath, hash: sha256.Sum256(raw)}, nil
}

// ConfirmBootstrapSource rejects a path-backed sidecar changed since startup
// selection. Detached fixtures have no physical file to compare.
func (s StartupInputs) ConfirmBootstrapSource() error {
	if !s.Valid() {
		return ErrStartupSource
	}
	if s.bootstrapPath == "" {
		return nil
	}
	current, err := readStartupFile(s.bootstrapPath, true)
	if err != nil || !bytes.Equal(current, s.bootstrapRaw) {
		return ErrStartupSourceChanged
	}
	return nil
}

type ExactWorkspaceSource struct {
	raw          []byte
	hash         [sha256.Size]byte
	logicalPath  string
	physicalPath string
	operational  Config
}

func (s ExactWorkspaceSource) Valid() bool {
	if len(s.raw) == 0 || len(s.raw) > maxStartupSourceBytes || sha256.Sum256(s.raw) != s.hash {
		return false
	}
	_, err := parser.NewSourceFile(s.logicalPath, nil)
	return err == nil
}
func (s ExactWorkspaceSource) Bytes() []byte             { return bytes.Clone(s.raw) }
func (s ExactWorkspaceSource) SHA256() [sha256.Size]byte { return s.hash }
func (s ExactWorkspaceSource) LogicalPath() string       { return s.logicalPath }
func (s ExactWorkspaceSource) PhysicalPath() (string, bool) {
	return s.physicalPath, s.Valid() && s.physicalPath != ""
}
func (s ExactWorkspaceSource) OperationalConfig() (Config, error) {
	if !s.Valid() {
		return Config{}, ErrUninitialized
	}
	return s.operational, nil
}
func (s ExactWorkspaceSource) String() string               { return "[REDACTED WORKSPACE SOURCE]" }
func (s ExactWorkspaceSource) GoString() string             { return s.String() }
func (s ExactWorkspaceSource) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// ConfirmPhysicalSource is called at the path-backed CAS boundary. A detached
// source deliberately has no filesystem authority and cannot call this port.
func (s ExactWorkspaceSource) ConfirmPhysicalSource() error {
	if !s.Valid() || s.physicalPath == "" {
		return ErrStartupSource
	}
	current, err := readStartupFile(s.physicalPath, false)
	if err != nil || !bytes.Equal(current, s.raw) {
		return ErrStartupSourceChanged
	}
	return nil
}

func (s StartupInputs) CaptureWorkspace() (ExactWorkspaceSource, error) {
	if !s.Valid() {
		return ExactWorkspaceSource{}, ErrUninitialized
	}
	var raw []byte
	var err error
	if s.workspacePath == "" {
		raw = bytes.Clone(s.detachedRaw)
	} else {
		raw, err = readStartupFile(s.workspacePath, false)
		if err != nil {
			return ExactWorkspaceSource{}, err
		}
	}
	operational, err := ParseOperationalWithEnv(raw, s.lookup)
	if err != nil {
		return ExactWorkspaceSource{}, err
	}
	return ExactWorkspaceSource{raw: bytes.Clone(raw), hash: sha256.Sum256(raw), logicalPath: s.logicalPath,
		physicalPath: s.workspacePath, operational: operational}, nil
}

// LoadStartupPaths chooses the database locator before touching the possibly
// dirty workspace. Only a missing default sidecar falls back to a valid legacy
// combined file; an explicit sidecar must already exist.
func LoadStartupPaths(configPath string, choice BootstrapChoice, lookup EnvLookup) (StartupInputs, error) {
	if configPath == "" || strings.ContainsRune(configPath, '\x00') || lookup == nil ||
		(choice.kind != bootstrapChoiceDefault && choice.kind != bootstrapChoiceExplicit) {
		return StartupInputs{}, ErrStartupSource
	}
	configAbs, err := filepath.Abs(filepath.Clean(configPath))
	if err != nil {
		return StartupInputs{}, ErrStartupSource
	}
	logical := filepath.Base(configAbs)
	if _, err := parser.NewSourceFile(logical, nil); err != nil {
		return StartupInputs{}, ErrStartupSource
	}
	bootstrapPath := configAbs + ".bootstrap.yaml"
	if choice.kind == bootstrapChoiceExplicit {
		bootstrapPath, err = filepath.Abs(filepath.Clean(choice.path))
		if err != nil {
			return StartupInputs{}, ErrStartupSource
		}
	}
	s := StartupInputs{bootstrapPath: bootstrapPath, workspacePath: configAbs, logicalPath: logical, lookup: lookup}
	bootstrapRaw, err := readStartupFile(bootstrapPath, true)
	if err == nil {
		s.launch, err = ParseBootstrapLaunchWithEnv(bootstrapRaw, lookup)
		if err != nil {
			return StartupInputs{}, err
		}
		s.bootstrapRaw = bytes.Clone(bootstrapRaw)
		if choice.kind == bootstrapChoiceExplicit {
			s.origin = BootstrapOriginExplicitSidecar
		} else {
			s.origin = BootstrapOriginDefaultSidecar
		}
		s.valid = true
		return s, nil
	}
	if choice.kind == bootstrapChoiceExplicit || !errors.Is(err, os.ErrNotExist) {
		return StartupInputs{}, ErrStartupSource
	}
	combined, err := readStartupFile(configAbs, false)
	if err != nil {
		return StartupInputs{}, err
	}
	s.launch, s.bootstrapRaw, err = admittedCombinedLaunch(combined, lookup)
	if err != nil {
		return StartupInputs{}, err
	}
	if _, err := ParseBootstrapLaunchWithEnv(s.bootstrapRaw, lookup); err != nil {
		return StartupInputs{}, err
	}
	s.origin, s.valid = BootstrapOriginLegacyCombined, true
	return s, nil
}

// NewDetachedStartup is a trusted Go fixture seam. It never claims a path
// CAS or writes a sidecar; production must use LoadStartupPaths.
func NewDetachedStartup(raw []byte, logicalPath string, bootstrapRaw []byte, lookup EnvLookup) (StartupInputs, error) {
	if lookup == nil || len(raw) == 0 || len(raw) > maxStartupSourceBytes || len(bootstrapRaw) > maxStartupSourceBytes {
		return StartupInputs{}, ErrStartupSource
	}
	if _, err := parser.NewSourceFile(logicalPath, nil); err != nil {
		return StartupInputs{}, ErrStartupSource
	}
	s := StartupInputs{workspacePath: "", logicalPath: logicalPath, detachedRaw: bytes.Clone(raw), lookup: lookup}
	legacyValidated := false
	if len(bootstrapRaw) == 0 {
		var err error
		s.launch, s.bootstrapRaw, err = admittedCombinedLaunch(raw, lookup)
		if err != nil {
			return StartupInputs{}, err
		}
		s.origin = BootstrapOriginLegacyCombined
		legacyValidated = true
	} else {
		s.bootstrapRaw = bytes.Clone(bootstrapRaw)
		s.origin = BootstrapOriginExplicitSidecar
	}
	parsedLaunch, err := ParseBootstrapLaunchWithEnv(s.bootstrapRaw, lookup)
	if err != nil {
		return StartupInputs{}, err
	}
	s.launch = parsedLaunch
	if !legacyValidated {
		if _, err := ParseOperationalWithEnv(raw, lookup); err != nil {
			return StartupInputs{}, err
		}
	}
	s.valid = true
	return s, nil
}

func admittedCombinedLaunch(combined []byte, lookup EnvLookup) (Config, []byte, error) {
	raw, err := decodeRawConfig(combined)
	if err != nil {
		return Config{}, nil, err
	}
	launch, err := parseLaunch(raw, lookup)
	if err != nil {
		return Config{}, nil, err
	}
	if _, err := parseOperational(raw); err != nil {
		return Config{}, nil, err
	}
	sidecar, err := sidecarFromRaw(raw)
	return launch, sidecar, err
}

func sidecarFromRaw(raw rawConfig) ([]byte, error) {
	// Only raw launch inputs are encoded. No parsed Config or resolved Settings
	// can become a sidecar, so the password value never enters this document.
	if raw.Listen == "" || raw.DataDir == "" {
		return nil, ErrStartupSource
	}
	launch := struct {
		Listen   string                  `yaml:"listen"`
		Postgres storage.ConnectionInput `yaml:"postgres"`
		DataDir  string                  `yaml:"data_dir"`
		Public   *publicInput            `yaml:"public,omitempty"`
		Recorder *RecorderInput          `yaml:"recorder,omitempty"`
		Backups  yaml.Node               `yaml:"backups,omitempty"`
	}{raw.Listen, raw.Postgres, raw.DataDir, raw.Public, raw.Recorder, raw.Backups}
	data, err := yaml.Marshal(launch)
	if err != nil || len(data) == 0 || len(data) > maxStartupSourceBytes {
		return nil, ErrStartupSource
	}
	return data, nil
}

func readStartupFile(path string, strictSidecar bool) ([]byte, error) {
	if path == "" || !filepath.IsAbs(path) || strings.ContainsRune(path, '\x00') {
		return nil, ErrStartupSource
	}
	var f *os.File
	var err error
	if strictSidecar {
		parent := filepath.Dir(path)
		parentInfo, statErr := os.Lstat(parent)
		if statErr != nil {
			return nil, statErr
		}
		if !parentInfo.IsDir() || parentInfo.Mode().Perm()&0022 != 0 || !trustedStartupOwner(parentInfo) {
			return nil, ErrStartupSource
		}
		root, openErr := os.OpenRoot(parent)
		if openErr != nil {
			return nil, ErrStartupSource
		}
		defer root.Close()
		dir, openErr := root.Open(".")
		if openErr != nil {
			return nil, ErrStartupSource
		}
		openedParent, statErr := dir.Stat()
		_ = dir.Close()
		if statErr != nil || !os.SameFile(parentInfo, openedParent) {
			return nil, ErrStartupSource
		}
		f, err = root.OpenFile(filepath.Base(path), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	} else {
		f, err = os.Open(path)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxStartupSourceBytes {
		return nil, ErrStartupSource
	}
	if strictSidecar && (info.Mode().Perm()&0022 != 0 || !trustedStartupOwner(info)) {
		return nil, ErrStartupSource
	}
	data, err := io.ReadAll(io.LimitReader(f, maxStartupSourceBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxStartupSourceBytes {
		return nil, ErrStartupSource
	}
	return data, nil
}

func trustedStartupOwner(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && (stat.Uid == uint32(os.Geteuid()) || stat.Uid == 0)
}

// PublishDefaultBootstrap durably publishes only a missing default sidecar.
// SQL remains the source authority after connection; this is a prerequisite
// for reaching that database on restart, not an FS+SQL atomic transaction.
func (s StartupInputs) PublishDefaultBootstrap() error {
	if !s.Valid() || s.origin != BootstrapOriginLegacyCombined || s.bootstrapPath == "" || s.workspacePath == "" {
		return ErrStartupSource
	}
	parent := filepath.Dir(s.bootstrapPath)
	name := filepath.Base(s.bootstrapPath)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0022 != 0 || !trustedStartupOwner(info) {
		return ErrStartupSource
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return ErrStartupSource
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return ErrStartupSource
	}
	defer dir.Close()
	opened, err := dir.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return ErrStartupSource
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return ErrStartupSource
	}
	tmp := ".bootstrap.pending-" + hex.EncodeToString(token[:])
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return ErrStartupSource
	}
	defer root.Remove(tmp)
	written, writeErr := f.Write(s.bootstrapRaw)
	if written != len(s.bootstrapRaw) && writeErr == nil {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		return ErrStartupSource
	}
	if err := root.Link(tmp, name); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return ErrStartupSource
		}
		existing, readErr := readStartupFile(s.bootstrapPath, true)
		if readErr != nil || !bytes.Equal(existing, s.bootstrapRaw) {
			return ErrStartupSourceChanged
		}
	}
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("%w: sidecar durability", ErrStartupSource)
	}
	return nil
}

// PublishDefaultBootstrapIfNeeded is idempotent for an already selected
// sidecar. Only the valid legacy-combined path can publish a new default.
func (s StartupInputs) PublishDefaultBootstrapIfNeeded() error {
	if !s.Valid() {
		return ErrStartupSource
	}
	if s.origin != BootstrapOriginLegacyCombined || s.workspacePath == "" {
		return nil
	}
	return s.PublishDefaultBootstrap()
}

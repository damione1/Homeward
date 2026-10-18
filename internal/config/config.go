// Package config validates configuration before the composition root opens resources.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/damione1/Homeward/internal/storage"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	listen   netip.AddrPort
	postgres storage.Settings
	dataDir  string
}

var ErrUninitialized = errors.New("configuration: uninitialized")
var ErrInvalidYAML = errors.New("configuration: invalid YAML or unknown field")

func Parse(data []byte) (Config, error) { return ParseWithEnv(data, os.LookupEnv) }

func ParseWithEnv(data []byte, lookup func(string) (string, bool)) (Config, error) {
	// Keep the unconstrained YAML representation at the parsing boundary.
	var raw struct {
		Listen   string                  `yaml:"listen"`
		Postgres storage.ConnectionInput `yaml:"postgres"`
		DataDir  string                  `yaml:"data_dir"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return Config{}, ErrInvalidYAML
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return Config{}, fmt.Errorf("configuration: exactly one YAML document required")
	}
	endpoint, err := netip.ParseAddrPort(raw.Listen)
	// Preserve the bootstrap contract: numeric IP, numeric uint16 port, no zone.
	if err != nil || endpoint.Addr().Zone() != "" {
		return Config{}, fmt.Errorf("configuration: listen must be a numeric IP:port without a zone")
	}
	postgres, err := storage.ParseSettings(raw.Postgres, lookup)
	if err != nil {
		return Config{}, fmt.Errorf("configuration: %w", err)
	}
	if !filepath.IsAbs(raw.DataDir) || filepath.Clean(raw.DataDir) != raw.DataDir || raw.DataDir == "/" || strings.ContainsRune(raw.DataDir, '\x00') {
		return Config{}, fmt.Errorf("configuration: data_dir must be an absolute normalized persistent directory")
	}
	return Config{listen: endpoint, postgres: postgres, dataDir: raw.DataDir}, nil
}

func (c Config) DataDir() (string, error) {
	if c.dataDir == "" {
		return "", ErrUninitialized
	}
	return c.dataDir, nil
}

func (c Config) PostgresSettings() (storage.Settings, error) {
	if !c.postgres.Valid() {
		return storage.Settings{}, ErrUninitialized
	}
	return c.postgres, nil
}

// ListenEndpoint returns a value copy. Only an uninitialized Config can fail;
// parsed configurations carry their invariant without reparsing at each consumer.
func (c Config) ListenEndpoint() (netip.AddrPort, error) {
	if !c.listen.IsValid() {
		return netip.AddrPort{}, ErrUninitialized
	}
	return c.listen, nil
}

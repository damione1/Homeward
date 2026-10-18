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

	"github.com/damione1/Homeward/internal/mqtt"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/worker"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	listen   netip.AddrPort
	postgres storage.Settings
	dataDir  string
	mqtt     *mqtt.Settings
	workers  []worker.Config
	public   PublicSettings
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
		MQTT     *mqtt.Input             `yaml:"mqtt"`
		Public   *publicInput            `yaml:"public"`
		Workers  []struct {
			Instance    string   `yaml:"instance"`
			Executable  string   `yaml:"executable"`
			Args        []string `yaml:"args"`
			SecretNames []string `yaml:"secret_names"`
			Bindings    []struct {
				Device string `yaml:"device"`
				Entity string `yaml:"entity"`
				Alias  string `yaml:"alias"`
			} `yaml:"bindings"`
		} `yaml:"external_workers"`
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
	result := Config{listen: endpoint, postgres: postgres, dataDir: raw.DataDir}
	public, err := parsePublicSettings(raw.Public)
	if err != nil {
		return Config{}, fmt.Errorf("configuration: %w", err)
	}
	result.public = public
	if raw.MQTT != nil {
		settings, err := mqtt.ParseSettings(*raw.MQTT)
		if err != nil {
			return Config{}, fmt.Errorf("configuration: %w", err)
		}
		result.mqtt = &settings
	}
	if len(raw.Workers) > 16 {
		return Config{}, ErrInvalidYAML
	}
	instances := make(map[string]bool, len(raw.Workers))
	for _, input := range raw.Workers {
		if instances[input.Instance] {
			return Config{}, ErrInvalidYAML
		}
		instances[input.Instance] = true
		configured, err := worker.NewConfig(input.Instance, input.Executable, input.Args, input.SecretNames)
		if err != nil {
			return Config{}, fmt.Errorf("configuration: %w", err)
		}
		bindings := make([]worker.ConfiguredBinding, 0, len(input.Bindings))
		for _, item := range input.Bindings {
			binding, err := worker.NewConfiguredBinding(item.Device, item.Entity, item.Alias)
			if err != nil {
				return Config{}, fmt.Errorf("configuration: %w", err)
			}
			bindings = append(bindings, binding)
		}
		configured, err = configured.WithBindings(bindings)
		if err != nil {
			return Config{}, fmt.Errorf("configuration: %w", err)
		}
		result.workers = append(result.workers, configured)
	}
	return result, nil
}

func (c Config) ExternalWorkers() []worker.Config { return append([]worker.Config(nil), c.workers...) }

func (c Config) PublicTransport() (PublicSettings, error) {
	if !c.public.valid {
		return PublicSettings{}, ErrUninitialized
	}
	return c.public, nil
}

func (c Config) MQTTSettings() (mqtt.Settings, bool) {
	if c.mqtt == nil {
		return mqtt.Settings{}, false
	}
	return *c.mqtt, true
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

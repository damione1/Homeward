// Package config validates configuration before the composition root opens resources.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	listen netip.AddrPort
}

var ErrUninitialized = errors.New("configuration: uninitialized listen endpoint")

func Parse(data []byte) (Config, error) {
	// Keep the unconstrained YAML representation at the parsing boundary.
	var raw struct {
		Listen string `yaml:"listen"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("configuration: %w", err)
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
	return Config{listen: endpoint}, nil
}

// ListenEndpoint returns a value copy. Only an uninitialized Config can fail;
// parsed configurations carry their invariant without reparsing at each consumer.
func (c Config) ListenEndpoint() (netip.AddrPort, error) {
	if !c.listen.IsValid() {
		return netip.AddrPort{}, ErrUninitialized
	}
	return c.listen, nil
}

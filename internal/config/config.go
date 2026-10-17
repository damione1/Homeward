// Package config validates configuration before the composition root opens resources.
package config

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	Listen string `yaml:"listen"`
}

func Parse(data []byte) (Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("configuration: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("configuration: exactly one YAML document required")
	}
	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}
func (c Config) Validate() error {
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil || net.ParseIP(host) == nil || port == "" {
		return fmt.Errorf("configuration: listen must be a numeric IP:port")
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("configuration: invalid port: %w", err)
	}
	return nil
}

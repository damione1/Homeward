// Package worker adapts one supervised local process to the session Host.
package worker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/storage"
)

var ErrInvalidConfig = errors.New("worker: invalid local worker configuration")
var ErrUnavailable = errors.New("worker: session or process unavailable")
var ErrCapacity = errors.New("worker: session capacity exhausted")
var ErrWorkerExit = errors.New("worker: process exited")
var ErrHeartbeat = errors.New("worker: heartbeat lease expired")
var ErrCommandFailed = errors.New("worker: command failed")
var ErrJoinTimeout = errors.New("worker: process join timed out")

type Config struct {
	instance    domain.IntegrationInstanceID
	executable  string
	args        []string
	secretNames map[string]struct{}
}

func NewConfig(instanceText, executable string, args, secretNames []string) (Config, error) {
	instance, err := domain.ParseIntegrationInstanceID(instanceText)
	if err != nil || !filepath.IsAbs(executable) || filepath.Clean(executable) != executable || strings.ContainsRune(executable, '\x00') || len(args) > 16 || len(secretNames) > 32 {
		return Config{}, ErrInvalidConfig
	}
	info, err := os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return Config{}, ErrInvalidConfig
	}
	for _, arg := range args {
		if len(arg) > 4096 || strings.ContainsRune(arg, '\x00') {
			return Config{}, ErrInvalidConfig
		}
	}
	names := make(map[string]struct{}, len(secretNames))
	for _, raw := range secretNames {
		if _, err := storage.ParseSecretName(raw); err != nil {
			return Config{}, ErrInvalidConfig
		}
		if _, exists := names[raw]; exists {
			return Config{}, ErrInvalidConfig
		}
		names[raw] = struct{}{}
	}
	return Config{instance: instance, executable: executable, args: append([]string(nil), args...), secretNames: names}, nil
}

func (c Config) Instance() (domain.IntegrationInstanceID, error) {
	if _, err := c.instance.Value(); err != nil || c.executable == "" {
		return domain.IntegrationInstanceID{}, ErrInvalidConfig
	}
	return c.instance, nil
}

func NewPilotSpec(c Config) (integrations.PilotSpec, error) {
	instance, err := c.Instance()
	if err != nil {
		return integrations.PilotSpec{}, err
	}
	return integrations.NewPilotSpec(instance, func() integrations.Pilot { return newPilot(c) })
}

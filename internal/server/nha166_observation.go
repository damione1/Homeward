package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/damione1/Homeward/internal/backups"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/helpers"
)

var errReadOnlyObservationScope = errors.New("observation requires an isolated strictly passive MQTT-only source configuration")

// Validation precedes every provider, runtime factory and pilot construction.
// Persisted automation/script/scene documents are retained for reads, but their
// executable source compilation is deliberately omitted by the Run assembly.
func validateMQTTReadOnlyObservation(c, launch config.Config, admitted configactivation.AdmittedGeneration, factory RuntimeFactory) error {
	settings, enabled := c.MQTTSettings()
	_, neviweb := c.NeviwebSettings()
	_, hilo := c.HiloSettings()
	_, ecobee := c.EcobeeSettings()
	_, remote := c.RemoteCalendarSettings()
	_, solar := c.SolarSettings()
	_, workday := c.WorkdaySettings()
	if factory != nil || !enabled || !settings.StrictlyPassive() || neviweb || hilo || ecobee || remote || solar || workday || len(c.ExternalWorkers()) != 0 || len(c.EnergyBindings()) != 0 || c.DerivedPowerProfile().Enabled() {
		return errReadOnlyObservationScope
	}
	if candidate, present := admitted.LegacyCandidate(); present {
		definitions, err := helpers.Definitions(candidate)
		if err != nil || len(definitions) != 0 {
			return errReadOnlyObservationScope
		}
	}
	providers, err := launch.BackupProviders()
	if err != nil {
		return errReadOnlyObservationScope
	}
	for _, provider := range providers.Instances() {
		if provider.Enabled() && provider.Kind() != backups.Local {
			return errReadOnlyObservationScope
		}
	}
	return nil
}

type readOnlyObservationBinding struct {
	executable, manifest, source string
	revision                     uint64
}

func newReadOnlyObservationBinding(admitted configactivation.AdmittedGeneration) (*readOnlyObservationBinding, error) {
	path, err := os.Executable()
	if err != nil {
		return nil, errReadOnlyObservationScope
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errReadOnlyObservationScope
	}
	digest := sha256.New()
	_, copyErr := io.Copy(digest, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return nil, errReadOnlyObservationScope
	}
	manifest, source := admitted.ManifestSHA256(), admitted.SourceSHA256()
	return &readOnlyObservationBinding{hex.EncodeToString(digest.Sum(nil)), hex.EncodeToString(manifest[:]), hex.EncodeToString(source[:]), uint64(admitted.Revision().Number())}, nil
}

// Only GetHealth's authorized READ response calls this. No credential, topic,
// endpoint, alias or state value enters this immutable evidence binding.
func (b *readOnlyObservationBinding) headers(header http.Header) {
	header.Set("Homeward-Observation-Profile", "mqtt-passive-readonly-v1")
	header.Set("Homeward-Executable-SHA256", b.executable)
	header.Set("Homeward-Manifest-SHA256", b.manifest)
	header.Set("Homeward-Source-SHA256", b.source)
	if raw, err := os.ReadFile("/proc/self/statm"); err == nil {
		fields := strings.Fields(string(raw))
		if len(fields) > 1 {
			if pages, err := strconv.ParseUint(fields[1], 10, 64); err == nil && pages <= ^uint64(0)/uint64(os.Getpagesize()) {
				header.Set("Homeward-Process-RSS-Bytes", strconv.FormatUint(pages*uint64(os.Getpagesize()), 10))
			}
		}
	}
	var usage syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) == nil {
		micros := usage.Utime.Sec*1_000_000 + int64(usage.Utime.Usec) + usage.Stime.Sec*1_000_000 + int64(usage.Stime.Usec)
		if micros >= 0 {
			header.Set("Homeward-Process-CPU-Microseconds", strconv.FormatInt(micros, 10))
		}
	}

}

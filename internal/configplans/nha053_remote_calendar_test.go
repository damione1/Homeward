package configplans

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/registry"
)

func nha053RemoteSource(url, zone string) string {
	return fmt.Sprintf("remote_calendar:\n  label: NHA053 synthetic calendar\n  url: %q\n  view_timezone: %s\n", url, zone)
}

func TestNHA053RemoteCalendarSemanticChangesKeepBootstrapNonOperational(t *testing.T) {
	first := nha053Candidate(t, Operational, "operational.main", nha053RemoteSource("https://calendar-a.invalid/calendar.ics?private=URL_CANARY_A", "America/Toronto"))
	second := nha053Candidate(t, Operational, "operational.main", nha053RemoteSource("https://calendar-b.invalid/calendar.ics?private=URL_CANARY_B", "America/Toronto"))
	before, after := nha053Compile(t, first), nha053Compile(t, second)
	changes := difference(before, after)
	if first.SourceSHA256() == second.SourceSHA256() || before.digest == after.digest || len(changes) != 1 || changes[0].Field() != "remote_calendar" || changes[0].Effect() != ConfigurationEffect || changes[0].ReferencesAdded() != 0 || changes[0].ReferencesRemoved() != 0 {
		t.Fatal("changed remote resource lost exact/semantic distinction or became dashboard/rights/action effect")
	}
	if !before.refs["calendar.famille"] || !after.refs["calendar.famille"] {
		t.Fatal("remote source identity was not checked against the actual manifest")
	}
	zoneChanged := nha053Compile(t, nha053Candidate(t, Operational, "operational.main", nha053RemoteSource("https://calendar-a.invalid/calendar.ics?private=URL_CANARY_A", "UTC")))
	if before.digest == zoneChanged.digest || len(difference(before, zoneChanged)) != 1 {
		t.Fatal("real remote view-zone change lost its projection effect")
	}
	removed := difference(before, nha053Compile(t, nha053Candidate(t, Operational, "operational.main", "{}\n")))
	if len(removed) != 1 || removed[0].Field() != "remote_calendar" || removed[0].Operation() != Removed || removed[0].Effect() != ConfigurationEffect || removed[0].ReferencesRemoved() != 1 {
		t.Fatal("removing the source lost its target/configuration effect")
	}

	// Launch configuration remains an independent authority even when exact
	// historical combined YAML also contains a real operational calendar.
	ignoredA := nha053Candidate(t, Operational, "operational.main", "listen: 127.0.0.1:1234\ndata_dir: /synthetic-not-created-a\nbackup_recovery_recipient: PUBLIC_LAUNCH_A\n"+nha053RemoteSource("https://calendar-a.invalid/calendar.ics", "America/Toronto"))
	ignoredB := nha053Candidate(t, Operational, "operational.main", "listen: 127.0.0.1:5678\ndata_dir: /synthetic-not-created-b\nbackup_recovery_recipient: PUBLIC_LAUNCH_B\n"+nha053RemoteSource("https://calendar-a.invalid/calendar.ics", "America/Toronto"))
	left, right := nha053Compile(t, ignoredA), nha053Compile(t, ignoredB)
	if ignoredA.SourceSHA256() == ignoredB.SourceSHA256() || left.digest != right.digest || len(difference(left, right)) != 0 {
		t.Fatal("bootstrap bytes invented an operational calendar/configuration effect")
	}
	source := nha053RemoteSource("https://calendar-a.invalid/calendar.ics", "America/Toronto")
	if _, err := config.ParseBootstrapLaunchWithEnv([]byte(source), func(string) (string, bool) { t.Fatal("preview resolved launch environment"); return "", false }); err == nil {
		t.Fatal("bootstrap acquired remote operational source authority")
	}
}

func TestNHA053RemoteCalendarPreviewDoesNotFetchOrResolveOpaqueHandles(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	raw := nha053RemoteSource(server.URL+"/never-fetch.ics?private=QUERY_CANARY", "America/Toronto") + fmt.Sprintf("  network:\n    origin: %q\n    prefixes: ['127.0.0.1/32']\n", server.URL)
	compiled := nha053Compile(t, nha053Candidate(t, Operational, "operational.main", raw))
	if !compiled.refs["calendar.famille"] || requests.Load() != 0 {
		t.Fatal("preview started remote fetch or lost its fixed manifest target")
	}
	secret := "remote_calendar:\n  label: Opaque source\n  secret_url: nha053-absent-url\n  auth: {bearer_secret: nha053-absent-bearer}\n  view_timezone: America/Toronto\n"
	configured, err := config.ParseOperationalWithEnv([]byte(secret), func(string) (string, bool) {
		t.Fatal("operational preview resolved environment credential")
		return "", false
	})
	if err != nil {
		t.Fatal("opaque absent handle must be admitted without resolving it", err)
	}
	if settings, present := configured.RemoteCalendarSettings(); !present || !settings.Valid() {
		t.Fatal("delivered101 settings were not used")
	}
	opaque := nha053Compile(t, nha053Candidate(t, Operational, "operational.main", secret))
	if !opaque.refs["calendar.famille"] || requests.Load() != 0 {
		t.Fatal("opaque preview fetched or resolved runtime resource")
	}
	base := nha053RemoteSource("https://calendar-a.invalid/calendar.ics", "America/Toronto")
	for name, source := range map[string]string{"unknown": base + "  unknown_future_option: true\n", "forged-target": base + "  entity_id: calendar.excluded_outside_manifest\n", "duplicate": base + "  view_timezone: America/Montreal\n", "incomplete-auth": base + "  auth: {basic_username_secret: user-only}\n", "unknown-zone": strings.Replace(base, "America/Toronto", "Missing/Zone", 1)} {
		t.Run(name, func(t *testing.T) {
			if _, diagnostic := compile(nha053Candidate(t, Operational, "operational.main", source), nha053Manifest(t)); diagnostic == DiagnosticInvalid {
				t.Fatal("remote preview admitted invalid/forged source")
			}
		})
	}
	if _, diagnostic := compile(nha053Candidate(t, Operational, "operational.main", base), registry.Manifest{}); diagnostic != BaseUnavailable {
		t.Fatal("zero/unadmitted scope granted remote calendar membership")
	}
	if requests.Load() != 0 {
		t.Fatal("invalid or secret-reference preview performed I/O")
	}
}

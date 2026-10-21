package report_test

import (
	"encoding/json"
	"github.com/damione1/Homeward/compat/ha/report"
	"strings"
	"testing"
)

const nha047OfflineEmpty = `{"schema":"homeward.compatibility-report/v1","configuration":{"id":"offline-boundary-test","revision":1},"entities":[],"declarations":[],"evidence":{"schema":"nha047/v1","comparisons":[]},"candidates":[]}`

func TestNHA047OfflineReportDoesNotTurnCallerMetadataIntoEvidence(t *testing.T) {
	value, err := report.OfflineReport([]byte(nha047OfflineEmpty))
	if err != nil {
		t.Fatal(err)
	}
	if len(value.Objects()) != 17 || len(value.Deferred()) != 72 {
		t.Fatal("offline report lost selection partition")
	}
	for _, object := range value.Objects() {
		if object.Verdict() != report.Refused || object.Activatable() {
			t.Fatal("absent runtime evidence acquired compatibility or activation")
		}
	}
	encoded, err := report.JSON(value)
	if err != nil || !json.Valid(encoded) {
		t.Fatal("invalid offline render", err)
	}
}

func TestNHA047OfflineBoundaryRejectsDuplicatedUnknownAndInvalidConfiguration(t *testing.T) {
	for _, input := range []string{
		strings.Replace(nha047OfflineEmpty, `"schema":"homeward.compatibility-report/v1"`, `"schema":"homeward.compatibility-report/v1","schema":"homeward.compatibility-report/v1"`, 1),
		strings.Replace(nha047OfflineEmpty, `"revision":1`, `"revision":1,"revision":2`, 1),
		strings.Replace(nha047OfflineEmpty, `"revision":1`, `"revision":0`, 1),
		strings.Replace(nha047OfflineEmpty, `"candidates":[]`, `"candidates":[],"activate":true`, 1),
		strings.Replace(nha047OfflineEmpty, `"entities":[]`, `"entities":[{"id":"switch.basement_relay","status":255,"supports":[]}]`, 1),
		strings.Replace(nha047OfflineEmpty, `"comparisons":[]`, `"comparisons":[],"tests":["TestNHA047"]`, 1),
		nha047OfflineEmpty + ` {}`,
	} {
		if _, err := report.OfflineReport([]byte(input)); err == nil {
			t.Fatal("malformed/unsupported offline boundary admitted")
		}
	}
}

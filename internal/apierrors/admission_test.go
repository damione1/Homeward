package apierrors

import (
	"encoding/hex"
	"os"
	"strings"
	"testing"

	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/proto"
)

func TestStatusAdmissionSharedGoRustCorpus(t *testing.T) {
	data, err := os.ReadFile("../../contracts/api_error_cases.tsv")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 5 {
			t.Fatal("invalid corpus shape")
		}
		t.Run(parts[0], func(t *testing.T) {
			encoded, err := hex.DecodeString(parts[1])
			if err != nil {
				t.Fatal(err)
			}
			var status statuspb.Status
			err = proto.Unmarshal(encoded, &status)
			var admitted Fault
			if err == nil {
				admitted, err = AdmitStatus(&status)
			}
			if (err == nil) != (parts[2] == "true") {
				t.Fatalf("admission disagrees with shared corpus: %v", err)
			}
			if err == nil && strings.TrimPrefix(admitted.Reason().String(), "API_ERROR_REASON_") != parts[3] {
				t.Fatal("admitted reason changed")
			}
			if err == nil && strings.Contains(admitted.Error(), "CANARY") {
				t.Fatal("untrusted diagnostic entered admitted fault")
			}
		})
	}
}
